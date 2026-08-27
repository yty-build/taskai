package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/yty-build/acp-gateway/internal/testkit/providerfixture/spec"
)

func TestNativeSessionAccessRequiresOneExactSafeID(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(spec.RootEnvironment, root)
	store, err := openFixtureStore()
	if err != nil {
		t.Fatal(err)
	}
	target := nativeThread{
		ID: "child-exact", Kind: "subordinate", Turns: []nativeTurn{{
			ID: "turn-one", Status: "completed", Items: []nativeItem{{Type: "agentMessage", Text: "synthetic"}},
		}},
	}
	if err := store.writeThread(target); err != nil {
		t.Fatal(err)
	}
	if err := store.writeThread(nativeThread{ID: "child-decoy", Kind: "subordinate", Turns: []nativeTurn{}}); err != nil {
		t.Fatal(err)
	}
	got, _, err := store.readThread(target.ID)
	if err != nil || got.ID != target.ID || len(got.Turns) != 1 {
		t.Fatalf("exact read = %#v, %v", got, err)
	}
	for _, unsafe := range []string{"", ".", "..", "../child-decoy", "child/decoy", "child.json"} {
		if _, _, err := store.readThread(unsafe); err == nil {
			t.Fatalf("unsafe native ID %q was accepted", unsafe)
		}
	}
}

func TestNativeSessionReadRejectsFinalSymlink(t *testing.T) {
	root := t.TempDir()
	if err := os.Chmod(root, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(spec.RootEnvironment, root)
	store, err := openFixtureStore()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(store.sessions, "outside.json")
	if err := os.WriteFile(target, []byte(`{"id":"linked","kind":"subordinate","turns":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link, err := store.sessionPath("linked")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.readThread("linked"); err == nil || errors.Is(err, os.ErrNotExist) {
		t.Fatalf("final symlink error = %v, want closed exact-source rejection", err)
	}
}
