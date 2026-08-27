package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/yty-build/acp-gateway/internal/testkit/providerfixture/spec"
)

const maxNativeSessionBytes = 8 << 20

type fixtureStore struct {
	root     string
	trace    string
	sessions string
	live     string
	sequence atomic.Uint64
}

type nativeThread struct {
	ID                     string            `json:"id"`
	Kind                   string            `json:"kind"`
	DeveloperInstructions  string            `json:"developerInstructions,omitempty"`
	DynamicTools           []json.RawMessage `json:"dynamicTools,omitempty"`
	DiscoveredInstructions []nativeItem      `json:"discoveredInstructions,omitempty"`
	Turns                  []nativeTurn      `json:"turns"`
}

type nativeTurn struct {
	ID     string       `json:"id"`
	Status string       `json:"status"`
	Items  []nativeItem `json:"items"`
}

type nativeItem struct {
	Type    string          `json:"type"`
	ID      string          `json:"id,omitempty"`
	Text    string          `json:"text,omitempty"`
	Name    string          `json:"name,omitempty"`
	Command string          `json:"command,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
}

func openFixtureStore() (*fixtureStore, error) {
	root := os.Getenv(spec.RootEnvironment)
	if root == "" || !filepath.IsAbs(root) || filepath.Clean(root) != root || strings.IndexByte(root, 0) >= 0 {
		return nil, errors.New("fixture root must be an absolute clean path")
	}
	info, err := os.Lstat(root)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || info.Mode().Perm() != 0o700 {
		return nil, errors.New("fixture root is not a restrictive directory")
	}
	store := &fixtureStore{
		root: root, trace: filepath.Join(root, spec.TraceFilename),
		sessions: filepath.Join(root, spec.SessionsDirname), live: filepath.Join(root, spec.LiveDirname),
	}
	for _, directory := range []string{store.sessions, store.live} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, err
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (store *fixtureStore) appendTrace(event spec.TraceEvent) error {
	event.PID = os.Getpid()
	event.ParentPID = os.Getppid()
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	file, err := os.OpenFile(store.trace, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Chmod(0o600); err != nil {
		return err
	}
	written, err := file.Write(raw)
	if err != nil {
		return err
	}
	if written != len(raw) {
		return errors.New("short trace write")
	}
	return file.Sync()
}

func (store *fixtureStore) markLive(executable string) (func(), error) {
	name := fmt.Sprintf("%d", os.Getpid())
	if !safeNativeID(name) {
		return nil, errors.New("invalid pid")
	}
	path := filepath.Join(store.live, name)
	if err := os.WriteFile(path, []byte(executable), 0o600); err != nil {
		return nil, err
	}
	return func() { _ = os.Remove(path) }, nil
}

func (store *fixtureStore) newID(prefix string) (string, error) {
	if !safeNativeID(prefix) {
		return "", errors.New("invalid id prefix")
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	return prefix + "-" + hex.EncodeToString(random[:]), nil
}

func (store *fixtureStore) nextID(prefix string) string {
	return fmt.Sprintf("%s-%d-%d", prefix, os.Getpid(), store.sequence.Add(1))
}

func safeNativeID(value string) bool {
	if value == "" || len(value) > 200 || value == "." || value == ".." {
		return false
	}
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') ||
			(char >= '0' && char <= '9') || char == '-' || char == '_' {
			continue
		}
		return false
	}
	return true
}

func (store *fixtureStore) sessionPath(id string) (string, error) {
	if !safeNativeID(id) {
		return "", errors.New("invalid exact session id")
	}
	return filepath.Join(store.sessions, id+".json"), nil
}

func (store *fixtureStore) writeThread(thread nativeThread) error {
	if !safeNativeID(thread.ID) || thread.Turns == nil {
		return errors.New("invalid native thread")
	}
	path, err := store.sessionPath(thread.ID)
	if err != nil {
		return err
	}
	raw, err := json.Marshal(thread)
	if err != nil || len(raw) > maxNativeSessionBytes {
		return errors.New("invalid native thread encoding")
	}
	temporary, err := os.CreateTemp(store.sessions, ".thread-*.tmp")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	cleanup := func() {
		_ = temporary.Close()
		_ = os.Remove(temporaryPath)
	}
	if err := temporary.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := temporary.Close(); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		_ = os.Remove(temporaryPath)
		return err
	}
	return os.Chmod(path, 0o600)
}

func (store *fixtureStore) readThread(id string) (nativeThread, json.RawMessage, error) {
	path, err := store.sessionPath(id)
	if err != nil {
		return nativeThread{}, nil, err
	}
	info, err := os.Lstat(path)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode().Perm()&0o077 != 0 || info.Size() < 2 || info.Size() > maxNativeSessionBytes {
		return nativeThread{}, nil, errors.New("exact native session unavailable")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nativeThread{}, nil, err
	}
	var thread nativeThread
	if json.Unmarshal(raw, &thread) != nil || thread.ID != id || thread.Turns == nil {
		return nativeThread{}, nil, errors.New("native session identity mismatch")
	}
	return thread, json.RawMessage(raw), nil
}

func projectInstructionItems(cwd string) []nativeItem {
	if cwd == "" || !filepath.IsAbs(cwd) || filepath.Clean(cwd) != cwd {
		return nil
	}
	var result []nativeItem
	for _, name := range []string{"AGENTS.md", "CLAUDE.md"} {
		path := filepath.Join(cwd, name)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() || info.Size() > 64<<10 {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		result = append(result, nativeItem{Type: "projectInstruction", Name: name, Text: string(raw)})
	}
	return result
}
