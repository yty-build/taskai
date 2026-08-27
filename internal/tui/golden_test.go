package tui

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

func TestWorkspaceGoldens(t *testing.T) {
	for _, size := range []struct{ width, height int }{{40, 16}, {60, 18}, {80, 24}, {120, 40}} {
		name := fmt.Sprintf("workspace_%dx%d.golden", size.width, size.height)
		t.Run(name, func(t *testing.T) {
			model, _, _ := loadedModel(t, baseSnapshot(), false)
			defer model.Close()
			model.currentTask = "task-new"
			model.screen = screenWorkspace
			model.selectedManager = "manager-a"
			p := testPane(model, "manager-a")
			p.append(paneFrame{sequence: 1, text: "First public frame", bytes: 18}, model.limits)
			p.append(paneFrame{sequence: 2, text: "Second public frame", bytes: 19}, model.limits)
			model.Update(tea.WindowSizeMsg{Width: size.width, Height: size.height})
			actual := goldenProjection(model.View().Content)
			path := filepath.Join("testdata", name)
			expected, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read %s: %v\nactual:\n%s", path, err, actual)
			}
			if actual != strings.TrimSuffix(string(expected), "\n") {
				t.Fatalf("golden mismatch %s\n--- want ---\n%s\n--- got ---\n%s", path, expected, actual)
			}
		})
	}
}

func goldenProjection(view string) string {
	var projected []string
	for row, line := range strings.Split(view, "\n") {
		line = strings.TrimRight(line, " ")
		if strings.Trim(strings.TrimSpace(line), "│ ") == "" {
			continue
		}
		projected = append(projected, fmt.Sprintf("%02d|%s", row, line))
	}
	return strings.Join(projected, "\n")
}
