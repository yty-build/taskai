package console

import (
	"context"
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/worker"
)

func TestTaskConsoleResponsiveLayoutsStayBounded(t *testing.T) {
	backend := newFakeConsoleBackend()
	for _, size := range []struct{ width, height int }{{40, 16}, {60, 20}, {100, 30}, {140, 42}} {
		model := baseModel(context.Background(), backend)
		model.mode = taskView
		model.task = backend.detail
		model.width, model.height = size.width, size.height
		view := strings.TrimSuffix(model.View(), "\n")
		lines := strings.Split(view, "\n")
		if len(lines) != size.height {
			t.Fatalf("%dx%d rendered %d lines", size.width, size.height, len(lines))
		}
		for index, line := range lines {
			if ansi.StringWidth(line) > size.width {
				t.Fatalf("%dx%d line %d width = %d: %q", size.width, size.height, index, ansi.StringWidth(line), line)
			}
		}
		if !strings.Contains(view, "Open") || strings.Contains(strings.ToLower(view), "native") {
			t.Fatalf("%dx%d task transport labels are wrong:\n%s", size.width, size.height, view)
		}
	}
}

func TestSessionChannelViewShowsImmediateFramesAndDetachLanguage(t *testing.T) {
	backend := newFakeConsoleBackend()
	current := backend.sessions["S-1"]
	current.Status = session.StatusWorking
	model := baseModel(context.Background(), backend)
	model.mode = sessionView
	model.direct = true
	model.current = current
	model.width, model.height = 100, 30
	model.addActivity("S-1", testLine(1, "REQ-1", worker.FrameClientCommand, map[string]any{"command": "prompt", "text": "Show status"}), false)
	model.addActivity("S-1", testLine(2, "REQ-1", worker.FrameProviderEvent, map[string]any{"type": "text_delta", "stream": "output", "text": "Working now"}), true)
	view := model.View()
	for _, want := range []string{"SESSION CHANNEL", "Show status", "Working now", "q Detach"} {
		if !strings.Contains(view, want) {
			t.Fatalf("Session Channel view missing %q:\n%s", want, view)
		}
	}
	for _, forbidden := range []string{"Provider history", "reconciliation", "Native CLI", "prepared"} {
		if strings.Contains(view, forbidden) {
			t.Fatalf("Session Channel view contains obsolete %q:\n%s", forbidden, view)
		}
	}
}

func TestProviderSelectorsRenderAdvertisedChoices(t *testing.T) {
	backend := newFakeConsoleBackend()
	model := baseModel(context.Background(), backend)
	model.task = backend.detail
	model.width, model.height = 100, 30
	model.wizard = &sessionWizard{
		agents: backend.profiles, connectedID: "S-2", providerOptions: backend.providerOptions,
		providerOptionsReady: true, field: wizardModel,
	}
	model.wizard.role.value = []rune("worker")
	model.wizard.repo.value = []rune("/repo")
	view := model.View()
	for _, want := range []string{"Model", "Provider default (currently provider-model)", "Reasoning", "Provider default (currently medium)", "live provider Port"} {
		if !strings.Contains(view, want) {
			t.Fatalf("provider selector missing %q:\n%s", want, view)
		}
	}
}
