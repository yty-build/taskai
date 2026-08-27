package store

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/session"
	"github.com/yty-build/acp-gateway/internal/task"
)

func TestConcurrentTaskWritesAndReadsRemainComplete(t *testing.T) {
	store := openTestStore(t)
	const workers = 48
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var wait sync.WaitGroup
	for index := 0; index < workers; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			id := task.ID(fmt.Sprintf("task_concurrent_%02d", index))
			value, err := task.New(id, fmt.Sprintf("title %02d", index), fixedTime.Add(time.Duration(index)*time.Microsecond))
			if err == nil {
				err = store.CreateTask(context.Background(), value)
			}
			if err == nil {
				_, err = store.GetTask(context.Background(), id)
			}
			errorsByWorker <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil {
			t.Errorf("concurrent worker: %v", err)
		}
	}
	values, err := store.ListTasks(context.Background(), task.ViewAll)
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != workers {
		t.Fatalf("got %d Tasks, want %d", len(values), workers)
	}
	for index := 1; index < len(values); index++ {
		if !values[index-1].CreatedAt.After(values[index].CreatedAt) {
			t.Fatalf("creation order not descending at %d: %v then %v", index, values[index-1].CreatedAt, values[index].CreatedAt)
		}
	}
}

func TestConcurrentSessionUpdatesConvergeOnNewestMechanicalTime(t *testing.T) {
	store := openTestStore(t)
	createTask(t, store, "task_updates", "updates", fixedTime)
	manager := createManager(t, store, "manager_updates", "task_updates", session.StateStarting, fixedTime)
	const workers = 64
	start := make(chan struct{})
	errorsByWorker := make(chan error, workers)
	var wait sync.WaitGroup
	for index := 1; index <= workers; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			state := session.StateActive
			if index == workers {
				state = session.StateClosed
			}
			err := store.UpdateSessionState(
				context.Background(), manager.ID, state,
				fixedTime.Add(time.Duration(index)*time.Microsecond),
			)
			errorsByWorker <- err
		}()
	}
	close(start)
	wait.Wait()
	close(errorsByWorker)
	for err := range errorsByWorker {
		if err != nil && !errors.Is(err, ErrStaleUpdate) {
			t.Errorf("concurrent state update: %v", err)
		}
	}
	stored, err := store.GetSession(context.Background(), manager.ID)
	if err != nil {
		t.Fatal(err)
	}
	wantTime := fixedTime.Add(workers * time.Microsecond)
	if !stored.LastActiveAt.Equal(wantTime) || stored.State != session.StateClosed {
		t.Fatalf("final Session = state %q at %v, want closed at %v", stored.State, stored.LastActiveAt, wantTime)
	}
}

func TestConcurrentCountReadsNeverExposePartialOrNegativeAggregates(t *testing.T) {
	store := openTestStore(t)
	createTask(t, store, "task_count_race", "count race", fixedTime)
	const sessionCount = 24
	for index := 0; index < sessionCount; index++ {
		createManager(
			t, store, session.ID(fmt.Sprintf("manager_count_%02d", index)),
			"task_count_race", session.StateStarting,
			fixedTime.Add(time.Duration(index)*time.Microsecond),
		)
	}

	start := make(chan struct{})
	done := make(chan struct{})
	writeErrors := make(chan error, sessionCount)
	readErrors := make(chan error, 1)
	go func() {
		<-start
		for {
			counts, err := store.TaskCounts(context.Background(), "task_count_race")
			if err != nil {
				readErrors <- err
				return
			}
			if counts.Active < 0 || counts.Dormant < 0 || counts.Active+counts.Dormant != sessionCount {
				readErrors <- fmt.Errorf("partial counts: %#v", counts)
				return
			}
			select {
			case <-done:
				readErrors <- nil
				return
			default:
			}
		}
	}()
	var wait sync.WaitGroup
	for index := 0; index < sessionCount; index++ {
		index := index
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			writeErrors <- store.UpdateSessionState(
				context.Background(), session.ID(fmt.Sprintf("manager_count_%02d", index)),
				session.StateDormant, fixedTime.Add(time.Hour+time.Duration(index)*time.Microsecond),
			)
		}()
	}
	close(start)
	wait.Wait()
	close(done)
	close(writeErrors)
	for err := range writeErrors {
		if err != nil {
			t.Errorf("state update: %v", err)
		}
	}
	if err := <-readErrors; err != nil {
		t.Fatal(err)
	}
	counts, err := store.TaskCounts(context.Background(), "task_count_race")
	if err != nil {
		t.Fatal(err)
	}
	if counts.Active != 0 || counts.Dormant != sessionCount {
		t.Fatalf("final counts = %#v", counts)
	}
}

func TestBusyWriterHonorsContextDeadlineAndRecovers(t *testing.T) {
	store := openTestStore(t)
	locker := openRawDatabase(t, store.Path())
	defer locker.Close()
	tx, err := locker.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	blocked, err := task.New("task_blocked", "blocked", fixedTime)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = store.CreateTask(ctx, blocked)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("busy write error = %v, want context deadline", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("context cancellation took %v", elapsed)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}

	createTask(t, store, "task_recovered", "recovered", fixedTime)
	if _, err := store.GetTask(context.Background(), "task_recovered"); err != nil {
		t.Fatalf("store did not recover after contention: %v", err)
	}
}

func TestCancelledContextsDoNotMutateMetadata(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	value, err := task.New("task_cancelled", "cancelled", fixedTime)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateTask(ctx, value); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled write error = %v", err)
	}
	if _, err := store.GetTask(context.Background(), value.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("cancelled write persisted metadata: %v", err)
	}
	if _, err := store.ListTasks(ctx, task.ViewAll); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled read error = %v", err)
	}
}

func TestConflictingCreateRollsBackWithoutChangingOriginal(t *testing.T) {
	t.Parallel()

	store := openTestStore(t)
	createTask(t, store, "task_rollback", "rollback", fixedTime)
	original := createManager(t, store, "manager_rollback", "task_rollback", session.StateActive, fixedTime)
	conflict, err := session.NewManager(
		original.ID, original.TaskID, "replacement", "other-provider", []byte("other-reference"),
		session.StateClosed, fixedTime.Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CreateSession(context.Background(), conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting create error = %v, want ErrConflict", err)
	}
	stored, err := store.GetSession(context.Background(), original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Label != original.Label || stored.ProviderID != original.ProviderID || stored.State != original.State || len(stored.ProviderReference) != 0 {
		t.Fatalf("conflicting transaction changed original: %#v", stored)
	}
}
