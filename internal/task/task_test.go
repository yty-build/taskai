package task

import (
	"errors"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestNewPreservesExactTitleAndCanonicalizesTime(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 8, 27, 10, 11, 12, 123456789, time.FixedZone("test", 3600))
	title := "  exact\tタイトル\n"
	value, err := New("task_exact", title, created)
	if err != nil {
		t.Fatal(err)
	}
	if value.Title != title {
		t.Fatalf("title changed: got %q want %q", value.Title, title)
	}
	wantTime := created.UTC().Truncate(time.Microsecond)
	if value.CreatedAt != wantTime || value.UpdatedAt != wantTime {
		t.Fatalf("unexpected times: %#v", value)
	}
}

func TestIDValidation(t *testing.T) {
	t.Parallel()

	valid := []ID{"a", "task_123", "TASK:one.two-three"}
	for _, id := range valid {
		if err := id.Validate(); err != nil {
			t.Errorf("Validate(%q): %v", id, err)
		}
	}
	invalid := []ID{"", "contains space", "slash/value", ID(strings.Repeat("a", MaxIDBytes+1))}
	for _, id := range invalid {
		if err := id.Validate(); !errors.Is(err, ErrInvalidID) {
			t.Errorf("Validate(%q) error = %v, want ErrInvalidID", id, err)
		}
	}
	generated, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := generated.Validate(); err != nil {
		t.Fatalf("generated ID invalid: %v", err)
	}
	parsed, err := ParseID("task:parsed")
	if err != nil || parsed != "task:parsed" {
		t.Fatalf("ParseID = %q, %v", parsed, err)
	}
	if _, err := ParseID("bad/id"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("invalid ParseID error = %v", err)
	}
}

func TestTitleValidationBoundaries(t *testing.T) {
	t.Parallel()

	invalidUTF8 := string([]byte{0xff})
	tests := []struct {
		name  string
		title string
		valid bool
	}{
		{name: "simple", title: "task", valid: true},
		{name: "preserved controls", title: "line\n\ttitle\x1b[31m", valid: true},
		{name: "max bytes", title: strings.Repeat("😀", MaxTitleBytes/4), valid: true},
		{name: "too many bytes", title: strings.Repeat("😀", MaxTitleBytes/4+1), valid: false},
		{name: "max runes", title: strings.Repeat("é", MaxTitleRunes), valid: true},
		{name: "too many runes", title: strings.Repeat("a", MaxTitleRunes+1), valid: false},
		{name: "empty", title: "", valid: false},
		{name: "unicode whitespace", title: " \t\n\u2003", valid: false},
		{name: "nul", title: "bad\x00title", valid: false},
		{name: "invalid utf8", title: invalidUTF8, valid: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := ValidateTitle(test.title)
			if test.valid && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !test.valid && !errors.Is(err, ErrInvalidTitle) {
				t.Fatalf("error = %v, want ErrInvalidTitle", err)
			}
		})
	}
	if got := utf8.RuneCountInString(strings.Repeat("é", MaxTitleRunes)); got != MaxTitleRunes {
		t.Fatalf("test construction error: %d", got)
	}
}

func TestWithTitleRejectsStaleTime(t *testing.T) {
	t.Parallel()

	created := time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC)
	value, err := New("task_one", "before", created)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := value.WithTitle(" after ", created.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != " after " {
		t.Fatalf("title changed: %q", updated.Title)
	}
	_, err = updated.WithTitle("stale", created)
	if !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("error = %v, want ErrInvalidTime", err)
	}
}

func TestTaskValidationAndViews(t *testing.T) {
	t.Parallel()

	now := time.Now().UTC()
	value, err := New("task_one", "one", now)
	if err != nil {
		t.Fatal(err)
	}
	value.UpdatedAt = value.CreatedAt.Add(-time.Microsecond)
	if err := value.Validate(); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("error = %v, want ErrInvalidTime", err)
	}
	for _, view := range []View{ViewActive, ViewAll} {
		if err := view.Validate(); err != nil {
			t.Errorf("view %q: %v", view, err)
		}
	}
	if err := View("newest").Validate(); !errors.Is(err, ErrInvalidView) {
		t.Fatalf("error = %v, want ErrInvalidView", err)
	}
	if err := (Counts{Active: -1}).Validate(); !errors.Is(err, ErrInvalidCount) {
		t.Fatalf("error = %v, want ErrInvalidCount", err)
	}
}
