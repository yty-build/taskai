// Command providerfixture is a deterministic, provider-owned executable used
// only by the acceptance suite. The same binary is exposed on the test PATH as
// codex and codex-acp; production composition contains no fixture switch.
package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yty-build/acp-gateway/internal/testkit/providerfixture/spec"
)

func main() {
	store, err := openFixtureStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "providerfixture: unavailable")
		os.Exit(70)
	}
	executable := filepath.Base(os.Args[0])
	releaseLive, err := store.markLive(executable)
	if err != nil {
		os.Exit(70)
	}
	defer releaseLive()
	_ = store.appendTrace(spec.TraceEvent{
		Event: spec.EventProcessStart, Executable: executable,
		Argv: append([]string(nil), os.Args[1:]...),
	})
	defer func() {
		_ = store.appendTrace(spec.TraceEvent{
			Event: spec.EventProcessExit, Executable: executable,
			Argv: append([]string(nil), os.Args[1:]...),
		})
	}()

	var runErr error
	switch executable {
	case "codex":
		runErr = runCodex(store, os.Args[1:])
	case "codex-acp":
		runErr = runACPAgent(store)
	default:
		runErr = fmt.Errorf("unsupported fixture executable")
	}
	if runErr != nil {
		fmt.Fprintln(os.Stderr, "providerfixture: failed")
		os.Exit(1)
	}
}

func runCodex(store *fixtureStore, args []string) error {
	switch {
	case len(args) == 2 && args[0] == "login" && args[1] == "status":
		_, err := os.Lstat(filepath.Join(store.root, spec.AuthFilename))
		connected := err == nil
		_ = store.appendTrace(spec.TraceEvent{Event: spec.EventAuthStatus, Connected: boolPointer(connected)})
		if !connected {
			return fmt.Errorf("not connected")
		}
		return nil
	case len(args) == 1 && args[0] == "login":
		value := map[string]string{
			"auth": spec.AuthCanary, "credential": spec.CredentialCanary,
		}
		raw, _ := json.Marshal(value)
		path := filepath.Join(store.root, spec.AuthFilename)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			return err
		}
		if err := os.Chmod(path, 0o600); err != nil {
			return err
		}
		_ = store.appendTrace(spec.TraceEvent{Event: spec.EventAuthLogin})
		fmt.Fprintln(os.Stdout, "Synthetic Codex authentication complete.")
		return nil
	case len(args) == 1 && args[0] == "app-server":
		return runAppServer(store)
	default:
		return fmt.Errorf("unsupported codex argv")
	}
}

func boolPointer(value bool) *bool { return &value }
