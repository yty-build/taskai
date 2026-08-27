package connector

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/yty-build/acp-gateway/internal/replay"
)

type Capabilities struct {
	Manager     bool
	Subordinate bool
	Resume      bool
	Replay      bool
}

func (c Capabilities) intersect(available Capabilities) Capabilities {
	return Capabilities{
		Manager:     c.Manager && available.Manager,
		Subordinate: c.Subordinate && available.Subordinate,
		Resume:      c.Resume && available.Resume,
		Replay:      c.Replay && available.Replay,
	}
}

type Versions struct {
	ACPProtocol     int
	ManagerRuntime  string
	Connector       string
	ProviderHarness string
	ConnectorSDK    string
}

func (v Versions) equal(other Versions) bool {
	return v == other
}

// Conformance binds capability evidence to one exact public version tuple.
// A version mismatch fails closed to no advertised capabilities.
type Conformance struct {
	Exact          Versions
	Proven         Capabilities
	ReplayProvider replay.ProviderID
	ReplayVersion  string
}

func (c Conformance) Derive(observed Versions, available Capabilities) Capabilities {
	if observed.ACPProtocol != 1 || !c.Exact.equal(observed) {
		return Capabilities{}
	}
	return c.Proven.intersect(available)
}

type Profile struct {
	ID            string
	DisplayName   string
	Versions      Versions
	Capabilities  Capabilities
	Auth          AuthPort
	Children      ChildFactory
	ChildResume   ResumeChildFactory
	Manager       ManagerFactory
	ManagerResume ManagerResumeFactory
	Replay        ReplayBinding

	conformance *Conformance
}

// ReplayPort is the provider-native, version-conformed history boundary. Its
// Sink is connector-owned; TaskAI core never implements it or receives native
// records.
type ReplayPort interface {
	Capability(replay.ProviderID, string) (replay.Profile, bool)
	Replay(context.Context, replay.BatchRequest, replay.Sink) error
}

// ReplayBinding pins a Replay port to the same provider/version identity
// proved by connector conformance.
type ReplayBinding struct {
	Provider         replay.ProviderID
	InstalledVersion string
	Port             ReplayPort
}

func (binding ReplayBinding) available(conformance *Conformance) bool {
	if conformance == nil || binding.Port == nil || binding.Provider == "" || binding.InstalledVersion == "" ||
		binding.Provider != conformance.ReplayProvider || binding.InstalledVersion != conformance.ReplayVersion {
		return false
	}
	profile, ok := binding.Port.Capability(binding.Provider, binding.InstalledVersion)
	return ok && profile.Provider == binding.Provider && profile.InstalledVersion == binding.InstalledVersion
}

func (p Profile) validate() error {
	if p.ID == "" || len(p.ID) > 128 || p.DisplayName == "" || len(p.DisplayName) > 256 {
		return fmt.Errorf("%w: incomplete profile", ErrProviderUnsupported)
	}
	available := Capabilities{
		Manager:     p.Manager != nil,
		Subordinate: p.Children != nil,
		Resume:      profileResumeAvailable(p.Manager, p.ManagerResume, p.ChildResume),
		Replay:      p.Replay.available(p.conformance),
	}
	if p.Capabilities != (Capabilities{}) {
		if p.conformance == nil || p.conformance.Derive(p.Versions, available) != p.Capabilities {
			return fmt.Errorf("%w: capability claim lacks exact conformance", ErrProviderUnsupported)
		}
	}
	if p.Capabilities.Manager && p.Manager == nil {
		return fmt.Errorf("%w: manager port absent", ErrProviderUnsupported)
	}
	if p.Capabilities.Subordinate && p.Children == nil {
		return fmt.Errorf("%w: subordinate port absent", ErrProviderUnsupported)
	}
	if p.ManagerResume != nil && p.Manager == nil {
		return fmt.Errorf("%w: manager Resume factory has no Manager Start factory", ErrProviderUnsupported)
	}
	if p.Capabilities.Resume && !profileResumeAvailable(p.Manager, p.ManagerResume, p.ChildResume) {
		return fmt.Errorf("%w: exact resume port absent", ErrProviderUnsupported)
	}
	if p.Capabilities.Replay && !p.Replay.available(p.conformance) {
		return fmt.Errorf("%w: Replay port absent or unconformed", ErrProviderUnsupported)
	}
	return nil
}

// A profile that advertises Manager must prove Manager Resume through the
// separate exact-Resume factory. A subordinate-only profile may instead prove
// Resume through its subordinate port. This intentionally prevents a
// Start-only Manager from inheriting a false Resume claim.
func profileResumeAvailable(manager ManagerFactory, managerResume ManagerResumeFactory, childResume ResumeChildFactory) bool {
	if manager != nil {
		return managerResume != nil
	}
	return childResume != nil
}

type Registry struct {
	mu       sync.RWMutex
	profiles map[string]Profile
}

func NewRegistry() *Registry {
	return &Registry{profiles: make(map[string]Profile)}
}

func (r *Registry) Register(profile Profile) error {
	if err := profile.validate(); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.profiles[profile.ID]; exists {
		return fmt.Errorf("%w: duplicate provider", ErrProviderUnsupported)
	}
	r.profiles[profile.ID] = profile
	return nil
}

func (r *Registry) Profile(id string) (Profile, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	profile, ok := r.profiles[id]
	if !ok {
		return Profile{}, ErrProviderUnknown
	}
	return profile, nil
}

func (r *Registry) Profiles() []Profile {
	r.mu.RLock()
	defer r.mu.RUnlock()
	profiles := make([]Profile, 0, len(r.profiles))
	for _, profile := range r.profiles {
		profiles = append(profiles, profile)
	}
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })
	return profiles
}

var codexG0Conformance = Conformance{
	Exact: Versions{
		ACPProtocol:     1,
		ManagerRuntime:  "0.150.1",
		Connector:       CodexACPConnectorVersion,
		ProviderHarness: CodexHarnessVersion,
		ConnectorSDK:    CodexACPSDKVersion,
	},
	Proven:         Capabilities{Manager: true, Subordinate: true, Resume: true, Replay: true},
	ReplayProvider: replay.ProviderCodex,
	ReplayVersion:  "0.150.1",
}

func CodexConformance() Conformance { return codexG0Conformance }

const (
	CodexACPConnectorVersion = "1.1.7"
	CodexHarnessVersion      = "0.145.0"
	CodexACPSDKVersion       = "1.3.0"

	ClaudeACPConnectorVersion = "0.64.0"
	ClaudeHarnessVersion      = "2.1.226"
	ClaudeAgentSDKVersion     = "0.3.220"

	GeminiCLIConnectorVersion = "0.25.0-preview.0"
	GeminiACPSDKVersion       = "0.11.0"

	GrokConnectorVersion = "1.0.5"
)

var claudeG0Conformance = Conformance{
	Exact: Versions{
		ACPProtocol: 1, Connector: ClaudeACPConnectorVersion,
		ProviderHarness: ClaudeHarnessVersion, ConnectorSDK: ClaudeAgentSDKVersion,
	},
	Proven:         Capabilities{Subordinate: true, Resume: true, Replay: true},
	ReplayProvider: replay.ProviderClaude,
	ReplayVersion:  ClaudeHarnessVersion,
}

var geminiG0Conformance = Conformance{
	Exact: Versions{
		ACPProtocol: 1, Connector: GeminiCLIConnectorVersion,
		ProviderHarness: GeminiCLIConnectorVersion, ConnectorSDK: GeminiACPSDKVersion,
	},
	// The pinned public ACP surface advertises loadSession=false and implements
	// no session/resume method. Native Replay remains independently conformed.
	Proven:         Capabilities{Subordinate: true, Replay: true},
	ReplayProvider: replay.ProviderGemini,
	ReplayVersion:  GeminiCLIConnectorVersion,
}

var grokG0Conformance = Conformance{
	Exact: Versions{
		ACPProtocol: 1, Connector: GrokConnectorVersion,
		ProviderHarness: GrokConnectorVersion,
	},
	Proven:         Capabilities{Subordinate: true, Resume: true, Replay: true},
	ReplayProvider: replay.ProviderGrok,
	ReplayVersion:  GrokConnectorVersion,
}

func ClaudeConformance() Conformance { return claudeG0Conformance }
func GeminiConformance() Conformance { return geminiG0Conformance }
func GrokConformance() Conformance   { return grokG0Conformance }

// NewConformedProfile is the only constructor for a profile with advertised
// capabilities. Registry.Register recomputes the exact version/port
// intersection, so callers cannot add a hard-coded flag after construction.
// A Replay claim additionally requires an exact binding to the separately
// owned internal/replay service; connector never decodes its opaque reference.
func NewConformedProfile(
	id, displayName string,
	observed Versions,
	conformance Conformance,
	auth AuthPort,
	children ChildFactory,
	childResume ResumeChildFactory,
	manager ManagerFactory,
	managerResume ManagerResumeFactory,
	replayBinding ReplayBinding,
) Profile {
	available := Capabilities{
		Manager:     manager != nil,
		Subordinate: children != nil,
		Resume:      profileResumeAvailable(manager, managerResume, childResume),
		Replay:      replayBinding.available(&conformance),
	}
	derived := conformance.Derive(observed, available)
	return Profile{
		ID:            id,
		DisplayName:   displayName,
		Versions:      observed,
		Capabilities:  derived,
		Auth:          auth,
		Children:      children,
		ChildResume:   childResume,
		Manager:       manager,
		ManagerResume: managerResume,
		Replay:        replayBinding,
		conformance:   &conformance,
	}
}

// CodexProfile derives claims from the approved G0 version tuple and from
// production ports that are actually present. Replay is advertised only when
// the integration owner supplies the exact version-conformed native adapter.
func CodexProfile(
	observed Versions,
	auth AuthPort,
	children ChildFactory,
	manager ManagerFactory,
	managerResume ManagerResumeFactory,
	replayBinding ReplayBinding,
) Profile {
	children, childResume := conformedProviderChildren(children, replay.ProviderCodex, observed)
	return NewConformedProfile(
		"codex", "Codex", observed, codexG0Conformance,
		auth, children, childResume, manager, managerResume, replayBinding,
	)
}

func ClaudeProfile(observed Versions, auth AuthPort, children ChildFactory, replayBinding ReplayBinding) Profile {
	children, childResume := conformedProviderChildren(children, replay.ProviderClaude, observed)
	return NewConformedProfile(
		string(replay.ProviderClaude), "Claude Code", observed, claudeG0Conformance,
		auth, children, childResume, nil, nil, replayBinding,
	)
}

func GeminiProfile(observed Versions, auth AuthPort, children ChildFactory, replayBinding ReplayBinding) Profile {
	children, childResume := conformedProviderChildren(children, replay.ProviderGemini, observed)
	return NewConformedProfile(
		string(replay.ProviderGemini), "Gemini CLI", observed, geminiG0Conformance,
		auth, children, childResume, nil, nil, replayBinding,
	)
}

func GrokProfile(observed Versions, auth AuthPort, children ChildFactory, replayBinding ReplayBinding) Profile {
	children, childResume := conformedProviderChildren(children, replay.ProviderGrok, observed)
	return NewConformedProfile(
		string(replay.ProviderGrok), "Grok", observed, grokG0Conformance,
		auth, children, childResume, nil, nil, replayBinding,
	)
}

func conformedProviderChildren(
	children ChildFactory,
	provider replay.ProviderID,
	observed Versions,
) (ChildFactory, ResumeChildFactory) {
	factory, ok := children.(ConformedSubordinateFactory)
	if !ok {
		return nil, nil
	}
	evidence, ok := factory.SubordinateConformance()
	if !ok || evidence.Provider != provider || !evidence.Versions.equal(observed) {
		return nil, nil
	}
	var resume ResumeChildFactory
	if evidence.Resume {
		resume, _ = children.(ResumeChildFactory)
	}
	return children, resume
}
