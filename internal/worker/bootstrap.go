package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"

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
	if current.ACPSessionID != "" && !usesRuntimeTransport(current.MetadataJSON) {
		return fmt.Errorf("Session %s was created by the legacy acpx CLI transport; refusing to start a public-runtime bridge while its detached queue owner may still exist. Keep its stored history, or create a fresh Session for the runtime bridge", current.ID)
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
	client := acp.NewRuntimeClient(binary, filepath.Join(home.Root, "acpx-runtime", current.ID))
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

func usesRuntimeTransport(metadata json.RawMessage) bool {
	if len(metadata) == 0 {
		return false
	}
	var values map[string]any
	if json.Unmarshal(metadata, &values) != nil {
		return false
	}
	transport, _ := values["acp_transport"].(string)
	return strings.TrimSpace(transport) == acp.RuntimeTransport
}
