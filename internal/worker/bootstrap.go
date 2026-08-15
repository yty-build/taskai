package worker

import (
	"context"
	"fmt"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/config"
	"github.com/yty-build/acp-gateway/internal/store"
)

// RunSession assembles the hidden worker process from persisted Session state.
func RunSession(ctx context.Context, home config.Home, repository *store.Store, sessionID string) error {
	current, err := repository.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	registry, err := agents.Load(home.Agents)
	if err != nil {
		return err
	}
	profile, err := registry.Get(current.Agent)
	if err != nil {
		return err
	}
	binary, err := acp.FindBinary()
	if err != nil {
		return err
	}
	client := acp.NewClient(binary)
	if err := client.Probe(ctx); err != nil {
		return err
	}
	runtime, err := New(Config{
		Session: current, Profile: profile, Repository: repository, ACP: client,
		WorkersDirectory: home.Workers, LogsDirectory: home.Logs,
	})
	if err != nil {
		return fmt.Errorf("initialize Session worker: %w", err)
	}
	return runtime.Run(ctx)
}
