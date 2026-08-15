package session

import "testing"

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

func TestCreateParamsValidation(t *testing.T) {
	if err := (CreateParams{Agent: "codex"}).Validate(); err == nil {
		t.Fatal("missing Task ID accepted")
	}
	if err := (CreateParams{TaskID: "T-1"}).Validate(); err == nil {
		t.Fatal("missing agent accepted")
	}
	if err := (CreateParams{TaskID: "T-1", Agent: "codex", MetadataJSON: []byte("no")}).Validate(); err == nil {
		t.Fatal("invalid metadata accepted")
	}
}
