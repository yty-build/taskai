package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/manager"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/settings"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
)

type systemClock struct{}

func (systemClock) Now() time.Time { return time.Now() }

type domainIDs struct{}

func (domainIDs) NewTaskID() (task.ID, error)       { return task.NewID() }
func (domainIDs) NewSessionID() (session.ID, error) { return session.NewID() }

type managerHandle interface {
	ID() session.ID
	ProviderID() string
	NativeReference() []byte
	Send(context.Context, string, manager.FrameSink) (json.RawMessage, error)
	Stop(context.Context) error
	Done() <-chan struct{}
	Terminal() (manager.Terminal, bool)
	Available() bool
}

type managerBackend interface {
	ActiveIDs() []session.ID
	Lookup(session.ID) (managerHandle, bool)
	Start(context.Context, manager.StartRequest) (managerHandle, error)
	Resume(context.Context, manager.ResumeRequest) (managerHandle, error)
}

type serviceBackend struct{ service *manager.Service }

func (backend serviceBackend) ActiveIDs() []session.ID { return backend.service.ActiveIDs() }

func (backend serviceBackend) Lookup(id session.ID) (managerHandle, bool) {
	instance, ok := backend.service.Lookup(id)
	if !ok {
		return nil, false
	}
	return instanceHandle{instance}, true
}

func (backend serviceBackend) Start(ctx context.Context, request manager.StartRequest) (managerHandle, error) {
	instance, err := backend.service.Start(ctx, request)
	if err != nil {
		return nil, err
	}
	return instanceHandle{instance}, nil
}

func (backend serviceBackend) Resume(ctx context.Context, request manager.ResumeRequest) (managerHandle, error) {
	instance, err := backend.service.Resume(ctx, request)
	if err != nil {
		return nil, err
	}
	return instanceHandle{instance}, nil
}

type instanceHandle struct{ instance *manager.Instance }

func (handle instanceHandle) ID() session.ID     { return handle.instance.ID() }
func (handle instanceHandle) ProviderID() string { return handle.instance.ProviderID() }
func (handle instanceHandle) NativeReference() []byte {
	return handle.instance.NativeReference().Bytes()
}
func (handle instanceHandle) Send(ctx context.Context, prompt string, sink manager.FrameSink) (json.RawMessage, error) {
	return handle.instance.Send(ctx, prompt, sink)
}
func (handle instanceHandle) Stop(ctx context.Context) error { return handle.instance.Stop(ctx) }
func (handle instanceHandle) Done() <-chan struct{}          { return handle.instance.Done() }
func (handle instanceHandle) Terminal() (manager.Terminal, bool) {
	return handle.instance.Terminal()
}
func (handle instanceHandle) Available() bool { return handle.instance.Available() }

type providerCatalog struct {
	spec    ProviderSpec
	profile connector.Profile
}

type liveManager struct {
	id     session.ID
	taskID task.ID
	handle managerHandle
	fatal  chan struct{}
	done   chan struct{}

	stopMu   sync.Mutex
	stopping bool
	stopDone chan struct{}
	stopErr  error

	finalizeOnce sync.Once
	finalizeErr  error
}

type viewer struct {
	id string

	generation uint64
	floor      uint64

	done      chan struct{}
	closeOnce sync.Once
	queue     chan Event
	nextMu    sync.Mutex

	attached map[task.ID]struct{} // guarded by Supervisor.mu
}

func (value *viewer) close() { value.closeOnce.Do(func() { close(value.done) }) }

// Supervisor is the worker-owned, content-free runtime. Its lifetime is
// independent from every viewer and all provider operations are lazy.
type Supervisor struct {
	ctx    context.Context
	cancel context.CancelFunc

	store        MetadataStore
	registry     *connector.Registry
	managers     managerBackend
	cwd          string
	clock        Clock
	taskIDs      TaskIDSource
	sessionIDs   SessionIDSource
	stopTimeout  time.Duration
	probeTimeout time.Duration

	// deliveryMu serializes sequence allocation and synchronous viewer
	// delivery. It is always acquired before mu. Blocking sends never hold mu.
	deliveryMu  sync.Mutex
	mu          sync.Mutex
	closed      bool
	closeDone   chan struct{}
	closeErr    error
	lastTime    time.Time
	generation  uint64
	sequence    uint64
	tasks       map[task.ID]task.Task
	sessions    map[session.ID]session.Session
	modes       map[session.ID]connector.Mode
	turns       map[session.ID]turnCorrelation
	providers   map[settings.ProviderID]settings.Provider
	providerOps map[settings.ProviderID]bool
	catalog     map[settings.ProviderID]providerCatalog
	live        map[session.ID]*liveManager
	viewers     map[string]*viewer

	wg sync.WaitGroup
}

const (
	managerStopTimeout   = 5 * time.Second
	providerProbeTimeout = 10 * time.Second
)

var generationCounter struct {
	sync.Mutex
	value uint64
}

func nextGeneration() uint64 {
	generationCounter.Lock()
	defer generationCounter.Unlock()
	generationCounter.value++
	if generationCounter.value == 0 {
		generationCounter.value++
	}
	return generationCounter.value
}

// NewSupervisor bootstraps a content-free mirror and reconciles stale
// lifecycle metadata before any client can observe it. It performs no probe,
// authentication, provider launch, Start, Resume, or Replay operation.
func NewSupervisor(ctx context.Context, config Config) (*Supervisor, error) {
	if config.Managers == nil {
		return nil, ErrInvalidRequest
	}
	return newSupervisor(ctx, config, serviceBackend{service: config.Managers})
}

func newSupervisor(ctx context.Context, config Config, backend managerBackend) (*Supervisor, error) {
	if ctx == nil || ctx.Err() != nil || config.Store == nil || config.Registry == nil || backend == nil ||
		validateProjectCWD(config.ProjectCWD) != nil {
		return nil, ErrInvalidRequest
	}
	clock := config.Clock
	if clock == nil {
		clock = systemClock{}
	}
	taskIDs := config.TaskIDs
	if taskIDs == nil {
		taskIDs = domainIDs{}
	}
	sessionIDs := config.SessionIDs
	if sessionIDs == nil {
		sessionIDs = domainIDs{}
	}
	lifetime, cancel := context.WithCancel(ctx)
	supervisor := &Supervisor{
		ctx: lifetime, cancel: cancel,
		store: config.Store, registry: config.Registry, managers: backend,
		cwd: config.ProjectCWD, clock: clock, taskIDs: taskIDs, sessionIDs: sessionIDs,
		stopTimeout: managerStopTimeout, probeTimeout: providerProbeTimeout,
		generation: nextGeneration(),
		tasks:      make(map[task.ID]task.Task), sessions: make(map[session.ID]session.Session),
		modes: make(map[session.ID]connector.Mode), turns: make(map[session.ID]turnCorrelation),
		providers:   make(map[settings.ProviderID]settings.Provider),
		providerOps: make(map[settings.ProviderID]bool),
		catalog:     make(map[settings.ProviderID]providerCatalog), live: make(map[session.ID]*liveManager),
		viewers: make(map[string]*viewer), closeDone: make(chan struct{}),
	}
	supervisor.mu.Lock()
	err := supervisor.bootstrap(config.Providers)
	supervisor.mu.Unlock()
	if err != nil {
		cancel()
		return nil, err
	}
	return supervisor, nil
}

func validateProjectCWD(value string) error {
	if value == "" || len(value) > manager.MaxProjectCWDBytes || !utf8.ValidString(value) ||
		strings.IndexByte(value, 0) >= 0 || !filepath.IsAbs(value) {
		return ErrInvalidRequest
	}
	return nil
}

func (supervisor *Supervisor) StateDir() string {
	if supervisor == nil || supervisor.store == nil {
		return ""
	}
	return supervisor.store.StateDir()
}

func (supervisor *Supervisor) bootstrap(specs []ProviderSpec) error {
	if err := supervisor.bootstrapCatalog(specs); err != nil {
		return err
	}
	summaries, err := supervisor.store.ListTasks(supervisor.ctx, task.ViewAll)
	if err != nil || len(summaries) > MaxTasks {
		return supervisor.persistenceOrCapacity(err, len(summaries) > MaxTasks)
	}
	maxTime := time.Time{}
	for _, summary := range summaries {
		value := summary.Task
		if value.Validate() != nil {
			return ErrPersistence
		}
		supervisor.tasks[value.ID] = value
		maxTime = laterTime(maxTime, value.UpdatedAt)
		owned, listErr := supervisor.store.ListSessions(supervisor.ctx, value.ID)
		if listErr != nil || len(supervisor.sessions)+len(owned) > MaxSessions {
			return supervisor.persistenceOrCapacity(listErr, len(supervisor.sessions)+len(owned) > MaxSessions)
		}
		for _, raw := range owned {
			stored := raw.Clone()
			if stored.Validate() != nil {
				return ErrPersistence
			}
			if _, duplicate := supervisor.sessions[stored.ID]; duplicate {
				return ErrPersistence
			}
			supervisor.sessions[stored.ID] = stored
			if stored.Role == session.RoleSubordinate {
				supervisor.modes[stored.ID] = connector.ModeAnswer
			}
			maxTime = laterTime(maxTime, stored.LastActiveAt)
		}
	}
	supervisor.lastTime = task.CanonicalTime(maxTime)
	if err := supervisor.reconcileLiveManagers(); err != nil {
		return err
	}
	return nil
}

func (supervisor *Supervisor) bootstrapCatalog(specs []ProviderSpec) error {
	profiles := supervisor.registry.Profiles()
	if len(profiles) > MaxProviders || len(specs) != len(profiles) {
		return ErrInvalidRequest
	}
	for _, spec := range specs {
		if err := validateProviderSpec(spec); err != nil {
			return err
		}
		profile, err := supervisor.registry.Profile(string(spec.ID))
		if err != nil || profile.ID != string(spec.ID) {
			return ErrProviderUnknown
		}
		if _, duplicate := supervisor.catalog[spec.ID]; duplicate {
			return ErrConflict
		}
		spec.Launch = spec.Launch.Clone()
		supervisor.catalog[spec.ID] = providerCatalog{spec: spec, profile: profile}
	}
	for _, profile := range profiles {
		if _, ok := supervisor.catalog[settings.ProviderID(profile.ID)]; !ok {
			return ErrInvalidRequest
		}
	}
	persisted, err := supervisor.store.ListProviderSettings(supervisor.ctx)
	if err != nil || len(persisted) > MaxProviders {
		return supervisor.persistenceOrCapacity(err, len(persisted) > MaxProviders)
	}
	rows := make(map[settings.ProviderID]settings.Provider, len(persisted))
	for _, raw := range persisted {
		row := raw.Clone()
		if row.Validate() != nil {
			return ErrPersistence
		}
		if _, known := supervisor.catalog[row.ID]; !known {
			return ErrProviderUnknown
		}
		if _, duplicate := rows[row.ID]; duplicate {
			return ErrPersistence
		}
		rows[row.ID] = row
	}
	ids := sortedProviderIDs(supervisor.catalog)
	for _, id := range ids {
		catalog := supervisor.catalog[id]
		current := capabilitiesFromProfile(catalog.profile)
		row, exists := rows[id]
		if !exists {
			row = settings.Provider{
				ID: id, ProbeStatus: settings.ProbeUnknown,
				LaunchConfig: catalog.spec.Launch.Clone(), Capabilities: current,
			}
		} else {
			launchMatches := launchEqual(row.LaunchConfig, catalog.spec.Launch)
			row.Enabled = row.Enabled && launchMatches
			if row.ProbeStatus == settings.ProbeChecking {
				row.ProbeStatus = settings.ProbeNeedsRepair
			}
			row.LaunchConfig = catalog.spec.Launch.Clone()
			// Compiled conformance is the current capability authority. A stored
			// true can never add a capability, and a stale stored false must not
			// permanently suppress a capability proved after restart.
			row.Capabilities = current
		}
		if err := supervisor.putProviderVerified(row); err != nil {
			return err
		}
		supervisor.providers[id] = row.Clone()
	}
	return nil
}

func validateProviderSpec(spec ProviderSpec) error {
	if err := spec.ID.Validate(); err != nil || spec.Launch.Validate() != nil || len(spec.Launch.Argv) == 0 {
		return ErrInvalidRequest
	}
	executable := spec.Launch.Argv[0]
	if executable == "." || executable == ".." || filepath.Base(executable) != executable ||
		strings.ContainsAny(executable, `/\\`) {
		return ErrInvalidRequest
	}
	return nil
}

func (supervisor *Supervisor) reconcileLiveManagers() error {
	activeIDs := supervisor.managers.ActiveIDs()
	active := make(map[session.ID]managerHandle, len(activeIDs))
	for _, id := range activeIDs {
		handle, ok := supervisor.managers.Lookup(id)
		if !ok || handle == nil || !handle.Available() {
			continue
		}
		stored, exists := supervisor.sessions[id]
		if !exists || stored.Role != session.RoleManager || stored.ParentID != nil ||
			stored.ProviderID != handle.ProviderID() || len(stored.ProviderReference) == 0 ||
			!bytes.Equal(stored.ProviderReference, handle.NativeReference()) {
			return ErrPersistence
		}
		active[id] = handle
	}
	managerIDs := supervisor.sortedManagerIDs()
	for _, id := range managerIDs {
		stored := supervisor.sessions[id]
		handle := active[id]
		if handle != nil {
			if stored.State != session.StateActive {
				stamp := supervisor.nextTimeLocked(stored.LastActiveAt)
				if err := supervisor.updateSessionStateVerified(id, session.StateActive, stamp); err != nil {
					return err
				}
				stored.State = session.StateActive
				stored.LastActiveAt = stamp
				supervisor.sessions[id] = stored
			}
			supervisor.installLiveLocked(stored, handle)
			continue
		}
		if !treeNeedsReconciliation(id, supervisor.sessions) {
			continue
		}
		stamp := supervisor.nextTreeTimeLocked(id)
		if err := supervisor.updateTreeStateVerified(id, session.StateInterrupted, stamp); err != nil {
			return err
		}
		supervisor.applyTreeStateLocked(id, session.StateInterrupted, stamp)
	}
	return nil
}

func treeNeedsReconciliation(managerID session.ID, sessions map[session.ID]session.Session) bool {
	managerSession, ok := sessions[managerID]
	if !ok || managerSession.Role != session.RoleManager {
		return false
	}
	if managerSession.State == session.StateStarting || managerSession.State == session.StateActive {
		return true
	}
	for _, candidate := range sessions {
		if candidate.ParentID != nil && *candidate.ParentID == managerID &&
			(candidate.State == session.StateStarting || candidate.State == session.StateActive) {
			return true
		}
	}
	return false
}

func (supervisor *Supervisor) persistenceOrCapacity(err error, capacity bool) error {
	if capacity {
		return ErrCapacity
	}
	if err != nil {
		return ErrPersistence
	}
	return nil
}

func (supervisor *Supervisor) nextTimeLocked(values ...time.Time) time.Time {
	candidate := task.CanonicalTime(supervisor.clock.Now())
	maxValue := supervisor.lastTime
	for _, value := range values {
		maxValue = laterTime(maxValue, task.CanonicalTime(value))
	}
	if !candidate.After(maxValue) {
		candidate = maxValue.Add(time.Microsecond)
	}
	supervisor.lastTime = candidate
	return candidate
}

func (supervisor *Supervisor) nextTreeTimeLocked(managerID session.ID) time.Time {
	latest := time.Time{}
	for _, value := range supervisor.sessions {
		if value.ID == managerID || (value.ParentID != nil && *value.ParentID == managerID) {
			latest = laterTime(latest, value.LastActiveAt)
			latest = laterTime(latest, value.CreatedAt)
		}
	}
	return supervisor.nextTimeLocked(latest)
}

func laterTime(left, right time.Time) time.Time {
	if right.After(left) {
		return right
	}
	return left
}

func (supervisor *Supervisor) sortedManagerIDs() []session.ID {
	ids := make([]session.ID, 0)
	for id, value := range supervisor.sessions {
		if value.Role == session.RoleManager && value.ParentID == nil {
			ids = append(ids, id)
		}
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	return ids
}

func sortedProviderIDs(values map[settings.ProviderID]providerCatalog) []settings.ProviderID {
	ids := make([]settings.ProviderID, 0, len(values))
	for id := range values {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	return ids
}

func capabilitiesFromProfile(profile connector.Profile) settings.Capabilities {
	return settings.Capabilities{
		Manager: profile.Capabilities.Manager, Subordinate: profile.Capabilities.Subordinate,
		Resume: profile.Capabilities.Resume, Replay: profile.Capabilities.Replay,
	}
}

func launchEqual(left, right settings.LaunchConfig) bool {
	if len(left.Argv) != len(right.Argv) {
		return false
	}
	for index := range left.Argv {
		if left.Argv[index] != right.Argv[index] {
			return false
		}
	}
	return true
}

func providerEqual(left, right settings.Provider) bool {
	return left.ID == right.ID && left.Enabled == right.Enabled && left.ProbeStatus == right.ProbeStatus &&
		left.Capabilities == right.Capabilities && launchEqual(left.LaunchConfig, right.LaunchConfig)
}

func (supervisor *Supervisor) putProviderVerified(value settings.Provider) error {
	if err := supervisor.store.PutProviderSettings(supervisor.ctx, value); err == nil {
		return nil
	}
	stored, err := supervisor.store.GetProviderSettings(supervisor.ctx, value.ID)
	if err == nil && providerEqual(stored, value) {
		return nil
	}
	return ErrPersistence
}

func (supervisor *Supervisor) updateSessionStateVerified(id session.ID, state session.State, stamp time.Time) error {
	if err := supervisor.store.UpdateSessionState(supervisor.ctx, id, state, stamp); err == nil {
		return nil
	}
	stored, err := supervisor.store.GetSession(supervisor.ctx, id)
	if err == nil && stored.State == state && stored.LastActiveAt.Equal(stamp) {
		return nil
	}
	return ErrPersistence
}

func (supervisor *Supervisor) updateTreeStateVerified(id session.ID, state session.State, stamp time.Time) error {
	if err := supervisor.store.UpdateOwnedTreeState(supervisor.ctx, id, state, stamp); err == nil {
		return nil
	}
	managerSession, ok := supervisor.sessions[id]
	if !ok {
		return ErrPersistence
	}
	owned, err := supervisor.store.ListSessions(supervisor.ctx, managerSession.TaskID)
	if err != nil {
		return ErrPersistence
	}
	found := false
	for _, value := range owned {
		if value.ID != id && (value.ParentID == nil || *value.ParentID != id) {
			continue
		}
		found = true
		if value.State != state || !value.LastActiveAt.Equal(stamp) {
			return ErrPersistence
		}
	}
	if !found {
		return ErrPersistence
	}
	return nil
}

func (supervisor *Supervisor) applyTreeStateLocked(id session.ID, state session.State, stamp time.Time) {
	for childID, value := range supervisor.sessions {
		if value.ID != id && (value.ParentID == nil || *value.ParentID != id) {
			continue
		}
		value.State = state
		value.LastActiveAt = stamp
		supervisor.sessions[childID] = value
		if value.Role == session.RoleSubordinate {
			supervisor.modes[childID] = connector.ModeAnswer
		}
	}
}

func (supervisor *Supervisor) installLiveLocked(stored session.Session, handle managerHandle) *liveManager {
	entry := &liveManager{
		id: stored.ID, taskID: stored.TaskID, handle: handle,
		fatal: make(chan struct{}, 1), done: make(chan struct{}), stopDone: make(chan struct{}),
	}
	supervisor.live[stored.ID] = entry
	supervisor.wg.Add(1)
	go supervisor.watchManager(entry)
	return entry
}

// Close ends the worker lifetime, interrupts remaining exact live trees, and
// releases viewers. It never deletes provider-owned state.
func (supervisor *Supervisor) Close() error {
	if supervisor == nil {
		return nil
	}
	supervisor.mu.Lock()
	if supervisor.closed {
		done := supervisor.closeDone
		supervisor.mu.Unlock()
		if done != nil {
			<-done
		}
		supervisor.mu.Lock()
		err := supervisor.closeErr
		supervisor.mu.Unlock()
		return err
	}
	supervisor.closed = true
	if supervisor.closeDone == nil {
		supervisor.closeDone = make(chan struct{})
	}
	done := supervisor.closeDone
	viewers := make([]*viewer, 0, len(supervisor.viewers))
	for _, value := range supervisor.viewers {
		viewers = append(viewers, value)
	}
	live := make([]*liveManager, 0, len(supervisor.live))
	for _, value := range supervisor.live {
		live = append(live, value)
	}
	supervisor.mu.Unlock()
	for _, value := range viewers {
		value.close()
	}
	// Release any producer blocked behind a viewer before clearing its sole
	// bounded pending delivery. Supervisor lifetime remains independent.
	supervisor.deliveryMu.Lock()
	supervisor.mu.Lock()
	clear(supervisor.viewers)
	supervisor.mu.Unlock()
	for _, value := range viewers {
		value.clearPending()
	}
	supervisor.deliveryMu.Unlock()
	sort.Slice(live, func(left, right int) bool { return live[left].id < live[right].id })
	results := make([]error, len(live))
	var stops sync.WaitGroup
	for index, entry := range live {
		stops.Add(1)
		go func(index int, entry *liveManager) {
			defer stops.Done()
			results[index] = supervisor.terminateManager(entry, false)
		}(index, entry)
	}
	stops.Wait()
	supervisor.cancel()
	supervisor.wg.Wait()
	var result error
	for _, err := range results {
		if err != nil {
			result = err
			break
		}
	}
	supervisor.mu.Lock()
	supervisor.closeErr = result
	close(done)
	supervisor.mu.Unlock()
	return result
}

func (supervisor *Supervisor) watchManager(entry *liveManager) {
	defer supervisor.wg.Done()
	defer close(entry.done)
	select {
	case <-entry.handle.Done():
		entry.stopMu.Lock()
		stopping := entry.stopping
		stopDone := entry.stopDone
		entry.stopMu.Unlock()
		if stopping {
			select {
			case <-stopDone:
			case <-supervisor.ctx.Done():
			}
			return
		}
		supervisor.finalizeManager(entry, session.StateInterrupted)
	case <-entry.fatal:
		supervisor.terminateManager(entry, false)
	case <-supervisor.ctx.Done():
		supervisor.terminateManager(entry, false)
	}
}

func (supervisor *Supervisor) signalFatal(entry *liveManager) {
	select {
	case entry.fatal <- struct{}{}:
	default:
	}
}

func (supervisor *Supervisor) terminateManager(entry *liveManager, explicit bool) error {
	entry.stopMu.Lock()
	if entry.stopping {
		done := entry.stopDone
		entry.stopMu.Unlock()
		<-done
		entry.stopMu.Lock()
		err := entry.stopErr
		entry.stopMu.Unlock()
		return err
	}
	entry.stopping = true
	entry.stopMu.Unlock()

	stopErr := supervisor.stopHandle(entry.handle)
	state := session.StateInterrupted
	if explicit && stopErr == nil {
		state = session.StateClosed
	}
	metadataErr := supervisor.finalizeManager(entry, state)
	result := metadataErr
	if stopErr != nil && result == nil {
		result = supervisor.managerError(stopErr)
	}

	entry.stopMu.Lock()
	entry.stopErr = result
	close(entry.stopDone)
	entry.stopMu.Unlock()
	return result
}

func (supervisor *Supervisor) stopHandle(handle managerHandle) error {
	timeout := supervisor.stopTimeout
	if timeout <= 0 {
		timeout = managerStopTimeout
	}
	return stopHandleWithin(handle, timeout)
}

func stopHandleWithin(handle managerHandle, timeout time.Duration) error {
	if handle == nil || timeout <= 0 {
		return ErrInvalidRequest
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	result := make(chan error, 1)
	go func() {
		result <- handle.Stop(ctx)
	}()
	select {
	case err := <-result:
		return err
	case <-ctx.Done():
		return ErrTransport
	}
}

func (supervisor *Supervisor) finalizeManager(entry *liveManager, state session.State) error {
	entry.finalizeOnce.Do(func() {
		entry.finalizeErr = supervisor.commitMutation(func() ([]eventDelivery, error) {
			stored, ok := supervisor.sessions[entry.id]
			if !ok {
				delete(supervisor.live, entry.id)
				return nil, ErrNotFound
			}
			stamp := supervisor.nextTreeTimeLocked(entry.id)
			if err := supervisor.updateTreeStateVerified(entry.id, state, stamp); err != nil {
				delete(supervisor.live, entry.id)
				return nil, err
			}
			supervisor.applyTreeStateLocked(entry.id, state, stamp)
			delete(supervisor.live, entry.id)
			var deliveries []eventDelivery
			for _, value := range supervisor.treeSessionsLocked(stored.ID) {
				prepared, err := supervisor.prepareSessionLocked(value)
				if err != nil {
					return nil, err
				}
				deliveries = appendDeliveries(deliveries, prepared)
			}
			return deliveries, nil
		})
	})
	return entry.finalizeErr
}

func (supervisor *Supervisor) treeSessionsLocked(managerID session.ID) []session.Session {
	values := make([]session.Session, 0)
	for _, value := range supervisor.sessions {
		if value.ID == managerID || (value.ParentID != nil && *value.ParentID == managerID) {
			values = append(values, value.Clone())
		}
	}
	sort.Slice(values, func(left, right int) bool {
		if values[left].Role != values[right].Role {
			return values[left].Role == session.RoleManager
		}
		return values[left].ID < values[right].ID
	})
	return values
}

func (supervisor *Supervisor) publicError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return ErrCancelled
	}
	switch {
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, store.ErrConflict):
		return ErrConflict
	case errors.Is(err, store.ErrClosed):
		return ErrClosed
	}
	code := codeOf(err)
	if code != CodeTransport {
		return codeError(code)
	}
	return ErrPersistence
}
