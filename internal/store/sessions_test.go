package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/session"
)

func TestSessionHierarchyRequiresSameTaskTopLevelManager(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	createTask(t, store, "task_one", "one", fixedTime)
	createTask(t, store, "task_two", "two", fixedTime)
	managerOne := createManager(t, store, "manager_one", "task_one", session.StateActive, fixedTime)
	managerTwo := createManager(t, store, "manager_two", "task_two", session.StateActive, fixedTime)
	child := createChild(t, store, "child_one", "task_one", managerOne.ID, session.StateStarting, fixedTime.Add(time.Second))

	crossTask, err := session.NewSubordinate(
		"child_cross", "task_one", managerTwo.ID, "cross", "codex", nil,
		session.StateStarting, fixedTime.Add(2*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(context.Background(), crossTask); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("cross-Task child error = %v, want ErrInvalidHierarchy", err)
	}

	depthTwo, err := session.NewSubordinate(
		"child_depth_two", "task_one", child.ID, "depth two", "codex", nil,
		session.StateStarting, fixedTime.Add(3*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(context.Background(), depthTwo); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("depth-two child error = %v, want ErrInvalidHierarchy", err)
	}

	missingParent, err := session.NewSubordinate(
		"child_missing", "task_one", "manager_missing", "missing", "codex", nil,
		session.StateStarting, fixedTime.Add(4*time.Second),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(context.Background(), missingParent); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("missing parent error = %v, want ErrInvalidHierarchy", err)
	}
	if _, err := store.GetSession(context.Background(), "child_cross"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rejected child was persisted: %v", err)
	}
}

func TestSessionSchemaEnforcesHierarchyAndImmutableIdentity(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	createTask(t, store, "task_one", "one", fixedTime)
	createTask(t, store, "task_two", "two", fixedTime)
	manager := createManager(t, store, "manager", "task_one", session.StateActive, fixedTime)
	child := createChild(t, store, "child", "task_one", manager.ID, session.StateActive, fixedTime.Add(time.Second))

	for name, statement := range map[string]string{
		"role":     `UPDATE sessions SET role = 'manager', parent_session_id = NULL WHERE id = 'child'`,
		"parent":   `UPDATE sessions SET parent_session_id = 'child' WHERE id = 'child'`,
		"task":     `UPDATE sessions SET task_id = 'task_two' WHERE id = 'child'`,
		"provider": `UPDATE sessions SET provider_id = 'other' WHERE id = 'child'`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := store.db.ExecContext(context.Background(), statement); err == nil {
				t.Fatal("immutable identity update unexpectedly succeeded")
			}
		})
	}
	stored, err := store.GetSession(context.Background(), child.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Role != session.RoleSubordinate || stored.ParentID == nil || *stored.ParentID != manager.ID ||
		stored.TaskID != "task_one" || stored.ProviderID != "claude" {
		t.Fatalf("identity changed after rejected writes: %#v", stored)
	}
}

func TestSessionListingIsDeterministicAndHierarchical(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	createTask(t, store, "task_order", "order", fixedTime)
	managerB := createManager(t, store, "manager_b", "task_order", session.StateActive, fixedTime)
	managerA := createManager(t, store, "manager_a", "task_order", session.StateActive, fixedTime)
	createChild(t, store, "child_z", "task_order", managerA.ID, session.StateDormant, fixedTime.Add(time.Second))
	createChild(t, store, "child_a", "task_order", managerA.ID, session.StateDormant, fixedTime.Add(time.Second))
	createChild(t, store, "child_b", "task_order", managerB.ID, session.StateDormant, fixedTime.Add(2*time.Second))

	values, err := store.ListSessions(context.Background(), "task_order")
	if err != nil {
		t.Fatal(err)
	}
	want := []session.ID{"manager_a", "child_a", "child_z", "manager_b", "child_b"}
	if len(values) != len(want) {
		t.Fatalf("got %d Sessions, want %d", len(values), len(want))
	}
	for index := range want {
		if values[index].ID != want[index] {
			t.Errorf("position %d ID = %q, want %q", index, values[index].ID, want[index])
		}
	}
	if _, err := store.ListSessions(context.Background(), "task_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing Task error = %v", err)
	}
}

func TestOpaqueProviderReferenceIsExactDefensiveAndSetOnce(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	createTask(t, store, "task_ref", "reference", fixedTime)
	manager := createManager(t, store, "manager_ref", "task_ref", session.StateStarting, fixedTime)
	reference := []byte{0x00, 0xff, 0x01, 'x', 0x00}
	if err := store.SetProviderReference(context.Background(), manager.ID, reference, fixedTime.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	reference[1] = 0x00
	stored, err := store.GetSession(context.Background(), manager.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []byte{0x00, 0xff, 0x01, 'x', 0x00}
	if !bytes.Equal(stored.ProviderReference, want) {
		t.Fatalf("reference = %x, want %x", stored.ProviderReference, want)
	}
	stored.ProviderReference[0] = 0x7f
	again, err := store.GetSession(context.Background(), manager.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(again.ProviderReference, want) {
		t.Fatalf("returned reference aliases database value: %x", again.ProviderReference)
	}
	if err := store.SetProviderReference(context.Background(), manager.ID, want, fixedTime.Add(2*time.Second)); err != nil {
		t.Fatalf("idempotent repeat: %v", err)
	}
	if err := store.SetProviderReference(context.Background(), manager.ID, []byte("different"), fixedTime.Add(3*time.Second)); !errors.Is(err, ErrProviderReferenceImmutable) {
		t.Fatalf("replacement error = %v, want ErrProviderReferenceImmutable", err)
	}
}

func TestSessionStateUsesFiniteValuesAndMonotonicTime(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	createTask(t, store, "task_state", "state", fixedTime)
	manager := createManager(t, store, "manager_state", "task_state", session.StateStarting, fixedTime)
	if err := store.UpdateSessionState(context.Background(), manager.ID, session.StateActive, fixedTime.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	if err := store.UpdateSessionState(context.Background(), manager.ID, session.StateClosed, fixedTime); !errors.Is(err, ErrStaleUpdate) {
		t.Fatalf("stale update error = %v", err)
	}
	if err := store.UpdateSessionState(context.Background(), manager.ID, session.State("invented"), fixedTime.Add(time.Hour)); !errors.Is(err, session.ErrInvalidState) {
		t.Fatalf("invalid state error = %v", err)
	}
	stored, err := store.GetSession(context.Background(), manager.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.State != session.StateActive || !stored.LastActiveAt.Equal(fixedTime.Add(time.Minute)) {
		t.Fatalf("stored state after rejected update = %#v", stored)
	}
}

func TestUpdateOwnedTreeStateUpdatesOnlyExactManagerTree(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	providerState := filepath.Join(root, "provider-owned-state")
	providerBytes := []byte("provider bytes remain outside metadata ownership")
	if err := os.WriteFile(providerState, providerBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), filepath.Join(root, "taskai-state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	createTask(t, store, "task_tree", "tree", fixedTime)
	taskBefore, err := store.GetTask(context.Background(), "task_tree")
	if err != nil {
		t.Fatal(err)
	}
	target := createManager(t, store, "manager_target", "task_tree", session.StateStarting, fixedTime)
	childOne := createChild(t, store, "child_target_one", "task_tree", target.ID, session.StateActive, fixedTime.Add(time.Second))
	childTwo := createChild(t, store, "child_target_two", "task_tree", target.ID, session.StateDormant, fixedTime.Add(2*time.Second))
	sibling := createManager(t, store, "manager_sibling", "task_tree", session.StateActive, fixedTime.Add(3*time.Second))
	siblingChild := createChild(t, store, "child_sibling", "task_tree", sibling.ID, session.StateClosed, fixedTime.Add(4*time.Second))
	siblingBefore, err := store.GetSession(context.Background(), sibling.ID)
	if err != nil {
		t.Fatal(err)
	}
	siblingChildBefore, err := store.GetSession(context.Background(), siblingChild.ID)
	if err != nil {
		t.Fatal(err)
	}

	updatedAt := fixedTime.Add(time.Hour)
	if err := store.UpdateOwnedTreeState(context.Background(), target.ID, session.StateInterrupted, updatedAt); err != nil {
		t.Fatal(err)
	}
	for _, id := range []session.ID{target.ID, childOne.ID, childTwo.ID} {
		stored, err := store.GetSession(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if stored.State != session.StateInterrupted || !stored.LastActiveAt.Equal(updatedAt) {
			t.Errorf("owned Session %q = state %q at %v", id, stored.State, stored.LastActiveAt)
		}
	}
	if stored, err := store.GetSession(context.Background(), sibling.ID); err != nil || !sameSessionMetadata(stored, siblingBefore) {
		t.Fatalf("sibling manager changed: %#v, %v", stored, err)
	}
	if stored, err := store.GetSession(context.Background(), siblingChild.ID); err != nil || !sameSessionMetadata(stored, siblingChildBefore) {
		t.Fatalf("sibling child changed: %#v, %v", stored, err)
	}
	if taskAfter, err := store.GetTask(context.Background(), "task_tree"); err != nil || taskAfter != taskBefore {
		t.Fatalf("Task metadata changed: %#v, %v", taskAfter, err)
	}
	if got, err := os.ReadFile(providerState); err != nil || !bytes.Equal(got, providerBytes) {
		t.Fatalf("provider-owned state changed: %q, %v", got, err)
	}
}

func TestUpdateOwnedTreeStateRollsBackWholeTreeOnOneStaleMember(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	createTask(t, store, "task_stale_tree", "stale tree", fixedTime)
	manager := createManager(t, store, "manager_stale_tree", "task_stale_tree", session.StateStarting, fixedTime)
	freshChild := createChild(t, store, "child_fresh", "task_stale_tree", manager.ID, session.StateActive, fixedTime.Add(time.Second))
	staleChild := createChild(t, store, "child_newer", "task_stale_tree", manager.ID, session.StateActive, fixedTime.Add(10*time.Minute))
	managerBefore := loadSession(t, store, manager.ID)
	freshBefore := loadSession(t, store, freshChild.ID)
	staleBefore := loadSession(t, store, staleChild.ID)

	err := store.UpdateOwnedTreeState(
		context.Background(), manager.ID, session.StateClosed, fixedTime.Add(5*time.Minute),
	)
	if !errors.Is(err, ErrStaleUpdate) {
		t.Fatalf("stale tree error = %v, want ErrStaleUpdate", err)
	}
	for id, want := range map[session.ID]session.Session{
		manager.ID:    managerBefore,
		freshChild.ID: freshBefore,
		staleChild.ID: staleBefore,
	} {
		got, err := store.GetSession(context.Background(), id)
		if err != nil {
			t.Fatal(err)
		}
		if !sameSessionMetadata(got, want) {
			t.Errorf("Session %q changed despite rollback: got %#v want %#v", id, got, want)
		}
	}
}

func TestUpdateOwnedTreeStateIsolatesOtherTasks(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	createTask(t, store, "task_target", "target", fixedTime)
	createTask(t, store, "task_other", "other", fixedTime.Add(time.Second))
	target := createManager(t, store, "manager_target_task", "task_target", session.StateActive, fixedTime)
	createChild(t, store, "child_target_task", "task_target", target.ID, session.StateActive, fixedTime)
	other := createManager(t, store, "manager_other_task", "task_other", session.StateDormant, fixedTime.Add(time.Second))
	otherChild := createChild(t, store, "child_other_task", "task_other", other.ID, session.StateClosed, fixedTime.Add(2*time.Second))
	otherBefore := loadSession(t, store, other.ID)
	otherChildBefore := loadSession(t, store, otherChild.ID)
	otherTaskBefore, err := store.GetTask(context.Background(), "task_other")
	if err != nil {
		t.Fatal(err)
	}

	if err := store.UpdateOwnedTreeState(context.Background(), target.ID, session.StateInterrupted, fixedTime.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got, err := store.GetSession(context.Background(), other.ID); err != nil || !sameSessionMetadata(got, otherBefore) {
		t.Fatalf("cross-Task manager changed: %#v, %v", got, err)
	}
	if got, err := store.GetSession(context.Background(), otherChild.ID); err != nil || !sameSessionMetadata(got, otherChildBefore) {
		t.Fatalf("cross-Task child changed: %#v, %v", got, err)
	}
	if got, err := store.GetTask(context.Background(), "task_other"); err != nil || got != otherTaskBefore {
		t.Fatalf("cross-Task Task changed: %#v, %v", got, err)
	}
}

func TestUpdateOwnedTreeStateRequiresTopLevelManagerAndFiniteInputs(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	createTask(t, store, "task_tree_inputs", "inputs", fixedTime)
	manager := createManager(t, store, "manager_tree_inputs", "task_tree_inputs", session.StateActive, fixedTime)
	child := createChild(t, store, "child_tree_inputs", "task_tree_inputs", manager.ID, session.StateActive, fixedTime)
	managerBefore := loadSession(t, store, manager.ID)
	childBefore := loadSession(t, store, child.ID)

	if err := store.UpdateOwnedTreeState(context.Background(), child.ID, session.StateClosed, fixedTime.Add(time.Hour)); !errors.Is(err, ErrInvalidHierarchy) {
		t.Fatalf("subordinate target error = %v, want ErrInvalidHierarchy", err)
	}
	if err := store.UpdateOwnedTreeState(context.Background(), "manager_missing", session.StateClosed, fixedTime.Add(time.Hour)); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing manager error = %v, want ErrNotFound", err)
	}
	if err := store.UpdateOwnedTreeState(context.Background(), manager.ID, session.State("invented"), fixedTime.Add(time.Hour)); !errors.Is(err, session.ErrInvalidState) {
		t.Fatalf("invalid state error = %v", err)
	}
	if err := store.UpdateOwnedTreeState(context.Background(), manager.ID, session.StateClosed, time.Time{}); !errors.Is(err, session.ErrInvalidTime) {
		t.Fatalf("zero time error = %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := store.UpdateOwnedTreeState(ctx, manager.ID, session.StateClosed, fixedTime.Add(time.Hour)); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled tree update error = %v", err)
	}
	if got := loadSession(t, store, manager.ID); !sameSessionMetadata(got, managerBefore) {
		t.Fatalf("manager changed after rejected input: %#v", got)
	}
	if got := loadSession(t, store, child.ID); !sameSessionMetadata(got, childBefore) {
		t.Fatalf("child changed after rejected input: %#v", got)
	}
}

func TestMetadataDeletionNeverTouchesProviderState(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	providerState := filepath.Join(root, "provider-owned-state")
	providerBytes := []byte("opaque provider bytes that TaskAI must never delete")
	if err := os.WriteFile(providerState, providerBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), filepath.Join(root, "taskai-state"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	createTask(t, store, "task_delete", "delete", fixedTime)
	manager := createManager(t, store, "manager_delete", "task_delete", session.StateClosed, fixedTime)
	createChild(t, store, "child_delete", "task_delete", manager.ID, session.StateClosed, fixedTime)

	if err := store.DeleteSession(context.Background(), manager.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSession(context.Background(), "child_delete"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("child metadata did not cascade: %v", err)
	}
	if got, err := os.ReadFile(providerState); err != nil || !bytes.Equal(got, providerBytes) {
		t.Fatalf("provider state changed after Session delete: %q, %v", got, err)
	}

	createManager(t, store, "manager_delete_two", "task_delete", session.StateClosed, fixedTime)
	if err := store.DeleteTask(context.Background(), "task_delete"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GetSession(context.Background(), "manager_delete_two"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Task delete did not cascade metadata: %v", err)
	}
	if got, err := os.ReadFile(providerState); err != nil || !bytes.Equal(got, providerBytes) {
		t.Fatalf("provider state changed after Task delete: %q, %v", got, err)
	}
}

func sameSessionMetadata(left, right session.Session) bool {
	if left.ID != right.ID || left.TaskID != right.TaskID || left.Role != right.Role ||
		left.Label != right.Label || left.ProviderID != right.ProviderID || left.State != right.State ||
		!left.CreatedAt.Equal(right.CreatedAt) || !left.LastActiveAt.Equal(right.LastActiveAt) ||
		!bytes.Equal(left.ProviderReference, right.ProviderReference) {
		return false
	}
	if left.ParentID == nil || right.ParentID == nil {
		return left.ParentID == nil && right.ParentID == nil
	}
	return *left.ParentID == *right.ParentID
}

func loadSession(t *testing.T, store *Store, id session.ID) session.Session {
	t.Helper()
	value, err := store.GetSession(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	return value
}
