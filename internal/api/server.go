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
	"strings"
	"time"

	"github.com/yty-build/acp-gateway/internal/render"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/sessionlog"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
)

const maxRequestBody = 1 << 20

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
	mux.HandleFunc("GET /api/v1/tasks/{taskID}/stream", server.streamTask)
	mux.HandleFunc("GET /api/v1/sessions/{sessionID}", server.getSession)
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/prompt", server.prompt)
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/cancel", server.cancel)
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/resume", server.resume)
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/stop", server.stop)
	mux.HandleFunc("POST /api/v1/sessions/{sessionID}/review", server.review)
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

type startSessionRequest struct {
	Agent               string `json:"agent"`
	Role                string `json:"role"`
	RepoPath            string `json:"repo_path"`
	Prompt              string `json:"prompt"`
	Worktree            bool   `json:"worktree"`
	ApproveAll          bool   `json:"approve_all"`
	AllowSharedCheckout bool   `json:"allow_shared_checkout"`
}

func (s *Server) startSession(writer http.ResponseWriter, request *http.Request) {
	var body startSessionRequest
	if err := decodeJSON(writer, request, &body, false); err != nil {
		writeBadRequest(writer, err)
		return
	}
	launched, err := s.sessions.Start(request.Context(), appservice.StartSessionParams{
		TaskID: request.PathValue("taskID"), Agent: body.Agent, Role: body.Role, RepoPath: body.RepoPath,
		Prompt: body.Prompt, Worktree: body.Worktree, ApproveAll: body.ApproveAll, AllowSharedCheckout: body.AllowSharedCheckout,
	})
	if err != nil {
		writeValidationError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{
		"session": makeSessionDTO(launched.Session), "request_id": launched.RequestID, "state": launched.State,
	})
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

type reviewRequest struct {
	Agent               string `json:"agent"`
	AllowActiveProducer bool   `json:"allow_active_producer"`
}

func (s *Server) review(writer http.ResponseWriter, request *http.Request) {
	var body reviewRequest
	if err := decodeJSON(writer, request, &body, false); err != nil {
		writeBadRequest(writer, err)
		return
	}
	launched, err := s.sessions.Review(request.Context(), appservice.ReviewSessionParams{
		ProducerID: request.PathValue("sessionID"), Agent: body.Agent, AllowActiveProducer: body.AllowActiveProducer,
	})
	if err != nil {
		writeValidationError(writer, err)
		return
	}
	writeJSON(writer, http.StatusCreated, map[string]any{
		"session": makeSessionDTO(launched.Session), "request_id": launched.RequestID, "state": launched.State,
	})
}

func (s *Server) streamSession(writer http.ResponseWriter, request *http.Request) {
	cursor, err := sessionCursor(request)
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
	startSSE(writer, flusher)
	err = s.sessions.Follow(request.Context(), id, cursor, func(line sessionlog.Line) error {
		event := makeActivityDTO(id, line, "")
		return writeSSE(writer, flusher, strconvUint(line.Seq), event)
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return
	}
}

func (s *Server) streamTask(writer http.ResponseWriter, request *http.Request) {
	cursor, err := taskCursor(request)
	if err != nil {
		writeBadRequest(writer, err)
		return
	}
	id := request.PathValue("taskID")
	if _, err := s.tasks.Get(request.Context(), id); err != nil {
		writeLookupError(writer, "Task", err)
		return
	}
	flusher, ok := writer.(http.Flusher)
	if !ok {
		writeJSONError(writer, http.StatusInternalServerError, "stream_unsupported", "Streaming is unavailable")
		return
	}
	startSSE(writer, flusher)
	err = s.sessions.FollowTask(request.Context(), id, cursor, func(activity appservice.TaskActivity) error {
		cursor[activity.SessionID] = activity.Line.Seq
		token, err := encodeTaskCursor(cursor)
		if err != nil {
			return err
		}
		event := makeActivityDTO(activity.SessionID, activity.Line, token)
		return writeSSE(writer, flusher, token, event)
	})
	if err != nil && !errors.Is(err, context.Canceled) {
		return
	}
}

func makeActivityDTO(sessionID string, line sessionlog.Line, cursor string) ActivityDTO {
	return ActivityDTO{
		SessionID: sessionID, Position: line.Seq, RequestID: line.RequestID, Kind: line.Type,
		Timestamp: formatTime(line.Timestamp), Activity: render.Activity(line, false), Cursor: cursor,
	}
}

func startSSE(writer http.ResponseWriter, flusher http.Flusher) {
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache, no-store")
	writer.Header().Set("Connection", "keep-alive")
	writer.WriteHeader(http.StatusOK)
	_, _ = io.WriteString(writer, ": connected\n\n")
	flusher.Flush()
}

func writeSSE(writer io.Writer, flusher http.Flusher, id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(writer, "id: %s\nevent: activity\ndata: %s\n\n", id, data); err != nil {
		return err
	}
	flusher.Flush()
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
	message := err.Error()
	for _, expected := range []string{"has no live worker", "worker is already running", "already used by active Session", "no active turn"} {
		if strings.Contains(message, expected) {
			writeJSONError(writer, http.StatusConflict, "session_conflict", message)
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
		next.ServeHTTP(writer, request)
	})
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
