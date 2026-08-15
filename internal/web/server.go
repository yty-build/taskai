// Package web provides the disposable loopback-only browser UI. Pages render
// shared Task/Session service data and browser interactions use the versioned
// API; no worker transport or ACP wire data is exposed here.
package web

import (
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"github.com/yty-build/acp-gateway/internal/agents"
	"github.com/yty-build/acp-gateway/internal/api"
	appservice "github.com/yty-build/acp-gateway/internal/service"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/store"
	"github.com/yty-build/acp-gateway/internal/task"
)

//go:embed templates/*.html assets/*
var content embed.FS

type Server struct {
	tasks     *task.Service
	sessions  *appservice.SessionService
	agents    []agents.Profile
	templates *template.Template
	handler   http.Handler
}

type taskSummary struct {
	Task         task.Task
	SessionCount int
}

type sessionView struct {
	ID             string
	TaskID         string
	Role           string
	Agent          string
	Provider       string
	RepoPath       string
	WorktreePath   string
	CheckoutLabel  string
	Branch         string
	Status         string
	Permission     string
	WorkerRunning  bool
	UpdatedAt      time.Time
	LastActivityAt *time.Time
}

type tasksPage struct {
	PageTitle string
	Page      string
	Tasks     []taskSummary
}

type taskPage struct {
	PageTitle string
	Page      string
	Task      task.Task
	Sessions  []sessionView
	Agents    []agents.Profile
}

type sessionPage struct {
	PageTitle string
	Page      string
	Task      task.Task
	Session   sessionView
	Agents    []agents.Profile
}

func New(tasks *task.Service, sessions *appservice.SessionService, registry *agents.Registry) (*Server, error) {
	if tasks == nil || sessions == nil || registry == nil {
		return nil, fmt.Errorf("web server requires Task, Session, and agent registry services")
	}
	templates, err := template.New("pages").Funcs(template.FuncMap{
		"statusTone": statusTone,
		"formatTime": func(value time.Time) string {
			if value.IsZero() {
				return ""
			}
			return value.Local().Format("2006-01-02 15:04:05")
		},
	}).ParseFS(content, "templates/*.html")
	if err != nil {
		return nil, fmt.Errorf("parse web templates: %w", err)
	}
	assets, err := fs.Sub(content, "assets")
	if err != nil {
		return nil, fmt.Errorf("load web assets: %w", err)
	}
	server := &Server{tasks: tasks, sessions: sessions, agents: registry.List(), templates: templates}
	apiHandler := api.New(tasks, sessions).Handler()
	mux := http.NewServeMux()
	mux.Handle("/api/", apiHandler)
	mux.Handle("/healthz", apiHandler)
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", http.FileServer(http.FS(assets))))
	mux.HandleFunc("GET /tasks/{taskID}", server.taskPage)
	mux.HandleFunc("GET /sessions/{sessionID}", server.sessionPage)
	mux.HandleFunc("/", server.tasksPage)
	server.handler = webHeaders(mux)
	return server, nil
}

func (s *Server) Handler() http.Handler { return s.handler }

func (s *Server) tasksPage(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		http.Error(writer, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if request.URL.Path != "/" {
		http.NotFound(writer, request)
		return
	}
	items, err := s.tasks.List(request.Context())
	if err != nil {
		writePageError(writer, err)
		return
	}
	summaries := make([]taskSummary, 0, len(items))
	for _, item := range items {
		detail, err := s.tasks.Get(request.Context(), item.ID)
		if err != nil {
			writePageError(writer, err)
			return
		}
		summaries = append(summaries, taskSummary{Task: item, SessionCount: len(detail.Sessions)})
	}
	s.execute(writer, "tasks", tasksPage{PageTitle: "Tasks · Agent Task Console", Page: "tasks", Tasks: summaries})
}

func (s *Server) taskPage(writer http.ResponseWriter, request *http.Request) {
	detail, err := s.tasks.Get(request.Context(), request.PathValue("taskID"))
	if err != nil {
		writePageError(writer, err)
		return
	}
	sessions := make([]sessionView, 0, len(detail.Sessions))
	for _, current := range detail.Sessions {
		sessions = append(sessions, makeSessionView(current))
	}
	s.execute(writer, "task", taskPage{
		PageTitle: detail.Task.Title + " · Agent Task Console", Page: "task", Task: detail.Task, Sessions: sessions, Agents: s.agents,
	})
}

func (s *Server) sessionPage(writer http.ResponseWriter, request *http.Request) {
	current, err := s.sessions.Get(request.Context(), request.PathValue("sessionID"))
	if err != nil {
		writePageError(writer, err)
		return
	}
	detail, err := s.tasks.Get(request.Context(), current.TaskID)
	if err != nil {
		writePageError(writer, err)
		return
	}
	s.execute(writer, "session", sessionPage{
		PageTitle: current.Agent + " " + current.Role + " · Agent Task Console", Page: "session",
		Task: detail.Task, Session: makeSessionView(current), Agents: s.agents,
	})
}

func (s *Server) execute(writer http.ResponseWriter, name string, data any) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(writer, name, data); err != nil {
		// Headers may already be committed. The error is intentionally not sent
		// to the browser because template internals are not a client contract.
		return
	}
}

func makeSessionView(current session.Session) sessionView {
	checkout := current.WorktreePath
	if checkout == "" {
		checkout = current.RepoPath
	}
	label := filepath.Base(checkout)
	if label == "." || label == string(filepath.Separator) || strings.TrimSpace(label) == "" {
		label = checkout
	}
	return sessionView{
		ID: current.ID, TaskID: current.TaskID, Role: current.Role, Agent: current.Agent, Provider: current.Provider,
		RepoPath: current.RepoPath, WorktreePath: current.WorktreePath, CheckoutLabel: label, Branch: current.Branch,
		Status: string(current.Status), Permission: appservice.PermissionLabel(current), WorkerRunning: current.WorkerSocket != "",
		UpdatedAt: current.UpdatedAt, LastActivityAt: current.LastActivityAt,
	}
}

func statusTone(status string) string {
	switch status {
	case "working", "starting":
		return "active"
	case "waiting", "review", "open", "stopped":
		return "waiting"
	case "done":
		return "done"
	case "failed", "cancelled":
		return "danger"
	default:
		return "neutral"
	}
}

func writePageError(writer http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		http.Error(writer, "Not found", http.StatusNotFound)
		return
	}
	http.Error(writer, "Internal server error", http.StatusInternalServerError)
}

func webHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("X-Content-Type-Options", "nosniff")
		writer.Header().Set("Referrer-Policy", "no-referrer")
		writer.Header().Set("X-Frame-Options", "DENY")
		writer.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; connect-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		writer.Header().Set("Cache-Control", "no-store")
		if !loopbackRequestHost(request.Host) {
			http.Error(writer, "Loopback Host required", http.StatusForbidden)
			return
		}
		if request.Method != http.MethodGet && request.Method != http.MethodHead && !sameBrowserOrigin(request) {
			http.Error(writer, "Cross-origin request rejected", http.StatusForbidden)
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func loopbackRequestHost(value string) bool {
	host := value
	if parsed, _, err := net.SplitHostPort(value); err == nil {
		host = parsed
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

func sameBrowserOrigin(request *http.Request) bool {
	if strings.EqualFold(request.Header.Get("Sec-Fetch-Site"), "cross-site") {
		return false
	}
	origin := request.Header.Get("Origin")
	if origin == "" {
		return true
	}
	parsed, err := url.Parse(origin)
	return err == nil && parsed.Scheme == "http" && parsed.Host == request.Host
}
