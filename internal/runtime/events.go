package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/settings"
	"github.com/yty-build/acp-gateway/internal/task"
)

type turnCorrelation struct {
	requestID string
	turnID    string
	mode      connector.Mode
}

type managerOperation struct {
	mu     sync.Mutex
	entry  *liveManager
	failed bool
}

func newManagerOperation() *managerOperation {
	return &managerOperation{}
}

func (operation *managerOperation) fail(supervisor *Supervisor) {
	operation.mu.Lock()
	operation.failed = true
	entry := operation.entry
	operation.mu.Unlock()
	if entry != nil {
		supervisor.signalFatal(entry)
	}
}

func (operation *managerOperation) adopt(supervisor *Supervisor, entry *liveManager) bool {
	operation.mu.Lock()
	operation.entry = entry
	failed := operation.failed
	operation.mu.Unlock()
	if failed {
		supervisor.signalFatal(entry)
	}
	return !failed
}

type childEventSink struct {
	supervisor *Supervisor
	managerID  session.ID
	operation  *managerOperation
}

func (sink childEventSink) ChildEvent(ctx context.Context, event connector.ChildEvent) error {
	if sink.supervisor == nil || sink.operation == nil {
		return ErrInvalidRequest
	}
	if err := sink.supervisor.childEvent(ctx, sink.managerID, event); err != nil {
		sink.operation.fail(sink.supervisor)
		return err
	}
	return nil
}

func (supervisor *Supervisor) childEvent(ctx context.Context, boundManager session.ID, event connector.ChildEvent) error {
	if ctx == nil || ctx.Err() != nil || validateChildEventEnvelope(boundManager, event) != nil {
		return ErrInvalidRequest
	}
	managerID, _ := session.ParseID(event.ParentSessionID)
	childID, _ := session.ParseID(event.ChildSessionID)
	providerID := settings.ProviderID(event.Provider)

	supervisor.deliveryMu.Lock()
	supervisor.mu.Lock()
	deliveries, err := supervisor.childEventLocked(managerID, childID, providerID, event)
	supervisor.mu.Unlock()
	var waits []viewerDeliveryWait
	if err == nil {
		waits, err = supervisor.deliver(deliveries)
	}
	supervisor.deliveryMu.Unlock()
	if err == nil {
		err = supervisor.waitViewerDeliveries(waits)
	}
	return err
}

func (supervisor *Supervisor) childEventLocked(
	managerID session.ID,
	childID session.ID,
	providerID settings.ProviderID,
	event connector.ChildEvent,
) ([]eventDelivery, error) {
	if supervisor.closed {
		return nil, ErrClosed
	}
	managerSession, ok := supervisor.sessions[managerID]
	if !ok || managerSession.Role != session.RoleManager || managerSession.ParentID != nil ||
		(managerSession.State != session.StateStarting && managerSession.State != session.StateActive) {
		return nil, ErrInvalidState
	}
	catalog, ok := supervisor.catalog[providerID]
	if !ok || !catalog.profile.Capabilities.Subordinate || catalog.profile.Children == nil {
		return nil, ErrUnsupported
	}

	child, exists := supervisor.sessions[childID]
	if !exists {
		if event.Lifecycle != "prompt_pending" || len(event.NativeReference) == 0 ||
			len(supervisor.sessions) >= MaxSessions {
			return nil, ErrInvalidRequest
		}
		if _, err := supervisor.providerEligibleLocked(providerID, settings.CapabilitySubordinate, true); err != nil {
			return nil, err
		}
		if _, busy := supervisor.turns[childID]; busy {
			return nil, ErrConflict
		}
		stamp := supervisor.nextTimeLocked(managerSession.LastActiveAt)
		created, err := session.NewSubordinate(
			childID, managerSession.TaskID, managerSession.ID, "", string(providerID),
			event.NativeReference, session.StateActive, stamp,
		)
		if err != nil {
			return nil, ErrInvalidRequest
		}
		if err := supervisor.createSessionVerified(created); err != nil {
			return nil, err
		}
		supervisor.sessions[childID] = created.Clone()
		supervisor.modes[childID] = event.Mode
		supervisor.turns[childID] = turnCorrelation{
			requestID: event.RequestID, turnID: event.TurnID, mode: event.Mode,
		}
		return supervisor.prepareSessionLocked(created)
	}

	if session.ValidateHierarchy(child, managerSession) != nil || child.ProviderID != string(providerID) ||
		child.State == session.StateClosed {
		return nil, ErrInvalidRequest
	}
	if len(event.NativeReference) != 0 && !bytes.Equal(event.NativeReference, child.ProviderReference) {
		return nil, ErrInvalidRequest
	}
	switch event.Lifecycle {
	case "prompt_pending":
		if _, busy := supervisor.turns[childID]; busy {
			return nil, ErrBusy
		}
		if _, err := supervisor.providerEligibleLocked(providerID, settings.CapabilitySubordinate, false); err != nil {
			return nil, err
		}
		stamp := supervisor.nextTimeLocked(child.LastActiveAt)
		if err := supervisor.updateSessionStateVerified(childID, session.StateActive, stamp); err != nil {
			return nil, err
		}
		child.State = session.StateActive
		child.LastActiveAt = stamp
		supervisor.sessions[childID] = child
		supervisor.modes[childID] = event.Mode
		supervisor.turns[childID] = turnCorrelation{
			requestID: event.RequestID, turnID: event.TurnID, mode: event.Mode,
		}
		return supervisor.prepareSessionLocked(child)
	case "terminal":
		turn, pending := supervisor.turns[childID]
		if !pending || turn.requestID != event.RequestID || turn.turnID != event.TurnID || turn.mode != event.Mode {
			return nil, ErrInvalidRequest
		}
		stamp := supervisor.nextTimeLocked(child.LastActiveAt)
		if err := supervisor.updateSessionStateVerified(childID, session.StateDormant, stamp); err != nil {
			return nil, err
		}
		child.State = session.StateDormant
		child.LastActiveAt = stamp
		supervisor.sessions[childID] = child
		supervisor.modes[childID] = connector.ModeAnswer
		delete(supervisor.turns, childID)
		return supervisor.prepareSessionLocked(child)
	default:
		return nil, ErrInvalidRequest
	}
}

func validateChildEventEnvelope(boundManager session.ID, event connector.ChildEvent) error {
	managerID, err := session.ParseID(event.ParentSessionID)
	if err != nil || managerID != boundManager {
		return ErrInvalidRequest
	}
	childID, err := session.ParseID(event.ChildSessionID)
	if err != nil || childID == managerID {
		return ErrInvalidRequest
	}
	if settings.ProviderID(event.Provider).Validate() != nil ||
		(event.Lifecycle != "prompt_pending" && event.Lifecycle != "terminal") ||
		(event.Mode != connector.ModeAnswer && event.Mode != connector.ModeFollow) ||
		!validCorrelation(event.RequestID) || !validCorrelation(event.TurnID) ||
		len(event.NativeReference) > session.MaxProviderReferenceBytes ||
		(len(event.NativeReference) != 0 && session.ValidateProviderReference(event.NativeReference) != nil) {
		return ErrInvalidRequest
	}
	return nil
}

func validCorrelation(value string) bool {
	if value == "" || len(value) > connector.MaxCorrelationIDBytes || !utf8.ValidString(value) ||
		strings.IndexByte(value, 0) >= 0 {
		return false
	}
	for _, current := range []byte(value) {
		if current < 0x21 || current > 0x7e {
			return false
		}
	}
	return true
}

func (supervisor *Supervisor) createSessionVerified(value session.Session) error {
	if err := supervisor.store.CreateSession(supervisor.ctx, value); err == nil {
		return nil
	}
	stored, err := supervisor.store.GetSession(supervisor.ctx, value.ID)
	if err == nil && sessionEqual(stored, value) {
		return nil
	}
	return ErrPersistence
}

func sessionEqual(left, right session.Session) bool {
	if left.ID != right.ID || left.TaskID != right.TaskID || left.Role != right.Role || left.Label != right.Label ||
		left.ProviderID != right.ProviderID || left.State != right.State ||
		!bytes.Equal(left.ProviderReference, right.ProviderReference) ||
		!left.CreatedAt.Equal(right.CreatedAt) || !left.LastActiveAt.Equal(right.LastActiveAt) {
		return false
	}
	if left.ParentID == nil || right.ParentID == nil {
		return left.ParentID == nil && right.ParentID == nil
	}
	return *left.ParentID == *right.ParentID
}

func (supervisor *Supervisor) registerViewer(id string) (*viewer, error) {
	if !validClientID(id) {
		return nil, ErrInvalidRequest
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.closed {
		return nil, ErrClosed
	}
	if len(supervisor.viewers) >= MaxSubscriberCount {
		return nil, ErrCapacity
	}
	if _, exists := supervisor.viewers[id]; exists {
		return nil, ErrConflict
	}
	value := &viewer{
		id: id, generation: supervisor.generation, floor: supervisor.sequence,
		done: make(chan struct{}), queue: make(chan Event, 1),
		deliveries:   make(chan queuedViewerEvent, viewerDeliveryBacklog),
		deliveryDone: make(chan struct{}), deliveryTimeout: viewerDeliveryStallTimeout,
		attached: make(map[task.ID]struct{}),
	}
	supervisor.viewers[id] = value
	supervisor.wg.Add(1)
	go supervisor.runViewerDelivery(value)
	return value, nil
}

func (supervisor *Supervisor) unregisterViewer(id string, expected *viewer) {
	if expected != nil {
		expected.close()
	}
	supervisor.deliveryMu.Lock()
	supervisor.mu.Lock()
	if current := supervisor.viewers[id]; current == expected {
		delete(supervisor.viewers, id)
	}
	supervisor.mu.Unlock()
	supervisor.deliveryMu.Unlock()
	if expected != nil {
		expected.waitDeliveryStop()
		expected.clearPending()
	}
}

func (value *viewer) clearPending() {
	for {
		select {
		case event := <-value.queue:
			clearEvent(&event)
		case delivery := <-value.deliveries:
			clearEvent(&delivery.event)
		default:
			return
		}
	}
}

func (value *viewer) waitDeliveryStop() {
	if value != nil && value.deliveryDone != nil {
		<-value.deliveryDone
	}
}

func validClientID(id string) bool {
	if len(id) != 32 || len(id) > MaxClientIDBytes {
		return false
	}
	for _, value := range []byte(id) {
		if !((value >= '0' && value <= '9') || (value >= 'a' && value <= 'f')) {
			return false
		}
	}
	return true
}

type eventDelivery struct {
	viewer *viewer
	event  Event
}

const (
	viewerDeliveryBacklog      = 8
	viewerDeliveryStallTimeout = 5 * time.Second
)

type queuedViewerEvent struct {
	event     Event
	delivered chan struct{}
}

type viewerDeliveryWait struct {
	viewer    *viewer
	delivered <-chan struct{}
	deadline  time.Time
}

// prepareSessionLocked allocates one global sequence while state is locked.
// The returned deliveries must be sent only after mu is released and while
// deliveryMu remains held.
func (supervisor *Supervisor) prepareSessionLocked(value session.Session) ([]eventDelivery, error) {
	public, err := supervisor.publicSessionLocked(value)
	if err != nil {
		return nil, err
	}
	supervisor.sequence++
	event := Event{
		Generation: supervisor.generation, Sequence: supervisor.sequence,
		Kind: EventSessionUpsert, TaskID: value.TaskID, SessionID: value.ID, Session: public,
	}
	result := make([]eventDelivery, 0, len(supervisor.viewers))
	for _, value := range supervisor.viewers {
		result = append(result, eventDelivery{viewer: value, event: cloneEvent(event)})
	}
	return result, nil
}

func (supervisor *Supervisor) prepareFrameLocked(managerSession session.Session, frame PublicFrame) ([]eventDelivery, error) {
	interested := make(map[*viewer]bool, len(supervisor.viewers))
	anyAttached := false
	for _, value := range supervisor.viewers {
		_, attached := value.attached[managerSession.TaskID]
		interested[value] = attached
		anyAttached = anyAttached || attached
	}
	if !anyAttached {
		return nil, nil
	}
	public, err := supervisor.publicSessionLocked(managerSession)
	if err != nil {
		return nil, err
	}
	supervisor.sequence++
	sequence := supervisor.sequence
	frameEvent := Event{
		Generation: supervisor.generation, Sequence: sequence,
		Kind: EventPublicFrame, TaskID: managerSession.TaskID, SessionID: managerSession.ID,
		Frame: clonePublicFrame(frame),
	}
	placeholder := Event{
		Generation: supervisor.generation, Sequence: sequence,
		Kind: EventSessionUpsert, TaskID: managerSession.TaskID, SessionID: managerSession.ID,
		Session: public,
	}
	result := make([]eventDelivery, 0, len(supervisor.viewers))
	for _, value := range supervisor.viewers {
		selected := placeholder
		if interested[value] {
			selected = frameEvent
		}
		result = append(result, eventDelivery{viewer: value, event: cloneEvent(selected)})
	}
	return result, nil
}

func (supervisor *Supervisor) runViewerDelivery(value *viewer) {
	defer supervisor.wg.Done()
	defer close(value.deliveryDone)
	defer value.clearPending()
	for {
		select {
		case delivery := <-value.deliveries:
			select {
			case value.queue <- delivery.event:
				delivery.event = Event{}
				if delivery.delivered != nil {
					close(delivery.delivered)
				}
			case <-value.done:
				clearEvent(&delivery.event)
				return
			case <-supervisor.ctx.Done():
				clearEvent(&delivery.event)
				value.close()
				return
			}
		case <-value.done:
			return
		case <-supervisor.ctx.Done():
			value.close()
			return
		}
	}
}

// deliver transfers transient events into ordered, bounded per-viewer
// dispatchers. It is called while deliveryMu is held, but never waits for a
// viewer. A full dispatcher is detached so it cannot stall unrelated work.
func (supervisor *Supervisor) deliver(deliveries []eventDelivery) ([]viewerDeliveryWait, error) {
	waits := make([]viewerDeliveryWait, 0, len(deliveries))
	for index := range deliveries {
		delivery := &deliveries[index]
		select {
		case <-supervisor.ctx.Done():
			clearDeliveries(deliveries[index:])
			return nil, ErrClosed
		case <-delivery.viewer.done:
			clearEvent(&delivery.event)
			continue
		default:
		}
		queued := queuedViewerEvent{event: delivery.event}
		if delivery.event.Kind == EventPublicFrame {
			queued.delivered = make(chan struct{})
		}
		select {
		case delivery.viewer.deliveries <- queued:
			delivery.event = Event{}
			if queued.delivered != nil {
				timeout := delivery.viewer.deliveryTimeout
				if timeout <= 0 {
					timeout = viewerDeliveryStallTimeout
				}
				waits = append(waits, viewerDeliveryWait{
					viewer: delivery.viewer, delivered: queued.delivered,
					deadline: time.Now().Add(timeout),
				})
			}
		case <-delivery.viewer.done:
			clearEvent(&delivery.event)
		default:
			supervisor.detachViewerLocked(delivery.viewer)
			clearEvent(&delivery.event)
		}
	}
	return waits, nil
}

func (supervisor *Supervisor) waitViewerDeliveries(waits []viewerDeliveryWait) error {
	for _, wait := range waits {
		remaining := time.Until(wait.deadline)
		if remaining <= 0 {
			supervisor.detachSlowViewer(wait.viewer, wait.delivered)
			continue
		}
		timer := time.NewTimer(remaining)
		select {
		case <-wait.delivered:
			stopViewerDeliveryTimer(timer)
		case <-wait.viewer.done:
			stopViewerDeliveryTimer(timer)
		case <-supervisor.ctx.Done():
			stopViewerDeliveryTimer(timer)
			return ErrClosed
		case <-timer.C:
			supervisor.detachSlowViewer(wait.viewer, wait.delivered)
		}
	}
	return nil
}

func stopViewerDeliveryTimer(timer *time.Timer) {
	if timer == nil || timer.Stop() {
		return
	}
	select {
	case <-timer.C:
	default:
	}
}

func (supervisor *Supervisor) detachSlowViewer(value *viewer, delivered <-chan struct{}) {
	select {
	case <-delivered:
		return
	default:
	}
	supervisor.deliveryMu.Lock()
	defer supervisor.deliveryMu.Unlock()
	select {
	case <-delivered:
		return
	default:
		supervisor.detachViewerLocked(value)
	}
}

// detachViewerLocked is called with deliveryMu held. Closing done releases the
// viewer's dispatcher and every producer waiting only on that viewer.
func (supervisor *Supervisor) detachViewerLocked(value *viewer) {
	if value == nil {
		return
	}
	value.close()
	supervisor.mu.Lock()
	if current := supervisor.viewers[value.id]; current == value {
		delete(supervisor.viewers, value.id)
	}
	supervisor.mu.Unlock()
}

func clearDeliveries(values []eventDelivery) {
	for index := range values {
		clearEvent(&values[index].event)
	}
}

func clearEvent(value *Event) {
	if value == nil {
		return
	}
	clear(value.Frame.Payload)
	value.Frame.Payload = nil
	value.Frame.PublicType = ""
}

// commitMutation is the sole event-producing mutation boundary. Store and
// mirror changes plus sequence allocation are atomic with Snapshot under mu;
// ordered admission happens under deliveryMu; selected-frame backpressure is
// awaited only after releasing it.
func (supervisor *Supervisor) commitMutation(
	mutation func() ([]eventDelivery, error),
) error {
	supervisor.deliveryMu.Lock()
	supervisor.mu.Lock()
	deliveries, err := mutation()
	supervisor.mu.Unlock()
	var waits []viewerDeliveryWait
	if err == nil {
		waits, err = supervisor.deliver(deliveries)
	}
	supervisor.deliveryMu.Unlock()
	if err == nil {
		err = supervisor.waitViewerDeliveries(waits)
	}
	return err
}

func appendDeliveries(target []eventDelivery, additions []eventDelivery) []eventDelivery {
	return append(target, additions...)
}

func clonePublicFrame(value PublicFrame) PublicFrame {
	value.Payload = append([]byte(nil), value.Payload...)
	return value
}

func cloneEvent(value Event) Event {
	value.Frame = clonePublicFrame(value.Frame)
	return value
}

func (supervisor *Supervisor) nextEvent(ctx context.Context, value *viewer, cursor Cursor) (Event, error) {
	if ctx == nil || ctx.Err() != nil || value == nil {
		return Event{}, ErrCancelled
	}
	value.nextMu.Lock()
	defer value.nextMu.Unlock()
	for {
		supervisor.mu.Lock()
		valid := !supervisor.closed && cursor.Generation == value.generation &&
			cursor.Generation == supervisor.generation && cursor.After >= value.floor &&
			cursor.After <= supervisor.sequence
		supervisor.mu.Unlock()
		if !valid {
			return Event{}, ErrCursor
		}
		select {
		case event := <-value.queue:
			if event.Generation != cursor.Generation || event.Sequence <= cursor.After {
				clearEvent(&event)
				continue
			}
			if event.Sequence != cursor.After+1 {
				clearEvent(&event)
				return Event{}, ErrCursor
			}
			result := cloneEvent(event)
			clearEvent(&event)
			return result, nil
		case <-ctx.Done():
			return Event{}, ErrCancelled
		case <-value.done:
			return Event{}, ErrClosed
		case <-supervisor.ctx.Done():
			return Event{}, ErrClosed
		}
	}
}

func (supervisor *Supervisor) publicSessionLocked(value session.Session) (Session, error) {
	if value.Validate() != nil {
		return Session{}, ErrPersistence
	}
	mode := connector.Mode("")
	if value.Role == session.RoleSubordinate {
		mode = supervisor.modes[value.ID]
		if mode != connector.ModeAnswer && mode != connector.ModeFollow {
			mode = connector.ModeAnswer
		}
	}
	resume := false
	if value.Role == session.RoleManager && len(value.ProviderReference) != 0 {
		if catalog, ok := supervisor.catalog[settings.ProviderID(value.ProviderID)]; ok {
			resume = catalog.profile.Capabilities.Manager && catalog.profile.Capabilities.Resume &&
				catalog.profile.ManagerResume != nil
		}
	}
	parent := session.ID("")
	if value.ParentID != nil {
		parent = *value.ParentID
	}
	return Session{
		ID: value.ID, TaskID: value.TaskID, ParentID: parent, Role: value.Role,
		Label: value.Label, ProviderID: settings.ProviderID(value.ProviderID), Mode: mode,
		State: value.State, ResumeAvailable: resume,
		CreatedAt: value.CreatedAt, LastActiveAt: value.LastActiveAt,
	}, nil
}

func decodePublicFrame(raw json.RawMessage, expectedSessionID string) (PublicFrame, error) {
	if len(raw) == 0 || len(raw) > acp.MaxFrameBytes {
		return PublicFrame{}, ErrCapacity
	}
	message, err := acp.ParseMessage(raw)
	if err != nil || message.Kind() != acp.KindNotification || message.Method != acp.MethodSessionUpdate {
		return PublicFrame{}, ErrTransport
	}
	var params struct {
		SessionID string          `json:"sessionId"`
		Update    json.RawMessage `json:"update"`
	}
	if !validManagerPublicSessionID(expectedSessionID) || strictDecode(message.Params, &params) != nil ||
		params.SessionID != expectedSessionID || len(params.Update) == 0 {
		return PublicFrame{}, ErrTransport
	}
	var envelope struct {
		SessionUpdate string `json:"sessionUpdate"`
	}
	if json.Unmarshal(params.Update, &envelope) != nil || envelope.SessionUpdate == "" {
		return PublicFrame{}, ErrTransport
	}
	if envelope.SessionUpdate == "agent_message_chunk" {
		var update struct {
			SessionUpdate string `json:"sessionUpdate"`
			MessageID     string `json:"messageId,omitempty"`
			Content       struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if strictDecode(params.Update, &update) != nil || update.Content.Type != "text" ||
			!utf8.ValidString(update.Content.Text) {
			return PublicFrame{}, ErrTransport
		}
		return PublicFrame{
			Kind: FrameText, PublicType: update.SessionUpdate,
			Payload: append([]byte(nil), update.Content.Text...),
		}, nil
	}
	return PublicFrame{
		Kind: FrameUnknown, PublicType: boundedPublicType(envelope.SessionUpdate),
		Payload: append([]byte(nil), raw...),
	}, nil
}

func boundedPublicType(value string) string {
	if !utf8.ValidString(value) {
		return "invalid"
	}
	const maxTypeBytes = 256
	if len(value) <= maxTypeBytes {
		return value
	}
	value = value[:maxTypeBytes]
	for !utf8.ValidString(value) {
		value = value[:len(value)-1]
	}
	return value
}

func strictDecode(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return ErrInvalidRequest
	}
	return nil
}

func (supervisor *Supervisor) snapshotLocked() (Snapshot, error) {
	result := Snapshot{StreamGeneration: supervisor.generation, ThroughSequence: supervisor.sequence}
	result.Tasks = make([]Task, 0, len(supervisor.tasks))
	counts := make(map[task.ID][2]int64, len(supervisor.tasks))
	for _, value := range supervisor.sessions {
		current := counts[value.TaskID]
		activity, err := value.State.Activity()
		if err != nil {
			return Snapshot{}, ErrPersistence
		}
		if activity == session.ActivityActive {
			current[0]++
		} else {
			current[1]++
		}
		counts[value.TaskID] = current
	}
	for _, value := range supervisor.tasks {
		count := counts[value.ID]
		result.Tasks = append(result.Tasks, Task{
			ID: value.ID, Title: value.Title, CreatedAt: value.CreatedAt,
			Active: count[0], Dormant: count[1],
		})
	}
	sort.Slice(result.Tasks, func(left, right int) bool {
		if !result.Tasks[left].CreatedAt.Equal(result.Tasks[right].CreatedAt) {
			return result.Tasks[left].CreatedAt.After(result.Tasks[right].CreatedAt)
		}
		return result.Tasks[left].ID < result.Tasks[right].ID
	})
	result.Sessions = make([]Session, 0, len(supervisor.sessions))
	for _, value := range supervisor.sessions {
		public, err := supervisor.publicSessionLocked(value)
		if err != nil {
			return Snapshot{}, err
		}
		result.Sessions = append(result.Sessions, public)
	}
	sort.Slice(result.Sessions, func(left, right int) bool {
		if result.Sessions[left].TaskID != result.Sessions[right].TaskID {
			return result.Sessions[left].TaskID < result.Sessions[right].TaskID
		}
		if result.Sessions[left].Role != result.Sessions[right].Role {
			return result.Sessions[left].Role == session.RoleManager
		}
		if !result.Sessions[left].CreatedAt.Equal(result.Sessions[right].CreatedAt) {
			return result.Sessions[left].CreatedAt.Before(result.Sessions[right].CreatedAt)
		}
		return result.Sessions[left].ID < result.Sessions[right].ID
	})
	ids := sortedProviderIDs(supervisor.catalog)
	result.Providers = make([]Provider, 0, len(ids))
	for _, id := range ids {
		stored := supervisor.providers[id]
		catalog := supervisor.catalog[id]
		result.Providers = append(result.Providers, Provider{
			ID: id, DisplayName: catalog.profile.DisplayName, Enabled: stored.Enabled,
			ProbeStatus: stored.ProbeStatus, Capabilities: stored.Capabilities,
		})
	}
	encoded, err := json.Marshal(result)
	if err != nil || len(encoded) > MaxSnapshotBytes {
		clear(encoded)
		if len(encoded) > MaxSnapshotBytes {
			return Snapshot{}, ErrCapacity
		}
		return Snapshot{}, ErrTransport
	}
	clear(encoded)
	return result, nil
}

func cloneSnapshot(value Snapshot) Snapshot {
	value.Tasks = append([]Task(nil), value.Tasks...)
	value.Sessions = append([]Session(nil), value.Sessions...)
	value.Providers = append([]Provider(nil), value.Providers...)
	return value
}
