package connector

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/acp"
)

func TestFrameQueueBlocksAtFrameHighWaterAndResumes(t *testing.T) {
	t.Parallel()
	queue := newFrameQueue()
	ctx := context.Background()
	for sequence := 1; sequence <= FollowHighWaterFrames; sequence++ {
		if err := queue.push(ctx, numberedFrame(sequence)); err != nil {
			t.Fatal(err)
		}
	}

	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		close(started)
		done <- queue.push(ctx, numberedFrame(FollowHighWaterFrames+1))
	}()
	<-started
	waitQueuePauses(t, queue, 1)

	batch, err := queue.next(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Frames) != FollowBatchFrames || !batch.More {
		t.Fatalf("batch = %d frames, more=%v", len(batch.Frames), batch.More)
	}
	if err := <-done; err != nil {
		t.Fatalf("blocked push error = %v", err)
	}
	stats := queue.statsSnapshot()
	if stats.PeakFrames != FollowHighWaterFrames || stats.Pauses != 1 || stats.Resumes != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func TestFrameQueueByteHighWaterAndTerminalDrain(t *testing.T) {
	t.Parallel()
	queue := newFrameQueue()
	ctx := context.Background()
	large := exactFrame(acp.MaxFrameBytes)
	if err := queue.push(ctx, large); err != nil {
		t.Fatal(err)
	}
	if err := queue.push(ctx, large); err != nil {
		t.Fatal(err)
	}

	done := make(chan error, 1)
	go func() { done <- queue.push(ctx, numberedFrame(3)) }()
	waitQueuePauses(t, queue, 1)
	first, err := queue.next(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Frames) != 2 {
		t.Fatalf("first batch size = %d", len(first.Frames))
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	queue.complete(Terminal{Success: false, StopReason: "refusal", Native: json.RawMessage(`{"stopReason":"refusal"}`)})
	last, err := queue.next(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(last.Frames) != 1 || last.More || last.Terminal == nil || last.Terminal.StopReason != "refusal" {
		t.Fatalf("last batch = %+v", last)
	}
	if _, err := queue.next(ctx, nil); !errors.Is(err, ErrStaleTurn) {
		t.Fatalf("post-terminal next error = %v", err)
	}
	stats := queue.statsSnapshot()
	if stats.PeakBytes != FollowHighWaterBytes || stats.Pauses != 1 || stats.Resumes != 1 {
		t.Fatalf("stats = %+v", stats)
	}
}

func waitQueuePauses(t *testing.T, queue *frameQueue, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if queue.statsSnapshot().Pauses >= want {
			return
		}
		runtime.Gosched()
	}
	t.Fatalf("queue never reported %d pauses", want)
}

func TestCanonicalResultPreservesRawUnknownFrameBytes(t *testing.T) {
	t.Parallel()
	unknown := json.RawMessage(`{"jsonrpc":"2.0", "method":"session/update", "params": {"update":{"future": [3, 2, 1]}}}`)
	result := capabilityResult{
		Operation:      OperationFollowNext,
		ChildSessionID: "child",
		TurnToken:      "token",
		Mode:           ModeFollow,
		Frames:         []json.RawMessage{unknown},
		More:           true,
	}
	result.setContinuation(OperationFollowNext, "child", "token")
	raw, err := result.marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, unknown) {
		t.Fatalf("canonical result compacted unknown frame\nresult: %s\nframe:  %s", raw, unknown)
	}
	var decoded struct {
		Frames        []json.RawMessage `json:"frames"`
		More          bool              `json:"more"`
		NextOperation Operation         `json:"next_operation"`
		NextArguments continuation      `json:"next_arguments"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Frames) != 1 || !bytes.Equal(decoded.Frames[0], unknown) {
		t.Fatalf("decoded frame changed: %q", decoded.Frames)
	}
	if !decoded.More || decoded.NextOperation != OperationFollowNext || decoded.NextArguments.ChildSessionID != "child" || decoded.NextArguments.TurnToken != "token" {
		t.Fatalf("continuation = %+v", decoded)
	}
}

func TestCanonicalFollowResultRejectsEncodedOverflow(t *testing.T) {
	t.Parallel()
	frame := exactFrame(acp.MaxFrameBytes)
	nativePrefix := `{"stopReason":"end_turn","opaque":"`
	nativeSuffix := `"}`
	native := json.RawMessage(nativePrefix + strings.Repeat("n", acp.MaxFrameBytes-len(nativePrefix)-len(nativeSuffix)) + nativeSuffix)
	result := capabilityResult{
		Operation:      OperationFollowNext,
		ChildSessionID: "child",
		TurnToken:      "token",
		Mode:           ModeFollow,
		Frames:         []json.RawMessage{frame, frame},
		Terminal:       &Terminal{Success: true, StopReason: "end_turn", Native: native},
		More:           false,
	}
	if _, err := result.marshal(); !errors.Is(err, ErrCapacity) {
		t.Fatalf("encoded overflow error = %v, want ErrCapacity", err)
	}
}

func TestFrameQueueBudgetsEncodedResultBeforeRemovingFrames(t *testing.T) {
	t.Parallel()
	queue := newFrameQueue()
	frame := exactFrame(acp.MaxFrameBytes)
	if err := queue.push(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	fit := func(batch frameBatch) bool {
		result := capabilityResult{
			Operation: OperationFollowNext, ChildSessionID: "child", TurnToken: "token",
			Mode: ModeFollow, Frames: batch.Frames, Terminal: batch.Terminal, More: batch.More,
		}
		if batch.More {
			result.setContinuation(OperationFollowNext, "child", "token")
		}
		_, err := result.marshal()
		return err == nil
	}
	if _, err := queue.next(context.Background(), fit); !errors.Is(err, ErrCapacity) {
		t.Fatalf("unreturnable frame error = %v, want ErrCapacity", err)
	}
	// A diagnostic unbounded dequeue proves the failed encoded-budget check did
	// not remove or alter the queued frame.
	batch, err := queue.next(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Frames) != 1 || !bytes.Equal(batch.Frames[0], frame) {
		t.Fatal("encoded-budget rejection lost the queued frame")
	}
}

func TestFrameQueueReservesDynamicToolEscapingHeadroomBeforeDequeue(t *testing.T) {
	t.Parallel()
	queue := newFrameQueue()
	// This is a valid ACP frame and its unescaped capability JSON is below the
	// ACP limit. Embedding it as App Server inputText doubles its many escaped
	// backslashes, so the provider-native response would exceed the wire bound.
	frame := escapeHeavyFrame(72 << 10)
	if len(frame) >= acp.MaxFrameBytes {
		t.Fatalf("fixture frame = %d bytes", len(frame))
	}
	if err := queue.push(context.Background(), frame); err != nil {
		t.Fatal(err)
	}
	fit := func(batch frameBatch) bool {
		result := capabilityResult{
			Operation: OperationFollowNext, ChildSessionID: "child", TurnToken: "token",
			Mode: ModeFollow, Frames: batch.Frames, Terminal: batch.Terminal, More: batch.More,
		}
		if batch.More {
			result.setContinuation(OperationFollowNext, "child", "token")
		}
		_, err := result.marshal()
		return err == nil
	}
	if _, err := queue.next(context.Background(), fit); !errors.Is(err, ErrCapacity) {
		t.Fatalf("escaping overflow error = %v, want ErrCapacity", err)
	}
	batch, err := queue.next(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Frames) != 1 || !bytes.Equal(batch.Frames[0], frame) {
		t.Fatal("escaping-budget rejection removed or changed the queued frame")
	}
}

func TestQueueRejectsOversizedAndMalformedFrames(t *testing.T) {
	t.Parallel()
	queue := newFrameQueue()
	if err := queue.push(context.Background(), append(exactFrame(acp.MaxFrameBytes), ' ')); !errors.Is(err, acp.ErrFrameTooLarge) {
		t.Fatalf("oversized push error = %v", err)
	}
	if err := queue.push(context.Background(), json.RawMessage(`{"not":"json"`)); !errors.Is(err, acp.ErrMalformedFrame) {
		t.Fatalf("malformed push error = %v", err)
	}
}

func numberedFrame(sequence int) json.RawMessage {
	value, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"method":  "session/update",
		"params": map[string]any{
			"sessionId": "fixture",
			"update":    map[string]any{"sessionUpdate": "future", "sequence": sequence},
		},
	})
	return value
}

func exactFrame(size int) json.RawMessage {
	prefix := `{"jsonrpc":"2.0","method":"session/update","params":{"x":"`
	suffix := `"}}`
	return json.RawMessage(prefix + strings.Repeat("x", size-len(prefix)-len(suffix)) + suffix)
}

func escapeHeavyFrame(size int) json.RawMessage {
	prefix := `{"jsonrpc":"2.0","method":"session/update","params":{"x":"`
	suffix := `"}}`
	remaining := size - len(prefix) - len(suffix)
	if remaining%2 != 0 {
		remaining--
	}
	return json.RawMessage(prefix + strings.Repeat(`\\`, remaining/2) + suffix)
}
