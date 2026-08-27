// Package api is the versioned local HTTP transport over shared application
// services. It deliberately exposes DTOs rather than persistence structures.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
	gitrepo "github.com/yty-build/acp-gateway/internal/git"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
	"github.com/yty-build/acp-gateway/internal/worker"
)

const (
	maxRequestBody  = 1 << 20
	sseWriteTimeout = 5 * time.Second
)

type Server struct {
	tasks    *task.Service
	sessions *appservice.SessionService
	handler  http.Handler
}

func New(tasks *task.Service, sessions *appservice.SessionService) *Server {
	server := &Server{tasks: tasks, sessions: sessions}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", server.health)
	mux.HandleFunc("GET /api/v1/tasks", server.listTasks)
	mux.HandleFunc("POST /api/v1/tasks", server.createTask)
	mux.HandleFunc("GET /api/v1/tasks/{taskID}", server.getTask)
	mux.HandleFunc("GET /api/v1/tasks/{taskID}/sessions", server.listTaskSessions)
	mux.HandleFunc("POST /api/v1/tasks/{taskID}/sessions", server.startSession)
	mux.HandleFunc("GET /api/v1/sessions/{sessionID}/children", server.listChildren)
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/children", server.createChild)
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/children/{childID}/prompt", server.promptChild)
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/children/{childID}/cancel", server.cancelChild)
	mux.HandleFunc("GET /api/v1/tasks/{taskID}/stream", server.streamTask)
	mux.HandleFunc("GET /api/v1/sessions/{sessionID}", server.getSession)
	mux.HandleFunc("GET /api/v1/sessions/{sessionID}/provider-options", server.providerOptions)
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/config", server.configureSession)
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/prompt", server.prompt)
	mux.HandleFunc("GET /api/v1/sessions/{sessionID}/permissions", server.permissions)
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/permissions/response", server.decidePermission)
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/cancel", server.cancel)
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/resume", server.resume)
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/stop", server.stop)
	mux.HandleFunc("GET /api/v1/sessions/{sessionID}/stream", server.streamSession)
	server.handler = securityHeaders(mux)
	return server
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) health(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]string{"status": "ok", "api_version": "v1"})
}

func (s *Server) listTasks(writer http.ResponseWriter, request *http.Request) {
	items, err := s.tasks.List(request.Context())
	if err != nil {
		writeInternalError(writer)
		return
	}
	result := make([]TaskDTO, 0, len(items))
	for _, item := range items {
		detail, err := s.tasks.Get(request.Context(), item.ID)
		if err != nil {
			writeInternalError(writer)
			return
		}
		result = append(result, makeTaskDTO(item, len(detail.Sessions)))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"tasks": result})
}

type createTaskRequest struct {
	Title       string         `json:"title"`
	Description string         `json:"description"`
	ExternalRef string         `json:"external_ref"`
	Metadata    map[string]any `json:"metadata"`
}

func (s *Server) createTask(writer http.ResponseWriter, request *http.Request) {
	var body createTaskRequest
	if err := decodeJSON(writer, request, &body, false); err != nil {
		writeBadRequest(writer, err)
		return
	}
	metadata, err := json.Marshal(body.Metadata)
	if err != nil {
		writeBadRequest(writer, fmt.Errorf("invalid metadata"))
		return
	}
	if body.Metadata == nil {
		metadata = nil
	}
	created, err := s.tasks.Create(request.Context(), task.CreateParams{
		Title: body.Title, Description: body.Description, ExternalRef: body.ExternalRef, MetadataJSON: metadata,
	})
	if err != nil {
		writeValidationError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{"task": makeTaskDTO(created, 0)})
}

func (s *Server) getTask(writer http.ResponseWriter, request *http.Request) {
	detail, err := s.tasks.Get(request.Context(), request.PathValue("taskID"))
	if err != nil {
		writeLookupError(writer, "Task", err)
		return
	}
	sessions := make([]SessionDTO, 0, len(detail.Sessions))
	for _, current := range detail.Sessions {
		sessions = append(sessions, makeSessionDTO(current))
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"task": makeTaskDTO(detail.Task, len(detail.Sessions)), "sessions": sessions,
	})
}

func (s *Server) listTaskSessions(writer http.ResponseWriter, request *http.Request) {
	sessions, err := s.sessions.ListByTask(request.Context(), request.PathValue("taskID"))
	if err != nil {
		writeLookupError(writer, "Task", err)
		return
	}
	result := make([]SessionDTO, 0, len(sessions))
	for _, current := range sessions {
		result = append(result, makeSessionDTO(current))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"sessions": result})
}

func (s *Server) listChildren(writer http.ResponseWriter, request *http.Request) {
	items, err := s.sessions.Children(request.Context(), request.PathValue("sessionID"))
	if err != nil {
		writeLookupError(writer, "Session", err)
		return
	}
	result := make([]SessionDTO, 0, len(items))
	for _, current := range items {
		result = append(result, makeSessionDTO(current))
	}
	writeJSON(writer, http.StatusOK, map[string]any{"sessions": result})
}

func (s *Server) createChild(writer http.ResponseWriter, request *http.Request) {
	var body startSessionRequest
	if err := decodeJSON(writer, request, &body, false); err != nil {
		writeBadRequest(writer, err)
		return
	}
	params := appservice.StartSessionParams{
		Agent: body.Agent, Role: body.Role, RepoPath: body.RepoPath,
		Worktree: body.Worktree, ApproveAll: body.ApproveAll, AllowSharedCheckout: body.AllowSharedCheckout,
	}
	connected, err := s.sessions.ConnectChild(request.Context(), request.PathValue("sessionID"), params)
	if err != nil {
		if connected.Session.ID != "" {
			writeSessionConnected(writer, http.StatusCreated, connected, "session_connect_failed", "The child Session was saved, but its Session Channel is unavailable. Resume this same Session to retry.")
			return
		}
		writeValidationError(writer, err)
		return
	}
	writeSessionConnected(writer, http.StatusCreated, connected, "", "")
}

func (s *Server) promptChild(writer http.ResponseWriter, request *http.Request) {
	var body promptRequest
	if err := decodeJSON(writer, request, &body, false); err != nil {
		writeBadRequest(writer, err)
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		writeBadRequest(writer, fmt.Errorf("message is required"))
		return
	}
	accepted, err := s.sessions.PromptChild(request.Context(), request.PathValue("sessionID"), request.PathValue("childID"), body.Message)
	if err != nil {
		writeActionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{"request_id": accepted.RequestID, "state": accepted.State})
}

func (s *Server) cancelChild(writer http.ResponseWriter, request *http.Request) {
	if err := requireEmptyBody(request); err != nil {
		writeBadRequest(writer, err)
		return
	}
	if err := s.sessions.CancelChild(request.Context(), request.PathValue("sessionID"), request.PathValue("childID")); err != nil {
		writeActionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]string{"status": "cancel_requested"})
}

type startSessionRequest struct {
	Agent               string `json:"agent"`
	Role                string `json:"role"`
	RepoPath            string `json:"repo_path"`
	Worktree            bool   `json:"worktree"`
	ApproveAll          bool   `json:"approve_all"`
	AllowSharedCheckout bool   `json:"allow_shared_checkout"`
	ParentSessionID     string `json:"parent_session_id"`
}

func (s *Server) startSession(writer http.ResponseWriter, request *http.Request) {
	var body startSessionRequest
	if err := decodeJSON(writer, request, &body, false); err != nil {
		writeBadRequest(writer, err)
		return
	}
	params := appservice.StartSessionParams{
		TaskID: request.PathValue("taskID"), Agent: body.Agent, Role: body.Role, RepoPath: body.RepoPath,
		ParentSessionID: body.ParentSessionID,
		Worktree:        body.Worktree, ApproveAll: body.ApproveAll, AllowSharedCheckout: body.AllowSharedCheckout,
	}
	connected, err := s.sessions.ConnectSession(request.Context(), params)
	if err != nil {
		if connected.Session.ID != "" {
			writeSessionConnected(writer, http.StatusCreated, connected, "session_connect_failed", "The Session was saved, but its Session Channel is unavailable. Resume this same Session to retry.")
			return
		}
		writeValidationError(writer, err)
		return
	}
	writeSessionConnected(writer, http.StatusCreated, connected, "", "")
}

func (s *Server) providerOptions(writer http.ResponseWriter, request *http.Request) {
	connected, err := s.sessions.ProviderOptions(request.Context(), request.PathValue("sessionID"))
	if err != nil {
		writeActionError(writer, err)
		return
	}
	writeConnectedSession(writer, http.StatusOK, connected, "", "")
}

type configureSessionRequest struct {
	Model           string `json:"model"`
	ReasoningEffort string `json:"reasoning_effort"`
}

func (s *Server) configureSession(writer http.ResponseWriter, request *http.Request) {
	var body configureSessionRequest
	if err := decodeJSON(writer, request, &body, false); err != nil {
		writeBadRequest(writer, err)
		return
	}
	configured, err := s.sessions.Configure(request.Context(), request.PathValue("sessionID"), session.ProviderOptions{
		Model: body.Model, ReasoningEffort: body.ReasoningEffort,
	})
	if err != nil {
		writeActionError(writer, err)
		return
	}
	writeConnectedSession(writer, http.StatusOK, configured, "", "")
}

func writeSessionConnected(writer http.ResponseWriter, status int, connected appservice.ConnectedSession, warningCode, warningMessage string) {
	payload := map[string]any{"session": makeSessionDTO(connected.Session)}
	if warningCode != "" {
		if connected.Session.ProviderRef == "" {
			warningCode = "session_identity_unavailable"
			warningMessage = "The Session was saved, but no provider identity was established. Create a new Session instead of retrying this one."
		}
		payload["warning"] = map[string]string{"code": warningCode, "message": warningMessage}
	}
	writeJSON(writer, status, payload)
}

func writeConnectedSession(writer http.ResponseWriter, status int, connected appservice.ConnectedSession, warningCode, warningMessage string) {
	payload := map[string]any{
		"session": makeSessionDTO(connected.Session), "provider_options": makeProviderOptionCatalogDTO(connected.ProviderOptions),
	}
	if warningCode != "" {
		payload["warning"] = map[string]string{"code": warningCode, "message": warningMessage}
	}
	writeJSON(writer, status, payload)
}

func (s *Server) getSession(writer http.ResponseWriter, request *http.Request) {
	current, err := s.sessions.Get(request.Context(), request.PathValue("sessionID"))
	if err != nil {
		writeLookupError(writer, "Session", err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"session": makeSessionDTO(current)})
}

type promptRequest struct {
	Message string `json:"message"`
}

func (s *Server) prompt(writer http.ResponseWriter, request *http.Request) {
	var body promptRequest
	if err := decodeJSON(writer, request, &body, false); err != nil {
		writeBadRequest(writer, err)
		return
	}
	if strings.TrimSpace(body.Message) == "" {
		writeBadRequest(writer, fmt.Errorf("message is required"))
		return
	}
	accepted, err := s.sessions.Prompt(request.Context(), request.PathValue("sessionID"), body.Message)
	if err != nil {
		writeActionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]string{"request_id": accepted.RequestID, "state": accepted.State})
}

func (s *Server) permissions(writer http.ResponseWriter, request *http.Request) {
	snapshot, err := s.sessions.PendingPermissionSnapshot(request.Context(), request.PathValue("sessionID"))
	if err != nil {
		writeActionError(writer, err)
		return
	}
	result := make([]PermissionDTO, 0, len(snapshot.Permissions))
	for _, permission := range snapshot.Permissions {
		result = append(result, makePendingPermissionDTO(permission))
	}
	writeJSON(writer, http.StatusOK, map[string]any{
		"permissions": result, "sequence": snapshot.Sequence, "decisions_ready": snapshot.DecisionsReady,
		"blocked_code": snapshot.BlockedCode, "blocked_message": snapshot.BlockedMessage,
	})
}

type permissionDecisionRequest struct {
	PermissionID string `json:"permission_id"`
	OptionID     string `json:"option_id"`
	Cancelled    bool   `json:"cancelled"`
}

func (s *Server) decidePermission(writer http.ResponseWriter, request *http.Request) {
	var body permissionDecisionRequest
	if err := decodeJSON(writer, request, &body, false); err != nil {
		writeBadRequest(writer, err)
		return
	}
	if body.PermissionID == "" || (!body.Cancelled && body.OptionID == "") {
		writeBadRequest(writer, fmt.Errorf("permission_id and an option_id or cancelled=true are required"))
		return
	}
	id := request.PathValue("sessionID")
	if err := s.sessions.DecidePermission(request.Context(), id, body.PermissionID, body.OptionID, body.Cancelled); err != nil {
		writeActionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]any{
		"session_id": id, "permission_id": body.PermissionID, "option_id": body.OptionID, "status": "decision_accepted",
	})
}

func (s *Server) cancel(writer http.ResponseWriter, request *http.Request) {
	if err := requireEmptyBody(request); err != nil {
		writeBadRequest(writer, err)
		return
	}
	id := request.PathValue("sessionID")
	if err := s.sessions.Cancel(request.Context(), id); err != nil {
		writeActionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusAccepted, map[string]string{"session_id": id, "status": "cancellation_requested"})
}

type resumeRequest struct {
	AllowSharedCheckout bool `json:"allow_shared_checkout"`
}

func (s *Server) resume(writer http.ResponseWriter, request *http.Request) {
	var body resumeRequest
	if err := decodeJSON(writer, request, &body, true); err != nil {
		writeBadRequest(writer, err)
		return
	}
	current, err := s.sessions.ResumeWithOptions(request.Context(), request.PathValue("sessionID"), body.AllowSharedCheckout)
	if err != nil {
		writeActionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]any{"session": makeSessionDTO(current)})
}

func (s *Server) stop(writer http.ResponseWriter, request *http.Request) {
	if err := requireEmptyBody(request); err != nil {
		writeBadRequest(writer, err)
		return
	}
	id := request.PathValue("sessionID")
	if err := s.sessions.Stop(request.Context(), id); err != nil {
		writeActionError(writer, err)
		return
	}
	writeJSON(writer, http.StatusOK, map[string]string{"session_id": id, "status": "stopped"})
}

func (s *Server) streamSession(writer http.ResponseWriter, request *http.Request) {
	requested, err := sessionCursor(request)
	if err != nil {
		writeBadRequest(writer, err)
		return
	}
	id := request.PathValue("sessionID")
	if _, err := s.sessions.Get(request.Context(), id); err != nil {
		writeLookupError(writer, "Session", err)
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeJSONError(writer, http.StatusInternalServerError, "stream_unsupported", "Streaming is unavailable")
		return
	}
	effectiveCursor := requested.Position
	clipped := false
	if requested.Fresh {
		effectiveCursor, clipped, err = s.sessions.FreshReplayCursor(request.Context(), id, 256)
		if err != nil {
			writeSSEInitializationError(writer, flusher, err)
			return
		}
	}
	if err := startSSE(writer, flusher); err != nil {
		return
	}
	if err := writeSSEReady(writer, flusher, effectiveCursor, clipped); err != nil {
		return
	}
	err = s.sessions.Follow(request.Context(), id, effectiveCursor, func(line sessionlog.Line) error {
		event := makeFrameDTO(id, line, "")
		return writeSSE(writer, flusher, strconvUint(line.Seq), event)
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		_ = writeSSEChannelError(writer, flusher, err, true)
		return
	}
}

func (s *Server) streamTask(writer http.ResponseWriter, request *http.Request) {
	cursor, membershipBoundary, err := taskCursorWithMembership(request)
	if err != nil {
		writeBadRequest(writer, err)
		return
	}
	id := request.PathValue("taskID")
	detail, err := s.tasks.Get(request.Context(), id)
	if err != nil {
		writeLookupError(writer, "Task", err)
		return
	}
	filtered := make(map[string]uint64, len(detail.Sessions))
	for _, current := range detail.Sessions {
		if position, exists := cursor[current.ID]; exists {
			filtered[current.ID] = position
		}
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeJSONError(writer, http.StatusInternalServerError, "stream_unsupported", "Streaming is unavailable")
		return
	}
	var effective map[string]uint64
	var windows map[string]appservice.ReplayWindow
	if membershipBoundary {
		effective, windows, err = s.sessions.TaskReplayWindowsAtMembershipBoundary(request.Context(), id, filtered, 256)
	} else {
		effective, windows, err = s.sessions.TaskReplayWindows(request.Context(), id, filtered, 256)
	}
	if err != nil {
		writeSSEInitializationError(writer, flusher, err)
		return
	}
	readyCursor, err := encodeTaskCursor(effective)
	if err != nil {
		writeSSEInitializationError(writer, flusher, err)
		return
	}
	cursor = effective
	if err := startSSE(writer, flusher); err != nil {
		return
	}
	if err := writeSSETaskReady(writer, flusher, readyCursor, windows); err != nil {
		return
	}
	err = s.sessions.FollowTask(request.Context(), id, cursor, func(activity appservice.TaskActivity) error {
		cursor[activity.SessionID] = activity.Line.Seq
		token, err := encodeTaskCursor(cursor)
		if err != nil {
			return err
		}
		event := makeFrameDTO(activity.SessionID, activity.Line, token)
		return writeSSE(writer, flusher, token, event)
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		_ = writeSSEChannelError(writer, flusher, err, true)
		return
	}
}

func writeSSEInitializationError(writer http.ResponseWriter, flusher http.Flusher, streamErr error) {
	if err := startSSE(writer, flusher); err != nil {
		return
	}
	_ = writeSSEChannelError(writer, flusher, streamErr, true)
}

func writeSSEChannelError(writer http.ResponseWriter, flusher http.Flusher, streamErr error, allowCursorReset bool) error {
	payload := map[string]any{
		"code":    "stream_unavailable",
		"message": "The Session Channel stream ended before replay completed.",
	}
	var gap *sessionlog.CursorGapError
	if errors.As(streamErr, &gap) {
		payload["code"] = "replay_cursor_reset"
		payload["message"] = "The saved replay cursor is outside retained Session Channel history."
		payload["reset_cursor"] = allowCursorReset
	}
	var unavailable *appservice.ReplayUnavailableError
	if errors.As(streamErr, &unavailable) {
		payload["code"] = "replay_unavailable"
		payload["message"] = "Saved Session Channel replay is unavailable. Resume the Session to establish a new channel; earlier gateway audit cannot be recovered."
		payload["reset_cursor"] = false
	}
	var taskReplay *appservice.TaskSessionReplayError
	if errors.As(streamErr, &taskReplay) {
		payload["session_id"] = taskReplay.SessionID
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return writeSSEChunk(writer, flusher, []byte(fmt.Sprintf("event: channel_error\ndata: %s\n\n", encoded)))
}

func makeFrameDTO(sessionID string, line sessionlog.Line, cursor string) FrameDTO {
	data := append(json.RawMessage(nil), line.Data...)
	if len(data) == 0 {
		data = json.RawMessage(`{}`)
	}
	return FrameDTO{
		SessionID: sessionID, Seq: line.Seq, RequestID: line.RequestID, Type: line.Type,
		Timestamp: formatTime(line.Timestamp), Cursor: cursor, Data: data,
	}
}

func startSSE(writer http.ResponseWriter, flusher http.Flusher) error {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache, no-store")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)
	return writeSSEChunk(writer, flusher, []byte(": connected\n\n"))
}

func writeSSE(writer http.ResponseWriter, flusher http.Flusher, id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeSSEChunk(writer, flusher, []byte(fmt.Sprintf("id: %s\nevent: frame\ndata: %s\n\n", id, data)))
}

func writeSSEReady(writer http.ResponseWriter, flusher http.Flusher, cursor uint64, clipped bool) error {
	earliest := cursor
	if cursor != ^uint64(0) {
		earliest++
	}
	payload, err := json.Marshal(map[string]any{
		"cursor": cursor, "earliest_sequence": earliest, "clipped": clipped,
	})
	if err != nil {
		return err
	}
	return writeSSEChunk(writer, flusher, []byte(fmt.Sprintf("id: %s\nevent: ready\ndata: %s\n\n", sessionCursorID(cursor), payload)))
}

func writeSSETaskReady(writer http.ResponseWriter, flusher http.Flusher, cursor string, windows map[string]appservice.ReplayWindow) error {
	type replayWindow struct {
		Cursor           uint64 `json:"cursor"`
		EarliestSequence uint64 `json:"earliest_sequence"`
		Clipped          bool   `json:"clipped"`
	}
	sessions := make(map[string]replayWindow, len(windows))
	for sessionID, window := range windows {
		sessions[sessionID] = replayWindow{
			Cursor: window.Cursor, EarliestSequence: window.EarliestSequence, Clipped: window.Clipped,
		}
	}
	payload, err := json.Marshal(map[string]any{"cursor": cursor, "sessions": sessions})
	if err != nil {
		return err
	}
	return writeSSEChunk(writer, flusher, []byte(fmt.Sprintf("id: %s\nevent: ready\ndata: %s\n\n", cursor, payload)))
}

func writeSSEChunk(writer http.ResponseWriter, flusher http.Flusher, payload []byte) error {
	controller := http.NewResponseController(writer)
	deadlineSupported := true
	if err := controller.SetWriteDeadline(time.Now().Add(sseWriteTimeout)); err != nil {
		if !errors.Is(err, http.ErrNotSupported) {
			return err
		}
		deadlineSupported = false
	}
	_, writeErr := writer.Write(payload)
	var flushErr error
	if writeErr == nil {
		flushErr = controller.Flush()
		if errors.Is(flushErr, http.ErrNotSupported) {
			flusher.Flush()
			flushErr = nil
		}
	}
	var clearErr error
	if deadlineSupported {
		clearErr = controller.SetWriteDeadline(time.Time{})
	}
	if writeErr != nil {
		return writeErr
	}
	if flushErr != nil {
		return flushErr
	}
	if clearErr != nil && !errors.Is(clearErr, http.ErrNotSupported) {
		return clearErr
	}
	return nil
}

func strconvUint(value uint64) string { return fmt.Sprintf("%d", value) }

func decodeJSON(writer http.ResponseWriter, request *http.Request, destination any, allowEmpty bool) error {
	request.Body = http.MaxBytesReader(writer, request.Body, maxRequestBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(destination); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return nil
		}
		return fmt.Errorf("invalid JSON body: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("request body must contain one JSON object")
	}
	return nil
}

func requireEmptyBody(request *http.Request) error {
	defer request.Body.Close()
	data, err := io.ReadAll(io.LimitReader(request.Body, 2))
	if err != nil {
		return err
	}
	if len(strings.TrimSpace(string(data))) != 0 {
		return fmt.Errorf("request body must be empty")
	}
	return nil
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json; charset=utf-8")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func writeBadRequest(writer http.ResponseWriter, err error) {
	writeJSONError(writer, http.StatusBadRequest, "invalid_request", err.Error())
}

func writeValidationError(writer http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeJSONError(writer, http.StatusNotFound, "not_found", "The requested Task or Session was not found")
		return
	}
	if strings.Contains(err.Error(), "already used by active Session") || strings.Contains(err.Error(), "checkout may still change") {
		writeJSONError(writer, http.StatusConflict, "checkout_conflict", err.Error())
		return
	}
	var repositorySuggestion *gitrepo.RepositorySuggestionError
	if errors.As(err, &repositorySuggestion) && repositorySuggestion.SuggestedRepositoryPath != "" {
		writeJSON(writer, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{
			"code": "repository_selection_required", "message": err.Error(),
			"suggested_repo_path": repositorySuggestion.SuggestedRepositoryPath,
		}})
		return
	}
	writeJSONError(writer, http.StatusUnprocessableEntity, "validation_failed", err.Error())
}

func writeLookupError(writer http.ResponseWriter, kind string, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeJSONError(writer, http.StatusNotFound, "not_found", kind+" was not found")
		return
	}
	writeInternalError(writer)
}

func writeActionError(writer http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeJSONError(writer, http.StatusNotFound, "not_found", "Session was not found")
		return
	}
	var upgrade *appservice.WorkerUpgradeRequiredError
	if errors.As(err, &upgrade) && upgrade != nil {
		writeJSONError(writer, http.StatusConflict, "worker_upgrade_required", upgrade.Error())
		return
	}
	if errors.Is(err, appservice.ErrWorkerUpgradeRequired) {
		writeJSONError(writer, http.StatusConflict, "worker_upgrade_required", "Session worker upgrade required; provider write was not sent. Stop and resume the Session before retrying")
		return
	}
	var replayUnavailable *appservice.ReplayUnavailableError
	if errors.As(err, &replayUnavailable) {
		payload := map[string]any{
			"code":         "replay_unavailable",
			"message":      "Saved Session Channel replay is unavailable. Resume the Session to establish a new channel; earlier gateway audit cannot be recovered.",
			"reset_cursor": false,
		}
		var taskReplay *appservice.TaskSessionReplayError
		if errors.As(err, &taskReplay) {
			payload["session_id"] = taskReplay.SessionID
		}
		writeJSON(writer, http.StatusGone, map[string]any{"error": payload})
		return
	}
	var busy *worker.BusyError
	if errors.As(err, &busy) {
		writeJSONError(writer, http.StatusConflict, "session_busy", "Session already has an active provider turn")
		return
	}
	var notReady *worker.NotReadyError
	if errors.As(err, &notReady) {
		writeJSONError(writer, http.StatusConflict, "session_starting", "The Session Channel is live while the provider connection is still starting")
		return
	}
	var channelUnavailable *worker.ChannelUnavailableError
	if errors.As(err, &channelUnavailable) {
		writeJSONError(writer, http.StatusBadGateway, "session_channel_failed", channelUnavailable.Error())
		return
	}
	var optionsError *acp.ProviderOptionsError
	var configurationError *acp.ProviderConfigurationError
	if errors.As(err, &optionsError) || errors.As(err, &configurationError) {
		writeJSONError(writer, http.StatusBadGateway, "provider_setup_unavailable", "The live provider could not supply or apply its configuration controls. Resume the Session if needed, then retry.")
		return
	}
	message := err.Error()
	for _, expected := range []string{
		"has no live provider connection", "worker is already running", "already used by active Session", "no active provider turn",
		"no longer pending", "was not offered", "is not a child",
	} {
		if strings.Contains(message, expected) {
			code := "session_conflict"
			if strings.Contains(strings.ToLower(message), "busy") {
				code = "session_busy"
			}
			writeJSONError(writer, http.StatusConflict, code, message)
			return
		}
	}
	writeJSONError(writer, http.StatusBadGateway, "session_operation_failed", "Session operation failed")
}

func writeInternalError(writer http.ResponseWriter) {
	writeJSONError(writer, http.StatusInternalServerError, "internal_error", "Internal server error")
}

func writeJSONError(writer http.ResponseWriter, status int, code, message string) {
	writeJSON(writer, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("Content-Security-Policy", "default-src 'none'; frame-ancestors 'none'")
		if !loopbackRequestHost(request.Host) {
			writeJSONError(writer, http.StatusForbidden, "host_forbidden", "Loopback Host is required")
			return
		}
		if request.Method != http.MethodGet && request.Method != http.MethodHead && request.Method != http.MethodOptions {
			if origin := strings.TrimSpace(request.Header.Get("Origin")); origin != "" {
				parsed, err := url.Parse(origin)
				if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || !strings.EqualFold(parsed.Host, request.Host) {
					writeJSONError(writer, http.StatusForbidden, "origin_forbidden", "Cross-origin mutations are not allowed")
					return
				}
			}
		}
		next.ServeHTTP(writer, request)
	})
}

func loopbackRequestHost(value string) bool {
	if value == "" || strings.TrimSpace(value) != value {
		return false
	}

	host := value
	if parsedHost, port, err := net.SplitHostPort(value); err == nil {
		if !validRequestPort(port) {
			return false
		}
		host = parsedHost
	} else if strings.HasPrefix(value, "[") || strings.HasSuffix(value, "]") {
		if len(value) < 3 || value[0] != '[' || value[len(value)-1] != ']' {
			return false
		}
		host = value[1 : len(value)-1]
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func validRequestPort(value string) bool {
	if value == "" {
		return false
	}
	port := 0
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
		port = port*10 + int(character-'0')
		if port > 65535 {
			return false
		}
	}
	return true
}

func ListenLoopback(address string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("invalid listen address %q: %w", address, err)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("refusing non-loopback listen address %q; local V1 supports only explicit loopback IPs", address)
	}
	listener, err := net.Listen("tcp", address)
	if err != nil {
		return nil, fmt.Errorf("listen on %s: %w", address, err)
	}
	return listener, nil
}

func Serve(ctx context.Context, listener net.Listener, handler http.Handler) error {
	server := &http.Server{
		Handler: handler, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	select {
	case err := <-done:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	case <-ctx.Done():
		shutdownContext, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownContext); err != nil {
			return err
		}
		err := <-done
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}
