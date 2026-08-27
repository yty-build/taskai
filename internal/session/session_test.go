package session

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/task"
)

func testTime() time.Time {
	return time.Date(2026, 8, 27, 12, 0, 0, 123456789, time.FixedZone("fixture", -7*60*60))
}

func TestConstructorsPreserveMetadataAndDefensivelyCopyReference(t *testing.T) {
	t.Parallel()

	reference := []byte{0, 1, 2, 0xff}
	manager, err := NewManager("session_manager", "task_one", "  manager\n", "codex", reference, StateStarting, testTime())
	if err != nil {
		t.Fatal(err)
	}
	reference[0] = 9
	if manager.ParentID != nil || manager.Role != RoleManager || manager.Label != "  manager\n" {
		t.Fatalf("unexpected manager: %#v", manager)
	}
	if manager.ProviderReference[0] != 0 {
		t.Fatal("provider reference was not copied")
	}
	wantTime := testTime().UTC().Truncate(time.Microsecond)
	if manager.CreatedAt != wantTime || manager.LastActiveAt != wantTime {
		t.Fatalf("unexpected canonical time: %#v", manager)
	}

	child, err := NewSubordinate("session_child", "task_one", manager.ID, "child", "claude", nil, StateActive, testTime())
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateHierarchy(child, manager); err != nil {
		t.Fatal(err)
	}
	cloned := child.Clone()
	*cloned.ParentID = "changed"
	cloned.ProviderReference = append(cloned.ProviderReference, 'x')
	if child.ParentID == nil || *child.ParentID != manager.ID || len(child.ProviderReference) != 0 {
		t.Fatal("Clone aliases Session pointer or BLOB metadata")
	}
}

func TestRoleParentAndSameTaskHierarchy(t *testing.T) {
	t.Parallel()

	manager, err := NewManager("manager", "task_one", "", "codex", nil, StateActive, testTime())
	if err != nil {
		t.Fatal(err)
	}
	child, err := NewSubordinate("child", "task_one", manager.ID, "", "claude", nil, StateDormant, testTime())
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name   string
		child  Session
		parent Session
	}{
		{name: "different Task", child: func() Session { value := child; value.TaskID = "task_two"; return value }(), parent: manager},
		{name: "subordinate parent", child: child, parent: func() Session {
			value := manager
			value.Role = RoleSubordinate
			parent := ID("other")
			value.ParentID = &parent
			return value
		}()},
		{name: "wrong parent ID", child: func() Session { value := child; parent := ID("other"); value.ParentID = &parent; return value }(), parent: manager},
		{name: "manager child", child: manager, parent: manager},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := ValidateHierarchy(test.child, test.parent); !errors.Is(err, ErrInvalidHierarchy) && !errors.Is(err, ErrInvalidRole) {
				t.Fatalf("error = %v, want hierarchy rejection", err)
			}
		})
	}

	invalidManager := manager
	parent := ID("other")
	invalidManager.ParentID = &parent
	if err := invalidManager.Validate(); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("manager parent error = %v", err)
	}
	invalidChild := child
	invalidChild.ParentID = nil
	if err := invalidChild.Validate(); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("child parent error = %v", err)
	}
}

func TestStateActivityClassification(t *testing.T) {
	t.Parallel()

	tests := []struct {
		state State
		want  Activity
	}{
		{StateStarting, ActivityActive},
		{StateActive, ActivityActive},
		{StateDormant, ActivityDormant},
		{StateInterrupted, ActivityDormant},
		{StateClosed, ActivityDormant},
	}
	for _, test := range tests {
		got, err := test.state.Activity()
		if err != nil || got != test.want {
			t.Errorf("Activity(%q) = %q, %v; want %q", test.state, got, err, test.want)
		}
	}
	if _, err := State("working").Activity(); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("error = %v, want ErrInvalidState", err)
	}
}

func TestStateAndReferenceUpdatesAreMonotonicAndOpaque(t *testing.T) {
	t.Parallel()

	value, err := NewManager("manager", "task_one", "", "codex", nil, StateStarting, testTime())
	if err != nil {
		t.Fatal(err)
	}
	value, err = value.WithState(StateActive, value.CreatedAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := value.WithState(StateDormant, value.CreatedAt); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("stale state error = %v", err)
	}

	reference := []byte("opaque\x00reference")
	value, err = value.WithProviderReference(reference, value.LastActiveAt.Add(time.Second))
	if err != nil {
		t.Fatal(err)
	}
	reference[0] = 'X'
	if !bytes.Equal(value.ProviderReference, []byte("opaque\x00reference")) {
		t.Fatal("opaque reference changed with caller slice")
	}
	if _, err := value.WithProviderReference([]byte("different"), value.LastActiveAt); !errors.Is(err, ErrProviderReferenceImmutable) {
		t.Fatalf("reference replacement error = %v", err)
	}
}

func TestBoundedFields(t *testing.T) {
	t.Parallel()

	if err := ValidateLabel(""); err != nil {
		t.Fatal(err)
	}
	if err := ValidateLabel(strings.Repeat("é", MaxLabelRunes)); err != nil {
		t.Fatalf("maximum label rejected: %v", err)
	}
	for _, label := range []string{" \t", "bad\x00label", strings.Repeat("x", MaxLabelRunes+1), string([]byte{0xff})} {
		if err := ValidateLabel(label); !errors.Is(err, ErrInvalidLabel) {
			t.Errorf("ValidateLabel(%q) error = %v", label, err)
		}
	}
	for _, provider := range []string{"", "contains space", strings.Repeat("p", MaxProviderIDBytes+1)} {
		if err := ValidateProviderID(provider); !errors.Is(err, ErrInvalidProvider) {
			t.Errorf("ValidateProviderID(%q) error = %v", provider, err)
		}
	}
	if err := ValidateProviderReference(make([]byte, MaxProviderReferenceBytes+1)); !errors.Is(err, ErrInvalidProviderReference) {
		t.Fatalf("reference error = %v", err)
	}
	if err := ValidateProviderReference(make([]byte, MaxProviderReferenceBytes)); err != nil {
		t.Fatalf("maximum reference rejected: %v", err)
	}
	if _, err := NewSubordinate("same", task.ID("task_one"), "same", "", "codex", nil, StateStarting, testTime()); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("self-parent error = %v", err)
	}
}

func TestGeneratedID(t *testing.T) {
	t.Parallel()

	id, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	if err := id.Validate(); err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseID("session:parsed")
	if err != nil || parsed != "session:parsed" {
		t.Fatalf("ParseID = %q, %v", parsed, err)
	}
	if _, err := ParseID("bad/session"); !errors.Is(err, ErrInvalidID) {
		t.Fatalf("invalid ParseID error = %v", err)
	}
}

func TestSessionValidationRejectsInvalidRoleAndTime(t *testing.T) {
	t.Parallel()

	value, err := NewManager("manager", "task_one", "", "codex", nil, StateStarting, testTime())
	if err != nil {
		t.Fatal(err)
	}
	invalidRole := value
	invalidRole.Role = Role("coordinator")
	if err := invalidRole.Validate(); !errors.Is(err, ErrInvalidRole) {
		t.Fatalf("invalid role error = %v", err)
	}
	invalidTime := value
	invalidTime.LastActiveAt = invalidTime.CreatedAt.Add(-time.Microsecond)
	if err := invalidTime.Validate(); !errors.Is(err, ErrInvalidTime) {
		t.Fatalf("invalid time error = %v", err)
	}
	if _, err := value.WithState(State("unknown"), value.LastActiveAt); !errors.Is(err, ErrInvalidState) {
		t.Fatalf("invalid WithState error = %v", err)
	}
	if _, err := value.WithProviderReference(nil, value.LastActiveAt); !errors.Is(err, ErrInvalidProviderReference) {
		t.Fatalf("empty reference error = %v", err)
	}
}
