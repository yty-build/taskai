package session

import (
	"strings"
	"testing"
)

func TestStatuses(t *testing.T) {
	for _, value := range []string{"starting", "WORKING", " waiting ", "done", "failed", "cancelled", "stopped"} {
		if _, err := ParseStatus(value); err != nil {
			t.Errorf("ParseStatus(%q): %v", value, err)
		}
	}
	if _, err := ParseStatus("paused"); err == nil {
		t.Fatal("ParseStatus(paused) error = nil")
	}
}

func TestCanResumeRequiresOwnerlessTerminalIdentity(t *testing.T) {
	for _, status := range []Status{StatusStopped, StatusFailed, StatusDone, StatusCancelled} {
		if !(Session{Status: status, ProviderRef: "provider-one"}).CanResume() {
			t.Errorf("ownerless %s Session was not resumable", status)
		}
	}
	for _, current := range []Session{
		{Status: StatusStarting, ProviderRef: "provider-one"},
		{Status: StatusWaiting, ProviderRef: "provider-one"},
		{Status: StatusStopped},
		{Status: StatusStopped, ProviderRef: "provider-one", WorkerPID: 42},
		{Status: StatusStopped, ProviderRef: "provider-one", WorkerSocket: "/worker.sock"},
	} {
		if current.CanResume() {
			t.Errorf("Session unexpectedly resumable: %#v", current)
		}
	}
}

func TestCreateParamsValidation(t *testing.T) {
	valid := CreateParams{ID: "S-child", TaskID: "T-1", ParentSessionID: "S-parent", Agent: "codex"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}

	tests := map[string]CreateParams{
		"missing Task":       {Agent: "codex"},
		"missing agent":      {TaskID: "T-1"},
		"invalid metadata":   {TaskID: "T-1", Agent: "codex", MetadataJSON: []byte("{")},
		"self parent":        {ID: "S-self", TaskID: "T-1", ParentSessionID: "S-self", Agent: "codex"},
		"parent whitespace":  {TaskID: "T-1", ParentSessionID: " S-parent", Agent: "codex"},
		"parent path":        {TaskID: "T-1", ParentSessionID: "../parent", Agent: "codex"},
		"runtime whitespace": {TaskID: "T-1", Agent: "codex", RuntimeRef: " runtime"},
		"invalid status":     {TaskID: "T-1", Agent: "codex", Status: "paused"},
	}
	if err := (CreateParams{TaskID: "T-1", Agent: "codex", ProviderRef: " provider-owned "}).Validate(); err != nil {
		t.Fatalf("opaque provider reference rejected: %v", err)
	}
	for name, params := range tests {
		t.Run(name, func(t *testing.T) {
			if err := params.Validate(); err == nil {
				t.Fatal("invalid params accepted")
			}
		})
	}
}

func TestProviderOptionsValidation(t *testing.T) {
	for name, model := range map[string]string{
		"bidi override":  "safe\u202Eevil",
		"line separator": "safe\u2028evil",
		"nul":            "safe\x00evil",
	} {
		t.Run(name, func(t *testing.T) {
			if err := (ProviderOptions{Model: model}).Validate(); err == nil {
				t.Fatalf("invalid model %q was accepted", model)
			}
		})
	}

	if err := (ProviderOptions{Model: strings.Repeat("界", 86)}).Validate(); err == nil || !strings.Contains(err.Error(), "256 bytes") {
		t.Fatalf("overlong model error = %v", err)
	}
	if err := (ProviderOptions{Model: string([]byte{0xff})}).Validate(); err == nil {
		t.Fatal("invalid UTF-8 model was accepted")
	}
	if options := (ProviderOptions{Model: "--opaque provider value", ReasoningEffort: "provider effort @ 2"}); options.Validate() != nil {
		t.Fatalf("printable opaque provider values were rejected: %#v", options)
	}
	if err := (ProviderOptions{ReasoningEffort: "high\nlow"}).Validate(); err == nil || !strings.Contains(err.Error(), "reasoning effort") {
		t.Fatalf("control-bearing effort error = %v", err)
	}
}
