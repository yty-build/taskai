package sessionlog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestBufferedProviderFrameIsVisibleAndFlushedByDurableBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "S-buffered.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if line, err := log.AppendBuffered("REQ-1", "provider_event", map[string]string{"text": "first token"}); err != nil || line.Seq != 1 {
		t.Fatalf("AppendBuffered() = %#v, %v", line, err)
	}
	if line, err := log.Append("REQ-1", "provider_result", map[string]string{"status": "completed"}); err != nil || line.Seq != 2 {
		t.Fatalf("durable Append() = %#v, %v", line, err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	lines, err := ReadFile(path, 0)
	if err != nil || len(lines) != 2 || lines[0].Type != "provider_event" || lines[1].Type != "provider_result" {
		t.Fatalf("durable replay = %#v, %v", lines, err)
	}
}

func TestAppendRejectsRecordThatReplayCouldNotBound(t *testing.T) {
	log, err := Open(filepath.Join(t.TempDir(), "S-oversized.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, err := log.AppendBuffered("REQ-large", "provider_event", map[string]string{"text": strings.Repeat("x", maxLineSize)}); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversized AppendBuffered() error = %v", err)
	}
	line, err := log.Append("REQ-valid", "provider_result", map[string]string{"status": "completed"})
	if err != nil || line.Seq != 1 {
		t.Fatalf("valid Append() after rejection = %#v, %v", line, err)
	}
}

func TestAppendAndReplayRecordAtExactSizeBoundary(t *testing.T) {
	path := filepath.Join(t.TempDir(), "S-boundary.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	log.now = func() time.Time { return fixed }
	raw, err := marshalData(map[string]string{"text": ""})
	if err != nil {
		t.Fatal(err)
	}
	base, err := json.Marshal(Line{Seq: 1, Timestamp: fixed, RequestID: "REQ-boundary", Type: "provider_event", Data: raw})
	if err != nil {
		t.Fatal(err)
	}
	textBytes := maxLineSize - len(base)
	if textBytes <= 0 {
		t.Fatalf("boundary fixture overhead = %d", len(base))
	}
	data := map[string]string{"text": strings.Repeat("x", textBytes)}
	line, err := log.AppendBuffered("REQ-boundary", "provider_event", data)
	if err != nil || line.Seq != 1 {
		t.Fatalf("exact-boundary AppendBuffered() = %#v, %v", line, err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := len(contents) - 1; got != maxLineSize {
		t.Fatalf("encoded record size = %d, want %d", got, maxLineSize)
	}
	lines, err := ReadFile(path, 0)
	if err != nil || len(lines) != 1 || lines[0].Seq != 1 {
		t.Fatalf("exact-boundary replay = %#v, %v", lines, err)
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

func TestWalkAfterDoesNotBlockAppendAndKeepsSnapshotCutoff(t *testing.T) {
	path := filepath.Join(t.TempDir(), "S-live-replay.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	for index := 0; index < 3; index++ {
		if _, err := log.Append("req", "activity", map[string]int{"index": index}); err != nil {
			t.Fatal(err)
		}
	}

	replayStarted := make(chan struct{})
	continueReplay := make(chan struct{})
	var releaseReplayOnce sync.Once
	releaseReplay := func() { releaseReplayOnce.Do(func() { close(continueReplay) }) }
	defer releaseReplay()
	replayDone := make(chan error, 1)
	var seen []uint64
	go func() {
		_, err := log.WalkAfter(context.Background(), 0, 0, func(line Line) error {
			seen = append(seen, line.Seq)
			if line.Seq == 1 {
				close(replayStarted)
				<-continueReplay
			}
			return nil
		})
		replayDone <- err
	}()
	select {
	case <-replayStarted:
	case err := <-replayDone:
		t.Fatalf("WalkAfter() ended before replay began: %v", err)
	case <-time.After(2 * time.Second):
		t.Fatal("WalkAfter() did not begin replay")
	}

	appendDone := make(chan error, 1)
	go func() {
		line, err := log.Append("req-live", "activity", map[string]int{"index": 3})
		if err == nil && line.Seq != 4 {
			err = errors.New("live append did not receive position 4")
		}
		appendDone <- err
	}()
	select {
	case err := <-appendDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Append blocked behind the replay consumer")
	}

	releaseReplay()
	if err := <-replayDone; err != nil {
		t.Fatal(err)
	}
	if got, want := seen, []uint64{1, 2, 3}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("WalkAfter() positions = %v, want %v", got, want)
	}

	lines, err := log.ReadAfter(3, 0)
	if err != nil || len(lines) != 1 || lines[0].Seq != 4 {
		t.Fatalf("later snapshot = %#v, %v", lines, err)
	}
}

func TestLiveViewerSnapshotsDoNotMultiplyDurabilitySyncs(t *testing.T) {
	log, err := Open(filepath.Join(t.TempDir(), "S-live-sync.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	originalSync := log.syncFile
	syncs := 0
	log.syncFile = func() error {
		syncs++
		return originalSync()
	}
	if _, err := log.AppendBuffered("REQ-1", "provider_event", map[string]string{"text": "delta"}); err != nil {
		t.Fatal(err)
	}
	for viewer := 0; viewer < 8; viewer++ {
		if _, err := log.WalkLiveAfter(context.Background(), 0, 1, func(Line) error { return nil }); err != nil {
			t.Fatalf("WalkLiveAfter(viewer %d) = %v", viewer, err)
		}
	}
	if syncs != 0 {
		t.Fatalf("live viewers forced %d durability syncs", syncs)
	}
	if _, err := log.WalkAfter(context.Background(), 0, 1, func(Line) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if syncs != 1 {
		t.Fatalf("durable WalkAfter syncs = %d, want 1", syncs)
	}
}

func TestDurableAppendSyncFailurePoisonsWriterWithoutReusingSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "S-sync-failure.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	originalSync := log.syncFile
	log.syncFile = func() error { return errors.New("injected sync failure") }
	if line, err := log.Append("REQ-1", "client_command", map[string]string{"text": "first"}); err == nil || line.Seq != 0 || !strings.Contains(err.Error(), "injected sync failure") {
		t.Fatalf("failed durable Append() = %#v, %v", line, err)
	}
	if got := log.LastSequence(); got != 1 {
		t.Fatalf("sequence after complete-but-unsynced append = %d, want 1", got)
	}
	if _, err := log.Append("REQ-2", "client_command", map[string]string{"text": "must not append"}); err == nil || !strings.Contains(err.Error(), "earlier write failure") {
		t.Fatalf("Append() after poison error = %v", err)
	}
	log.syncFile = originalSync
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	lines, err := ReadFile(path, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(lines) != 1 || lines[0].Seq != 1 || lines[0].RequestID != "REQ-1" {
		t.Fatalf("poisoned log contents = %#v", lines)
	}
	reopened, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	line, err := reopened.Append("REQ-3", "worker_error", map[string]string{"code": "recovered"})
	if err != nil || line.Seq != 2 {
		t.Fatalf("Append() after reopen = %#v, %v", line, err)
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

func TestReplayCursorWaitsForNewlineBeforeDeliveringAppendedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "S-partial-cursor.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append("req-1", "activity", map[string]int{"index": 1}); err != nil {
		t.Fatal(err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	reader := NewReplayCursor(path, 0, 0)
	line, ok, err := reader.Next(context.Background())
	if err != nil || !ok || line.Seq != 1 {
		t.Fatalf("first Next() = %#v, %v, %v", line, ok, err)
	}
	completeOffset := reader.offset

	appended := Line{
		Seq:       2,
		Timestamp: time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC),
		RequestID: "req-2",
		Type:      "activity",
		Data:      json.RawMessage(`{"index":2}`),
	}
	encoded, err := json.Marshal(appended)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Close()
	if _, err := writer.Write(encoded); err != nil {
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatal(err)
	}

	if line, ok, err := reader.Next(context.Background()); err != nil || ok {
		t.Fatalf("unterminated Next() = %#v, %v, %v", line, ok, err)
	}
	if reader.offset != completeOffset || reader.previous != 1 || reader.cursor != 1 {
		t.Fatalf("unterminated record advanced cursor: offset=%d previous=%d cursor=%d", reader.offset, reader.previous, reader.cursor)
	}

	if _, err := writer.Write([]byte{'\n'}); err != nil {
		t.Fatal(err)
	}
	if err := writer.Sync(); err != nil {
		t.Fatal(err)
	}
	line, ok, err = reader.Next(context.Background())
	if err != nil || !ok || line.Seq != 2 || line.RequestID != "req-2" {
		t.Fatalf("completed Next() = %#v, %v, %v", line, ok, err)
	}
	if _, ok, err := reader.Next(context.Background()); err != nil || ok {
		t.Fatalf("Next() after completed record = %v, %v", ok, err)
	}
}

func TestReplayCursorSkipsLargePriorRecordWithoutGrowingScanner(t *testing.T) {
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

	// A near-tail cursor seeks directly past the old large record, so the active
	// scanner remains on the small record instead of allocating for history the
	// viewer did not request.
	reader := NewReplayCursor(path, 1, 0)
	line, ok, err := reader.Next(context.Background())
	if err != nil || !ok || line.Seq != 2 {
		t.Fatalf("Next() = %#v, %v, %v", line, ok, err)
	}
	if reader.file == nil || reader.scanner == nil || reader.oversized {
		t.Fatal("ReplayCursor did not use the bounded near-tail replay offset")
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

func TestOpenPreservesAndSegmentsInvalidFinalRecord(t *testing.T) {
	for name, body := range map[string]string{
		"corrupt":       "not-json\n",
		"unterminated":  "{\"seq\":1,\"timestamp\":\"2026-01-01T00:00:00Z\",\"type\":\"x\",\"data\":{}}",
		"zero position": "{\"seq\":0,\"timestamp\":\"2026-01-01T00:00:00Z\",\"type\":\"x\",\"data\":{}}\n",
	} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "log.jsonl")
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			log, err := Open(path)
			if err != nil {
				t.Fatalf("Open() error = %v", err)
			}
			if !log.ReplayReset() || log.LastSequence() != 1 {
				t.Fatalf("replacement log reset = %v, sequence = %d", log.ReplayReset(), log.LastSequence())
			}
			lines, err := log.ReadAfter(0, 0)
			if err != nil || len(lines) != 1 || lines[0].Seq != 1 || lines[0].Type != "worker_error" || !bytes.Contains(lines[0].Data, []byte(`"code":"replay_gap"`)) {
				t.Fatalf("durable replacement gap = %#v, %v", lines, err)
			}
			line, err := log.Append("", "connection", map[string]string{"state": "connecting"})
			if err != nil || line.Seq != 2 {
				t.Fatalf("append after segmentation = %#v, %v", line, err)
			}
			if err := log.Close(); err != nil {
				t.Fatal(err)
			}
			preserved, err := filepath.Glob(path + ".interrupted-*")
			if err != nil || len(preserved) != 1 {
				t.Fatalf("preserved segments = %#v, %v", preserved, err)
			}
			got, err := os.ReadFile(preserved[0])
			if err != nil || string(got) != body {
				t.Fatalf("preserved body = %q, %v", got, err)
			}
		})
	}
}

func TestInterruptedReplacementPreservesHighWaterAndCannotAliasOldCursor(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	var original bytes.Buffer
	encoder := json.NewEncoder(&original)
	for sequence := uint64(1); sequence <= 5; sequence++ {
		if err := encoder.Encode(Line{
			Seq: sequence, Timestamp: time.Unix(int64(sequence), 0).UTC(), Type: "provider_event", Data: json.RawMessage(`{}`),
		}); err != nil {
			t.Fatal(err)
		}
	}
	original.WriteString(`{"seq":6,"type":"provider_event"`)
	if err := os.WriteFile(path, original.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if !log.ReplayReset() || log.LastSequence() != 6 {
		t.Fatalf("replacement high-water = reset %v sequence %d", log.ReplayReset(), log.LastSequence())
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	// A second crash before the worker can append `connecting` must not erase
	// either the continued high-water or the already-durable gap record.
	log, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if log.ReplayReset() || log.LastSequence() != 6 {
		t.Fatalf("reopened replacement = reset %v sequence %d", log.ReplayReset(), log.LastSequence())
	}
	for _, lineType := range []string{"connection", "provider_event", "provider_result"} {
		if _, err := log.Append("", lineType, map[string]string{"state": "test"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}

	var replay []Line
	last, err := WalkFile(context.Background(), path, 5, 0, func(line Line) error {
		replay = append(replay, line)
		return nil
	})
	if err != nil || last != 9 || len(replay) != 4 || replay[0].Seq != 6 || replay[0].Type != "worker_error" || !bytes.Contains(replay[0].Data, []byte(`"code":"replay_gap"`)) {
		t.Fatalf("old boundary replay = last %d lines %#v error %v", last, replay, err)
	}
	_, err = WalkFile(context.Background(), path, 4, 0, func(Line) error { return nil })
	var gap *CursorGapError
	if !errors.As(err, &gap) || gap.Cursor != 4 || gap.Last != 5 {
		t.Fatalf("older cursor gap = %T %#v, %v", err, gap, err)
	}
}

func TestOpenDefersOlderCorruptionToReplayInsteadOfBlockingStartup(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	body := "not-json\n{\"seq\":41,\"timestamp\":\"2026-01-01T00:00:00Z\",\"type\":\"connection\",\"data\":{}}\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	log, err := Open(path)
	if err != nil {
		t.Fatalf("Open scanned historical corruption: %v", err)
	}
	line, err := log.Append("", "connection", map[string]string{"state": "connecting"})
	if err != nil || line.Seq != 42 {
		t.Fatalf("append after tail recovery = %#v, %v", line, err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadFile(path, 0); err == nil {
		t.Fatal("static replay accepted historical corruption")
	}
}

func TestReplayRejectsCursorAheadOfRetainedChannel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append("", "connection", map[string]string{"state": "connected"}); err != nil {
		t.Fatal(err)
	}
	_, err = log.WalkAfter(context.Background(), 9, 0, func(Line) error { return nil })
	var gap *CursorGapError
	if !errors.As(err, &gap) || gap.Cursor != 9 || gap.Last != 1 {
		t.Fatalf("live cursor gap = %T %#v, %v", err, gap, err)
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	_, err = WalkFile(context.Background(), path, 9, 0, func(Line) error { return nil })
	if !errors.As(err, &gap) || gap.Cursor != 9 || gap.Last != 1 {
		t.Fatalf("static cursor gap = %T %#v, %v", err, gap, err)
	}
}

func TestStaticReplayRejectsUnterminatedRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	body := `{"seq":1,"timestamp":"2026-01-01T00:00:00Z","type":"connection","data":{}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WalkFile(context.Background(), path, 0, 0, func(Line) error { return nil }); err == nil || !strings.Contains(err.Error(), "incomplete") {
		t.Fatalf("static unterminated replay error = %v", err)
	}
}

func TestTailCursorBoundsFreshViewerReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "log.jsonl")
	log, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	for sequence := 1; sequence <= 1000; sequence++ {
		if _, err := log.AppendBuffered("", "provider_event", map[string]int{"index": sequence}); err != nil {
			t.Fatal(err)
		}
	}
	if err := log.Close(); err != nil {
		t.Fatal(err)
	}
	cursor, clipped, err := TailCursor(path, 256)
	if err != nil || !clipped || cursor != 744 {
		t.Fatalf("TailCursor() = %d, %v, %v; want 744, true", cursor, clipped, err)
	}
	count := 0
	last, err := WalkFile(context.Background(), path, cursor, 0, func(line Line) error {
		count++
		return nil
	})
	if err != nil || count != 256 || last != 1000 {
		t.Fatalf("bounded tail replay = count %d last %d error %v", count, last, err)
	}
}

func TestBoundedTailReplayDoesNotDecodeClippedPrefix(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large-prefix.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(strings.Repeat("x", tailReplayMaxBytes+1024) + "\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	for sequence := 1; sequence <= 1000; sequence++ {
		encoded, err := json.Marshal(Line{
			Seq: uint64(sequence), Timestamp: time.Unix(int64(sequence), 0).UTC(), Type: "provider_event", Data: json.RawMessage(`{"type":"text_delta"}`),
		})
		if err != nil {
			file.Close()
			t.Fatal(err)
		}
		if _, err := file.Write(append(encoded, '\n')); err != nil {
			file.Close()
			t.Fatal(err)
		}
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	cursor, clipped, err := TailCursor(path, 256)
	if err != nil || !clipped || cursor != 744 {
		t.Fatalf("TailCursor() = %d, %v, %v", cursor, clipped, err)
	}
	count := 0
	last, err := WalkFile(context.Background(), path, cursor, 0, func(Line) error {
		count++
		return nil
	})
	if err != nil || count != 256 || last != 1000 {
		t.Fatalf("bounded offset replay = count %d last %d error %v", count, last, err)
	}
	if _, err := WalkFile(context.Background(), path, 0, 0, func(Line) error { return nil }); err == nil {
		t.Fatal("explicit full replay unexpectedly ignored the corrupt clipped prefix")
	}
}
