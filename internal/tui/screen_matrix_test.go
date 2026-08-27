package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

type screenEvidenceScenario struct {
	name     string
	snapshot func() Snapshot
	unloaded bool
	setup    func(*Model)
	want     []string
	forbid   []string
}

// TestOwnedScreenGoldenMatrix is the complete TaskAI-owned screen evidence.
// Every scenario has semantic assertions and contributes a separately labeled
// section to each reviewed size-specific golden.
func TestOwnedScreenGoldenMatrix(t *testing.T) {
	sizes := []struct{ width, height int }{{40, 16}, {60, 18}, {80, 24}, {120, 40}}
	for _, size := range sizes {
		name := fmt.Sprintf("owned_screens_%dx%d.golden", size.width, size.height)
		t.Run(name, func(t *testing.T) {
			var sections []string
			for _, scenario := range ownedScreenScenarios() {
				model := buildScreenEvidenceModel(t, scenario)
				model.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
				view := model.View()
				assertViewBounds(t, view, size.width, size.height)
				assertContainsAll(t, view.Content, scenario.want...)
				for _, forbidden := range scenario.forbid {
					if strings.Contains(view.Content, forbidden) {
						t.Fatalf("screen %s contains contradictory text %q", scenario.name, forbidden)
					}
				}
				sections = append(sections, "## "+scenario.name+"\n"+goldenProjection(view.Content))
				model.Close()
			}
			actual := strings.Join(sections, "\n\n")
			path := filepath.Join("testdata", name)
			expected, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v\n--- inspected candidate ---\n%s", path, err, actual)
			}
			if actual != strings.TrimSuffix(string(expected), "\n") {
				t.Fatalf("golden mismatch %s\n--- want ---\n%s\n--- got ---\n%s", path, expected, actual)
			}
		})
	}
}

func buildScreenEvidenceModel(t *testing.T, scenario screenEvidenceScenario) *Model {
	t.Helper()
	snapshot := scenario.snapshot()
	if scenario.unloaded {
		fake := newFake(snapshot)
		model, err := New(Config{Catalog: fake, Tasks: fake, Providers: fake, Runtime: fake})
		if err != nil {
			t.Fatal(err)
		}
		if scenario.setup != nil {
			scenario.setup(model)
		}
		return model
	}
	model, _, _ := loadedModel(t, snapshot, false)
	if scenario.setup != nil {
		scenario.setup(model)
	}
	return model
}

func ownedScreenScenarios() []screenEvidenceScenario {
	base := func() Snapshot { return baseSnapshot() }
	empty := func() Snapshot { return Snapshot{StreamGeneration: 1} }
	historyOnly := func() Snapshot {
		snapshot := baseSnapshot()
		snapshot.Tasks = snapshot.Tasks[:1]
		snapshot.Sessions = snapshot.Sessions[:1]
		return snapshot
	}
	noManagers := func() Snapshot {
		snapshot := baseSnapshot()
		snapshot.Tasks = []Task{snapshot.Tasks[1]}
		snapshot.Sessions = nil
		return snapshot
	}
	noManagerProviders := func() Snapshot {
		snapshot := baseSnapshot()
		for i := range snapshot.Providers {
			snapshot.Providers[i].Enabled = false
		}
		return snapshot
	}
	disconnected := func() Snapshot {
		snapshot := baseSnapshot()
		for i := range snapshot.Providers {
			if snapshot.Providers[i].ID == "codex" {
				snapshot.Providers[i].ProbeStatus = ProbeDisconnected
			}
		}
		return snapshot
	}
	answered := func() Snapshot {
		snapshot := baseSnapshot()
		for i := range snapshot.Sessions {
			switch snapshot.Sessions[i].ID {
			case "manager-a":
				snapshot.Sessions[i].State = StateDormant
			case "child-answer":
				snapshot.Sessions[i].State = StateDormant
			}
		}
		return snapshot
	}
	workspace := func(model *Model) {
		model.currentTask = "task-new"
		model.screen = screenWorkspace
		model.selectedManager = "manager-a"
		model.focus = focusList
	}

	return []screenEvidenceScenario{
		{
			name: "tasks-loading", snapshot: base, unloaded: true,
			want:   []string{"TaskAI / Tasks", "Loading metadata", "Active", "Add Task", "Settings"},
			forbid: []string{"No Tasks yet", "No providers enabled"},
		},
		{
			name: "tasks-active", snapshot: base,
			want: []string{"TaskAI / Tasks", "Created ↓", "Newest Task", "A:3 D:1", "[Active]"},
		},
		{
			name: "tasks-all-history", snapshot: base,
			setup: func(model *Model) { model.filter = filterAll },
			want:  []string{"TaskAI / Tasks", "[All]", "Newest Task", "Older Task", "A:0 D:1"},
		},
		{
			name: "tasks-empty-no-provider", snapshot: empty,
			want: []string{"TaskAI / Tasks", "No Tasks yet", "No providers enabled", "Add Task", "Settings"},
		},
		{
			name: "tasks-no-active-history", snapshot: historyOnly,
			want: []string{"TaskAI / Tasks", "No active Tasks", "Choose All for history"},
		},
		{
			name: "tasks-safe-error", snapshot: base,
			setup: func(model *Model) { model.status = "Metadata refresh failed (unavailable)" },
			want:  []string{"TaskAI / Tasks", "Metadata refresh failed (unavailable)", "Newest Task"},
		},
		{
			name: "settings-standard", snapshot: base,
			setup: func(model *Model) { model.screen = screenSettings; model.returnTo = screenTasks },
			want: []string{"TaskAI / Provider Settings", "Enable, authenticate, and recheck", "Codex", "Connected",
				"Manager:yes", "Subordinate:yes", "Resume:yes", "Replay:no"},
		},
		{
			name: "settings-empty", snapshot: empty,
			setup: func(model *Model) { model.screen = screenSettings; model.returnTo = screenTasks },
			want:  []string{"TaskAI / Provider Settings", "No provider connectors registered", "No provider selected"},
		},
		{
			name: "settings-disconnected-error", snapshot: disconnected,
			setup: func(model *Model) {
				model.screen = screenSettings
				model.returnTo = screenTasks
				model.status = "Provider recheck failed (disconnected)"
			},
			want: []string{"TaskAI / Provider Settings", "Provider recheck failed (disconnected)", "status:Disconnected"},
		},
		{
			name: "add-task-draft", snapshot: base,
			setup: func(model *Model) {
				model.screen = screenAddTask
				model.focus = focusInput
				model.taskDraft.insert("  Exact Task title  ")
			},
			want: []string{"TaskAI / Tasks / Add Task", "Title is preserved", "Exact Task title", "Add Task", "Esc back"},
		},
		{
			name: "add-task-validation-error", snapshot: empty,
			setup: func(model *Model) {
				model.screen = screenAddTask
				model.focus = focusInput
				model.status = "Task title is required"
			},
			want: []string{"TaskAI / Tasks / Add Task", "Task title is required", "Add Task"},
		},
		{
			name: "add-manager-standard", snapshot: base,
			setup: func(model *Model) {
				workspace(model)
				model.openAddManager()
				model.managerLabel.insert("Exact Manager label")
			},
			want: []string{"TaskAI / Task / Add Manager", "Only enabled providers", "Codex", "Optional label",
				"Exact Manager label", "Create Manager"},
		},
		{
			name: "add-manager-unavailable", snapshot: noManagerProviders,
			setup: func(model *Model) { workspace(model); model.openAddManager() },
			want:  []string{"TaskAI / Task / Add Manager", "No enabled Manager-capable provider", "Open Settings"},
		},
		{
			name: "add-manager-safe-error", snapshot: base,
			setup: func(model *Model) {
				workspace(model)
				model.openAddManager()
				model.managerLabel.insert("Retry label")
				model.status = "Manager start failed (connector-unavailable)"
			},
			want: []string{"TaskAI / Task / Add Manager", "Retry label", "Manager start failed"},
		},
		{
			name: "workspace-standard", snapshot: base,
			setup: func(model *Model) {
				workspace(model)
				p := testPane(model, "manager-a")
				p.append(paneFrame{sequence: 1, text: "First public frame", bytes: 18}, model.limits)
				p.append(paneFrame{sequence: 2, text: "Second public frame", bytes: 19}, model.limits)
			},
			want: []string{"TaskAI / Task / Newest Task", "Manager manager-a", "Manager A", "Codex", "Working",
				"Subordinate Answer", "Subordinate Follow", "First public frame", "Second public frame", "Input", "Stop"},
		},
		{
			name: "workspace-no-manager", snapshot: noManagers,
			setup: func(model *Model) { workspace(model); model.selectedManager = "" },
			want: []string{"TaskAI / Task / Newest Task", "Selected Manager pane: unavailable", "No Manager Sessions",
				"No selected Manager", "Manager input unavailable", "Add"},
		},
		{
			name: "workspace-live-safe-error", snapshot: disconnected,
			setup: func(model *Model) {
				workspace(model)
				testPane(model, "manager-a").append(paneFrame{sequence: 1, text: "Live public frame", bytes: 17}, model.limits)
				model.status = "Provider recheck failed (disconnected)"
			},
			want: []string{"TaskAI / Task / Newest Task", "Live public frame", "Provider recheck failed", "Manager manager-a"},
		},
		{
			name: "workspace-answered-resume", snapshot: answered,
			setup: workspace,
			want: []string{"TaskAI / Task / Newest Task", "Manager manager-a", "Dormant", "Subordinate Answer Answered",
				"Exact Session Resume", "Resume"},
		},
		{
			name: "session-details", snapshot: base,
			setup: func(model *Model) { workspace(model); model.openSessionDetails("child-answer") },
			want: []string{"TaskAI / Exact Session Details", "Session 2 of 3", "Task ID: task-new",
				"Session ID: child-answer", "Parent Session ID: manager-a", "Previous", "Next", "Close"},
		},
		{
			name: "help-modal", snapshot: base,
			setup: func(model *Model) { workspace(model); model.help = true },
			want:  []string{"TaskAI / Help", "Back detaches", "Subordinates are depth one", "Close Help"},
		},
		{
			name: "stop-confirmation", snapshot: base,
			setup: func(model *Model) { workspace(model); model.confirmStop = true },
			want:  []string{"TaskAI / Task / Newest Task", "Selected Manager: Manager A", "Stop selected Manager tree?", "[Yes] [No]"},
		},
	}
}
