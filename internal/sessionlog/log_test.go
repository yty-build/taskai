package sessionlog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestAppendReplayAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "S1.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		line, err := log.Append("req", "activity", map[string]int{"index": index})
		if err != nil || line.Seq != uint64(index+1) {
			t.Fatalf("Append() = %#v, %v", line, err)
		}
	}
	lines, err := log.ReadAfter(1, 2)
	if err != nil || len(lines) != 1 || lines[0].Seq != 2 {
		t.Fatalf("ReadAfter() = %#v, %v", lines, err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	line, err := reopened.Append("req-2", "activity", json.RawMessage(`{"ok":true}`))
	if err != nil || line.Seq != 4 {
		t.Fatalf("Append after reopen = %#v, %v", line, err)
	}
	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("log mode = %v, err = %v", info.Mode().Perm(), err)
	}
}

func TestConcurrentAppendIsMonotonic(t *testing.T) {
	log, err := Open(filepath.Join(t.TempDir(), "S1.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	const count = 50
	var wg sync.WaitGroup
	for index := 0; index < count; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			if _, err := log.Append("", "line", map[string]int{"index": index}); err != nil {
				t.Errorf("Append() error = %v", err)
			}
		}(index)
	}
	wg.Wait()
	lines, err := log.ReadAfter(0, 0)
	if err != nil || len(lines) != count {
		t.Fatalf("ReadAfter() count = %d, err = %v", len(lines), err)
	}
	for index, line := range lines {
		if line.Seq != uint64(index+1) {
			t.Fatalf("line %d sequence = %d", index, line.Seq)
		}
	}
}

func TestOpenRejectsCorruptOrNonMonotonicHistory(t *testing.T) {
	for name, body := range map[string]string{
		"corrupt":   "not-json\n",
		"duplicate": "{\"seq\":1,\"timestamp\":\"2026-01-01T00:00:00Z\",\"type\":\"x\",\"data\":{}}\n{\"seq\":1,\"timestamp\":\"2026-01-01T00:00:00Z\",\"type\":\"x\",\"data\":{}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "log.jsonl")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); err == nil {
				t.Fatal("Open() error = nil")
			}
		})
	}
}
