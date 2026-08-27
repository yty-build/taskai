// Package web provides the disposable loopback-only browser UI. Pages render
// shared Task/Session service data and browser interactions use the versioned
// API, including the mechanical Session frame stream.
package web

import (
	"context"
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

type sessionView struct {
	ID              string
	TaskID          string
	ParentSessionID string
	Role            string
	Agent           string
	RepoPath        string
	WorktreePath    string
	CheckoutLabel   string
	Branch          string
	Status          string
	Permission      string
	Resumable       bool
	HasProviderRef  bool
}

type navigationTask struct {
	Task         task.Task
	SessionCount int
	Sessions     []sessionView
}

type pageBase struct {
	PageTitle        string
	Page             string
	Navigation       []navigationTask
	CurrentTaskID    string
	CurrentSessionID string
}

type tasksPage struct {
	pageBase
	Tasks []navigationTask
}

type taskPage struct {
	pageBase
	Task     task.Task
	Sessions []sessionView
	Agents   []agents.Profile
}

type sessionPage struct {
	pageBase
	Task    task.Task
	Session sessionView
	Agents  []agents.Profile
}

func New(tasks *task.Service, sessions *appservice.SessionService, registry *agents.Registry) (*Server, error) {
	if tasks == nil || sessions == nil || registry == nil {
		return nil, fmt.Errorf("web server requires Task, Session, and agent registry services")
	}
	templates, err := template.New("pages").Funcs(template.FuncMap{
		"statusTone":  statusTone,
		"statusLabel": statusLabel,
		"agentInitial": func(value string) string {
			value = strings.TrimSpace(value)
			if value == "" {
				return "A"
			}
			return strings.ToUpper(string([]rune(value)[0]))
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
	navigation, err := s.navigation(request.Context(), "")
	if err != nil {
		writePageError(writer, err)
		return
	}
	s.execute(writer, "tasks", tasksPage{
		pageBase: pageBase{PageTitle: "Tasks · Agent Task Console", Page: "tasks", Navigation: navigation},
		Tasks:    navigation,
	})
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
	navigation, err := s.navigation(request.Context(), detail.Task.ID)
	if err != nil {
		writePageError(writer, err)
		return
	}
	s.execute(writer, "task", taskPage{
		pageBase: pageBase{PageTitle: detail.Task.Title + " · Agent Task Console", Page: "task", Navigation: navigation, CurrentTaskID: detail.Task.ID},
		Task:     detail.Task, Sessions: sessions, Agents: s.agents,
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
	navigation, err := s.navigation(request.Context(), detail.Task.ID)
	if err != nil {
		writePageError(writer, err)
		return
	}
	s.execute(writer, "session", sessionPage{
		pageBase: pageBase{
			PageTitle: current.Agent + " " + current.Role + " · Agent Task Console", Page: "session",
			Navigation: navigation, CurrentTaskID: detail.Task.ID, CurrentSessionID: current.ID,
		},
		Task: detail.Task, Session: makeSessionView(current), Agents: s.agents,
	})
}

func (s *Server) navigation(ctx context.Context, currentTaskID string) ([]navigationTask, error) {
	items, err := s.tasks.List(ctx)
	if err != nil {
		return nil, err
	}
	navigation := make([]navigationTask, 0, len(items))
	for _, item := range items {
		detail, err := s.tasks.Get(ctx, item.ID)
		if err != nil {
			return nil, err
		}
		entry := navigationTask{Task: item, SessionCount: len(detail.Sessions)}
		if item.ID == currentTaskID {
			entry.Sessions = make([]sessionView, 0, len(detail.Sessions))
			for _, current := range detail.Sessions {
				entry.Sessions = append(entry.Sessions, makeSessionView(current))
			}
		}
		navigation = append(navigation, entry)
	}
	return navigation, nil
}

func (s *Server) execute(writer http.ResponseWriter, name string, data any) {
	writer.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := s.templates.ExecuteTemplate(writer, name, data); err != nil {
		// Headers may already have been written. The error is intentionally not sent
		// to the browser because template internals are not a client contract.
		return
	}
}

func makeSessionView(current session.Session) sessionView {
	label := filepath.Base(current.RepoPath)
	if label == "." || label == string(filepath.Separator) || strings.TrimSpace(label) == "" {
		label = current.RepoPath
	}
	if current.WorktreePath != "" {
		label += " · isolated worktree"
	}
	return sessionView{
		ID: current.ID, TaskID: current.TaskID, ParentSessionID: current.ParentSessionID, Role: current.Role, Agent: current.Agent,
		RepoPath: current.RepoPath, WorktreePath: current.WorktreePath, CheckoutLabel: label, Branch: current.Branch,
		Status: string(current.Status), Permission: appservice.PermissionLabel(current),
		Resumable: current.CanResume(), HasProviderRef: current.ProviderRef != "",
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

func statusLabel(status string) string {
	switch status {
	case "starting":
		return "Starting"
	case "working":
		return "Working"
	case "waiting":
		return "Ready"
	case "review":
		return "In review"
	case "open":
		return "Open"
	case "stopped":
		return "Stopped"
	case "done":
		return "Complete"
	case "failed":
		return "Failed"
	case "cancelled":
		return "Cancelled"
	default:
		if strings.TrimSpace(status) == "" {
			return "Unknown"
		}
		return strings.ToUpper(status[:1]) + status[1:]
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
