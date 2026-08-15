package web

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/api"
	"github.com/yty-build/acp-gateway/internal/config"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

type webRuntime struct {
	mu       sync.Mutex
	commands []worker.Command
	spawns   int
}

func (r *webRuntime) Spawn(context.Context, string) (worker.SpawnResult, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.spawns++
	return worker.SpawnResult{PID: 9001, Socket: "fake.sock"}, nil
}

func (r *webRuntime) Send(_ context.Context, _ string, command worker.Command) (worker.Message, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.commands = append(r.commands, command)
	return worker.Message{Type: "accepted", State: "ready", RequestID: command.RequestID}, nil
}

func (r *webRuntime) Subscribe(context.Context, string, uint64, func(worker.Message) error) error {
	return nil
}

type webFixture struct {
	handler    http.Handler
	repository *store.Store
	runtime    *webRuntime
	home       config.Home
}

func newWebFixture(t *testing.T) *webFixture {
	t.Helper()
	root := t.TempDir()
	home := config.Home{
		Root: root, Database: filepath.Join(root, "agent.db"), Agents: filepath.Join(root, "agents"),
		Workers: filepath.Join(root, "workers"), Logs: filepath.Join(root, "logs"), Worktrees: filepath.Join(root, "worktrees"),
	}
	for _, directory := range []string{home.Agents, home.Workers, home.Logs, home.Worktrees} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := store.Open(context.Background(), home.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	if _, err := repository.CreateTask(context.Background(), task.CreateParams{
		ID: "T-web", Title: `Ship <script>alert("unsafe")</script>`, Description: "Complete the local console", ExternalRef: "APP-91",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.CreateSession(context.Background(), session.CreateParams{
		ID: "S-web", TaskID: "T-web", Role: "implementer", Agent: "codex", Provider: "codex",
		RepoPath: "/private/product", Branch: "main", Status: session.StatusStarting,
		LogPath: filepath.Join(home.Logs, "S-web.jsonl"),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpdateSessionRuntimeIDs(context.Background(), "S-web", "acpx-private", "acp-private", "native-private"); err != nil {
		t.Fatal(err)
	}
	if _, err := repository.UpdateSessionWorker(
		context.Background(), "S-web", 4242, filepath.Join(home.Workers, "S-web.sock"), filepath.Join(home.Logs, "S-web.jsonl"), session.StatusWaiting,
	); err != nil {
		t.Fatal(err)
	}
	registry, err := agents.Load(home.Agents)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &webRuntime{}
	tasks := task.NewService(repository)
	sessions := appservice.NewSessionService(repository, registry, home, runtime)
	server, err := New(tasks, sessions, registry)
	if err != nil {
		t.Fatal(err)
	}
	return &webFixture{handler: server.Handler(), repository: repository, runtime: runtime, home: home}
}

func TestServerRenderedPagesAndVersionedBrowserBoundary(t *testing.T) {
	fixture := newWebFixture(t)

	tasksPage := webRequest(fixture.handler, "/")
	assertWebStatus(t, tasksPage, http.StatusOK)
	for _, expected := range []string{"Agent Task Console", "T-web", "Create a Task", "1 Session", "&lt;script&gt;"} {
		if !strings.Contains(tasksPage.Body.String(), expected) {
			t.Errorf("Tasks page does not contain %q", expected)
		}
	}
	if strings.Contains(tasksPage.Body.String(), `<script>alert`) {
		t.Fatalf("Task title was not HTML escaped: %s", tasksPage.Body.String())
	}

	taskPage := webRequest(fixture.handler, "/tasks/T-web")
	assertWebStatus(t, taskPage, http.StatusOK)
	for _, expected := range []string{"Sessions", "/sessions/S-web", "Start a Session", "Current activity", "Show raw execution", "sanitized DTO"} {
		if !strings.Contains(taskPage.Body.String(), expected) {
			t.Errorf("Task page does not contain %q", expected)
		}
	}
	assertNoWebRuntimeLeak(t, taskPage.Body.String(), fixture)

	sessionPage := webRequest(fixture.handler, "/sessions/S-web")
	assertWebStatus(t, sessionPage, http.StatusOK)
	for _, expected := range []string{"Stored + live", "Send prompt", "Cancel turn", "Resume", "Stop worker", "Start review", "Create reviewer Session", "Detach is safe", "Execution context", "/private/product"} {
		if !strings.Contains(sessionPage.Body.String(), expected) {
			t.Errorf("Session page does not contain %q", expected)
		}
	}
	assertNoWebRuntimeLeak(t, sessionPage.Body.String(), fixture)

	javascript := webRequest(fixture.handler, "/assets/app.js")
	assertWebStatus(t, javascript, http.StatusOK)
	for _, expected := range []string{"new EventSource", "/api/v1/tasks/", "/api/v1/sessions/", "/permissions/decision", "PERSISTENT", "JSON.stringify(activity", "ACP wire payloads"} {
		if !strings.Contains(javascript.Body.String(), expected) {
			t.Errorf("browser client does not contain %q", expected)
		}
	}
	if strings.Contains(javascript.Body.String(), "React") {
		t.Fatal("browser client unexpectedly uses React")
	}

	apiResponse := webRequest(fixture.handler, "/api/v1/sessions/S-web")
	assertWebStatus(t, apiResponse, http.StatusOK)
	if !strings.Contains(apiResponse.Body.String(), `"id":"S-web"`) {
		t.Fatalf("versioned API was not mounted: %s", apiResponse.Body.String())
	}
	assertNoWebRuntimeLeak(t, apiResponse.Body.String(), fixture)

	missing := webRequest(fixture.handler, "/not-a-page")
	assertWebStatus(t, missing, http.StatusNotFound)
	if sessionPage.Header().Get("Content-Security-Policy") == "" || sessionPage.Header().Get("X-Frame-Options") != "DENY" {
		t.Fatalf("web security headers = %#v", sessionPage.Header())
	}
}

func TestWebRejectsDNSRebindingAndCrossOriginControls(t *testing.T) {
	fixture := newWebFixture(t)
	rebinding := httptest.NewRequest(http.MethodGet, "/", nil)
	rebinding.Host = "attacker.example"
	rebindingResponse := httptest.NewRecorder()
	fixture.handler.ServeHTTP(rebindingResponse, rebinding)
	assertWebStatus(t, rebindingResponse, http.StatusForbidden)

	crossOrigin := httptest.NewRequest(http.MethodPost, "/api/v1/sessions/S-web/stop", nil)
	crossOrigin.Host = "127.0.0.1:8080"
	crossOrigin.Header.Set("Origin", "https://attacker.example")
	crossOrigin.Header.Set("Sec-Fetch-Site", "cross-site")
	crossOriginResponse := httptest.NewRecorder()
	fixture.handler.ServeHTTP(crossOriginResponse, crossOrigin)
	assertWebStatus(t, crossOriginResponse, http.StatusForbidden)

	current, err := fixture.repository.GetSession(context.Background(), "S-web")
	if err != nil {
		t.Fatal(err)
	}
	if current.WorkerPID != 4242 || current.Status != session.StatusWaiting {
		t.Fatalf("rejected control changed Session = %#v", current)
	}
}

func TestStoppingWebServerLeavesSessionWorkerUntouched(t *testing.T) {
	fixture := newWebFixture(t)
	before, err := fixture.repository.GetSession(context.Background(), "S-web")
	if err != nil {
		t.Fatal(err)
	}
	listener, err := api.ListenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- api.Serve(ctx, listener, fixture.handler) }()

	response, err := http.Get("http://" + listener.Addr().String() + "/sessions/S-web")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		cancel()
		t.Fatalf("Session page status = %d", response.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("web server shutdown = %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("web server did not shut down")
	}

	after, err := fixture.repository.GetSession(context.Background(), "S-web")
	if err != nil {
		t.Fatal(err)
	}
	if after.WorkerPID != before.WorkerPID || after.WorkerSocket != before.WorkerSocket || after.Status != before.Status {
		t.Fatalf("web shutdown changed worker: before=%#v after=%#v", before, after)
	}
	fixture.runtime.mu.Lock()
	defer fixture.runtime.mu.Unlock()
	if fixture.runtime.spawns != 0 || len(fixture.runtime.commands) != 0 {
		t.Fatalf("web shutdown contacted runtime: spawns=%d commands=%#v", fixture.runtime.spawns, fixture.runtime.commands)
	}
}

func TestNewRequiresSharedServices(t *testing.T) {
	if _, err := New(nil, nil, nil); err == nil {
		t.Fatal("web server accepted missing shared services")
	}
}

func webRequest(handler http.Handler, path string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.Host = "127.0.0.1:8080"
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertWebStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, want, response.Body.String())
	}
}

func assertNoWebRuntimeLeak(t *testing.T, body string, fixture *webFixture) {
	t.Helper()
	for _, secret := range []string{
		"worker_pid", "worker_socket", "log_path", "acpx-private", "acp-private", "native-private",
		filepath.Join(fixture.home.Workers, "S-web.sock"), filepath.Join(fixture.home.Logs, "S-web.jsonl"),
	} {
		if strings.Contains(body, secret) {
			t.Errorf("web boundary leaked %q: %s", secret, body)
		}
	}
}
