package runtime

import (
	"bytes"
	"context"
	"encoding/json"
	"sort"
	"time"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/manager"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/settings"
	"github.com/yty-build/acp-gateway/internal/task"
)

func (supervisor *Supervisor) snapshot(clientID string) (Snapshot, error) {
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.closed {
		return Snapshot{}, ErrClosed
	}
	if _, ok := supervisor.viewers[clientID]; !ok {
		return Snapshot{}, ErrUnavailable
	}
	value, err := supervisor.snapshotLocked()
	if err != nil {
		return Snapshot{}, err
	}
	return cloneSnapshot(value), nil
}

func (supervisor *Supervisor) addTask(ctx context.Context, clientID, title string) (task.ID, error) {
	if ctx == nil || ctx.Err() != nil || task.ValidateTitle(title) != nil {
		return "", ErrInvalidRequest
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.closed {
		return "", ErrClosed
	}
	if _, ok := supervisor.viewers[clientID]; !ok {
		return "", ErrUnavailable
	}
	if len(supervisor.tasks) >= MaxTasks {
		return "", ErrCapacity
	}
	id, err := supervisor.taskIDs.NewTaskID()
	if err != nil || id.Validate() != nil {
		return "", ErrCapacity
	}
	if _, duplicate := supervisor.tasks[id]; duplicate {
		return "", ErrConflict
	}
	stamp := supervisor.nextTimeLocked()
	value, err := task.New(id, title, stamp)
	if err != nil {
		return "", ErrInvalidRequest
	}
	if err := supervisor.createTaskVerified(value); err != nil {
		return "", err
	}
	supervisor.tasks[id] = value
	return id, nil
}

func (supervisor *Supervisor) createTaskVerified(value task.Task) error {
	if err := supervisor.store.CreateTask(supervisor.ctx, value); err == nil {
		return nil
	}
	stored, err := supervisor.store.GetTask(supervisor.ctx, value.ID)
	if err == nil && stored.ID == value.ID && stored.Title == value.Title &&
		stored.CreatedAt.Equal(value.CreatedAt) && stored.UpdatedAt.Equal(value.UpdatedAt) {
		return nil
	}
	return ErrPersistence
}

func (supervisor *Supervisor) addManager(
	ctx context.Context,
	clientID string,
	taskID task.ID,
	providerID settings.ProviderID,
	label string,
) (session.ID, error) {
	if ctx == nil || ctx.Err() != nil || taskID.Validate() != nil || providerID.Validate() != nil ||
		session.ValidateLabel(label) != nil {
		return "", ErrInvalidRequest
	}

	operation := newManagerOperation()
	var id session.ID
	var metadata session.Session
	err := supervisor.commitMutation(func() ([]eventDelivery, error) {
		if supervisor.closed {
			return nil, ErrClosed
		}
		if !supervisor.viewerAttachedLocked(clientID, taskID) {
			return nil, ErrUnavailable
		}
		if len(supervisor.sessions) >= MaxSessions {
			return nil, ErrCapacity
		}
		if _, exists := supervisor.tasks[taskID]; !exists {
			return nil, ErrNotFound
		}
		provider, eligibleErr := supervisor.providerEligibleLocked(providerID, settings.CapabilityManager, true)
		if eligibleErr != nil {
			return nil, eligibleErr
		}
		if provider.ProbeStatus != settings.ProbeConnected {
			return nil, ErrUnavailable
		}
		generated, idErr := supervisor.sessionIDs.NewSessionID()
		if idErr != nil || generated.Validate() != nil {
			return nil, ErrCapacity
		}
		if _, duplicate := supervisor.sessions[generated]; duplicate {
			return nil, ErrConflict
		}
		stamp := supervisor.nextTimeLocked()
		created, newErr := session.NewManager(
			generated, taskID, label, string(providerID), nil, session.StateStarting, stamp,
		)
		if newErr != nil {
			return nil, ErrInvalidRequest
		}
		if persistErr := supervisor.createSessionVerified(created); persistErr != nil {
			return nil, persistErr
		}
		id = generated
		metadata = created.Clone()
		supervisor.sessions[id] = metadata.Clone()
		return supervisor.prepareSessionLocked(metadata)
	})
	if err != nil {
		if id != "" {
			_ = supervisor.interruptTree(id)
		}
		return id, err
	}

	instance, startErr := supervisor.managers.Start(supervisor.ctx, manager.StartRequest{
		Manager: metadata, ProjectCWD: supervisor.cwd,
		ChildEvents: childEventSink{supervisor: supervisor, managerID: id, operation: operation},
	})
	if startErr != nil || instance == nil {
		metadataErr := supervisor.interruptTree(id)
		if metadataErr != nil {
			return id, metadataErr
		}
		return id, supervisor.managerError(startErr)
	}

	reference := append([]byte(nil), instance.NativeReference()...)
	if len(reference) == 0 || len(reference) > session.MaxProviderReferenceBytes ||
		session.ValidateProviderReference(reference) != nil {
		_ = supervisor.stopHandle(instance)
		_ = supervisor.interruptTree(id)
		return id, ErrTransport
	}
	var entry *liveManager
	adopted := false
	err = supervisor.commitMutation(func() ([]eventDelivery, error) {
		if supervisor.closed {
			return nil, ErrClosed
		}
		current, ok := supervisor.sessions[id]
		if !ok || current.State != session.StateStarting || len(current.ProviderReference) != 0 {
			return nil, ErrInvalidState
		}
		refStamp := supervisor.nextTimeLocked(current.LastActiveAt)
		if persistErr := supervisor.setReferenceVerified(id, reference, refStamp); persistErr != nil {
			return nil, persistErr
		}
		current.ProviderReference = append([]byte(nil), reference...)
		current.LastActiveAt = refStamp
		supervisor.sessions[id] = current.Clone()
		activeStamp := supervisor.nextTimeLocked(refStamp)
		if persistErr := supervisor.updateSessionStateVerified(id, session.StateActive, activeStamp); persistErr != nil {
			return nil, persistErr
		}
		current.State = session.StateActive
		current.LastActiveAt = activeStamp
		supervisor.sessions[id] = current.Clone()
		metadata = current.Clone()
		entry = supervisor.installLiveLocked(current, instance)
		adopted = operation.adopt(supervisor, entry)
		return supervisor.prepareSessionLocked(current)
	})
	if err != nil {
		_ = supervisor.stopHandle(instance)
		_ = supervisor.interruptTree(id)
		if entry != nil {
			supervisor.signalFatal(entry)
		}
		return id, err
	}
	if entry == nil {
		_ = supervisor.stopHandle(instance)
		_ = supervisor.interruptTree(id)
		return id, ErrTransport
	}
	if !adopted {
		_ = supervisor.terminateManager(entry, false)
		return id, ErrPersistence
	}
	return id, nil
}

func (supervisor *Supervisor) setReferenceVerified(id session.ID, reference []byte, stamp time.Time) error {
	if err := supervisor.store.SetProviderReference(supervisor.ctx, id, reference, stamp); err == nil {
		return nil
	}
	stored, err := supervisor.store.GetSession(supervisor.ctx, id)
	if err == nil && bytes.Equal(stored.ProviderReference, reference) && stored.LastActiveAt.Equal(stamp) {
		return nil
	}
	return ErrPersistence
}

func (supervisor *Supervisor) interruptTree(id session.ID) error {
	return supervisor.commitMutation(func() ([]eventDelivery, error) {
		return supervisor.interruptTreeLocked(id)
	})
}

func (supervisor *Supervisor) interruptTreeLocked(id session.ID) ([]eventDelivery, error) {
	if _, exists := supervisor.sessions[id]; !exists {
		return nil, ErrNotFound
	}
	stamp := supervisor.nextTreeTimeLocked(id)
	if err := supervisor.updateTreeStateVerified(id, session.StateInterrupted, stamp); err != nil {
		return nil, err
	}
	supervisor.applyTreeStateLocked(id, session.StateInterrupted, stamp)
	var deliveries []eventDelivery
	for _, value := range supervisor.treeSessionsLocked(id) {
		prepared, err := supervisor.prepareSessionLocked(value)
		if err != nil {
			return nil, err
		}
		deliveries = appendDeliveries(deliveries, prepared)
	}
	return deliveries, nil
}

func (supervisor *Supervisor) resume(ctx context.Context, clientID string, managerID session.ID) error {
	if ctx == nil || ctx.Err() != nil || managerID.Validate() != nil {
		return ErrInvalidRequest
	}
	operation := newManagerOperation()
	var metadata session.Session
	var children []session.Session
	err := supervisor.commitMutation(func() ([]eventDelivery, error) {
		if supervisor.closed {
			return nil, ErrClosed
		}
		current, ok := supervisor.sessions[managerID]
		if !ok {
			return nil, ErrNotFound
		}
		if !supervisor.viewerAttachedLocked(clientID, current.TaskID) || current.Role != session.RoleManager ||
			current.ParentID != nil || (current.State != session.StateInterrupted && current.State != session.StateDormant) ||
			len(current.ProviderReference) == 0 {
			return nil, ErrInvalidState
		}
		if _, live := supervisor.live[managerID]; live {
			return nil, ErrBusy
		}
		catalog, exists := supervisor.catalog[settings.ProviderID(current.ProviderID)]
		if !exists || !catalog.profile.Capabilities.Manager || !catalog.profile.Capabilities.Resume ||
			catalog.profile.ManagerResume == nil {
			return nil, ErrUnsupported
		}
		if _, eligibleErr := supervisor.providerEligibleLocked(settings.ProviderID(current.ProviderID), settings.CapabilityResume, false); eligibleErr != nil {
			return nil, eligibleErr
		}
		owned, childErr := supervisor.resumeChildrenLocked(current)
		if childErr != nil {
			return nil, childErr
		}
		startingStamp := supervisor.nextTimeLocked(current.LastActiveAt)
		if persistErr := supervisor.updateSessionStateVerified(managerID, session.StateStarting, startingStamp); persistErr != nil {
			return nil, persistErr
		}
		current.State = session.StateStarting
		current.LastActiveAt = startingStamp
		supervisor.sessions[managerID] = current.Clone()
		metadata = current.Clone()
		children = owned
		return supervisor.prepareSessionLocked(current)
	})
	if err != nil {
		if metadata.ID != "" {
			_ = supervisor.interruptTree(managerID)
		}
		return err
	}

	instance, resumeErr := supervisor.managers.Resume(supervisor.ctx, manager.ResumeRequest{
		Manager: metadata, OwnedChildren: children,
		ChildEvents: childEventSink{supervisor: supervisor, managerID: managerID, operation: operation},
	})
	if resumeErr != nil || instance == nil {
		metadataErr := supervisor.interruptTree(managerID)
		if metadataErr != nil {
			return metadataErr
		}
		return supervisor.managerError(resumeErr)
	}
	if !bytes.Equal(instance.NativeReference(), metadata.ProviderReference) {
		_ = supervisor.stopHandle(instance)
		_ = supervisor.interruptTree(managerID)
		return ErrTransport
	}

	var entry *liveManager
	adopted := false
	err = supervisor.commitMutation(func() ([]eventDelivery, error) {
		if supervisor.closed {
			return nil, ErrClosed
		}
		current, ok := supervisor.sessions[managerID]
		if !ok || current.State != session.StateStarting ||
			!bytes.Equal(current.ProviderReference, metadata.ProviderReference) {
			return nil, ErrInvalidState
		}
		activeStamp := supervisor.nextTimeLocked(current.LastActiveAt)
		if persistErr := supervisor.updateSessionStateVerified(managerID, session.StateActive, activeStamp); persistErr != nil {
			return nil, persistErr
		}
		current.State = session.StateActive
		current.LastActiveAt = activeStamp
		supervisor.sessions[managerID] = current.Clone()
		for _, child := range children {
			supervisor.modes[child.ID] = connector.ModeAnswer
		}
		entry = supervisor.installLiveLocked(current, instance)
		adopted = operation.adopt(supervisor, entry)
		return supervisor.prepareSessionLocked(current)
	})
	if err != nil {
		_ = supervisor.stopHandle(instance)
		_ = supervisor.interruptTree(managerID)
		if entry != nil {
			supervisor.signalFatal(entry)
		}
		return err
	}
	if entry == nil || !adopted {
		if entry != nil {
			_ = supervisor.terminateManager(entry, false)
		} else {
			_ = supervisor.stopHandle(instance)
			_ = supervisor.interruptTree(managerID)
		}
		return ErrPersistence
	}
	if err != nil {
		supervisor.signalFatal(entry)
		return err
	}
	return nil
}

func (supervisor *Supervisor) resumeChildrenLocked(managerSession session.Session) ([]session.Session, error) {
	children := make([]session.Session, 0)
	seenReferences := make(map[string]struct{})
	for _, raw := range supervisor.sessions {
		if raw.ParentID == nil || *raw.ParentID != managerSession.ID {
			continue
		}
		child := raw.Clone()
		if session.ValidateHierarchy(child, managerSession) != nil || len(child.ProviderReference) == 0 {
			return nil, ErrInvalidState
		}
		catalog, ok := supervisor.catalog[settings.ProviderID(child.ProviderID)]
		if !ok || !catalog.profile.Capabilities.Subordinate || catalog.profile.Children == nil {
			return nil, ErrUnsupported
		}
		if _, duplicate := seenReferences[string(child.ProviderReference)]; duplicate {
			return nil, ErrConflict
		}
		seenReferences[string(child.ProviderReference)] = struct{}{}
		children = append(children, child)
	}
	if len(children) > connector.MaxChildrenPerManager {
		return nil, ErrCapacity
	}
	sort.Slice(children, func(left, right int) bool { return children[left].ID < children[right].ID })
	return children, nil
}

func (supervisor *Supervisor) send(ctx context.Context, clientID string, managerID session.ID, prompt string) error {
	if ctx == nil || ctx.Err() != nil || managerID.Validate() != nil || len(prompt) == 0 ||
		len(prompt) > manager.MaxPromptBytes {
		return ErrInvalidRequest
	}
	supervisor.mu.Lock()
	metadata, ok := supervisor.sessions[managerID]
	entry := supervisor.live[managerID]
	valid := !supervisor.closed && ok && metadata.Role == session.RoleManager && metadata.ParentID == nil &&
		metadata.State == session.StateActive && entry != nil && entry.handle.Available() &&
		supervisor.viewerAttachedLocked(clientID, metadata.TaskID)
	supervisor.mu.Unlock()
	if !valid {
		return ErrUnavailable
	}
	completion, err := entry.handle.Send(supervisor.ctx, prompt, manager.FrameSinkFunc(
		func(_ context.Context, id session.ID, raw json.RawMessage) error {
			if id != managerID {
				return ErrInvalidRequest
			}
			frame, decodeErr := decodePublicFrame(raw)
			if decodeErr != nil {
				return decodeErr
			}
			return supervisor.commitMutation(func() ([]eventDelivery, error) {
				current, exists := supervisor.sessions[managerID]
				if supervisor.closed || !exists || supervisor.live[managerID] != entry {
					return nil, ErrClosed
				}
				return supervisor.prepareFrameLocked(current, frame)
			})
		},
	))
	clear(completion)
	if err != nil {
		return supervisor.managerError(err)
	}
	return nil
}

func (supervisor *Supervisor) stop(ctx context.Context, clientID string, managerID session.ID) error {
	if ctx == nil || ctx.Err() != nil || managerID.Validate() != nil {
		return ErrInvalidRequest
	}
	supervisor.mu.Lock()
	metadata, ok := supervisor.sessions[managerID]
	entry := supervisor.live[managerID]
	valid := !supervisor.closed && ok && metadata.Role == session.RoleManager && metadata.ParentID == nil &&
		entry != nil && supervisor.viewerAttachedLocked(clientID, metadata.TaskID)
	supervisor.mu.Unlock()
	if !valid {
		return ErrUnavailable
	}
	return supervisor.terminateManager(entry, true)
}

func (supervisor *Supervisor) attach(ctx context.Context, clientID string, taskID task.ID) error {
	if ctx == nil || ctx.Err() != nil || taskID.Validate() != nil {
		return ErrInvalidRequest
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.closed {
		return ErrClosed
	}
	value, ok := supervisor.viewers[clientID]
	if !ok {
		return ErrUnavailable
	}
	if _, ok := supervisor.tasks[taskID]; !ok {
		return ErrNotFound
	}
	clear(value.attached)
	value.attached[taskID] = struct{}{}
	return nil
}

func (supervisor *Supervisor) detach(ctx context.Context, clientID string, taskID task.ID) error {
	if ctx == nil || taskID.Validate() != nil {
		return ErrInvalidRequest
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	value, ok := supervisor.viewers[clientID]
	if !ok {
		return nil
	}
	delete(value.attached, taskID)
	return nil
}

func (supervisor *Supervisor) viewerAttachedLocked(clientID string, taskID task.ID) bool {
	value, ok := supervisor.viewers[clientID]
	if !ok {
		return false
	}
	_, attached := value.attached[taskID]
	return attached
}

func (supervisor *Supervisor) setEnabled(ctx context.Context, clientID string, providerID settings.ProviderID, enabled bool) error {
	if ctx == nil || ctx.Err() != nil || providerID.Validate() != nil {
		return ErrInvalidRequest
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.closed {
		return ErrClosed
	}
	if _, ok := supervisor.viewers[clientID]; !ok {
		return ErrUnavailable
	}
	catalog, ok := supervisor.catalog[providerID]
	if !ok {
		return ErrProviderUnknown
	}
	stored := supervisor.providers[providerID]
	stored.Enabled = enabled
	stored.LaunchConfig = catalog.spec.Launch.Clone()
	stored.Capabilities = capabilitiesFromProfile(catalog.profile)
	if err := supervisor.putProviderVerified(stored); err != nil {
		return err
	}
	supervisor.providers[providerID] = stored.Clone()
	return nil
}

func (supervisor *Supervisor) authenticate(ctx context.Context, clientID string, providerID settings.ProviderID) error {
	if ctx == nil || ctx.Err() != nil || providerID.Validate() != nil {
		return ErrInvalidRequest
	}
	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	if supervisor.closed {
		return ErrClosed
	}
	if _, ok := supervisor.viewers[clientID]; !ok {
		return ErrUnavailable
	}
	if _, ok := supervisor.catalog[providerID]; !ok {
		return ErrProviderUnknown
	}
	// Interactive authentication is viewer-owned so a detached worker never
	// retains or targets terminal descriptors. The app authenticates through
	// the immutable profile and then calls Recheck.
	return ErrUnsupported
}

func (supervisor *Supervisor) recheck(ctx context.Context, clientID string, providerID settings.ProviderID) error {
	return supervisor.providerOperation(ctx, clientID, providerID)
}

func (supervisor *Supervisor) providerOperation(
	ctx context.Context,
	clientID string,
	providerID settings.ProviderID,
) error {
	if ctx == nil || ctx.Err() != nil || providerID.Validate() != nil {
		return ErrInvalidRequest
	}
	supervisor.mu.Lock()
	if supervisor.closed {
		supervisor.mu.Unlock()
		return ErrClosed
	}
	if _, ok := supervisor.viewers[clientID]; !ok {
		supervisor.mu.Unlock()
		return ErrUnavailable
	}
	catalog, ok := supervisor.catalog[providerID]
	if !ok {
		supervisor.mu.Unlock()
		return ErrProviderUnknown
	}
	if catalog.profile.Auth == nil {
		supervisor.mu.Unlock()
		return ErrUnsupported
	}
	if supervisor.providerOps[providerID] {
		supervisor.mu.Unlock()
		return ErrBusy
	}
	supervisor.providerOps[providerID] = true
	stored := supervisor.providers[providerID]
	stored.ProbeStatus = settings.ProbeChecking
	stored.LaunchConfig = catalog.spec.Launch.Clone()
	stored.Capabilities = capabilitiesFromProfile(catalog.profile)
	if err := supervisor.putProviderVerified(stored); err != nil {
		delete(supervisor.providerOps, providerID)
		supervisor.mu.Unlock()
		return err
	}
	supervisor.providers[providerID] = stored.Clone()
	supervisor.mu.Unlock()

	timeout := supervisor.probeTimeout
	if timeout <= 0 {
		timeout = providerProbeTimeout
	}
	state := probeWithin(ctx, catalog.profile.Auth, timeout)
	status := probeStatus(state)

	supervisor.mu.Lock()
	defer supervisor.mu.Unlock()
	delete(supervisor.providerOps, providerID)
	if supervisor.closed {
		return ErrClosed
	}
	stored = supervisor.providers[providerID]
	stored.ProbeStatus = status
	stored.LaunchConfig = catalog.spec.Launch.Clone()
	stored.Capabilities = capabilitiesFromProfile(catalog.profile)
	if err := supervisor.putProviderVerified(stored); err != nil {
		return err
	}
	supervisor.providers[providerID] = stored.Clone()
	return nil
}

func probeWithin(ctx context.Context, port connector.AuthPort, timeout time.Duration) connector.AuthState {
	if ctx == nil || port == nil || timeout <= 0 {
		return connector.AuthNeedsRepair
	}
	probeCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result := make(chan connector.AuthState, 1)
	go func() { result <- port.Probe(probeCtx) }()
	select {
	case state := <-result:
		return state
	case <-probeCtx.Done():
		return connector.AuthNeedsRepair
	}
}

func probeStatus(state connector.AuthState) settings.ProbeStatus {
	switch state {
	case connector.AuthConnected:
		return settings.ProbeConnected
	case connector.AuthDisconnected:
		return settings.ProbeDisconnected
	case connector.AuthMissingConnector:
		return settings.ProbeMissingConnector
	default:
		return settings.ProbeNeedsRepair
	}
}

func (supervisor *Supervisor) providerEligibleLocked(
	id settings.ProviderID,
	capability settings.Capability,
	requireEnabled bool,
) (settings.Provider, error) {
	catalog, ok := supervisor.catalog[id]
	if !ok {
		return settings.Provider{}, ErrProviderUnknown
	}
	current := capabilitiesFromProfile(catalog.profile)
	if !current.Has(capability) {
		return settings.Provider{}, ErrUnsupported
	}
	persisted, err := supervisor.store.GetProviderSettings(supervisor.ctx, id)
	if err != nil {
		return settings.Provider{}, supervisor.publicError(err)
	}
	if !launchEqual(persisted.LaunchConfig, catalog.spec.Launch) {
		return settings.Provider{}, ErrInvalidState
	}
	if requireEnabled && !persisted.Enabled {
		return settings.Provider{}, ErrProviderDisabled
	}
	persisted.Capabilities = current
	persisted.LaunchConfig = catalog.spec.Launch.Clone()
	return persisted, nil
}

func (supervisor *Supervisor) managerError(err error) error {
	if err == nil {
		return ErrTransport
	}
	code := manager.CodeOf(err)
	switch code {
	case manager.CodeInvalidRequest:
		return ErrInvalidRequest
	case manager.CodeProviderUnknown:
		return ErrProviderUnknown
	case manager.CodeUnsupported:
		return ErrUnsupported
	case manager.CodeAlreadyActive, manager.CodeBusy:
		return ErrBusy
	case manager.CodeUnavailable:
		return ErrUnavailable
	case manager.CodeCapacity:
		return ErrCapacity
	case manager.CodeCancelled:
		return ErrCancelled
	default:
		return ErrTransport
	}
}
