package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	mu                 sync.Mutex
	repo               *store.Store
	home               config.Home
	sockets            map[string]string
	commands           []worker.Command
	busy               bool
	starting           bool
	spawnErr           error
	catalog            acp.ProviderOptionCatalog
	permissions        []worker.PendingPermission
	permissionSequence uint64
}

func (r *apiRuntime) Spawn(ctx context.Context, sessionID string) (worker.SpawnResult, error) {
	r.mu.Lock()
	spawnErr := r.spawnErr
	r.mu.Unlock()
	if spawnErr != nil {
		return worker.SpawnResult{}, spawnErr
	}
	socket := filepath.Join(r.home.Workers, sessionID+".sock")
	r.mu.Lock()
	r.sockets[socket] = sessionID
	starting := r.starting
	r.mu.Unlock()
	if _, err := r.repo.UpdateSessionProviderRef(ctx, sessionID, "record-"+sessionID, "provider-"+sessionID); err != nil {
		return worker.SpawnResult{}, err
	}
	status := session.StatusWaiting
	log, err := sessionlog.Open(sessionlog.ChannelPath(r.home.Logs, sessionID))
	if err != nil {
		return worker.SpawnResult{}, err
	}
	if starting {
		status = session.StatusStarting
		if _, err := log.Append("", worker.FrameConnection, map[string]string{"state": "connecting"}); err != nil {
			_ = log.Close()
			return worker.SpawnResult{}, err
		}
	}
	if err := log.Close(); err != nil {
		return worker.SpawnResult{}, err
	}
	if _, err := r.repo.UpdateSessionWorker(ctx, sessionID, 8123, socket, sessionlog.ChannelPath(r.home.Logs, sessionID), status); err != nil {
		return worker.SpawnResult{}, err
	}
	return worker.SpawnResult{PID: 8123, Socket: socket, PortReady: !starting}, nil
}

func (r *apiRuntime) Send(ctx context.Context, socket string, command worker.Command) (worker.Message, error) {
	r.mu.Lock()
	sessionID, exists := r.sockets[socket]
	r.commands = append(r.commands, command)
	busy := r.busy
	starting := r.starting
	permissions := append([]worker.PendingPermission(nil), r.permissions...)
	permissionSequence := r.permissionSequence
	r.mu.Unlock()
	if !exists {
		return worker.Message{}, fmt.Errorf("test Session Port unavailable")
	}
	switch command.Type {
	case "ping":
		return worker.Message{Type: "pong", SessionID: sessionID, ProtocolVersion: worker.ProtocolVersion, Capabilities: []string{worker.CapabilitySessionChannel}}, nil
	case "provider_options":
		if starting {
			return worker.Message{}, &worker.NotReadyError{SessionID: sessionID}
		}
		catalog := r.catalog
		return worker.Message{Type: "provider_options", ProviderOptions: &catalog}, nil
	case "configure":
		catalog := r.catalog
		return worker.Message{Type: "configured", ProviderOptions: &catalog}, nil
	case "prompt":
		if busy {
			return worker.Message{}, &worker.BusyError{SessionID: sessionID, ActiveRequestID: "REQ-running"}
		}
		return worker.Message{Type: "accepted", State: "working", RequestID: command.RequestID}, nil
	case "status":
		status := session.StatusWaiting
		if starting {
			status = session.StatusStarting
		}
		return worker.Message{Type: "status", Status: string(status), Seq: permissionSequence, Permissions: permissions}, nil
	case "cancel", "permission_response":
		return worker.Message{Type: "accepted"}, nil
	case "stop":
		_, err := r.repo.ClearSessionWorker(ctx, sessionID, session.StatusStopped)
		return worker.Message{Type: "stopping"}, err
	default:
		return worker.Message{}, fmt.Errorf("unexpected command %q", command.Type)
	}
}

func TestCreateReturnsStartingSessionAndStreamsBeforeProviderIsReady(t *testing.T) {
	harness := newAPIHarness(t)
	harness.runtime.mu.Lock()
	harness.runtime.starting = true
	harness.runtime.mu.Unlock()
	payload := connectSession(t, harness, "")
	sessionID := sessionIDFromPayload(t, payload)
	encoded := string(mustJSON(t, payload["session"]))
	if !strings.Contains(encoded, `"status":"starting"`) || strings.Contains(string(mustJSON(t, payload)), "provider_options") {
		t.Fatalf("early create payload = %#v", payload)
	}
	options := requestJSON(t, harness.handler, http.MethodGet, "/api/v1/sessions/"+sessionID+"/provider-options", nil)
	if options.Code != http.StatusConflict || !strings.Contains(options.Body.String(), `"code":"session_starting"`) {
		t.Fatalf("starting options status=%d body=%s", options.Code, options.Body.String())
	}
	stream := requestJSON(t, harness.handler, http.MethodGet, "/api/v1/sessions/"+sessionID+"/stream?cursor=0", nil)
	if stream.Code != http.StatusOK || !strings.Contains(stream.Body.String(), `"type":"connection"`) || !strings.Contains(stream.Body.String(), `"state":"connecting"`) {
		t.Fatalf("early startup stream status=%d body=%s", stream.Code, stream.Body.String())
	}
}

func TestPermissionsSnapshotIncludesAtomicJournalWatermark(t *testing.T) {
	harness := newAPIHarness(t)
	payload := connectSession(t, harness, "")
	sessionID := sessionIDFromPayload(t, payload)
	harness.runtime.mu.Lock()
	harness.runtime.permissions = []worker.PendingPermission{{
		ID: "permission-1", RequestID: "REQ-1", Title: "Write file", Kind: "edit",
		Options: []worker.PermissionOption{{ID: "allow-once", Name: "Allow once", Kind: "allow_once"}},
	}}
	harness.runtime.permissionSequence = 23
	harness.runtime.mu.Unlock()

	response := requestJSON(t, harness.handler, http.MethodGet, "/api/v1/sessions/"+sessionID+"/permissions", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("permissions status=%d body=%s", response.Code, response.Body.String())
	}
	var result struct {
		Sequence       uint64          `json:"sequence"`
		DecisionsReady bool            `json:"decisions_ready"`
		Permissions    []PermissionDTO `json:"permissions"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Sequence != 23 || !result.DecisionsReady || len(result.Permissions) != 1 || result.Permissions[0].ID != "permission-1" {
		t.Fatalf("permission snapshot = %#v", result)
	}
}

func TestPermissionResponsePreservesPrintableWhitespaceTokens(t *testing.T) {
	harness := newAPIHarness(t)
	sessionID := sessionIDFromPayload(t, connectSession(t, harness, ""))
	response := requestJSON(t, harness.handler, http.MethodPost, "/api/v1/sessions/"+sessionID+"/permissions/response", map[string]any{
		"permission_id": " ", "option_id": " ", "cancelled": false,
	})
	if response.Code != http.StatusAccepted {
		t.Fatalf("whitespace permission response status=%d body=%s", response.Code, response.Body.String())
	}
	harness.runtime.mu.Lock()
	defer harness.runtime.mu.Unlock()
	var delivered *worker.Command
	for index := range harness.runtime.commands {
		if harness.runtime.commands[index].Type == "permission_response" {
			copyCommand := harness.runtime.commands[index]
			delivered = &copyCommand
		}
	}
	if delivered == nil || delivered.PermissionID != " " || delivered.OptionID != " " {
		t.Fatalf("permission tokens were normalized or dropped: %#v", delivered)
	}
}

func (r *apiRuntime) Subscribe(ctx context.Context, socket string, cursor uint64, consume func(worker.Message) error) error {
	r.mu.Lock()
	sessionID, exists := r.sockets[socket]
	r.mu.Unlock()
	if !exists {
		return fmt.Errorf("test Session Port is unavailable")
	}
	current, err := r.repo.GetSession(ctx, sessionID)
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
			LineType: line.Type, Data: append(json.RawMessage(nil), line.Data...),
		}); err != nil {
			return err
		}
	}
	return context.Canceled
}

type apiHarness struct {
	handler http.Handler
	repo    *store.Store
	runtime *apiRuntime
	task    task.Task
	path    string
}

func newAPIHarness(t *testing.T) *apiHarness {
	t.Helper()
	ctx := context.Background()
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
	repository, err := store.Open(ctx, home.Database)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repository.Close() })
	taskItem, err := repository.CreateTask(ctx, task.CreateParams{ID: "T-api", Title: "API Session Channel"})
	if err != nil {
		t.Fatal(err)
	}
	repoPath := filepath.Join(root, "repo")
	if err := os.MkdirAll(repoPath, 0o700); err != nil {
		t.Fatal(err)
	}
	command := exec.CommandContext(ctx, "git", "init", "-b", "main")
	command.Dir = repoPath
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, output)
	}
	registry, err := agents.Load(home.Agents)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &apiRuntime{
		repo: repository, home: home, sockets: make(map[string]string),
		catalog: acp.ProviderOptionCatalog{
			Model:           acp.ProviderSelectCatalog{Supported: true, CurrentValue: "provider-default", Choices: []acp.ProviderOptionChoice{{Value: "model-a", Label: "Model A"}, {Value: "model-b", Label: "Model B"}}},
			ReasoningEffort: acp.ProviderSelectCatalog{Supported: true, CurrentValue: "medium", Choices: []acp.ProviderOptionChoice{{Value: "medium", Label: "Medium"}, {Value: "high", Label: "High"}}},
		},
	}
	sessions := appservice.NewSessionService(repository, registry, home, runtime)
	return &apiHarness{handler: New(task.NewService(repository), sessions).Handler(), repo: repository, runtime: runtime, task: taskItem, path: repoPath}
}

func requestJSON(t *testing.T, handler http.Handler, method, target string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var encoded []byte
	if body != nil {
		var err error
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, target, bytes.NewReader(encoded))
	request.Host = "127.0.0.1:8080"
	if body != nil {
		request.Header.Set("Content-Type", "application/json")
	}
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	return response
}

func connectSession(t *testing.T, harness *apiHarness, parent string) map[string]any {
	t.Helper()
	target := "/api/v1/tasks/" + harness.task.ID + "/sessions"
	if parent != "" {
		target = "/api/v1/sessions/" + parent + "/children"
	}
	response := requestJSON(t, harness.handler, http.MethodPost, target, map[string]any{
		"agent": "codex", "role": "worker", "repo_path": harness.path, "allow_shared_checkout": true,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("connect status=%d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	return payload
}

func sessionIDFromPayload(t *testing.T, payload map[string]any) string {
	t.Helper()
	value, ok := payload["session"].(map[string]any)
	if !ok {
		t.Fatalf("missing Session payload: %#v", payload)
	}
	id, _ := value["id"].(string)
	if id == "" {
		t.Fatalf("missing Session ID: %#v", value)
	}
	return id
}

func appendAPIProgressFrames(t *testing.T, repository *store.Store, sessionID string, count int) {
	t.Helper()
	current, err := repository.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	log, err := sessionlog.Open(current.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= count; index++ {
		if _, err := log.AppendBuffered("", worker.FrameProviderEvent, map[string]any{"type": "progress", "index": index}); err != nil {
			_ = log.Close()
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func sseEventID(t *testing.T, body, event string) string {
	t.Helper()
	marker := "\nevent: " + event + "\n"
	position := strings.Index(body, marker)
	if position < 0 {
		t.Fatalf("SSE body has no %s event: %s", event, body)
	}
	prefix := body[:position]
	lineStart := strings.LastIndex(prefix, "\n") + 1
	line := prefix[lineStart:]
	if !strings.HasPrefix(line, "id: ") || len(line) == len("id: ") {
		t.Fatalf("SSE %s event has no ID: %s", event, body)
	}
	return strings.TrimPrefix(line, "id: ")
}

func TestConnectConfigPromptAndDynamicCatalog(t *testing.T) {
	harness := newAPIHarness(t)
	payload := connectSession(t, harness, "")
	sessionID := sessionIDFromPayload(t, payload)
	if _, exists := payload["provider_options"]; exists {
		t.Fatalf("create response fabricated synchronous provider options: %#v", payload)
	}
	createdSession := payload["session"].(map[string]any)
	if createdSession["status"] != "starting" || createdSession["resumable"] != false {
		t.Fatalf("channel admission DTO = %#v", createdSession)
	}
	optionsResponse := requestJSON(t, harness.handler, http.MethodGet, "/api/v1/sessions/"+sessionID+"/provider-options", nil)
	if optionsResponse.Code != http.StatusOK {
		t.Fatalf("provider options status=%d body=%s", optionsResponse.Code, optionsResponse.Body.String())
	}
	var optionsPayload map[string]any
	if err := json.Unmarshal(optionsResponse.Body.Bytes(), &optionsPayload); err != nil {
		t.Fatal(err)
	}
	options, ok := optionsPayload["provider_options"].(map[string]any)
	if !ok || options["model"] == nil || options["reasoning_effort"] == nil {
		t.Fatalf("dynamic provider options = %#v", payload["provider_options"])
	}
	encoded := string(mustJSON(t, payload["session"]))
	for _, forbidden := range []string{"launch_phase", "is_prepared_review", "conversation_ready", "conversation_verified"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("Session DTO retained %q: %s", forbidden, encoded)
		}
	}

	configured := requestJSON(t, harness.handler, http.MethodPost, "/api/v1/sessions/"+sessionID+"/config", map[string]any{"model": "model-b", "reasoning_effort": "high"})
	if configured.Code != http.StatusOK {
		t.Fatalf("config status=%d body=%s", configured.Code, configured.Body.String())
	}
	prompt := requestJSON(t, harness.handler, http.MethodPost, "/api/v1/sessions/"+sessionID+"/prompt", map[string]any{"message": "stream this immediately"})
	if prompt.Code != http.StatusAccepted {
		t.Fatalf("prompt status=%d body=%s", prompt.Code, prompt.Body.String())
	}
	harness.runtime.mu.Lock()
	defer harness.runtime.mu.Unlock()
	var commandTypes []string
	for _, command := range harness.runtime.commands {
		commandTypes = append(commandTypes, command.Type)
	}
	joined := strings.Join(commandTypes, ",")
	if strings.Contains(joined, "refresh") || strings.Contains(joined, "resume") || strings.Contains(joined, "launch") {
		t.Fatalf("historical activation command escaped through API: %s", joined)
	}
}

func TestIdentitylessConnectFailureDoesNotOfferResume(t *testing.T) {
	harness := newAPIHarness(t)
	harness.runtime.mu.Lock()
	harness.runtime.spawnErr = errors.New("private provider bootstrap failure")
	harness.runtime.mu.Unlock()
	response := requestJSON(t, harness.handler, http.MethodPost, "/api/v1/tasks/"+harness.task.ID+"/sessions", map[string]any{
		"agent": "codex", "role": "worker", "repo_path": harness.path, "allow_shared_checkout": true,
	})
	if response.Code != http.StatusCreated {
		t.Fatalf("identityless failure status=%d body=%s", response.Code, response.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	warning, _ := payload["warning"].(map[string]any)
	item, _ := payload["session"].(map[string]any)
	if warning["code"] != "session_identity_unavailable" || item["resumable"] != false {
		t.Fatalf("identityless failure payload = %#v", payload)
	}
	if strings.Contains(response.Body.String(), "Resume this same Session") || strings.Contains(response.Body.String(), "private provider") {
		t.Fatalf("identityless failure guidance leaked or offered impossible Resume: %s", response.Body.String())
	}
}

func TestSessionCreationRejectsPromptAndConfigurationFields(t *testing.T) {
	harness := newAPIHarness(t)
	for _, target := range []string{
		"/api/v1/tasks/" + harness.task.ID + "/sessions",
		"/api/v1/sessions/S-parent-that-must-not-be-read/children",
	} {
		for _, forbidden := range []map[string]any{
			{"prompt": "send this after connecting"},
			{"model": "model-b"},
			{"reasoning_effort": "high"},
		} {
			body := map[string]any{
				"agent": "codex", "role": "worker", "repo_path": harness.path, "allow_shared_checkout": true,
			}
			for key, value := range forbidden {
				body[key] = value
			}
			response := requestJSON(t, harness.handler, http.MethodPost, target, body)
			if response.Code != http.StatusBadRequest || !strings.Contains(response.Body.String(), "unknown field") {
				t.Fatalf("creation with %#v status=%d body=%s", forbidden, response.Code, response.Body.String())
			}
		}
	}
	items, err := harness.repo.ListSessionsByTask(context.Background(), harness.task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 0 {
		t.Fatalf("rejected connect-only configuration created Sessions: %#v", items)
	}
}

func TestBusyPromptReturnsStableConflict(t *testing.T) {
	harness := newAPIHarness(t)
	sessionID := sessionIDFromPayload(t, connectSession(t, harness, ""))
	harness.runtime.mu.Lock()
	harness.runtime.busy = true
	harness.runtime.mu.Unlock()
	response := requestJSON(t, harness.handler, http.MethodPost, "/api/v1/sessions/"+sessionID+"/prompt", map[string]any{"message": "do not queue this"})
	if response.Code != http.StatusConflict || !strings.Contains(response.Body.String(), `"code":"session_busy"`) {
		t.Fatalf("busy response status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestManagerChildRoutesUseExplicitParent(t *testing.T) {
	harness := newAPIHarness(t)
	managerID := sessionIDFromPayload(t, connectSession(t, harness, ""))
	childPayload := connectSession(t, harness, managerID)
	childID := sessionIDFromPayload(t, childPayload)
	child, err := harness.repo.GetSession(context.Background(), childID)
	if err != nil || child.ParentSessionID != managerID || child.TaskID != harness.task.ID {
		t.Fatalf("child = %#v, %v", child, err)
	}
	response := requestJSON(t, harness.handler, http.MethodGet, "/api/v1/sessions/"+managerID+"/children", nil)
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), childID) {
		t.Fatalf("children status=%d body=%s", response.Code, response.Body.String())
	}
	prompt := requestJSON(t, harness.handler, http.MethodPost, "/api/v1/sessions/"+managerID+"/children/"+childID+"/prompt", map[string]any{"message": "research this exact question"})
	if prompt.Code != http.StatusAccepted {
		t.Fatalf("child prompt status=%d body=%s", prompt.Code, prompt.Body.String())
	}
}

func TestStreamReplaysRawNormalizedFrame(t *testing.T) {
	harness := newAPIHarness(t)
	sessionID := sessionIDFromPayload(t, connectSession(t, harness, ""))
	current, err := harness.repo.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	log, err := sessionlog.Open(current.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append("REQ-one", worker.FrameProviderEvent, map[string]any{"type": "text_delta", "text": "hello", "stream": "assistant"}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	response := requestJSON(t, harness.handler, http.MethodGet, "/api/v1/sessions/"+sessionID+"/stream?cursor=0", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, wanted := range []string{"event: frame", `"seq":1`, `"type":"provider_event"`, `"data":{"stream":"assistant","text":"hello","type":"text_delta"}`, "event: ready"} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("stream missing %q: %s", wanted, body)
		}
	}
	for _, forbidden := range []string{"display", "activity", "committed", "surface"} {
		if strings.Contains(body, `"`+forbidden+`"`) {
			t.Fatalf("stream invented %q semantics: %s", forbidden, body)
		}
	}
}

func TestFreshSessionStreamDeclaresAndUsesBoundedReplayTail(t *testing.T) {
	harness := newAPIHarness(t)
	sessionID := sessionIDFromPayload(t, connectSession(t, harness, ""))
	current, err := harness.repo.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	log, err := sessionlog.Open(current.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 300; index++ {
		if _, err := log.AppendBuffered("", worker.FrameProviderEvent, map[string]any{"type": "progress", "index": index}); err != nil {
			log.Close()
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	response := requestJSON(t, harness.handler, http.MethodGet, "/api/v1/sessions/"+sessionID+"/stream?cursor=0", nil)
	body := response.Body.String()
	if response.Code != http.StatusOK || !strings.Contains(body, `"cursor":44`) || !strings.Contains(body, `"earliest_sequence":45`) || !strings.Contains(body, `"clipped":true`) {
		t.Fatalf("bounded replay ready status=%d body=%s", response.Code, body)
	}
	if strings.Contains(body, `"seq":1,`) || !strings.Contains(body, `"seq":45,`) || !strings.Contains(body, `"seq":300,`) {
		t.Fatalf("bounded replay frames were not 45..300: %s", body)
	}
}

func TestSessionReadyIDResumesExactZeroAfterDisconnectBeforeFirstFrame(t *testing.T) {
	harness := newAPIHarness(t)
	sessionID := sessionIDFromPayload(t, connectSession(t, harness, ""))

	first := requestJSON(t, harness.handler, http.MethodGet, "/api/v1/sessions/"+sessionID+"/stream", nil)
	readyID := sseEventID(t, first.Body.String(), "ready")
	if readyID != exactZeroCursor || strings.Contains(first.Body.String(), "event: frame") {
		t.Fatalf("empty Session ready boundary id=%q body=%s", readyID, first.Body.String())
	}

	// Model a browser that received ready and disconnected before any frame. A
	// later fresh tail would skip 44 records; the ready Last-Event-ID must replay
	// the exact boundary instead.
	appendAPIProgressFrames(t, harness.repo, sessionID, 300)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/"+sessionID+"/stream", nil)
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Last-Event-ID", readyID)
	response := httptest.NewRecorder()
	harness.handler.ServeHTTP(response, request)
	body := response.Body.String()
	if response.Code != http.StatusOK || strings.Count(body, "event: frame") != 300 ||
		!strings.Contains(body, `"seq":1,`) || !strings.Contains(body, `"seq":300,`) {
		t.Fatalf("exact-zero Session reconnect status=%d body=%s", response.Code, body)
	}
}

func TestFreshTaskStreamDeclaresBoundedPerSessionReplayWindows(t *testing.T) {
	harness := newAPIHarness(t)
	sessionID := sessionIDFromPayload(t, connectSession(t, harness, ""))
	current, err := harness.repo.GetSession(context.Background(), sessionID)
	if err != nil {
		t.Fatal(err)
	}
	log, err := sessionlog.Open(current.LogPath)
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index <= 300; index++ {
		if _, err := log.AppendBuffered("", worker.FrameProviderEvent, map[string]any{"type": "progress", "index": index}); err != nil {
			log.Close()
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1/tasks/"+harness.task.ID+"/stream", nil)
	request.Host = "127.0.0.1:8080"
	response := httptest.NewRecorder()
	harness.handler.ServeHTTP(response, request)
	body := response.Body.String()
	for _, wanted := range []string{
		"event: ready", `"sessions":{"` + sessionID + `":{"cursor":44,"earliest_sequence":45,"clipped":true}}`,
		`"seq":45,`, `"seq":300,`,
	} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("Task stream missing %q: %s", wanted, body)
		}
	}
	if strings.Contains(body, `"seq":1,`) {
		t.Fatalf("Task stream replayed its clipped prefix: %s", body)
	}
}

func TestTaskReadyIDKeepsZeroChildCursorExactAcrossReconnect(t *testing.T) {
	harness := newAPIHarness(t)
	sessionID := sessionIDFromPayload(t, connectSession(t, harness, ""))

	firstContext, firstCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	firstRequest := httptest.NewRequestWithContext(firstContext, http.MethodGet, "/api/v1/tasks/"+harness.task.ID+"/stream", nil)
	firstRequest.Host = "127.0.0.1:8080"
	firstResponse := httptest.NewRecorder()
	harness.handler.ServeHTTP(firstResponse, firstRequest)
	firstCancel()
	readyID := sseEventID(t, firstResponse.Body.String(), "ready")
	decoded, err := taskCursor(httptest.NewRequest(http.MethodGet, "/?cursor="+readyID, nil))
	if err != nil || decoded[sessionID] != 0 || strings.Contains(firstResponse.Body.String(), "event: frame") {
		t.Fatalf("empty Task ready cursor=%#v err=%v body=%s", decoded, err, firstResponse.Body.String())
	}

	appendAPIProgressFrames(t, harness.repo, sessionID, 300)
	reconnectContext, reconnectCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer reconnectCancel()
	reconnect := httptest.NewRequestWithContext(reconnectContext, http.MethodGet, "/api/v1/tasks/"+harness.task.ID+"/stream", nil)
	reconnect.Host = "127.0.0.1:8080"
	reconnect.Header.Set("Last-Event-ID", readyID)
	response := httptest.NewRecorder()
	harness.handler.ServeHTTP(response, reconnect)
	body := response.Body.String()
	if response.Code != http.StatusOK || strings.Count(body, "event: frame") != 300 ||
		!strings.Contains(body, `"seq":1,`) || !strings.Contains(body, `"seq":300,`) {
		t.Fatalf("exact-zero Task reconnect status=%d body=%s", response.Code, body)
	}
}

func TestEmptyTaskReadyIDResumesNewChildFromExactZero(t *testing.T) {
	harness := newAPIHarness(t)
	firstContext, firstCancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	firstRequest := httptest.NewRequestWithContext(firstContext, http.MethodGet, "/api/v1/tasks/"+harness.task.ID+"/stream", nil)
	firstRequest.Host = "127.0.0.1:8080"
	firstResponse := httptest.NewRecorder()
	harness.handler.ServeHTTP(firstResponse, firstRequest)
	firstCancel()
	readyID := sseEventID(t, firstResponse.Body.String(), "ready")
	positions, membershipBoundary, err := taskCursorWithMembership(httptest.NewRequest(http.MethodGet, "/?cursor="+readyID, nil))
	if err != nil || !membershipBoundary || len(positions) != 0 || strings.Contains(firstResponse.Body.String(), "event: frame") {
		t.Fatalf("empty Task membership boundary positions=%#v marked=%v err=%v body=%s", positions, membershipBoundary, err, firstResponse.Body.String())
	}

	// The child did not exist at the saved ready boundary. Reconnecting that
	// exact aggregate cursor must begin the new child at zero, even when a fresh
	// tail would otherwise skip its first 44 frames.
	sessionID := sessionIDFromPayload(t, connectSession(t, harness, ""))
	appendAPIProgressFrames(t, harness.repo, sessionID, 300)
	reconnectContext, reconnectCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer reconnectCancel()
	reconnect := httptest.NewRequestWithContext(reconnectContext, http.MethodGet, "/api/v1/tasks/"+harness.task.ID+"/stream", nil)
	reconnect.Host = "127.0.0.1:8080"
	reconnect.Header.Set("Last-Event-ID", readyID)
	response := httptest.NewRecorder()
	harness.handler.ServeHTTP(response, reconnect)
	body := response.Body.String()
	if response.Code != http.StatusOK || strings.Count(body, "event: frame") != 300 ||
		!strings.Contains(body, `"seq":1,`) || !strings.Contains(body, `"seq":300,`) {
		t.Fatalf("new child exact-zero reconnect status=%d body=%s", response.Code, body)
	}
}

type deadlineResponseWriter struct {
	header    http.Header
	deadline  time.Time
	deadlines []time.Time
	writes    int
	flushes   int
	writeErr  error
}

func (w *deadlineResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *deadlineResponseWriter) WriteHeader(int) {}

func (w *deadlineResponseWriter) SetWriteDeadline(deadline time.Time) error {
	w.deadline = deadline
	w.deadlines = append(w.deadlines, deadline)
	return nil
}

func (w *deadlineResponseWriter) Write(value []byte) (int, error) {
	w.writes++
	if w.deadline.IsZero() {
		return 0, fmt.Errorf("slow client write was attempted without a deadline")
	}
	if w.writeErr != nil {
		return 0, w.writeErr
	}
	return len(value), nil
}

func (w *deadlineResponseWriter) Flush() { w.flushes++ }

func TestSSESlowClientWriteIsBoundedByPerWriteDeadline(t *testing.T) {
	writer := &deadlineResponseWriter{writeErr: os.ErrDeadlineExceeded}
	err := writeSSE(writer, writer, "1", map[string]string{"state": "slow"})
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("slow SSE write error = %v, want deadline", err)
	}
	if writer.writes != 1 || len(writer.deadlines) != 2 || writer.deadlines[0].IsZero() || !writer.deadlines[1].IsZero() {
		t.Fatalf("per-write deadline lifecycle = writes %d deadlines %#v", writer.writes, writer.deadlines)
	}
}

func TestSSERefreshesAndClearsDeadlineForEveryChunk(t *testing.T) {
	writer := &deadlineResponseWriter{}
	if err := startSSE(writer, writer); err != nil {
		t.Fatal(err)
	}
	if err := writeSSEReady(writer, writer, 0, false); err != nil {
		t.Fatal(err)
	}
	if err := writeSSE(writer, writer, "1", map[string]string{"state": "connected"}); err != nil {
		t.Fatal(err)
	}
	if writer.writes != 3 || writer.flushes != 3 || len(writer.deadlines) != 6 {
		t.Fatalf("SSE write boundaries = writes %d flushes %d deadlines %#v", writer.writes, writer.flushes, writer.deadlines)
	}
	for index := 0; index < len(writer.deadlines); index += 2 {
		if writer.deadlines[index].IsZero() || !writer.deadlines[index+1].IsZero() {
			t.Fatalf("SSE deadline pair %d = %#v, want set then clear", index/2, writer.deadlines[index:index+2])
		}
	}
}

func TestTaskStreamIdentifiesAndResetsAnInvalidChildCursor(t *testing.T) {
	harness := newAPIHarness(t)
	sessionID := sessionIDFromPayload(t, connectSession(t, harness, ""))
	token, err := encodeTaskCursor(map[string]uint64{sessionID: 999})
	if err != nil {
		t.Fatal(err)
	}
	response := requestJSON(t, harness.handler, http.MethodGet, "/api/v1/tasks/"+harness.task.ID+"/stream?cursor="+token, nil)
	body := response.Body.String()
	for _, wanted := range []string{
		"event: ready", "event: channel_error", `"code":"replay_cursor_reset"`, `"reset_cursor":true`, `"session_id":"` + sessionID + `"`,
	} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("Task cursor reset missing %q: %s", wanted, body)
		}
	}
}

func TestSessionStreamSurfacesReplayCursorGapWithoutPrivateDetail(t *testing.T) {
	harness := newAPIHarness(t)
	sessionID := sessionIDFromPayload(t, connectSession(t, harness, ""))
	response := requestJSON(t, harness.handler, http.MethodGet, "/api/v1/sessions/"+sessionID+"/stream?cursor=999", nil)
	if response.Code != http.StatusOK {
		t.Fatalf("stream status=%d body=%s", response.Code, response.Body.String())
	}
	body := response.Body.String()
	for _, wanted := range []string{"event: channel_error", `"code":"replay_cursor_reset"`, `"reset_cursor":true`} {
		if !strings.Contains(body, wanted) {
			t.Fatalf("cursor gap stream missing %q: %s", wanted, body)
		}
	}
	for _, forbidden := range []string{"worker_pid", "worker_socket", "provider-", "runtime-"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("cursor gap stream leaked %q: %s", forbidden, body)
		}
	}
}

func TestCorruptReplayInitializationIsABoundedSSEChannelError(t *testing.T) {
	harness := newAPIHarness(t)
	created, err := harness.repo.CreateSession(context.Background(), session.CreateParams{
		ID: "S-corrupt-replay", TaskID: harness.task.ID, Agent: "codex", Status: session.StatusStopped,
		LogPath: sessionlog.ChannelPath(harness.runtime.home.Logs, "S-corrupt-replay"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(created.LogPath, []byte("{complete-but-corrupt}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for name, target := range map[string]string{
		"Session": "/api/v1/sessions/" + created.ID + "/stream",
		"Task":    "/api/v1/tasks/" + harness.task.ID + "/stream",
	} {
		t.Run(name, func(t *testing.T) {
			response := requestJSON(t, harness.handler, http.MethodGet, target, nil)
			body := response.Body.String()
			for _, wanted := range []string{": connected", "event: channel_error", `"code":"stream_unavailable"`} {
				if response.Code != http.StatusOK || !strings.Contains(body, wanted) {
					t.Fatalf("corrupt replay initialization status=%d missing %q: %s", response.Code, wanted, body)
				}
			}
			if contentType := response.Header().Get("Content-Type"); !strings.HasPrefix(contentType, "text/event-stream") {
				t.Fatalf("corrupt replay initialization content type = %q", contentType)
			}
			if strings.Contains(body, "event: ready") || strings.Contains(body, created.LogPath) ||
				strings.Contains(body, "decode Session log") || strings.Contains(body, "complete-but-corrupt") {
				t.Fatalf("corrupt replay initialization leaked detail or declared ready: %s", body)
			}
			if name == "Task" && !strings.Contains(body, `"session_id":"`+created.ID+`"`) {
				t.Fatalf("corrupt Task replay did not identify its child: %s", body)
			}
		})
	}
}

func TestMissingSessionReplayIsAVisibleTerminalStreamError(t *testing.T) {
	harness := newAPIHarness(t)
	missingPath := filepath.Join(harness.runtime.home.Logs, "missing-private-channel.jsonl")
	created, err := harness.repo.CreateSession(context.Background(), session.CreateParams{
		ID: "S-replay-missing", TaskID: harness.task.ID, Agent: "codex", Status: session.StatusStopped,
		LogPath: missingPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := requestJSON(t, harness.handler, http.MethodGet, "/api/v1/sessions/"+created.ID+"/stream", nil)
	body := response.Body.String()
	for _, wanted := range []string{
		"event: channel_error", `"code":"replay_unavailable"`, `"reset_cursor":false`, "Resume the Session",
	} {
		if response.Code != http.StatusOK || !strings.Contains(body, wanted) {
			t.Fatalf("missing replay stream status=%d missing %q: %s", response.Code, wanted, body)
		}
	}
	if strings.Contains(body, missingPath) || strings.Contains(body, "missing-private-channel") {
		t.Fatalf("missing replay stream leaked private journal path: %s", body)
	}
}

func TestTaskMissingReplayNamesChildInVisibleTerminalStreamError(t *testing.T) {
	harness := newAPIHarness(t)
	missingPath := filepath.Join(harness.runtime.home.Logs, "missing-task-private.jsonl")
	created, err := harness.repo.CreateSession(context.Background(), session.CreateParams{
		ID: "S-task-replay-missing", TaskID: harness.task.ID, Agent: "codex", Status: session.StatusFailed,
		LogPath: missingPath,
	})
	if err != nil {
		t.Fatal(err)
	}
	response := requestJSON(t, harness.handler, http.MethodGet, "/api/v1/tasks/"+harness.task.ID+"/stream", nil)
	body := response.Body.String()
	for _, wanted := range []string{
		"event: channel_error", `"code":"replay_unavailable"`, `"reset_cursor":false`, `"session_id":"` + created.ID + `"`,
	} {
		if response.Code != http.StatusOK || !strings.Contains(body, wanted) {
			t.Fatalf("missing Task child replay status=%d missing %q: %s", response.Code, wanted, body)
		}
	}
	if strings.Contains(body, missingPath) || strings.Contains(body, "missing-task-private") {
		t.Fatalf("missing Task replay leaked private journal path: %s", body)
	}
}

func TestReplayUnavailableActionErrorIsBoundedAndNamesTaskChild(t *testing.T) {
	response := httptest.NewRecorder()
	writeActionError(response, &appservice.TaskSessionReplayError{
		SessionID: "S-child", Err: &appservice.ReplayUnavailableError{},
	})
	body := response.Body.String()
	if response.Code != http.StatusGone || !strings.Contains(body, `"code":"replay_unavailable"`) ||
		!strings.Contains(body, `"reset_cursor":false`) || !strings.Contains(body, `"session_id":"S-child"`) {
		t.Fatalf("replay-unavailable action error status=%d body=%s", response.Code, body)
	}
}

func TestRemovedHistoricalRoutesAreNotFound(t *testing.T) {
	harness := newAPIHarness(t)
	for _, target := range []string{
		"/api/v1/tasks/" + harness.task.ID + "/sessions/prepare",
		"/api/v1/sessions/S-missing/launch",
		"/api/v1/sessions/S-missing/review",
		"/api/v1/sessions/S-missing/conversation",
	} {
		response := requestJSON(t, harness.handler, http.MethodPost, target, map[string]any{})
		if response.Code != http.StatusNotFound && response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("removed route %s status=%d body=%s", target, response.Code, response.Body.String())
		}
	}
}

func TestSecurityHeadersAndCrossOriginMutationGuard(t *testing.T) {
	harness := newAPIHarness(t)
	health := requestJSON(t, harness.handler, http.MethodGet, "/healthz", nil)
	if health.Header().Get("X-Content-Type-Options") != "nosniff" || health.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("security headers = %#v", health.Header())
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(`{"title":"cross site"}`))
	request.Host = "127.0.0.1:8080"
	request.Header.Set("Origin", "https://attacker.example")
	response := httptest.NewRecorder()
	harness.handler.ServeHTTP(response, request)
	if response.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestLoopbackHostGuardAcceptsLiteralLoopbackAuthorities(t *testing.T) {
	harness := newAPIHarness(t)
	for _, host := range []string{"127.0.0.1", "127.0.0.1:8080", "[::1]", "[::1]:8080"} {
		t.Run(host, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			request.Host = host
			response := httptest.NewRecorder()
			harness.handler.ServeHTTP(response, request)
			if response.Code != http.StatusOK {
				t.Fatalf("host %q status=%d body=%s", host, response.Code, response.Body.String())
			}
		})
	}
}

func TestLoopbackHostGuardRejectsUntrustedOrMalformedAuthorities(t *testing.T) {
	harness := newAPIHarness(t)
	for _, host := range []string{
		"attacker.example",
		"localhost",
		"0.0.0.0:8080",
		"192.0.2.1:8080",
		"127.0.0.1:service",
		"[::1]:65536",
		" 127.0.0.1:8080",
	} {
		t.Run(host, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodPost, "/api/v1/tasks", strings.NewReader(`{"title":"must not be created"}`))
			request.Host = host
			request.Header.Set("Content-Type", "application/json")
			response := httptest.NewRecorder()
			harness.handler.ServeHTTP(response, request)
			if response.Code != http.StatusForbidden {
				t.Fatalf("host %q status=%d body=%s", host, response.Code, response.Body.String())
			}
			var envelope struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(response.Body.Bytes(), &envelope); err != nil {
				t.Fatalf("decode host %q response: %v", host, err)
			}
			if envelope.Error.Code != "host_forbidden" {
				t.Fatalf("host %q error code=%q body=%s", host, envelope.Error.Code, response.Body.String())
			}
		})
	}

	items, err := harness.repo.ListTasks(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("untrusted Host mutations created tasks: %#v", items)
	}
}

func TestFrameDTOCopiesPayload(t *testing.T) {
	line := sessionlog.Line{Seq: 9, Timestamp: time.Unix(10, 0).UTC(), RequestID: "REQ-x", Type: worker.FrameProviderEvent, Data: json.RawMessage(`{"type":"status","state":"working"}`)}
	frame := makeFrameDTO("S-x", line, "cursor")
	line.Data[2] = 'X'
	if frame.Seq != 9 || frame.Type != worker.FrameProviderEvent || !strings.Contains(string(frame.Data), `"status"`) {
		t.Fatalf("FrameDTO = %#v", frame)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}
