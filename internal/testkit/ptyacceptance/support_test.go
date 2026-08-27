//go:build acceptance && (darwin || linux)

package ptyacceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/runtime"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/testkit/providerfixture/spec"
)

const hiddenWorkerArgument = "--taskai-runtime-worker"

type lockedBuffer struct {
	mu  sync.Mutex
	raw bytes.Buffer
}

func (buffer *lockedBuffer) Write(value []byte) (int, error) {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.raw.Write(value)
}

func (buffer *lockedBuffer) String() string {
	buffer.mu.Lock()
	defer buffer.mu.Unlock()
	return buffer.raw.String()
}

type workerProcess struct {
	command *exec.Cmd
	result  *processResult
	stderr  *lockedBuffer
}

func startWorker(t *testing.T, binary, project, stateDir string, environment []string) *workerProcess {
	t.Helper()
	command := exec.Command(binary, hiddenWorkerArgument)
	command.Dir = project
	command.Env = append([]string(nil), environment...)
	stderr := &lockedBuffer{}
	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = stderr
	if err := command.Start(); err != nil {
		t.Fatalf("start exact test-owned worker: %v", err)
	}
	worker := &workerProcess{command: command, result: newProcessResult(), stderr: stderr}
	go func() { worker.result.finish(command.Wait()) }()
	t.Cleanup(func() { worker.stop(t) })
	worker.waitReady(t, stateDir)
	return worker
}

func (worker *workerProcess) pid() int {
	if worker == nil || worker.command == nil || worker.command.Process == nil {
		return 0
	}
	return worker.command.Process.Pid
}

func (worker *workerProcess) waitReady(t *testing.T, stateDir string) {
	t.Helper()
	deadline := time.NewTimer(acceptanceWait)
	defer deadline.Stop()
	ticker := time.NewTicker(15 * time.Millisecond)
	defer ticker.Stop()
	for {
		attempt, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
		client, err := runtime.Dial(attempt, stateDir)
		cancel()
		if err == nil {
			closeErr := client.Close()
			if closeErr != nil {
				t.Fatalf("close readiness client: %v", closeErr)
			}
			return
		}
		select {
		case <-worker.result.done:
			t.Fatalf("worker exited before IPC readiness: %v\n%s", worker.result.error(), worker.stderr.String())
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("worker IPC readiness deadline exceeded: %v\n%s", err, worker.stderr.String())
		}
	}
}

func (worker *workerProcess) stop(t *testing.T) {
	t.Helper()
	if worker == nil || worker.command == nil || worker.command.Process == nil {
		return
	}
	pid := worker.command.Process.Pid
	if err := worker.command.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		t.Errorf("signal exact worker pid %d: %v", pid, err)
	}
	if err, completed := worker.result.wait(acceptanceWait); completed {
		if err != nil {
			t.Errorf("worker pid %d exited unsuccessfully: %v\n%s", pid, err, worker.stderr.String())
		}
	} else {
		_ = worker.command.Process.Kill()
		if _, completed := worker.result.wait(3 * time.Second); !completed {
			t.Errorf("worker pid %d could not be reaped", pid)
		}
	}
	worker.command = nil
}

type acceptanceLayout struct {
	projectRoot  string
	stateRoot    string
	providerRoot string
	controlRoot  string
	fixtureBin   string
	fixturePath  string
	releaseFIFO  string
	homeRoot     string
	environment  []string
}

func prepareLayout(t *testing.T, taskaiBinary string) acceptanceLayout {
	t.Helper()
	if taskaiBinary == "" || !filepath.IsAbs(taskaiBinary) || filepath.Clean(taskaiBinary) != taskaiBinary {
		t.Fatal("TASKAI_ACCEPTANCE_BINARY must be an absolute clean path")
	}
	info, err := os.Stat(taskaiBinary)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("TASKAI_ACCEPTANCE_BINARY is not an executable regular file: %v", err)
	}
	projectRoot := restrictiveTempDir(t, "taskai-project-")
	stateParent := restrictiveTempDir(t, "taskai-state-parent-")
	providerRoot := restrictiveTempDir(t, "taskai-provider-")
	controlRoot := restrictiveTempDir(t, "taskai-control-")
	fixtureBin := filepath.Join(restrictiveTempDir(t, "taskai-fixture-bin-"), "bin")
	homeRoot := restrictiveTempDir(t, "taskai-home-")
	if err := os.Mkdir(fixtureBin, 0o700); err != nil {
		t.Fatalf("create fixture bin: %v", err)
	}
	stateRoot := filepath.Join(stateParent, "state")
	releaseFIFO := filepath.Join(providerRoot, "release.fifo")
	if err := syscall.Mkfifo(releaseFIFO, 0o600); err != nil {
		t.Fatalf("create release FIFO: %v", err)
	}
	if err := os.Chmod(releaseFIFO, 0o600); err != nil {
		t.Fatalf("chmod release FIFO: %v", err)
	}
	fixturePath := buildFixture(t, fixtureBin)
	for _, name := range []string{"codex", "codex-acp"} {
		if err := os.Link(fixturePath, filepath.Join(fixtureBin, name)); err != nil {
			t.Fatalf("link provider fixture as %s: %v", name, err)
		}
	}
	for _, directory := range []string{
		filepath.Join(homeRoot, ".codex"), filepath.Join(homeRoot, ".config"),
		filepath.Join(homeRoot, ".claude"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatalf("create synthetic home directory: %v", err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			t.Fatalf("chmod synthetic home directory: %v", err)
		}
	}
	environment := []string{
		"PATH=" + fixtureBin,
		"HOME=" + homeRoot,
		"XDG_CONFIG_HOME=" + filepath.Join(homeRoot, ".config"),
		"CODEX_HOME=" + filepath.Join(homeRoot, ".codex"),
		"CLAUDE_CONFIG_DIR=" + filepath.Join(homeRoot, ".claude"),
		"TASKAI_STATE_DIR=" + stateRoot,
		spec.RootEnvironment + "=" + providerRoot,
		spec.ReleaseFIFOEnvironment + "=" + releaseFIFO,
		"TERM=xterm-256color",
		"LANG=C",
		"LC_ALL=C",
		"TZ=UTC",
		"NO_COLOR=1",
	}
	layout := acceptanceLayout{
		projectRoot: projectRoot, stateRoot: stateRoot, providerRoot: providerRoot,
		controlRoot: controlRoot, fixtureBin: fixtureBin, fixturePath: fixturePath,
		releaseFIFO: releaseFIFO, homeRoot: homeRoot, environment: environment,
	}
	assertDisjointRoots(t, layout.projectRoot, layout.stateRoot, layout.providerRoot,
		layout.controlRoot, layout.fixtureBin, layout.homeRoot)
	return layout
}

func assertDisjointRoots(t *testing.T, roots ...string) {
	t.Helper()
	for left := range roots {
		for right := left + 1; right < len(roots); right++ {
			leftToRight, leftErr := filepath.Rel(roots[left], roots[right])
			rightToLeft, rightErr := filepath.Rel(roots[right], roots[left])
			if leftErr != nil || rightErr != nil || leftToRight == "." || rightToLeft == "." ||
				(leftToRight != ".." && !strings.HasPrefix(leftToRight, ".."+string(filepath.Separator))) ||
				(rightToLeft != ".." && !strings.HasPrefix(rightToLeft, ".."+string(filepath.Separator))) {
				t.Fatalf("acceptance roots overlap: %q and %q", roots[left], roots[right])
			}
		}
	}
}

func restrictiveTempDir(t *testing.T, pattern string) string {
	t.Helper()
	// Keep Unix-domain endpoint paths comfortably below Darwin's shorter
	// sockaddr_un limit. EvalSymlinks below still gives the worker one exact
	// canonical path when /tmp is an operating-system alias.
	directory, err := os.MkdirTemp("/tmp", pattern)
	if err != nil {
		t.Fatalf("create temporary directory: %v", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatalf("chmod temporary directory: %v", err)
	}
	canonical, err := filepath.EvalSymlinks(directory)
	if err != nil || !filepath.IsAbs(canonical) {
		t.Fatalf("canonicalize temporary directory: %v", err)
	}
	directory = canonical
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	return directory
}

func buildFixture(t *testing.T, binDir string) string {
	t.Helper()
	repository := repositoryRoot(t)
	output := filepath.Join(binDir, "providerfixture")
	command := exec.Command("go", "build", "-trimpath", "-o", output, "./internal/testkit/providerfixture")
	command.Dir = repository
	raw, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("build provider fixture: %v\n%s", err, raw)
	}
	if err := os.Chmod(output, 0o700); err != nil {
		t.Fatalf("chmod provider fixture: %v", err)
	}
	return output
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("get working directory: %v", err)
	}
	for {
		if info, statErr := os.Stat(filepath.Join(directory, "go.mod")); statErr == nil && info.Mode().IsRegular() {
			return directory
		}
		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatal("repository root containing go.mod not found")
		}
		directory = parent
	}
}

type traceLog struct{ path string }

func (log traceLog) snapshot(t *testing.T) []spec.TraceEvent {
	t.Helper()
	raw, err := os.ReadFile(log.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatalf("read provider trace: %v", err)
	}
	if len(raw) > 32<<20 {
		t.Fatalf("provider trace exceeds bound: %d", len(raw))
	}
	lines := bytes.Split(raw, []byte{'\n'})
	events := make([]spec.TraceEvent, 0, len(lines))
	for index, line := range lines {
		if len(line) == 0 {
			continue
		}
		var event spec.TraceEvent
		if err := json.Unmarshal(line, &event); err != nil {
			if index == len(lines)-1 && raw[len(raw)-1] != '\n' {
				continue
			}
			t.Fatalf("decode provider trace line %d: %v", index+1, err)
		}
		if event.Event == "" || event.PID <= 0 || event.ParentPID <= 0 {
			t.Fatalf("invalid provider trace line %d: %#v", index+1, event)
		}
		events = append(events, event)
	}
	return events
}

func (log traceLog) wait(t *testing.T, description string, predicate func([]spec.TraceEvent) bool) []spec.TraceEvent {
	t.Helper()
	deadline := time.NewTimer(acceptanceWait)
	defer deadline.Stop()
	ticker := time.NewTicker(15 * time.Millisecond)
	defer ticker.Stop()
	for {
		events := log.snapshot(t)
		if predicate(events) {
			return events
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatalf("provider trace deadline exceeded waiting for %s\n%s", description, traceSummary(events))
		}
	}
}

func traceSummary(events []spec.TraceEvent) string {
	var result strings.Builder
	for _, event := range events {
		fmt.Fprintf(&result, "%s pid=%d ppid=%d exe=%s method=%s operation=%s thread=%s session=%s\n",
			event.Event, event.PID, event.ParentPID, event.Executable, event.Method,
			event.Operation, event.ThreadID, event.SessionID)
	}
	return result.String()
}

func countEvents(events []spec.TraceEvent, eventName string) int {
	count := 0
	for _, event := range events {
		if event.Event == eventName {
			count++
		}
	}
	return count
}

func eventsNamed(events []spec.TraceEvent, eventName string) []spec.TraceEvent {
	var result []spec.TraceEvent
	for _, event := range events {
		if event.Event == eventName {
			result = append(result, event)
		}
	}
	return result
}

func assertWorkerEndpointModes(t *testing.T, stateDir string) {
	t.Helper()
	assertPathMode(t, stateDir, os.ModeDir, 0o700)
	assertPathMode(t, filepath.Join(stateDir, runtime.SocketFilename), os.ModeSocket, 0o600)
	assertPathMode(t, filepath.Join(stateDir, runtime.LockFilename), 0, 0o600)
	assertPathMode(t, filepath.Join(stateDir, store.DatabaseFilename), 0, 0o600)
}

func assertPathMode(t *testing.T, path string, wantType fs.FileMode, wantPermission fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("lstat %s: %v", path, err)
	}
	if info.Mode().Perm() != wantPermission {
		t.Fatalf("mode %s = %04o, want %04o", path, info.Mode().Perm(), wantPermission)
	}
	if wantType == os.ModeDir && !info.IsDir() {
		t.Fatalf("%s is not a directory", path)
	}
	if wantType == os.ModeSocket && info.Mode()&os.ModeSocket == 0 {
		t.Fatalf("%s is not a socket", path)
	}
	if wantType == 0 && !info.Mode().IsRegular() {
		t.Fatalf("%s is not a regular file", path)
	}
}

func assertStoppedEndpointRemoved(t *testing.T, stateDir string) {
	t.Helper()
	for _, name := range []string{runtime.SocketFilename, runtime.LockFilename} {
		if _, err := os.Lstat(filepath.Join(stateDir, name)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("stopped worker retained %s: %v", name, err)
		}
	}
}

func assertRestrictiveTree(t *testing.T, root string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		switch {
		case info.IsDir():
			if info.Mode().Perm() != 0o700 {
				return fmt.Errorf("directory %s mode %04o", path, info.Mode().Perm())
			}
		case info.Mode().IsRegular(), info.Mode()&os.ModeSocket != 0, info.Mode()&os.ModeNamedPipe != 0:
			if info.Mode().Perm()&0o077 != 0 {
				return fmt.Errorf("private path %s mode %04o", path, info.Mode().Perm())
			}
		default:
			return fmt.Errorf("unexpected state path type %s mode %s", path, info.Mode())
		}
		return nil
	})
	if err != nil {
		t.Fatalf("restrictive tree audit: %v", err)
	}
}

func assertTreeExcludes(t *testing.T, root string, forbidden []string) {
	t.Helper()
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if info.Size() > 64<<20 {
			return fmt.Errorf("file exceeds audit bound: %s", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, value := range forbidden {
			if value != "" && bytes.Contains(raw, []byte(value)) {
				return fmt.Errorf("forbidden content found in %s", path)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("TaskAI content persistence audit: %v", err)
	}
}

func assertTreeContains(t *testing.T, root string, required []string) {
	t.Helper()
	found := make(map[string]bool, len(required))
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}
		info, err := entry.Info()
		if err != nil || !info.Mode().IsRegular() {
			return err
		}
		if info.Size() > 64<<20 {
			return fmt.Errorf("file exceeds audit bound: %s", path)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, value := range required {
			if bytes.Contains(raw, []byte(value)) {
				found[value] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("provider content audit: %v", err)
	}
	var missing []string
	for _, value := range required {
		if !found[value] {
			missing = append(missing, value)
		}
	}
	if len(missing) != 0 {
		t.Fatalf("provider-owned content audit missing %q", missing)
	}
}

func fileHash(t *testing.T, path string) [sha256.Size]byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("hash %s: %v", path, err)
	}
	return sha256.Sum256(raw)
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func providerPIDs(events []spec.TraceEvent) []int {
	set := make(map[int]struct{})
	for _, event := range events {
		if event.Event == spec.EventProcessStart && event.PID > 0 &&
			(event.Executable == "codex" || event.Executable == "codex-acp") {
			set[event.PID] = struct{}{}
		}
	}
	result := make([]int, 0, len(set))
	for pid := range set {
		result = append(result, pid)
	}
	sort.Ints(result)
	return result
}

func assertProviderProcessesStop(t *testing.T, providerRoot string, events []spec.TraceEvent) {
	t.Helper()
	for _, pid := range providerPIDs(events) {
		waitForProcessExit(t, providerRoot, pid)
	}
}

func waitForProcessExit(t *testing.T, providerRoot string, pid int) {
	t.Helper()
	deadline := time.NewTimer(acceptanceWait)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for processAlive(pid) {
		select {
		case <-ticker.C:
		case <-deadline.C:
			if !ownedProviderMarker(providerRoot, pid) {
				t.Fatalf("provider pid %d remains but no exact ownership marker permits cleanup", pid)
			}
			_ = syscall.Kill(pid, syscall.SIGTERM)
			if !waitForExactPID(pid, 2*time.Second) && ownedProviderMarker(providerRoot, pid) {
				_ = syscall.Kill(pid, syscall.SIGKILL)
			}
			if !waitForExactPID(pid, 2*time.Second) {
				t.Fatalf("exact provider pid %d did not stop", pid)
			}
			return
		}
	}
}

func ownedProviderMarker(providerRoot string, pid int) bool {
	path := filepath.Join(providerRoot, spec.LiveDirname, strconv.Itoa(pid))
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return false
	}
	raw, err := os.ReadFile(path)
	return err == nil && (string(raw) == "codex" || string(raw) == "codex-acp")
}

func waitForExactPID(pid int, timeout time.Duration) bool {
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for processAlive(pid) {
		select {
		case <-ticker.C:
		case <-deadline.C:
			return false
		}
	}
	return true
}

func releaseSubordinate(t *testing.T, fifo string) {
	t.Helper()
	deadline := time.NewTimer(acceptanceWait)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	var writer *os.File
	for writer == nil {
		file, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		if err == nil {
			writer = file
			break
		}
		if !errors.Is(err, syscall.ENXIO) {
			t.Fatalf("open exact subordinate release FIFO: %v", err)
		}
		select {
		case <-ticker.C:
		case <-deadline.C:
			t.Fatal("subordinate release FIFO never acquired a reader")
		}
	}
	if _, err := writer.Write([]byte{1}); err != nil {
		_ = writer.Close()
		t.Fatalf("release subordinate: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close subordinate release FIFO: %v", err)
	}
}

func runCommand(t *testing.T, binary, project string, environment, args []string) (int, string, string) {
	t.Helper()
	command := exec.Command(binary, args...)
	command.Dir = project
	command.Env = append([]string(nil), environment...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	if err == nil {
		return 0, stdout.String(), stderr.String()
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), stdout.String(), stderr.String()
	}
	t.Fatalf("run command %q: %v", args, err)
	return -1, "", ""
}
