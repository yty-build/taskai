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
	return worker.SpawnResult{PID: 9001, Socket: "fake.sock", PortReady: true}, nil
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
	if _, err := repository.UpdateSessionProviderRef(context.Background(), "S-web", "runtime-private", "provider-private"); err != nil {
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
	for _, expected := range []string{"Agent Console", "Create a Task", "1 Session", "&lt;script&gt;", "Persistent workspaces"} {
		if !strings.Contains(tasksPage.Body.String(), expected) {
			t.Errorf("Tasks page does not contain %q", expected)
		}
	}
	if strings.Contains(tasksPage.Body.String(), `<script>alert`) {
		t.Fatalf("Task title was not HTML escaped: %s", tasksPage.Body.String())
	}

	taskPage := webRequest(fixture.handler, "/tasks/T-web")
	assertWebStatus(t, taskPage, http.StatusOK)
	for _, expected := range []string{"Conversations", "/sessions/S-web", "Connect a Session", "activity as the agent emits it", "New Session", "Task → Sessions", "Connect Session"} {
		if !strings.Contains(taskPage.Body.String(), expected) {
			t.Errorf("Task page does not contain %q", expected)
		}
	}
	if strings.Contains(taskPage.Body.String(), `data-provider-option="model"`) || strings.Contains(taskPage.Body.String(), `name="prompt"`) {
		t.Fatal("Task Session creation still mixes configuration or prompts into identity")
	}
	for _, removed := range []string{"Current activity", "Show raw execution", "activity-feed"} {
		if strings.Contains(taskPage.Body.String(), removed) {
			t.Errorf("Task page still exposes the old activity console %q", removed)
		}
	}
	assertNoWebRuntimeLeak(t, taskPage.Body.String(), fixture)
	if !strings.Contains(taskPage.Body.String(), `aria-label="New Session"`) || !strings.Contains(taskPage.Body.String(), `data-session-id="S-web"`) {
		t.Fatal("Task page is missing the named mobile action or stable Session row key")
	}

	sessionPage := webRequest(fixture.handler, "/sessions/S-web")
	assertWebStatus(t, sessionPage, http.StatusOK)
	for _, expected := range []string{"Session activity", "Connecting", "Message codex", "Stop response", "Resume", "Stop worker", "Create child Session", "Closing this page only detaches", "Session details", "Session diagnostics", "/private/product", "Reasoning effort", "Provider controls", "Apply configuration", `id="create-child-session-form"`} {
		if !strings.Contains(sessionPage.Body.String(), expected) {
			t.Errorf("Session page does not contain %q", expected)
		}
	}
	assertNoWebRuntimeLeak(t, sessionPage.Body.String(), fixture)
	for _, expected := range []string{`role="log"`, `aria-live="polite"`, `aria-live="assertive"`, `class="mobile-session-actions"`, `aria-controls="app-sidebar"`} {
		if !strings.Contains(sessionPage.Body.String(), expected) {
			t.Errorf("accessible Session page does not contain %q", expected)
		}
	}
	if strings.Count(sessionPage.Body.String(), "Stop response") < 2 || strings.Count(sessionPage.Body.String(), "Resume") < 2 {
		t.Fatal("Session response controls are not available in both desktop and mobile layouts")
	}
	if strings.Count(sessionPage.Body.String(), `aria-current="page"`) != 1 || !strings.Contains(sessionPage.Body.String(), `aria-current="location"`) {
		t.Fatal("Session navigation does not distinguish the current page from its Task ancestor")
	}
	for _, removed := range []string{"launch_phase", "prepared", "Resume initial launch", "review-dialog", "review-form", "canonical conversation", "provider-store", "history-summary", "continuity-chip"} {
		if strings.Contains(sessionPage.Body.String(), removed) {
			t.Errorf("Session page still contains removed staged/history surface %q", removed)
		}
	}

	javascript := webRequest(fixture.handler, "/assets/app.js")
	assertWebStatus(t, javascript, http.StatusOK)
	for _, expected := range []string{"new EventSource", `addEventListener("frame"`, "/api/v1/tasks/", "/provider-options", "/config", "/children", "/api/v1/sessions/", "/permissions/response", "allow_always", "provider_event", "text_delta", "tool_call", "provider_result", "makeRollingTextRoom", "completedResponseBlock", "turnHasReasoning", "nearBottom", "patchTurnElement", "patchBlockElement", "blocksPerTurn", "beginFormSubmission", "reasoning_effort"} {
		if !strings.Contains(javascript.Body.String(), expected) {
			t.Errorf("browser client does not contain %q", expected)
		}
	}
	if strings.Contains(javascript.Body.String(), "React") {
		t.Fatal("browser client unexpectedly uses React")
	}
	for _, removed := range []string{"/conversation", "/sessions/prepare", "/review", "/launch", "provider-options/reconnect", "launch_phase", "prepared", "sessionStorage", "reconcileConversation", "provider-store", "conversationSnapshotSignature", "committed", "surface-native", "surface-acp", "assistant_chunk", "thought_chunk", "tool_upsert", "plan_replace", `addEventListener("activity"`} {
		if strings.Contains(javascript.Body.String(), removed) {
			t.Errorf("browser client still contains removed conversation authority %q", removed)
		}
	}
	streamBootstrap := strings.LastIndex(javascript.Body.String(), "attachSessionChannel(`/api/v1/sessions/${encodeURIComponent(sessionID)}/stream`);")
	permissionBootstrap := strings.LastIndex(javascript.Body.String(), "refreshPermissions().then(scheduleTranscriptRender)")
	sessionBootstrap := strings.LastIndex(javascript.Body.String(), `if (page === "session")`)
	if streamBootstrap < 0 || permissionBootstrap < 0 || sessionBootstrap < 0 || streamBootstrap > permissionBootstrap {
		t.Fatal("Session page does not attach its EventSource before asynchronous option/permission hydration")
	}
	if strings.Contains(javascript.Body.String()[sessionBootstrap:], "loadProviderOptions(sessionID).catch") {
		t.Fatal("Session bootstrap reads provider options before replay ready and provider-connected boundaries")
	}
	if strings.Contains(javascript.Body.String(), "cursor=${transcriptState.lastPosition}") {
		t.Fatal("browser client pins a stale cursor across EventSource reconnects")
	}

	stylesheet := webRequest(fixture.handler, "/assets/app.css")
	assertWebStatus(t, stylesheet, http.StatusOK)
	for _, expected := range []string{"100dvh", "safe-area-inset-bottom", "prefers-reduced-motion", "forced-colors", "@media (max-width: 760px)", ".approval-shelf", ".composer-region", ".mobile-session-actions", ".tool-inline-preview", ".thought-card", ".thought-source", ".provider-status", ".activity-limit-note", ".provider-options", ".nav-session.child-session", ".session-row.child-session", ".parent-session-link"} {
		if !strings.Contains(stylesheet.Body.String(), expected) {
			t.Errorf("responsive stylesheet does not contain %q", expected)
		}
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

func TestServerRendersManagerChildRelationships(t *testing.T) {
	fixture := newWebFixture(t)
	if _, err := fixture.repository.CreateSession(context.Background(), session.CreateParams{
		ID: "S-child", TaskID: "T-web", ParentSessionID: "S-web", Role: "reviewer", Agent: "claude", Provider: "claude",
		RepoPath: "/private/child", Branch: "child", Status: session.StatusWaiting,
		LogPath: filepath.Join(fixture.home.Logs, "S-child.jsonl"),
	}); err != nil {
		t.Fatal(err)
	}

	taskPage := webRequest(fixture.handler, "/tasks/T-web")
	assertWebStatus(t, taskPage, http.StatusOK)
	for _, expected := range []string{
		`class="nav-session child-session"`,
		`class="session-row child-session"`,
		`data-parent-session-id="S-web"`,
		`Child · reviewer`,
	} {
		if !strings.Contains(taskPage.Body.String(), expected) {
			t.Errorf("Task page does not expose child relationship %q", expected)
		}
	}

	childPage := webRequest(fixture.handler, "/sessions/S-child")
	assertWebStatus(t, childPage, http.StatusOK)
	for _, expected := range []string{
		`<dt>Parent Session</dt>`,
		`class="mono parent-session-link" href="/sessions/S-web">S-web</a>`,
	} {
		if !strings.Contains(childPage.Body.String(), expected) {
			t.Errorf("Child Session details do not contain %q", expected)
		}
	}

	managerPage := webRequest(fixture.handler, "/sessions/S-web")
	assertWebStatus(t, managerPage, http.StatusOK)
	if strings.Contains(managerPage.Body.String(), `<dt>Parent Session</dt>`) {
		t.Fatal("root manager Session renders a nonexistent parent relationship")
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
		"worker_pid", "worker_socket", "log_path", "runtime_ref", "provider_ref", "runtime-private", "provider-private",
		filepath.Join(fixture.home.Workers, "S-web.sock"), filepath.Join(fixture.home.Logs, "S-web.jsonl"),
	} {
		if strings.Contains(body, secret) {
			t.Errorf("web boundary leaked %q: %s", secret, body)
		}
	}
}
