// Package sessionlog manages append-only Session JSONL scrollback. A Line is
// runtime history, not a persistent domain entity or database record.
package sessionlog

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

const maxLineSize = 20 << 20
const (
	scannerMaxTokenSize              = maxLineSize + 1
	replayCursorBufferSize           = 4 << 10
	replayCursorMaxRetainedTokenSize = 32 << 10
	tailReplayMaxBytes               = maxLineSize + 2
)

// ChannelPath is the v1 Session Channel replay namespace. Legacy gateway
// JSONL files used different outer event semantics and remain preserved on
// disk, but are never reinterpreted or streamed through this contract.
func ChannelPath(directory, sessionID string) string {
	return filepath.Join(directory, sessionID+".channel-v1.jsonl")
}

type Line struct {
	Seq       uint64          `json:"seq"`
	Timestamp time.Time       `json:"timestamp"`
	RequestID string          `json:"request_id,omitempty"`
	Type      string          `json:"type"`
	Data      json.RawMessage `json:"data"`
}

type Log struct {
	mu          sync.Mutex
	path        string
	file        *os.File
	syncFile    func() error
	poisoned    error
	seq         uint64
	replayReset bool
	now         func() time.Time
}

// CursorGapError means a viewer cursor cannot be continued against the
// retained channel segment. Clients must reset their replay cursor instead of
// silently waiting for a sequence that this log can never produce.
type CursorGapError struct {
	Cursor uint64
	Last   uint64
}

func (e *CursorGapError) Error() string {
	return fmt.Sprintf("Session Channel replay cursor %d is outside retained history ending at %d", e.Cursor, e.Last)
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
			if cursor > 0 {
				return cursor, &CursorGapError{Cursor: cursor, Last: 0}
			}
			return cursor, nil
		}
		return cursor, fmt.Errorf("open Session log for replay: %w", err)
	}
	defer file.Close()
	return walkFile(ctx, file, cursor, through, consume)
}

// TailCursor returns the cursor immediately before a bounded tail window.
// It reads at most one maximum-sized record plus framing from the end of the
// file and retains only maxRecords sequence numbers. Viewers use this for a
// fresh attachment so old scrollback cannot delay current live activity;
// explicit nonzero cursors still replay exactly from their saved position.
func TailCursor(path string, maxRecords int) (cursor uint64, clipped bool, err error) {
	if maxRecords <= 0 {
		return 0, false, fmt.Errorf("tail replay record limit must be positive")
	}
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return 0, false, nil
		}
		return 0, false, fmt.Errorf("open Session log tail: %w", err)
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return 0, false, fmt.Errorf("stat Session log tail: %w", err)
	}
	if info.Size() == 0 {
		return 0, false, nil
	}
	buffer, _, clipped, err := readTailWindow(file, info.Size())
	if err != nil || len(buffer) == 0 {
		return 0, clipped, err
	}
	ring := make([]uint64, maxRecords)
	count, head := 0, 0
	var previous uint64
	scanner := bufio.NewScanner(bytes.NewReader(buffer))
	scanner.Buffer(make([]byte, 64*1024), scannerMaxTokenSize)
	scanner.Split(scanStrictJSONLRecord)
	for scanner.Scan() {
		var position struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &position); err != nil || position.Seq == 0 {
			return 0, false, fmt.Errorf("decode Session log tail position")
		}
		if previous != 0 && position.Seq != previous+1 {
			return 0, false, fmt.Errorf("non-contiguous Session log tail position %d after %d", position.Seq, previous)
		}
		previous = position.Seq
		if count < maxRecords {
			ring[count] = position.Seq
			count++
		} else {
			ring[head] = position.Seq
			head = (head + 1) % maxRecords
			clipped = true
		}
	}
	if err := scanner.Err(); err != nil {
		return 0, false, err
	}
	if count == 0 {
		return 0, clipped, nil
	}
	earliest := ring[head]
	if earliest <= 1 {
		return 0, clipped, nil
	}
	return earliest - 1, true, nil
}

// locateTailCursor returns the byte offset immediately after cursor when that
// boundary is present in the bounded tail window. It also recognizes a cursor
// immediately before the first complete retained tail record. Callers fall
// back to strict full-file validation when the cursor is older than the tail.
func locateTailCursor(path string, size int64, cursor uint64) (int64, bool, error) {
	if cursor == 0 || size == 0 {
		return 0, false, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return 0, false, err
	}
	defer file.Close()
	buffer, base, _, err := readTailWindow(file, size)
	if err != nil {
		return 0, false, err
	}
	var previous uint64
	for position := 0; position < len(buffer); {
		newline := bytes.IndexByte(buffer[position:], '\n')
		if newline < 0 {
			break
		}
		end := position + newline
		var value struct {
			Seq uint64 `json:"seq"`
		}
		if err := json.Unmarshal(buffer[position:end], &value); err != nil || value.Seq == 0 {
			return 0, false, fmt.Errorf("decode Session log tail position")
		}
		if previous != 0 && value.Seq != previous+1 {
			return 0, false, fmt.Errorf("non-contiguous Session log tail position %d after %d", value.Seq, previous)
		}
		previous = value.Seq
		if value.Seq == cursor+1 {
			return base + int64(position), true, nil
		}
		if value.Seq == cursor {
			return base + int64(end+1), true, nil
		}
		position = end + 1
	}
	return 0, false, nil
}

func readTailWindow(file *os.File, size int64) (buffer []byte, base int64, clipped bool, err error) {
	start := size - tailReplayMaxBytes
	if start < 0 {
		start = 0
	}
	buffer = make([]byte, size-start)
	if _, err := file.ReadAt(buffer, start); err != nil && !errors.Is(err, io.EOF) {
		return nil, 0, false, fmt.Errorf("read Session log tail: %w", err)
	}
	base = start
	if start > 0 {
		boundary := bytes.IndexByte(buffer, '\n')
		if boundary < 0 {
			return nil, 0, false, fmt.Errorf("Session log tail contains no bounded record boundary")
		}
		buffer = buffer[boundary+1:]
		base += int64(boundary + 1)
		clipped = true
	}
	// Ignore only a concurrently-written unterminated suffix. The writer will
	// expose it through the exact Follow boundary once its newline is durable.
	if end := bytes.LastIndexByte(buffer, '\n'); end >= 0 {
		buffer = buffer[:end+1]
	} else {
		buffer = nil
	}
	return buffer, base, clipped, nil
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
	identity  os.FileInfo
	strictEOF bool
}

func NewReplayCursor(path string, cursor, through uint64) *ReplayCursor {
	return &ReplayCursor{path: path, cursor: cursor, through: through}
}

// NewStrictReplayCursor creates a cursor for a finite/static replay. Unlike a
// live cursor, it reports an unterminated final record instead of withholding
// it for a writer that may complete it later.
func NewStrictReplayCursor(path string, cursor, through uint64) *ReplayCursor {
	return &ReplayCursor{path: path, cursor: cursor, through: through, strictEOF: true}
}

// SetStrictEOF switches the EOF policy before the next read. Task followers
// use live-safe parsing while a worker may append, then strict parsing once a
// Session is terminal or has no writer ownership.
func (r *ReplayCursor) SetStrictEOF(strict bool) error {
	if r == nil || r.strictEOF == strict {
		return nil
	}
	if err := r.closeReader(); err != nil {
		return err
	}
	r.strictEOF = strict
	return nil
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
		if line.Seq == 0 || line.Seq != r.previous+1 {
			r.closeReader()
			if r.previous == 0 && line.Seq > 1 {
				return Line{}, false, &CursorGapError{Cursor: r.cursor, Last: line.Seq - 1}
			}
			return Line{}, false, fmt.Errorf("non-contiguous Session log position %d after %d", line.Seq, r.previous)
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
	if r.cursor > r.previous {
		return Line{}, false, &CursorGapError{Cursor: r.cursor, Last: r.previous}
	}
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

// HasUnreadBytes reports whether the current append-only file contains bytes
// after the cursor's last complete newline. Aggregate replay uses this to pay
// for an exact worker-liveness probe only when live-safe parsing withheld a
// possible in-progress record. A replacement or truncation is a cursor gap.
func (r *ReplayCursor) HasUnreadBytes() (bool, error) {
	if r == nil {
		return false, nil
	}
	info, err := os.Stat(r.path)
	if err != nil {
		if os.IsNotExist(err) && r.offset == 0 && r.identity == nil {
			return false, nil
		}
		return false, fmt.Errorf("stat Session log cursor: %w", err)
	}
	if r.identity != nil && !os.SameFile(r.identity, info) {
		return false, &CursorGapError{Cursor: r.cursor, Last: 0}
	}
	if r.offset > info.Size() {
		return false, &CursorGapError{Cursor: r.cursor, Last: 0}
	}
	return r.offset < info.Size(), nil
}

func (r *ReplayCursor) openReader() error {
	if r.file != nil {
		return nil
	}
	file, err := os.Open(r.path)
	if err != nil {
		if os.IsNotExist(err) {
			if r.cursor > 0 {
				return &CursorGapError{Cursor: r.cursor, Last: 0}
			}
			return nil
		}
		return fmt.Errorf("open Session log cursor: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("stat Session log cursor: %w", err)
	}
	if r.identity != nil && !os.SameFile(r.identity, info) {
		_ = file.Close()
		return &CursorGapError{Cursor: r.cursor, Last: 0}
	}
	if r.offset > info.Size() {
		_ = file.Close()
		return &CursorGapError{Cursor: r.cursor, Last: 0}
	}
	if r.offset == 0 && r.previous == 0 && r.cursor > 0 {
		offset, found, locateErr := locateTailCursor(r.path, info.Size(), r.cursor)
		if locateErr != nil {
			_ = file.Close()
			return locateErr
		}
		if found {
			r.offset = offset
			r.previous = r.cursor
		}
	}
	if _, err := file.Seek(r.offset, 0); err != nil {
		_ = file.Close()
		return fmt.Errorf("seek Session log cursor: %w", err)
	}
	if r.identity == nil {
		r.identity = info
	}
	if r.buffer == nil {
		r.buffer = make([]byte, replayCursorBufferSize)
	}
	scanner := bufio.NewScanner(file)
	scanner.Buffer(r.buffer, scannerMaxTokenSize)
	if r.strictEOF {
		scanner.Split(scanStrictJSONLRecord)
	} else {
		scanner.Split(scanCompleteJSONLRecord)
	}
	r.file = file
	r.scanner = scanner
	r.oversized = false
	return nil
}

// scanCompleteJSONLRecord intentionally withholds an unterminated EOF token.
// Session log records are newline-terminated, and the active writer may be
// between bytes when a ReplayCursor polls it. Returning no token leaves the
// cursor's durable byte offset unchanged so its next poll rereads the fragment
// after the writer completes it.
func scanCompleteJSONLRecord(data []byte, _ bool) (advance int, token []byte, err error) {
	if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
		return newline + 1, data[:newline], nil
	}
	return 0, nil, nil
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
	sequence, recoveryErr := recoverLastSequence(file)
	replayReset := false
	if recoveryErr != nil {
		highWater, highWaterErr := recoverInterruptedHighWater(file)
		if highWaterErr != nil {
			_ = file.Close()
			return nil, fmt.Errorf("recover interrupted Session log %q high-water: %w", path, highWaterErr)
		}
		// A crash may leave one partial buffered provider frame. Replace it only
		// with an already-synced gap record in one atomic namespace operation.
		// At every crash boundary the canonical path therefore names either the
		// interrupted segment (which will be recovered again) or the monotonic
		// replacement; it can never briefly name an empty sequence-zero log.
		file, sequence, err = replaceInterruptedLog(path, file, highWater)
		if err != nil {
			_ = file.Close()
			return nil, err
		}
		replayReset = true
	}
	result := &Log{path: path, file: file, seq: sequence, replayReset: replayReset, now: func() time.Time { return time.Now().UTC() }}
	result.syncFile = result.file.Sync
	return result, nil
}

// replaceInterruptedLog atomically exchanges an invalid canonical log for a
// complete, durable replay-gap segment. The hard link preserves the original
// inode before the exchange without ever removing it from the canonical path.
func replaceInterruptedLog(path string, interrupted *os.File, highWater uint64) (*os.File, uint64, error) {
	if highWater == ^uint64(0) {
		return interrupted, highWater, fmt.Errorf("recover interrupted Session log %q: sequence is exhausted", path)
	}
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, "."+filepath.Base(path)+".replacement-*")
	if err != nil {
		return interrupted, highWater, fmt.Errorf("create replacement Session log %q: %w", path, err)
	}
	temporaryPath := temporary.Name()
	replaced := false
	defer func() {
		if !replaced {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return interrupted, highWater, fmt.Errorf("secure replacement Session log %q: %w", path, err)
	}
	replacement := &Log{
		path: path, file: temporary, seq: highWater, replayReset: true,
		now: func() time.Time { return time.Now().UTC() }, syncFile: temporary.Sync,
	}
	gap, err := replacement.Append("", "worker_error", map[string]string{
		"code": "replay_gap", "message": "An interrupted final channel frame was preserved separately; earlier replay is unavailable from this channel segment.",
	})
	if err != nil {
		_ = replacement.Close()
		return interrupted, highWater, fmt.Errorf("journal interrupted Session log %q replay gap: %w", path, err)
	}
	if err := replacement.Close(); err != nil {
		return interrupted, highWater, fmt.Errorf("close replacement Session log %q: %w", path, err)
	}

	preserved := path + ".interrupted-" + strconv.FormatInt(time.Now().UTC().UnixNano(), 10) + "-" + strconv.Itoa(os.Getpid())
	if err := os.Link(path, preserved); err != nil {
		return interrupted, highWater, fmt.Errorf("preserve interrupted Session log %q: %w", path, err)
	}
	if err := syncDirectory(directory); err != nil {
		return interrupted, highWater, fmt.Errorf("sync preserved Session log %q: %w", path, err)
	}
	if err := interrupted.Close(); err != nil {
		return interrupted, highWater, fmt.Errorf("close interrupted Session log %q: %w", path, err)
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return interrupted, highWater, fmt.Errorf("install replacement Session log %q: %w", path, err)
	}
	replaced = true
	if err := syncDirectory(directory); err != nil {
		return interrupted, highWater, fmt.Errorf("sync replacement Session log %q: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_RDWR, 0o600)
	if err != nil {
		return interrupted, highWater, fmt.Errorf("open replacement Session log %q: %w", path, err)
	}
	return file, gap.Seq, nil
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

// recoverInterruptedHighWater returns the last complete record immediately
// before an invalid or unterminated final record. A writer-created record is
// bounded by maxLineSize, so two records plus framing are sufficient to prove
// the predecessor without scanning arbitrary history. Oversized corruption
// fails closed instead of guessing a sequence that could alias an old cursor.
func recoverInterruptedHighWater(file *os.File) (uint64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat Session log: %w", err)
	}
	if info.Size() == 0 {
		return 0, nil
	}
	const recoveryWindow = int64(2*maxLineSize + 3)
	start := info.Size() - recoveryWindow
	if start < 0 {
		start = 0
	}
	encoded := make([]byte, info.Size()-start)
	if _, err := file.ReadAt(encoded, start); err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("read Session log recovery tail: %w", err)
	}
	if len(encoded) == 0 {
		return 0, nil
	}
	// An unterminated suffix follows the last newline. If the file ends with a
	// newline, the invalid final record ends there and its predecessor ends at
	// the prior newline.
	candidateEnd := bytes.LastIndexByte(encoded, '\n')
	if encoded[len(encoded)-1] == '\n' {
		candidateEnd = bytes.LastIndexByte(encoded[:len(encoded)-1], '\n')
	}
	if candidateEnd < 0 {
		if start > 0 {
			return 0, fmt.Errorf("interrupted suffix exceeds %d bytes", maxLineSize)
		}
		return 0, nil
	}
	candidateStart := bytes.LastIndexByte(encoded[:candidateEnd], '\n') + 1
	if candidateStart == 0 && start > 0 {
		return 0, fmt.Errorf("complete predecessor exceeds %d bytes", maxLineSize)
	}
	candidate := encoded[candidateStart:candidateEnd]
	if len(candidate) == 0 || len(candidate) > maxLineSize {
		return 0, fmt.Errorf("invalid complete predecessor")
	}
	var line Line
	if err := json.Unmarshal(candidate, &line); err != nil {
		return 0, fmt.Errorf("decode complete predecessor: %w", err)
	}
	if line.Seq == 0 {
		return 0, fmt.Errorf("invalid complete predecessor position 0")
	}
	return line.Seq, nil
}

// recoverLastSequence reads only the final complete bounded record needed to
// allocate the next channel position. Historical replay validates every older
// line lazily; provider startup and viewer attachment must not wait for a full
// audit-log scan.
func recoverLastSequence(file *os.File) (uint64, error) {
	info, err := file.Stat()
	if err != nil {
		return 0, fmt.Errorf("stat Session log: %w", err)
	}
	size := info.Size()
	if size == 0 {
		return 0, nil
	}
	readSize := int64(maxLineSize + 2)
	if size < readSize {
		readSize = size
	}
	encoded := make([]byte, readSize)
	if _, err := file.ReadAt(encoded, size-readSize); err != nil && !errors.Is(err, io.EOF) {
		return 0, fmt.Errorf("read Session log tail: %w", err)
	}
	if len(encoded) == 0 || encoded[len(encoded)-1] != '\n' {
		return 0, fmt.Errorf("Session log ends with an incomplete record")
	}
	withoutTerminator := encoded[:len(encoded)-1]
	start := bytes.LastIndexByte(withoutTerminator, '\n') + 1
	if start == 0 && size > readSize {
		return 0, fmt.Errorf("final Session log record exceeds %d bytes", maxLineSize)
	}
	last := withoutTerminator[start:]
	if len(last) == 0 || len(last) > maxLineSize {
		return 0, fmt.Errorf("invalid final Session log record")
	}
	var line Line
	if err := json.Unmarshal(last, &line); err != nil {
		return 0, fmt.Errorf("decode final JSONL line: %w", err)
	}
	if line.Seq == 0 {
		return 0, fmt.Errorf("invalid final Session log position 0")
	}
	return line.Seq, nil
}

func (l *Log) Path() string { return l.path }

// ReplayReset reports that Open preserved an interrupted previous segment and
// created a fresh channel segment whose first durable record declares the gap.
func (l *Log) ReplayReset() bool { return l.replayReset }

func (l *Log) LastSequence() uint64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

func (l *Log) Append(requestID, lineType string, data any) (Line, error) {
	return l.append(requestID, lineType, data, true)
}

// AppendBuffered writes a high-volume replay frame without forcing an fsync
// before it may be streamed to attached viewers. The next durable Append,
// WalkAfter snapshot, or Close flushes it. Use this only for provider deltas;
// accepted commands, permissions, terminal results, and lifecycle frames must
// use Append so their ordering boundary is durable before acknowledgement.
func (l *Log) AppendBuffered(requestID, lineType string, data any) (Line, error) {
	return l.append(requestID, lineType, data, false)
}

func (l *Log) append(requestID, lineType string, data any, durable bool) (Line, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return Line{}, fmt.Errorf("Session log is closed")
	}
	if l.poisoned != nil {
		return Line{}, fmt.Errorf("Session log is unavailable after an earlier write failure: %w", l.poisoned)
	}
	if lineType == "" {
		return Line{}, fmt.Errorf("Session log line type is required")
	}
	if l.seq == ^uint64(0) {
		return Line{}, fmt.Errorf("Session log sequence is exhausted")
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
	if len(encoded) > maxLineSize {
		return Line{}, fmt.Errorf("Session log record exceeds %d bytes", maxLineSize)
	}
	record := append(encoded, '\n')
	written, err := l.file.Write(record)
	if err != nil || written != len(record) {
		if err == nil {
			err = io.ErrShortWrite
		}
		l.poisoned = fmt.Errorf("append Session log after writing %d of %d bytes: %w", written, len(record), err)
		return Line{}, l.poisoned
	}
	// Once the complete newline-terminated record is visible, its sequence may
	// never be allocated again even if the durability proof below fails. Poison
	// the writer on that ambiguity so callers fail-stop instead of appending a
	// duplicate sequence to an otherwise valid JSONL suffix.
	l.seq = line.Seq
	if durable {
		if err := l.syncFile(); err != nil {
			l.poisoned = fmt.Errorf("sync Session log: %w", err)
			return Line{}, l.poisoned
		}
	}
	return line, nil
}

// ReadAfter returns lines with positions greater than cursor. If through is
// nonzero, lines after that inclusive snapshot boundary are omitted.
func (l *Log) ReadAfter(cursor, through uint64) ([]Line, error) {
	var result []Line
	_, err := l.WalkAfter(context.Background(), cursor, through, func(line Line) error {
		result = append(result, line)
		return nil
	})
	return result, err
}

// WalkAfter streams lines with positions greater than cursor from one stable
// snapshot of the append-only log. It holds the writer mutex only long enough
// to sync the append file and capture its path and sequence cutoff; decoding
// and consumer work happen after the mutex is released so a slow replay cannot
// block live appends. If through is nonzero, it further bounds the snapshot at
// that inclusive position.
func (l *Log) WalkAfter(ctx context.Context, cursor, through uint64, consume func(Line) error) (uint64, error) {
	return l.walkAfter(ctx, cursor, through, true, consume)
}

// WalkLiveAfter streams one in-process live snapshot without turning viewer
// fan-out into a durability boundary. os.File writes are already visible to a
// separate read descriptor; accepted commands and terminal records retain
// their explicit fsync boundaries. Only the worker live-subscriber path may
// use this method.
func (l *Log) WalkLiveAfter(ctx context.Context, cursor, through uint64, consume func(Line) error) (uint64, error) {
	return l.walkAfter(ctx, cursor, through, false, consume)
}

func (l *Log) walkAfter(ctx context.Context, cursor, through uint64, durable bool, consume func(Line) error) (uint64, error) {
	if consume == nil {
		return cursor, fmt.Errorf("Session log consumer is required")
	}
	l.mu.Lock()
	if l.file == nil {
		l.mu.Unlock()
		return cursor, fmt.Errorf("Session log is closed")
	}
	if durable {
		if l.poisoned != nil {
			err := l.poisoned
			l.mu.Unlock()
			return cursor, fmt.Errorf("Session log is unavailable after an earlier write failure: %w", err)
		}
		if err := l.syncFile(); err != nil {
			l.poisoned = fmt.Errorf("sync Session log snapshot: %w", err)
			l.mu.Unlock()
			return cursor, l.poisoned
		}
	}
	info, err := l.file.Stat()
	if err != nil {
		l.mu.Unlock()
		return cursor, fmt.Errorf("stat Session log snapshot: %w", err)
	}
	path := l.path
	byteCutoff := info.Size()
	cutoff := l.seq
	if through != 0 && through < cutoff {
		cutoff = through
	}
	l.mu.Unlock()
	if cursor > cutoff {
		return cursor, &CursorGapError{Cursor: cursor, Last: cutoff}
	}
	if cutoff <= cursor {
		return cursor, nil
	}

	file, err := os.Open(path)
	if err != nil {
		return cursor, fmt.Errorf("open Session log snapshot for replay: %w", err)
	}
	defer file.Close()
	offset, found, err := locateTailCursor(path, byteCutoff, cursor)
	if err != nil {
		return cursor, err
	}
	if found {
		return walkReader(ctx, io.NewSectionReader(file, offset, byteCutoff-offset), cursor, cutoff, cursor, consume)
	}
	return walkReader(ctx, io.NewSectionReader(file, 0, byteCutoff), cursor, cutoff, 0, consume)
}

func (l *Log) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	syncErr := l.syncFile()
	err := l.file.Close()
	l.file = nil
	if syncErr != nil {
		return syncErr
	}
	return err
}

func walkFile(ctx context.Context, file *os.File, cursor, through uint64, consume func(Line) error) (uint64, error) {
	if cursor > 0 {
		info, err := file.Stat()
		if err != nil {
			return cursor, err
		}
		offset, found, err := locateTailCursor(file.Name(), info.Size(), cursor)
		if err != nil {
			return cursor, err
		}
		if found {
			if _, err := file.Seek(offset, 0); err != nil {
				return cursor, err
			}
			return walkReader(ctx, file, cursor, through, cursor, consume)
		}
	}
	if _, err := file.Seek(0, 0); err != nil {
		return cursor, err
	}
	return walkReader(ctx, file, cursor, through, 0, consume)
}

func walkReader(ctx context.Context, reader io.Reader, cursor, through, previous uint64, consume func(Line) error) (uint64, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), scannerMaxTokenSize)
	scanner.Split(scanStrictJSONLRecord)
	last := cursor
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return last, err
		}
		var line Line
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			return last, fmt.Errorf("decode JSONL line: %w", err)
		}
		if line.Seq == 0 || line.Seq != previous+1 {
			if previous == 0 && line.Seq > 1 {
				return last, &CursorGapError{Cursor: cursor, Last: line.Seq - 1}
			}
			return last, fmt.Errorf("non-contiguous Session log position %d after %d", line.Seq, previous)
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
	if cursor > previous {
		return last, &CursorGapError{Cursor: cursor, Last: previous}
	}
	return last, nil
}

func scanStrictJSONLRecord(data []byte, atEOF bool) (advance int, token []byte, err error) {
	if newline := bytes.IndexByte(data, '\n'); newline >= 0 {
		return newline + 1, data[:newline], nil
	}
	if atEOF && len(data) > 0 {
		return 0, nil, fmt.Errorf("Session log ends with an incomplete record")
	}
	return 0, nil, nil
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
