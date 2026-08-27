package manager

import (
	"bytes"
	"context"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/yty-build/acp-gateway/internal/connector"
	"github.com/yty-build/acp-gateway/internal/replay"
	"github.com/yty-build/acp-gateway/internal/session"
)

const (
	MaxProjectCWDBytes = 4096
	// MaxActiveManagers bounds the Service map and its one watcher goroutine
	// per live connector composite. Runtime may start another Manager only
	// after an existing instance reaches its one-time terminal state.
	MaxActiveManagers = 256
)

// StartRequest contains only the metadata and transient project directory
// required to create one new provider-owned Manager Session. The connector,
// not this package, owns fixed-prefix and typed-capability installation.
type StartRequest struct {
	Manager     session.Session
	ProjectCWD  string
	ChildEvents connector.ChildEventSink
}

// ResumeRequest contains the exact Manager metadata and complete direct-child
// restore set. It deliberately has no CWD, prefix, context, prompt, tool,
// history, native path, or reinjection field.
type ResumeRequest struct {
	Manager       session.Session
	OwnedChildren []session.Session
	ChildEvents   connector.ChildEventSink
}

// Service lazily selects conformed connector profiles and owns the uniqueness
// reservation for live top-level Managers. Construction and enumeration are
// metadata-only and never probe, authenticate, launch, Start, or Resume a
// provider.
type Service struct {
	registry *connector.Registry

	mu     sync.RWMutex
	active map[session.ID]*Instance // nil is an in-progress Start/Resume reservation.
}

func NewService(registry *connector.Registry) (*Service, error) {
	if registry == nil {
		return nil, ErrInvalidRequest
	}
	return &Service{registry: registry, active: make(map[session.ID]*Instance)}, nil
}

// ActiveIDs returns established live Manager IDs in deterministic order. It
// performs no connector or provider operation.
func (service *Service) ActiveIDs() []session.ID {
	if service == nil {
		return nil
	}
	service.mu.RLock()
	ids := make([]session.ID, 0, len(service.active))
	for id, instance := range service.active {
		if instance != nil && instance.Available() {
			ids = append(ids, id)
		}
	}
	service.mu.RUnlock()
	sort.Slice(ids, func(left, right int) bool { return ids[left] < ids[right] })
	return ids
}

// Lookup returns an already established live instance and never launches one.
func (service *Service) Lookup(id session.ID) (*Instance, bool) {
	if service == nil || id.Validate() != nil {
		return nil, false
	}
	service.mu.RLock()
	instance := service.active[id]
	service.mu.RUnlock()
	return instance, instance != nil && instance.Available()
}

// Start opens exactly one new Manager connection through the selected
// registered and conformed Manager port. ctx is the runtime supervisor
// lifetime and must remain live after Start returns; it is not a finite
// provider-launch timeout.
func (service *Service) Start(ctx context.Context, request StartRequest) (*Instance, error) {
	manager := request.Manager.Clone()
	if nilPort(ctx) || ctx.Err() != nil || nilPort(request.ChildEvents) ||
		validateTopLevelManager(manager) != nil || len(manager.ProviderReference) != 0 ||
		validateProjectCWD(request.ProjectCWD) != nil {
		return nil, ErrInvalidRequest
	}
	profile, err := service.startProfile(manager.ProviderID)
	if err != nil {
		return nil, err
	}
	if err := service.reserve(manager.ID); err != nil {
		return nil, err
	}

	lifetime, cancel := context.WithCancel(ctx)
	native, startErr := profile.Manager.Start(lifetime, connector.ManagerStartRequest{
		ParentSessionID: string(manager.ID),
		CWD:             request.ProjectCWD,
		Registry:        service.registry,
		Events:          request.ChildEvents,
	})
	if startErr != nil {
		cancel()
		service.releaseReservation(manager.ID)
		return nil, sanitizeError(startErr)
	}
	if lifetime.Err() != nil {
		cancel()
		closeInvalidNative(native)
		service.releaseReservation(manager.ID)
		return nil, sanitizeError(lifetime.Err())
	}
	instance, err := service.adopt(manager, native, lifetime, cancel, nil)
	if err != nil {
		cancel()
		closeInvalidNative(native)
		service.releaseReservation(manager.ID)
		return nil, err
	}
	service.install(manager.ID, instance)
	instance.watch()
	return instance, nil
}

// Resume opens exactly one Manager connection through the distinct exact-
// Resume port. It supplies the same opaque Manager reference and complete
// validated connector OwnedChild set, and never falls back to Start. ctx is
// the runtime supervisor lifetime and must remain live after Resume returns.
func (service *Service) Resume(ctx context.Context, request ResumeRequest) (*Instance, error) {
	manager := request.Manager.Clone()
	if nilPort(ctx) || ctx.Err() != nil || nilPort(request.ChildEvents) ||
		validateTopLevelManager(manager) != nil || len(manager.ProviderReference) == 0 {
		return nil, ErrInvalidRequest
	}
	profile, err := service.resumeProfile(manager.ProviderID)
	if err != nil {
		return nil, err
	}
	owned, err := connectorOwnedChildren(service.registry, manager, request.OwnedChildren)
	if err != nil {
		return nil, err
	}
	if err := service.reserve(manager.ID); err != nil {
		return nil, err
	}

	reference := replay.OpaqueRefFromBytes(manager.ProviderReference)
	lifetime, cancel := context.WithCancel(ctx)
	native, resumeErr := profile.ManagerResume.Resume(lifetime, connector.ManagerResumeRequest{
		ParentSessionID: string(manager.ID),
		NativeReference: reference,
		OwnedChildren:   owned,
		Registry:        service.registry,
		Events:          request.ChildEvents,
	})
	if resumeErr != nil {
		cancel()
		service.releaseReservation(manager.ID)
		return nil, sanitizeError(resumeErr)
	}
	if lifetime.Err() != nil {
		cancel()
		closeInvalidNative(native)
		service.releaseReservation(manager.ID)
		return nil, sanitizeError(lifetime.Err())
	}
	instance, err := service.adopt(manager, native, lifetime, cancel, manager.ProviderReference)
	if err != nil {
		cancel()
		closeInvalidNative(native)
		service.releaseReservation(manager.ID)
		return nil, err
	}
	service.install(manager.ID, instance)
	instance.watch()
	return instance, nil
}

func (service *Service) startProfile(providerID string) (connector.Profile, error) {
	if service == nil || service.registry == nil {
		return connector.Profile{}, ErrInvalidRequest
	}
	profile, err := service.registry.Profile(providerID)
	if err != nil {
		return connector.Profile{}, sanitizeError(err)
	}
	if !profile.Capabilities.Manager || nilPort(profile.Manager) {
		return connector.Profile{}, ErrUnsupported
	}
	return profile, nil
}

func (service *Service) resumeProfile(providerID string) (connector.Profile, error) {
	profile, err := service.startProfile(providerID)
	if err != nil {
		return connector.Profile{}, err
	}
	if !profile.Capabilities.Resume || nilPort(profile.ManagerResume) {
		return connector.Profile{}, ErrUnsupported
	}
	return profile, nil
}

func (service *Service) adopt(
	manager session.Session,
	native connector.NativeManagerSession,
	lifetime context.Context,
	cancel context.CancelFunc,
	expectedReference []byte,
) (*Instance, error) {
	if nilPort(native.Connection) {
		return nil, ErrTransport
	}
	reference := native.NativeReference.Bytes()
	connectionReference := native.Connection.NativeReference().Bytes()
	if len(reference) == 0 || len(reference) > session.MaxProviderReferenceBytes ||
		!bytes.Equal(reference, connectionReference) ||
		(expectedReference != nil && !bytes.Equal(reference, expectedReference)) {
		return nil, ErrTransport
	}
	return newInstance(
		manager.ID, manager.ProviderID, replay.OpaqueRefFromBytes(reference),
		native.Connection, lifetime, cancel,
		func(instance *Instance) { service.release(manager.ID, instance) },
	), nil
}

func (service *Service) reserve(id session.ID) error {
	service.mu.Lock()
	defer service.mu.Unlock()
	if _, exists := service.active[id]; exists {
		return ErrAlreadyActive
	}
	if len(service.active) >= MaxActiveManagers {
		return ErrCapacity
	}
	service.active[id] = nil
	return nil
}

func (service *Service) install(id session.ID, instance *Instance) {
	service.mu.Lock()
	if _, reserved := service.active[id]; reserved {
		service.active[id] = instance
	}
	service.mu.Unlock()
}

func (service *Service) releaseReservation(id session.ID) {
	service.mu.Lock()
	if instance, exists := service.active[id]; exists && instance == nil {
		delete(service.active, id)
	}
	service.mu.Unlock()
}

func (service *Service) release(id session.ID, instance *Instance) {
	service.mu.Lock()
	if service.active[id] == instance {
		delete(service.active, id)
	}
	service.mu.Unlock()
}

func validateTopLevelManager(manager session.Session) error {
	if err := manager.Validate(); err != nil || manager.Role != session.RoleManager || manager.ParentID != nil {
		return ErrInvalidRequest
	}
	return nil
}

func validateProjectCWD(cwd string) error {
	if cwd == "" || len(cwd) > MaxProjectCWDBytes || !utf8.ValidString(cwd) ||
		strings.IndexByte(cwd, 0) >= 0 || !filepath.IsAbs(cwd) {
		return ErrInvalidRequest
	}
	return nil
}

// connectorOwnedChildren is the only adapter to the connector's restore API.
// It validates same-Task direct ownership and produces defensive opaque refs;
// it neither retains them nor performs provider/history I/O.
func connectorOwnedChildren(
	registry *connector.Registry,
	manager session.Session,
	children []session.Session,
) ([]connector.OwnedChild, error) {
	if len(children) > connector.MaxChildrenPerManager {
		return nil, ErrCapacity
	}
	owned := make([]connector.OwnedChild, len(children))
	ids := make(map[session.ID]struct{}, len(children))
	references := make(map[string]struct{}, len(children))
	for index, raw := range children {
		child := raw.Clone()
		if session.ValidateHierarchy(child, manager) != nil || len(child.ProviderReference) == 0 {
			return nil, ErrInvalidRequest
		}
		if _, duplicate := ids[child.ID]; duplicate {
			return nil, ErrInvalidRequest
		}
		if _, duplicate := references[string(child.ProviderReference)]; duplicate {
			return nil, ErrInvalidRequest
		}
		profile, err := registry.Profile(child.ProviderID)
		if err != nil {
			return nil, sanitizeError(err)
		}
		if !profile.Capabilities.Subordinate || nilPort(profile.Children) {
			return nil, ErrUnsupported
		}
		ids[child.ID] = struct{}{}
		references[string(child.ProviderReference)] = struct{}{}
		owned[index] = connector.OwnedChild{
			SessionID:       string(child.ID),
			ProviderID:      child.ProviderID,
			NativeReference: replay.OpaqueRefFromBytes(child.ProviderReference),
		}
	}
	return owned, nil
}

func closeInvalidNative(native connector.NativeManagerSession) {
	if !nilPort(native.Connection) {
		_ = native.Connection.Close()
	}
}
