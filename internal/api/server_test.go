package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/config"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

type apiRuntime struct {
	mu         sync.Mutex
	repository *store.Store
	home       config.Home
	nextPID    int
	sockets    map[string]string
	commands   []worker.Command
	pending    map[string][]worker.PendingPermission
}

func (r *apiRuntime) Spawn(ctx context.Context, sessionID string) (worker.SpawnResult, error) {
	r.mu.Lock()
	r.nextPID++
	pid := 7000 + r.nextPID
	socket := filepath.Join(r.home.Workers, sessionID+".sock")
	r.sockets[socket] = sessionID
	r.mu.Unlock()
	current, err := r.repository.UpdateSessionWorker(ctx, sessionID, pid, socket, filepath.Join(r.home.Logs, sessionID+".jsonl"), session.StatusWaiting)
	if err != nil {
		return worker.SpawnResult{}, err
	}
	return worker.SpawnResult{PID: current.WorkerPID, Socket: current.WorkerSocket}, nil
}

func (r *apiRuntime) Send(ctx context.Context, socket string, command worker.Command) (worker.Message, error) {
	r.mu.Lock()
	sessionID, ok := r.sockets[socket]
	r.commands = append(r.commands, command)
	r.mu.Unlock()
	if !ok {
		return worker.Message{}, fmt.Errorf("fake worker unavailable")
	}
	if command.Type == "stop" {
		if _, err := r.repository.ClearSessionWorker(ctx, sessionID, session.StatusStopped); err != nil {
			return worker.Message{}, err
		}
		r.mu.Lock()
		delete(r.sockets, socket)
		r.mu.Unlock()
	}
	if command.Type == "status" {
		r.mu.Lock()
		permissions := append([]worker.PendingPermission(nil), r.pending[sessionID]...)
		r.mu.Unlock()
		return worker.Message{Type: "status", Permissions: permissions}, nil
	}
	if command.Type == "permission_decision" {
		r.mu.Lock()
		delete(r.pending, sessionID)
		r.mu.Unlock()
		return worker.Message{Type: "permission_decided"}, nil
	}
	return worker.Message{Type: "accepted", State: "ready", RequestID: command.RequestID}, nil
}

func (r *apiRuntime) Subscribe(ctx context.Context, socket string, cursor uint64, consume func(worker.Message) error) error {
	r.mu.Lock()
	sessionID, ok := r.sockets[socket]
	r.mu.Unlock()
	if !ok {
		return fmt.Errorf("fake worker unavailable")
	}
	current, err := r.repository.GetSession(ctx, sessionID)
	if err != nil {
		return err
	}
	lines, err := sessionlog.ReadFile(current.LogPath, cursor)
	if err != nil {
		return err
	}
	for _, line := range lines {
		if err := consume(worker.Message{
			Type: "line", Seq: line.Seq, Timestamp: line.Timestamp, RequestID: line.RequestID,
			LineType: line.Type, Data: line.Data,
		}); err != nil {
			return err
		}
	}
	return nil
}

type apiFixture struct {
	handler    http.Handler
	repository *store.Store
	runtime    *apiRuntime
	home       config.Home
	repo       string
}

func newAPIFixture(t *testing.T) *apiFixture {
	t.Helper()
	root := t.TempDir()
	home := config.Home{
		Root: root, Database: filepath.Join(root, "agent.db"), Agents: filepath.Join(root, "agents"),
		Workers: filepath.Join(root, "workers"), Logs: filepath.Join(root, "logs"), Worktrees: filepath.Join(root, "worktrees"),
	}
	for _, path := range []string{home.Agents, home.Workers, home.Logs, home.Worktrees} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	repository, err := store.Open(context.Background(), home.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	repo := filepath.Join(root, "repo")
	if err := os.MkdirAll(repo, 0o700); err != nil {
		t.Fatal(err)
	}
	initializeAPIRepository(t, repo)
	registry, err := agents.Load(home.Agents)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &apiRuntime{repository: repository, home: home, sockets: make(map[string]string), pending: make(map[string][]worker.PendingPermission)}
	sessions := appservice.NewSessionService(repository, registry, home, runtime)
	server := New(task.NewService(repository), sessions)
	return &apiFixture{handler: server.Handler(), repository: repository, runtime: runtime, home: home, repo: repo}
}

func TestVersionedTaskAndSessionAPI(t *testing.T) {
	fixture := newAPIFixture(t)

	health := performRequest(t, fixture.handler, http.MethodGet, "/healthz", nil, nil)
	assertStatus(t, health, http.StatusOK)
	if health.Header().Get("X-Content-Type-Options") != "nosniff" || health.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("security headers = %#v", health.Header())
	}

	invalid := performJSON(t, fixture.handler, http.MethodPost, "/api/v1/tasks", map[string]any{"title": "bad", "unknown": true})
	assertStatus(t, invalid, http.StatusBadRequest)

	createdTaskResponse := performJSON(t, fixture.handler, http.MethodPost, "/api/v1/tasks", map[string]any{
		"title": "Ship console", "description": "Implement local V1", "external_ref": "APP-91",
		"metadata": map[string]any{"priority": "high"},
	})
	assertStatus(t, createdTaskResponse, http.StatusCreated)
	var createdTask struct {
		Task TaskDTO `json:"task"`
	}
	decodeResponse(t, createdTaskResponse, &createdTask)
	if createdTask.Task.ID == "" || createdTask.Task.Status != "open" || createdTask.Task.SessionCount != 0 || createdTask.Task.Metadata["priority"] != "high" {
		t.Fatalf("created Task DTO = %#v", createdTask.Task)
	}
	taskID := createdTask.Task.ID

	listTasks := performRequest(t, fixture.handler, http.MethodGet, "/api/v1/tasks", nil, nil)
	assertStatus(t, listTasks, http.StatusOK)
	assertContains(t, listTasks.Body.String(), taskID)
	taskDetail := performRequest(t, fixture.handler, http.MethodGet, "/api/v1/tasks/"+taskID, nil, nil)
	assertStatus(t, taskDetail, http.StatusOK)
	listSessions := performRequest(t, fixture.handler, http.MethodGet, "/api/v1/tasks/"+taskID+"/sessions", nil, nil)
	assertStatus(t, listSessions, http.StatusOK)
	assertContains(t, listSessions.Body.String(), `"sessions":[]`)

	startedResponse := performJSON(t, fixture.handler, http.MethodPost, "/api/v1/tasks/"+taskID+"/sessions", map[string]any{
		"agent": "codex", "role": "implementer", "repo_path": fixture.repo,
		"prompt": "Start D12", "approve_all": true,
	})
	assertStatus(t, startedResponse, http.StatusCreated)
	var started struct {
		Session   SessionDTO `json:"session"`
		RequestID string     `json:"request_id"`
		State     string     `json:"state"`
	}
	decodeResponse(t, startedResponse, &started)
	if started.Session.ID == "" || started.Session.TaskID != taskID || started.Session.Permission != "approve-all" || !started.Session.WorkerRunning {
		t.Fatalf("started Session DTO = %#v", started.Session)
	}
	if started.RequestID == "" || started.State != "ready" {
		t.Fatalf("initial correlation = request %q state %q", started.RequestID, started.State)
	}
	sessionID := started.Session.ID

	if _, err := fixture.repository.UpdateSessionRuntimeIDs(context.Background(), sessionID, "acpx-secret", "acp-secret", "native-secret"); err != nil {
		t.Fatal(err)
	}
	getSession := performRequest(t, fixture.handler, http.MethodGet, "/api/v1/sessions/"+sessionID, nil, nil)
	assertStatus(t, getSession, http.StatusOK)
	assertDTOIsRedacted(t, getSession.Body.String(), fixture.home, sessionID)
	taskDetail = performRequest(t, fixture.handler, http.MethodGet, "/api/v1/tasks/"+taskID, nil, nil)
	assertStatus(t, taskDetail, http.StatusOK)
	assertDTOIsRedacted(t, taskDetail.Body.String(), fixture.home, sessionID)
	listSessions = performRequest(t, fixture.handler, http.MethodGet, "/api/v1/tasks/"+taskID+"/sessions", nil, nil)
	assertStatus(t, listSessions, http.StatusOK)
	assertContains(t, listSessions.Body.String(), sessionID)
	assertDTOIsRedacted(t, listSessions.Body.String(), fixture.home, sessionID)

	prompt := performJSON(t, fixture.handler, http.MethodPost, "/api/v1/sessions/"+sessionID+"/prompt", map[string]string{"message": "Continue safely"})
	assertStatus(t, prompt, http.StatusAccepted)
	var promptResult map[string]string
	decodeResponse(t, prompt, &promptResult)
	if promptResult["request_id"] == "" || promptResult["request_id"] == started.RequestID {
		t.Fatalf("prompt request ID = %q", promptResult["request_id"])
	}
	fixture.runtime.mu.Lock()
	fixture.runtime.pending[sessionID] = []worker.PendingPermission{{
		ID: "permission-safe-1", RequestID: "request-permission",
		Title: "Write marker\x1b]52;c;hidden\a", Kind: "edit",
		Options: []worker.PermissionOption{
			{ID: "allow-once", Name: "Allow once", Kind: "allow_once"},
			{ID: "reject-once", Name: "Reject", Kind: "reject_once"},
		},
	}}
	fixture.runtime.mu.Unlock()
	permissions := performRequest(t, fixture.handler, http.MethodGet, "/api/v1/sessions/"+sessionID+"/permissions", nil, nil)
	assertStatus(t, permissions, http.StatusOK)
	var permissionResult struct {
		Permissions []PermissionDTO `json:"permissions"`
	}
	decodeResponse(t, permissions, &permissionResult)
	if len(permissionResult.Permissions) != 1 || permissionResult.Permissions[0].ID != "permission-safe-1" || len(permissionResult.Permissions[0].Options) != 2 {
		t.Fatalf("permissions response = %#v", permissionResult)
	}
	if strings.Contains(permissionResult.Permissions[0].Title, "\x1b") {
		t.Fatalf("permission title retained terminal controls: %q", permissionResult.Permissions[0].Title)
	}
	decision := performJSON(t, fixture.handler, http.MethodPost, "/api/v1/sessions/"+sessionID+"/permissions/decision", map[string]any{
		"permission_id": "permission-safe-1", "option_id": "allow-once",
	})
	assertStatus(t, decision, http.StatusAccepted)
	fixture.runtime.mu.Lock()
	lastCommand := fixture.runtime.commands[len(fixture.runtime.commands)-1]
	fixture.runtime.mu.Unlock()
	if lastCommand.Type != "permission_decision" || lastCommand.PermissionID != "permission-safe-1" || lastCommand.OptionID != "allow-once" {
		t.Fatalf("permission transport command = %#v", lastCommand)
	}
	cancel := performRequest(t, fixture.handler, http.MethodPost, "/api/v1/sessions/"+sessionID+"/cancel", nil, nil)
	assertStatus(t, cancel, http.StatusAccepted)
	stopped := performRequest(t, fixture.handler, http.MethodPost, "/api/v1/sessions/"+sessionID+"/stop", nil, nil)
	assertStatus(t, stopped, http.StatusOK)
	resumed := performJSON(t, fixture.handler, http.MethodPost, "/api/v1/sessions/"+sessionID+"/resume", map[string]any{})
	assertStatus(t, resumed, http.StatusOK)
	assertContains(t, resumed.Body.String(), `"worker_running":true`)
	stopped = performRequest(t, fixture.handler, http.MethodPost, "/api/v1/sessions/"+sessionID+"/stop", nil, nil)
	assertStatus(t, stopped, http.StatusOK)

	current, err := fixture.repository.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	log, err := sessionlog.Open(current.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append("request-one", "notice", map[string]any{"message": "visible one", "raw_secret": "DO_NOT_EXPOSE"}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append("request-two", "notice", map[string]any{"message": "visible two", "raw_secret": "DO_NOT_EXPOSE"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	sessionStream := performRequest(t, fixture.handler, http.MethodGet, "/api/v1/sessions/"+sessionID+"/stream", nil, map[string]string{"Last-Event-ID": "1"})
	assertStatus(t, sessionStream, http.StatusOK)
	sessionEvents := parseSSE(t, sessionStream.Body.String())
	if len(sessionEvents) != 1 || sessionEvents[0].ID != "2" || sessionEvents[0].Activity.RequestID != "request-two" {
		t.Fatalf("Session reconnect events = %#v", sessionEvents)
	}
	if strings.Contains(sessionStream.Body.String(), "DO_NOT_EXPOSE") || strings.Contains(sessionStream.Body.String(), `"data"`) {
		t.Fatalf("raw Session data crossed API boundary: %s", sessionStream.Body.String())
	}
	if sessionEvents[0].Activity.SessionID != sessionID || sessionEvents[0].Activity.Position != 2 || sessionEvents[0].Activity.Activity != "notice" {
		t.Fatalf("normalized Session activity = %#v", sessionEvents[0].Activity)
	}
	if strings.Contains(sessionStream.Body.String(), "visible two") {
		t.Fatalf("arbitrary diagnostic message crossed API boundary: %s", sessionStream.Body.String())
	}
	if !strings.Contains(sessionStream.Body.String(), "event: ready\ndata: {}") {
		t.Fatalf("Session stream did not mark the replay/live boundary: %s", sessionStream.Body.String())
	}

	disagree := performRequest(t, fixture.handler, http.MethodGet, "/api/v1/sessions/"+sessionID+"/stream?cursor=1", nil, map[string]string{"Last-Event-ID": "2"})
	assertStatus(t, disagree, http.StatusBadRequest)
	badTaskCursor := performRequest(t, fixture.handler, http.MethodGet, "/api/v1/tasks/"+taskID+"/stream?cursor=not_base64!", nil, nil)
	assertStatus(t, badTaskCursor, http.StatusBadRequest)
	foreign := make(map[string]uint64, maxCursorSessions)
	for index := 0; index < maxCursorSessions; index++ {
		foreign[fmt.Sprintf("%x", index)] = 0
	}
	foreignToken, err := encodeTaskCursor(foreign)
	if err != nil {
		t.Fatal(err)
	}
	filteredTaskEvents := taskStreamEvents(t, fixture.handler, "/api/v1/tasks/"+taskID+"/stream?cursor="+foreignToken, "")
	if len(filteredTaskEvents) != 2 {
		t.Fatalf("foreign cursor replay = %#v", filteredTaskEvents)
	}
	for _, event := range filteredTaskEvents {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.Header.Set("Last-Event-ID", event.ID)
		decoded, decodeErr := taskCursor(request)
		if decodeErr != nil || len(decoded) != 1 || decoded[sessionID] == 0 {
			t.Fatalf("emitted Task cursor is not self-validating: %#v, %v", decoded, decodeErr)
		}
	}

	allTaskEvents := taskStreamEvents(t, fixture.handler, "/api/v1/tasks/"+taskID+"/stream", "")
	if len(allTaskEvents) != 2 || allTaskEvents[0].Activity.SessionID != sessionID || allTaskEvents[1].Activity.SessionID != sessionID {
		t.Fatalf("initial Task stream = %#v", allTaskEvents)
	}
	if allTaskEvents[0].ID == "" || allTaskEvents[0].Activity.Cursor != allTaskEvents[0].ID {
		t.Fatalf("Task cursor event = %#v", allTaskEvents[0])
	}

	secondResponse := performJSON(t, fixture.handler, http.MethodPost, "/api/v1/tasks/"+taskID+"/sessions", map[string]any{
		"agent": "codex", "role": "reviewer", "repo_path": fixture.repo,
	})
	assertStatus(t, secondResponse, http.StatusCreated)
	var secondStarted struct {
		Session SessionDTO `json:"session"`
	}
	decodeResponse(t, secondResponse, &secondStarted)
	secondSessionID := secondStarted.Session.ID
	assertStatus(t, performRequest(t, fixture.handler, http.MethodPost, "/api/v1/sessions/"+secondSessionID+"/stop", nil, nil), http.StatusOK)
	secondCurrent, err := fixture.repository.GetSession(context.Background(), secondSessionID)
	if err != nil {
		t.Fatal(err)
	}
	secondLog, err := sessionlog.Open(secondCurrent.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := secondLog.Append("review-request", "notice", map[string]string{"message": "review visible"}); err != nil {
		t.Fatal(err)
	}
	if err := secondLog.Close(); err != nil {
		t.Fatal(err)
	}

	replayedTaskEvents := taskStreamEvents(t, fixture.handler, "/api/v1/tasks/"+taskID+"/stream", allTaskEvents[0].ID)
	if len(replayedTaskEvents) != 2 {
		t.Fatalf("Task reconnect events = %#v", replayedTaskEvents)
	}
	seen := map[string]bool{}
	for _, event := range replayedTaskEvents {
		seen[event.Activity.SessionID] = true
	}
	if !seen[sessionID] || !seen[secondSessionID] {
		t.Fatalf("Task replay did not aggregate every Session: %#v", replayedTaskEvents)
	}
	reviewResponse := performJSON(t, fixture.handler, http.MethodPost, "/api/v1/sessions/"+sessionID+"/review", map[string]any{"agent": "claude"})
	assertStatus(t, reviewResponse, http.StatusCreated)
	var reviewStarted struct {
		Session   SessionDTO `json:"session"`
		RequestID string     `json:"request_id"`
	}
	decodeResponse(t, reviewResponse, &reviewStarted)
	if reviewStarted.Session.TaskID != taskID || reviewStarted.Session.Role != "reviewer" || reviewStarted.Session.Agent != "claude" || reviewStarted.RequestID == "" {
		t.Fatalf("review response = %#v", reviewStarted)
	}
	assertStatus(t, performRequest(t, fixture.handler, http.MethodPost, "/api/v1/sessions/"+reviewStarted.Session.ID+"/stop", nil, nil), http.StatusOK)
	assertDTOIsRedacted(t, getSession.Body.String(), fixture.home, sessionID)
}

func TestActivityPermissionDTOExposesOnlyActionableSafeFields(t *testing.T) {
	line := sessionlog.Line{
		Seq: 4, RequestID: "REQ", Type: "permission_requested",
		Data: json.RawMessage(`{"id":"permission-4:tool","request_id":"REQ","tool_call_id":"tool","title":"Write marker","kind":"edit","options":[{"id":"allow","name":"Allow once","kind":"allow_once"}]}`),
	}
	dto := makeActivityDTO("S-one", line, "")
	if dto.Permission == nil || dto.Permission.ID != "permission-4:tool" || dto.Permission.State != "waiting" || len(dto.Permission.Options) != 1 {
		t.Fatalf("permission activity DTO = %#v", dto.Permission)
	}
	encoded, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"worker_socket", "log_path", "rawInput", "environment", "tool_call_id"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("permission DTO leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestAbandonedPermissionDTOHidesTransportError(t *testing.T) {
	line := sessionlog.Line{
		Seq: 5, RequestID: "REQ", Type: "permission_abandoned",
		Data: json.RawMessage(`{"permission_id":"permission-5:tool","message":"bridge failed: TOKEN=must-not-cross /private/provider/path"}`),
	}
	dto := makeActivityDTO("S-one", line, "")
	if dto.Permission == nil || dto.Permission.State != "abandoned" || dto.Permission.Decision != "unavailable" {
		t.Fatalf("abandoned permission DTO = %#v", dto.Permission)
	}
	encoded, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"must-not-cross", "/private/provider/path", "bridge failed"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("abandoned permission leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestActivityDTOCarriesBoundedSemanticDisplayWithoutACPEnvelope(t *testing.T) {
	event, err := json.Marshal(acp.Event{
		EventVersion: 1, RequestID: "REQ", Type: "agent_message_chunk",
		Raw: json.RawMessage(`{"jsonrpc":"2.0","params":{"update":{"content":{"type":"text","text":"A readable answer"},"messageId":"message-1"}},"environment":{"TOKEN":"must-not-appear"}}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	dto := makeActivityDTO("S-one", sessionlog.Line{Seq: 5, RequestID: "REQ", Type: "acp", Data: event}, "")
	if dto.Display == nil || dto.Display.Type != "assistant_chunk" || dto.Display.Text != "A readable answer" {
		t.Fatalf("semantic display = %#v", dto.Display)
	}
	encoded, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"jsonrpc", "must-not-appear", "environment", "eventVersion"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("activity DTO leaked %q: %s", forbidden, encoded)
		}
	}
}

func TestLoopbackListenerAndGracefulServe(t *testing.T) {
	for _, address := range []string{"0.0.0.0:0", "[::]:0", ":0", "localhost:0", "192.0.2.10:0"} {
		if listener, err := ListenLoopback(address); err == nil {
			_ = listener.Close()
			t.Errorf("ListenLoopback(%q) accepted public or implicit binding", address)
		}
	}
	listener, err := ListenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	handler := New(nil, nil).Handler()
	go func() { done <- Serve(ctx, listener, handler) }()
	response, err := http.Get("http://" + listener.Addr().String() + "/healthz")
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("health status = %d", response.StatusCode)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Serve shutdown error = %v", err)
		}
	case <-time.After(4 * time.Second):
		t.Fatal("Serve did not shut down")
	}
}

type parsedSSE struct {
	ID       string
	Activity ActivityDTO
}

func taskStreamEvents(t *testing.T, handler http.Handler, path, lastEventID string) []parsedSSE {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 75*time.Millisecond)
	defer cancel()
	headers := map[string]string{}
	if lastEventID != "" {
		headers["Last-Event-ID"] = lastEventID
	}
	response := performRequestWithContext(t, ctx, handler, http.MethodGet, path, nil, headers)
	assertStatus(t, response, http.StatusOK)
	return parseSSE(t, response.Body.String())
}

func parseSSE(t *testing.T, body string) []parsedSSE {
	t.Helper()
	var result []parsedSSE
	for _, block := range strings.Split(body, "\n\n") {
		if strings.TrimSpace(block) == "" || strings.HasPrefix(block, ":") {
			continue
		}
		var event parsedSSE
		for _, line := range strings.Split(block, "\n") {
			switch {
			case strings.HasPrefix(line, "id: "):
				event.ID = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event.Activity); err != nil {
					t.Fatalf("decode SSE data: %v\n%s", err, line)
				}
			}
		}
		if event.ID != "" {
			result = append(result, event)
		}
	}
	return result
}

func performJSON(t *testing.T, handler http.Handler, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return performRequest(t, handler, method, path, encoded, map[string]string{"Content-Type": "application/json"})
}

func performRequest(t *testing.T, handler http.Handler, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return performRequestWithContext(t, context.Background(), handler, method, path, body, headers)
}

func performRequestWithContext(t *testing.T, ctx context.Context, handler http.Handler, method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewReader(body)).WithContext(ctx)
	for name, value := range headers {
		request.Header.Set(name, value)
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func assertStatus(t *testing.T, response *httptest.ResponseRecorder, want int) {
	t.Helper()
	if response.Code != want {
		t.Fatalf("status = %d, want %d; body=%s", response.Code, want, response.Body.String())
	}
}

func decodeResponse(t *testing.T, response *httptest.ResponseRecorder, destination any) {
	t.Helper()
	if err := json.Unmarshal(response.Body.Bytes(), destination); err != nil {
		t.Fatalf("decode response: %v\n%s", err, response.Body.String())
	}
}

func assertContains(t *testing.T, value, expected string) {
	t.Helper()
	if !strings.Contains(value, expected) {
		t.Fatalf("%q does not contain %q", value, expected)
	}
}

func assertDTOIsRedacted(t *testing.T, value string, home config.Home, sessionID string) {
	t.Helper()
	for _, forbidden := range []string{
		"worker_pid", "worker_socket", "log_path", "metadata_json", "acpx_record_id", "acp_session_id", "native_session_id",
		"acpx-secret", "acp-secret", "native-secret", filepath.Join(home.Workers, sessionID+".sock"), filepath.Join(home.Logs, sessionID+".jsonl"),
	} {
		if strings.Contains(value, forbidden) {
			t.Errorf("API DTO leaked %q: %s", forbidden, value)
		}
	}
}

func initializeAPIRepository(t *testing.T, path string) {
	t.Helper()
	for _, args := range [][]string{
		{"init", "-b", "main"}, {"config", "user.name", "API Test"}, {"config", "user.email", "api@example.invalid"},
	} {
		command := exec.Command("git", args...)
		command.Dir = path
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
		}
	}
	if err := os.WriteFile(filepath.Join(path, "README.md"), []byte("fixture\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{{"add", "README.md"}, {"commit", "-m", "fixture"}} {
		command := exec.Command("git", args...)
		command.Dir = path
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, output)
		}
	}
}

func TestCursorValidation(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/?cursor=12", nil)
	request.Header.Set("Last-Event-ID", "12")
	if cursor, err := sessionCursor(request); err != nil || cursor != 12 {
		t.Fatalf("Session cursor = %d, %v", cursor, err)
	}
	request = httptest.NewRequest(http.MethodGet, "/?seed=11", nil)
	request.Header.Set("Last-Event-ID", "12")
	if cursor, err := sessionCursor(request); err != nil || cursor != 12 {
		t.Fatalf("Session reconnect cursor over seed = %d, %v", cursor, err)
	}
	request = httptest.NewRequest(http.MethodGet, "/?seed=11", nil)
	if cursor, err := sessionCursor(request); err != nil || cursor != 11 {
		t.Fatalf("Session initial seed = %d, %v", cursor, err)
	}

	token, err := encodeTaskCursor(map[string]uint64{"S-one": 4, "S-two": 9})
	if err != nil {
		t.Fatal(err)
	}
	request = httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("Last-Event-ID", token)
	cursor, err := taskCursor(request)
	if err != nil || cursor["S-one"] != 4 || cursor["S-two"] != 9 {
		t.Fatalf("Task cursor = %#v, %v", cursor, err)
	}

	invalidPayload := "eyJTLW9uZSI6MX17fQ"
	request = httptest.NewRequest(http.MethodGet, "/?cursor="+invalidPayload, nil)
	if _, err := taskCursor(request); err == nil {
		t.Fatal("Task cursor accepted concatenated JSON payload")
	}

	maximum := make(map[string]uint64, maxCursorSessions)
	for index := 0; index < maxCursorSessions; index++ {
		maximum[fmt.Sprintf("%x", index)] = 0
	}
	if _, err := encodeTaskCursor(maximum); err != nil {
		t.Fatalf("maximum Task cursor was not encodable: %v", err)
	}
	maximum["overflow"] = 0
	if _, err := encodeTaskCursor(maximum); err == nil {
		t.Fatal("Task cursor encoder emitted a token its decoder would reject")
	}
}

func TestServeReturnsListenerFailure(t *testing.T) {
	listener, err := ListenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	err = Serve(context.Background(), listener, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve closed-listener error = %v", err)
	}
}
