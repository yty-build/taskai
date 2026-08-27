package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/config"
	gitrepo "github.com/yty-build/acp-gateway/internal/git"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/store"
)

// RunSession assembles the hidden worker process from persisted Session state.
func RunSession(ctx context.Context, home config.Home, repository *store.Store, sessionID string) error {
	current, err := repository.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	if current.WorkerPID != 0 || current.WorkerSocket != "" {
		return fmt.Errorf("Session %s retains worker ownership and cannot start another worker; provider-process teardown must be proven before clearing quarantine", current.ID)
	}
	if current.Status != session.StatusStarting {
		return fmt.Errorf("Session %s is %s, not admitted for worker startup", current.ID, current.Status)
	}
	registry, err := agents.Load(home.Agents)
	if err != nil {
		return err
	}
	profile, err := registry.Get(current.Agent)
	if err != nil {
		return err
	}
	// Binary discovery and compatibility probing are intentionally lazy. The
	// worker first publishes an addressable `connecting` Session Channel, then
	// runs bootstrap validation and probes through SessionPort.Probe so viewers
	// see startup progress/failure.
	client := acp.NewRuntimeClient("", filepath.Join(home.Root, "acpx-runtime", current.ID))
	runtime, err := New(Config{
		Session: current, Profile: profile, Repository: repository, Port: client,
		WorkersDirectory: home.Workers, LogsDirectory: home.Logs,
		Preflight: func(ctx context.Context) error {
			if err := validateBootstrapProfile(current, profile); err != nil {
				return err
			}
			return validateBootstrapCheckout(ctx, current)
		},
	})
	if err != nil {
		return fmt.Errorf("initialize Session worker: %w", err)
	}
	return runtime.Run(ctx)
}

// validateBootstrapProfile prevents an opaque provider reference from ever
// crossing an adapter boundary after a user-owned profile changes. Agent
// command/version changes within the same provider remain an adapter concern,
// but changing the provider requires a new Session.
func validateBootstrapProfile(current session.Session, profile agents.Profile) error {
	if strings.TrimSpace(current.Provider) == "" {
		return fmt.Errorf("Session %s has no persisted provider and cannot start a provider Port", current.ID)
	}
	if profile.Provider != current.Provider {
		return fmt.Errorf("Session %s was created for provider %q, but agent profile %q now selects provider %q; create a new Session instead of crossing provider adapters", current.ID, current.Provider, current.Agent, profile.Provider)
	}
	return nil
}

// validateBootstrapCheckout closes the validation-to-spawn gap. Persisted
// paths are immutable Session identity: the worker must neither follow a new
// symlink target nor accept a deleted/replaced checkout immediately before it
// opens the one provider Port.
func validateBootstrapCheckout(ctx context.Context, current session.Session) error {
	repository, err := gitrepo.ValidateRepository(ctx, current.RepoPath)
	if err != nil {
		return fmt.Errorf("validate persisted repository for Session %s: %w", current.ID, err)
	}
	if repository != current.RepoPath {
		return fmt.Errorf("Session %s repository drifted from %q to %q; refusing provider startup", current.ID, current.RepoPath, repository)
	}
	if current.WorktreePath == "" {
		return nil
	}
	worktree, err := gitrepo.ValidateRepository(ctx, current.WorktreePath)
	if err != nil {
		return fmt.Errorf("validate persisted worktree for Session %s: %w", current.ID, err)
	}
	if worktree != current.WorktreePath {
		return fmt.Errorf("Session %s worktree drifted from %q to %q; refusing provider startup", current.ID, current.WorktreePath, worktree)
	}
	same, err := gitrepo.SameRepository(ctx, repository, worktree)
	if err != nil {
		return fmt.Errorf("validate persisted worktree repository for Session %s: %w", current.ID, err)
	}
	if !same {
		return fmt.Errorf("Session %s worktree %q no longer belongs to repository %q; refusing provider startup", current.ID, worktree, repository)
	}
	return nil
}
