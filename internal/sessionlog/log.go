// Package sessionlog manages append-only Session JSONL scrollback. A Line is
// runtime history, not a persistent domain entity or database record.
package sessionlog

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const maxLineSize = 20 << 20

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
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("open Session log for replay: %w", err)
	}
	defer file.Close()
	return scanFile(file, cursor, 0)
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
	last, err := scanFile(file, 0, 0)
	if err != nil {
		file.Close()
		return nil, fmt.Errorf("read Session log %q: %w", path, err)
	}
	var sequence uint64
	if len(last) > 0 {
		sequence = last[len(last)-1].Seq
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
	if _, err := file.Seek(0, 0); err != nil {
		return nil, err
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), maxLineSize)
	var result []Line
	var previous uint64
	for scanner.Scan() {
		var line Line
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			return nil, fmt.Errorf("decode JSONL line: %w", err)
		}
		if line.Seq == 0 || line.Seq <= previous {
			return nil, fmt.Errorf("non-monotonic Session log position %d after %d", line.Seq, previous)
		}
		previous = line.Seq
		if line.Seq > cursor && (through == 0 || line.Seq <= through) {
			result = append(result, line)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return result, nil
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
