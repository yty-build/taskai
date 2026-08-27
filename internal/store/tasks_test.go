package store

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/task"
)

func TestTaskExactTitleNewestOrderingAndDeterministicTieBreak(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	older := fixedTime.Add(-time.Hour)
	tied := fixedTime
	createTask(t, store, "task_z_old", "old", older)
	createTask(t, store, "task_b", "duplicate", tied)
	createTask(t, store, "task_a", "duplicate", tied)
	exactTitle := "  exact\tタイトル\n\x1b[31m"
	createTask(t, store, "task_new", exactTitle, fixedTime.Add(time.Hour))

	if err := store.UpdateTaskTitle(context.Background(), "task_z_old", "updated but still old", fixedTime.Add(24*time.Hour)); err != nil {
		t.Fatal(err)
	}
	summaries, err := store.ListTasks(context.Background(), task.ViewAll)
	if err != nil {
		t.Fatal(err)
	}
	wantIDs := []task.ID{"task_new", "task_a", "task_b", "task_z_old"}
	if len(summaries) != len(wantIDs) {
		t.Fatalf("got %d Tasks, want %d", len(summaries), len(wantIDs))
	}
	for index, want := range wantIDs {
		if summaries[index].ID != want {
			t.Errorf("position %d ID = %q, want %q", index, summaries[index].ID, want)
		}
	}
	if summaries[0].Title != exactTitle {
		t.Fatalf("exact title changed: %q", summaries[0].Title)
	}
}

func TestActiveAndAllTaskViewsUseMechanicalSessionState(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	createTask(t, store, "task_empty", "empty", fixedTime.Add(2*time.Hour))
	createTask(t, store, "task_active", "active", fixedTime.Add(time.Hour))
	createTask(t, store, "task_dormant", "dormant", fixedTime)
	createManager(t, store, "manager_active", "task_active", session.StateStarting, fixedTime.Add(3*time.Hour))
	createManager(t, store, "manager_dormant", "task_dormant", session.StateClosed, fixedTime.Add(3*time.Hour))

	active, err := store.ListTasks(context.Background(), task.ViewActive)
	if err != nil {
		t.Fatal(err)
	}
	if got := summaryIDs(active); got != "task_empty,task_active" {
		t.Fatalf("active IDs = %s", got)
	}
	all, err := store.ListTasks(context.Background(), task.ViewAll)
	if err != nil {
		t.Fatal(err)
	}
	if got := summaryIDs(all); got != "task_empty,task_active,task_dormant" {
		t.Fatalf("all IDs = %s", got)
	}
}

func TestCountsIncludeManagersAndSubordinatesAcrossAllFiniteStates(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	createTask(t, store, "task_counts", "counts", fixedTime)
	starting := createManager(t, store, "manager_starting", "task_counts", session.StateStarting, fixedTime)
	createChild(t, store, "child_active", "task_counts", starting.ID, session.StateActive, fixedTime.Add(time.Second))
	createManager(t, store, "manager_dormant", "task_counts", session.StateDormant, fixedTime.Add(2*time.Second))
	createManager(t, store, "manager_interrupted", "task_counts", session.StateInterrupted, fixedTime.Add(3*time.Second))
	createManager(t, store, "manager_closed", "task_counts", session.StateClosed, fixedTime.Add(4*time.Second))

	counts, err := store.TaskCounts(context.Background(), "task_counts")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Active != 2 || counts.Dormant != 3 {
		t.Fatalf("counts = %#v, want active=2 dormant=3", counts)
	}
	summaries, err := store.ListTasks(context.Background(), task.ViewAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 || summaries[0].Counts != counts {
		t.Fatalf("summary counts = %#v, want %#v", summaries, counts)
	}

	if err := store.UpdateSessionState(context.Background(), starting.ID, session.StateDormant, fixedTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	counts, err = store.TaskCounts(context.Background(), "task_counts")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Active != 1 || counts.Dormant != 4 {
		t.Fatalf("updated counts = %#v", counts)
	}
}

func TestTaskUpdateStalenessConflictsAndNotFound(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	value := createTask(t, store, "task_one", "before", fixedTime)
	if err := store.CreateTask(context.Background(), value); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate error = %v, want ErrConflict", err)
	}
	if err := store.UpdateTaskTitle(context.Background(), value.ID, "after", fixedTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateTaskTitle(context.Background(), value.ID, "stale", fixedTime); !errors.Is(err, ErrStaleUpdate) {
		t.Fatalf("stale error = %v", err)
	}
	stored, err := store.GetTask(context.Background(), value.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Title != "after" {
		t.Fatalf("stale update changed title: %q", stored.Title)
	}
	if err := store.UpdateTaskTitle(context.Background(), "task_missing", "title", fixedTime); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing update error = %v", err)
	}
	if err := store.DeleteTask(context.Background(), "task_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing delete error = %v", err)
	}
	if _, err := store.TaskCounts(context.Background(), "task_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing counts error = %v", err)
	}
}

func TestLargeCountsRemainExactAndNonNegative(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	createTask(t, store, "task_large", "large", fixedTime)
	const managers = 128
	for index := 0; index < managers; index++ {
		state := session.StateActive
		if index%2 == 1 {
			state = session.StateDormant
		}
		createManager(t, store, session.ID(fmt.Sprintf("manager_%03d", index)), "task_large", state, fixedTime.Add(time.Duration(index)*time.Microsecond))
	}
	counts, err := store.TaskCounts(context.Background(), "task_large")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Active != managers/2 || counts.Dormant != managers/2 {
		t.Fatalf("counts = %#v", counts)
	}
}

func summaryIDs(values []task.Summary) string {
	result := ""
	for index, value := range values {
		if index > 0 {
			result += ","
		}
		result += string(value.ID)
	}
	return result
}
