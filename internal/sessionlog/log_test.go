package sessionlog

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestWalkFileStreamsAndStopsAtConsumerError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "S-stream.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 3; index++ {
		if _, err := log.Append("req", "activity", map[string]int{"index": index}); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop replay")
	var seen []uint64
	last, err := WalkFile(context.Background(), path, 0, 0, func(line Line) error {
		seen = append(seen, line.Seq)
		if line.Seq == 2 {
			return stop
		}
		return nil
	})
	if !errors.Is(err, stop) || last != 1 || len(seen) != 2 {
		t.Fatalf("WalkFile() = last %d seen %v err %v", last, seen, err)
	}
	seen = nil
	last, err = WalkFile(context.Background(), path, 1, 2, func(line Line) error {
		seen = append(seen, line.Seq)
		return nil
	})
	if err != nil || last != 2 || len(seen) != 1 || seen[0] != 2 {
		t.Fatalf("bounded WalkFile() = last %d seen %v err %v", last, seen, err)
	}
}

func TestReplayCursorDiscoversAppendedLinesWithoutRetainingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "S-cursor.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append("req-1", "activity", map[string]int{"index": 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append("req-2", "activity", map[string]int{"index": 2}); err != nil {
		t.Fatal(err)
	}
	reader := NewReplayCursor(path, 0, 0)
	line, ok, err := reader.Next(context.Background())
	if err != nil || !ok || line.Seq != 1 {
		t.Fatalf("first Next() = %#v, %v, %v", line, ok, err)
	}
	activeFile, activeScanner := reader.file, reader.scanner
	buffer := &reader.buffer[0]
	line, ok, err = reader.Next(context.Background())
	if err != nil || !ok || line.Seq != 2 {
		t.Fatalf("second Next() = %#v, %v, %v", line, ok, err)
	}
	if reader.file != activeFile || reader.scanner != activeScanner || &reader.buffer[0] != buffer {
		t.Fatal("ReplayCursor reopened or reallocated its reader between records")
	}
	if _, ok, err := reader.Next(context.Background()); err != nil || ok {
		t.Fatalf("Next() at EOF = %v, %v", ok, err)
	}
	if reader.file != nil || reader.scanner != nil {
		t.Fatal("ReplayCursor retained its file after EOF")
	}
	if _, err := log.Append("req-3", "activity", map[string]int{"index": 3}); err != nil {
		t.Fatal(err)
	}
	line, ok, err = reader.Next(context.Background())
	if err != nil || !ok || line.Seq != 3 {
		t.Fatalf("appended Next() = %#v, %v, %v", line, ok, err)
	}
	if &reader.buffer[0] != buffer {
		t.Fatal("ReplayCursor did not reuse its buffer after appended history")
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestReplayCursorDropsGrownScannerBeforeReturningLaterHead(t *testing.T) {
	path := filepath.Join(t.TempDir(), "S-large-cursor.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, err := log.Append("req-large", "activity", map[string]string{
		"content": strings.Repeat("x", replayCursorMaxRetainedTokenSize*2),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append("req-small", "activity", map[string]string{"content": "small"}); err != nil {
		t.Fatal(err)
	}

	// Skip the large record, then return a tiny head. The scanner grew while
	// finding that head, so it must not remain pinned on the cursor.
	reader := NewReplayCursor(path, 1, 0)
	line, ok, err := reader.Next(context.Background())
	if err != nil || !ok || line.Seq != 2 {
		t.Fatalf("Next() = %#v, %v, %v", line, ok, err)
	}
	if reader.file != nil || reader.scanner != nil || reader.oversized {
		t.Fatal("ReplayCursor retained a scanner that had grown for an older large record")
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
