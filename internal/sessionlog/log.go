// Package sessionlog manages append-only Session JSONL scrollback. A Line is
// runtime history, not a persistent domain entity or database record.
package sessionlog

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxLineSize = 20 << 20
const (
	replayCursorBufferSize           = 4 << 10
	replayCursorMaxRetainedTokenSize = 32 << 10
)

type Line struct {
	Seq       uint64          `json:"seq"`
	Timestamp time.Time       `json:"timestamp"`
	RequestID string          `json:"request_id,omitempty"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
}

type Log struct {
	mu   sync.Mutex
	path string
	file *os.File
	seq  uint64
	now  func() time.Time
}

// ReadFile reads existing history without creating or opening an append writer.
func ReadFile(path string, cursor uint64) ([]Line, error) {
	var result []Line
	_, err := WalkFile(context.Background(), path, cursor, 0, func(line Line) error {
		result = append(result, line)
		return nil
	})
	return result, err
}

// WalkFile streams existing history without retaining the complete replay in
// memory. The returned position is the last line delivered, or cursor when no
// newer line exists. If through is nonzero, later positions are not read.
func WalkFile(ctx context.Context, path string, cursor, through uint64, consume func(Line) error) (uint64, error) {
	if consume == nil {
		return cursor, fmt.Errorf("Session log consumer is required")
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cursor, nil
		}
		return cursor, fmt.Errorf("open Session log for replay: %w", err)
	}
	defer file.Close()
	return walkFile(ctx, file, cursor, through, consume)
}

// ReplayCursor incrementally reads one append-only Session log while retaining
// only its byte offset and last position. Consecutive Next calls reuse one
// buffered reader; reaching EOF closes it so an idle Task-level merge does not
// hold one file descriptor per child Session.
type ReplayCursor struct {
	path      string
	offset    int64
	previous  uint64
	cursor    uint64
	through   uint64
	finished  bool
	file      *os.File
	scanner   *bufio.Scanner
	buffer    []byte
	oversized bool
}

func NewReplayCursor(path string, cursor, through uint64) *ReplayCursor {
	return &ReplayCursor{path: path, cursor: cursor, through: through}
}

func (r *ReplayCursor) Next(ctx context.Context) (Line, bool, error) {
	if r == nil || r.finished {
		return Line{}, false, nil
	}
	if err := ctx.Err(); err != nil {
		r.closeReader()
		return Line{}, false, err
	}
	if err := r.openReader(); err != nil {
		return Line{}, false, err
	}
	if r.scanner == nil {
		return Line{}, false, nil
	}
	for r.scanner.Scan() {
		if err := ctx.Err(); err != nil {
			r.closeReader()
			return Line{}, false, err
		}
		encoded := r.scanner.Bytes()
		if len(encoded) > replayCursorMaxRetainedTokenSize {
			r.oversized = true
		}
		r.offset += int64(len(encoded) + 1) // Session logs always end records with '\n'.
		var line Line
		if err := json.Unmarshal(encoded, &line); err != nil {
			r.closeReader()
			return Line{}, false, fmt.Errorf("decode JSONL line: %w", err)
		}
		if line.Seq == 0 || line.Seq <= r.previous {
			r.closeReader()
			return Line{}, false, fmt.Errorf("non-monotonic Session log position %d after %d", line.Seq, r.previous)
		}
		r.previous = line.Seq
		if r.through != 0 && line.Seq > r.through {
			r.finished = true
			r.closeReader()
			return Line{}, false, nil
		}
		if line.Seq > r.cursor {
			r.cursor = line.Seq
			// Scanner grows to fit large records and cannot expose or shrink that
			// private buffer. Drop the reader before returning so an old large
			// record cannot stay pinned behind a later tiny Task-merge head.
			if r.oversized {
				r.closeReader()
			}
			return line, true, nil
		}
	}
	if err := r.scanner.Err(); err != nil {
		r.closeReader()
		return Line{}, false, err
	}
	r.closeReader()
	return Line{}, false, nil
}

// Close releases an active reader. ReplayCursor opens files lazily and closes
// them automatically at EOF; callers should still close cursors when ending a
// partially consumed aggregate replay.
func (r *ReplayCursor) Close() error {
	if r == nil {
		return nil
	}
	r.finished = true
	return r.closeReader()
}

// ReaderActive reports whether the cursor currently owns an open file. It is
// used by aggregate replay to cap file descriptors without discarding cursor
// position or the reusable small buffer.
func (r *ReplayCursor) ReaderActive() bool {
	return r != nil && r.file != nil
}

// ReleaseReader closes the current file without finishing the cursor. The
// next call to Next resumes at the saved byte offset.
func (r *ReplayCursor) ReleaseReader() error {
	if r == nil {
		return nil
	}
	return r.closeReader()
}

func (r *ReplayCursor) openReader() error {
	if r.file != nil {
		return nil
	}
	file, err := os.Open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open Session log cursor: %w", err)
	}
	if _, err := file.Seek(r.offset, 0); err != nil {
		_ = file.Close()
		return fmt.Errorf("seek Session log cursor: %w", err)
	}
	if r.buffer == nil {
		r.buffer = make([]byte, replayCursorBufferSize)
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(r.buffer, maxLineSize)
	r.file = file
	r.scanner = scanner
	r.oversized = false
	return nil
}

func (r *ReplayCursor) closeReader() error {
	if r.file == nil {
		r.scanner = nil
		r.oversized = false
		return nil
	}
	err := r.file.Close()
	r.file = nil
	r.scanner = nil
	r.oversized = false
	return err
}

func Open(path string) (*Log, error) {
	if path == "" {
		return nil, fmt.Errorf("Session log path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, fmt.Errorf("create Session log directory: %w", err)
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open Session log %q: %w", path, err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		file.Close()
		return nil, fmt.Errorf("secure Session log %q: %w", path, err)
	}
	var sequence uint64
	_, err = walkFile(context.Background(), file, 0, 0, func(line Line) error {
		sequence = line.Seq
		return nil
	})
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("read Session log %q: %w", path, err)
	}
	return &Log{path: path, file: file, seq: sequence, now: func() time.Time { return time.Now().UTC() }}, nil
}

func (l *Log) Path() string { return l.path }

func (l *Log) LastSequence() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

func (l *Log) Append(requestID, lineType string, data any) (Line, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return Line{}, fmt.Errorf("Session log is closed")
	}
	if lineType == "" {
		return Line{}, fmt.Errorf("Session log line type is required")
	}
	raw, err := marshalData(data)
	if err != nil {
		return Line{}, fmt.Errorf("marshal Session log data: %w", err)
	}
	line := Line{Seq: l.seq + 1, Timestamp: l.now().UTC(), RequestID: requestID, Type: lineType, Data: raw}
	encoded, err := json.Marshal(line)
	if err != nil {
		return Line{}, fmt.Errorf("marshal Session log line: %w", err)
	}
	if _, err := l.file.Write(append(encoded, '\n')); err != nil {
		return Line{}, fmt.Errorf("append Session log: %w", err)
	}
	if err := l.file.Sync(); err != nil {
		return Line{}, fmt.Errorf("sync Session log: %w", err)
	}
	l.seq = line.Seq
	return line, nil
}

// ReadAfter returns lines with positions greater than cursor. If through is
// nonzero, lines after that inclusive snapshot boundary are omitted.
func (l *Log) ReadAfter(cursor, through uint64) ([]Line, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil, fmt.Errorf("Session log is closed")
	}
	if err := l.file.Sync(); err != nil {
		return nil, err
	}
	file, err := os.Open(l.path)
	if err != nil {
		return nil, fmt.Errorf("open Session log for replay: %w", err)
	}
	defer file.Close()
	return scanFile(file, cursor, through)
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

func scanFile(file *os.File, cursor, through uint64) ([]Line, error) {
	var result []Line
	_, err := walkFile(context.Background(), file, cursor, through, func(line Line) error {
		result = append(result, line)
		return nil
	})
	return result, err
}

func walkFile(ctx context.Context, file *os.File, cursor, through uint64, consume func(Line) error) (uint64, error) {
	if _, err := file.Seek(0, 0); err != nil {
		return cursor, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)
	var previous uint64
	last := cursor
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		var line Line
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			return last, fmt.Errorf("decode JSONL line: %w", err)
		}
		if line.Seq == 0 || line.Seq <= previous {
			return last, fmt.Errorf("non-monotonic Session log position %d after %d", line.Seq, previous)
		}
		previous = line.Seq
		if through != 0 && line.Seq > through {
			break
		}
		if line.Seq > cursor {
			if err := consume(line); err != nil {
				return last, err
			}
			last = line.Seq
		}
	}
	if err := scanner.Err(); err != nil {
		return last, err
	}
	return last, nil
}

func marshalData(data any) (json.RawMessage, error) {
	if raw, ok := data.(json.RawMessage); ok {
		if !json.Valid(raw) {
			return nil, fmt.Errorf("invalid raw JSON")
		}
		return append(json.RawMessage(nil), raw...), nil
	}
	if data == nil {
		return json.RawMessage(`{}`), nil
	}
	encoded, err := json.Marshal(data)
	return json.RawMessage(encoded), err
}
