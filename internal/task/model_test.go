package task

import "testing"

func TestStatuses(t *testing.T) {
	for _, value := range []string{"open", "WORKING", " review ", "done", "failed", "cancelled"} {
		if _, err := ParseStatus(value); err != nil {
			t.Errorf("ParseStatus(%q): %v", value, err)
		}
	}
	if _, err := ParseStatus("running"); err == nil {
		t.Fatal("ParseStatus(running) error = nil")
	}
}

func TestCreateParamsValidation(t *testing.T) {
	if err := (CreateParams{Title: ""}).Validate(); err == nil {
		t.Fatal("empty title accepted")
	}
	if err := (CreateParams{Title: "valid", MetadataJSON: []byte("{")}).Validate(); err == nil {
		t.Fatal("invalid metadata accepted")
	}
}
