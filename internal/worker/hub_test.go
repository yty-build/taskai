package worker

import (
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

func TestHubCoalescesViewerWakeupsWithoutRetainingFrames(t *testing.T) {
	hub := newHub(4)
	id, cutoff, wake, ok := hub.add()
	if !ok || cutoff != 4 {
		t.Fatalf("add = id %d cutoff %d ok %v", id, cutoff, ok)
	}
	for sequence := uint64(5); sequence <= 10_000; sequence++ {
		hub.broadcast(sequence)
	}
	if got := len(wake); got != 1 {
		t.Fatalf("coalesced wake count = %d, want 1", got)
	}
	if got := hub.sequence(); got != 10_000 {
		t.Fatalf("latest sequence = %d", got)
	}
	<-wake
	hub.broadcast(10_001)
	if got := len(wake); got != 1 {
		t.Fatalf("wake count after drain = %d, want 1", got)
	}
	hub.remove(id)
	if len(hub.subscribers) != 0 {
		t.Fatalf("subscriber was not removed: %#v", hub.subscribers)
	}
}

func TestHubBoundsConcurrentViewers(t *testing.T) {
	hub := newHub(0)
	for index := 0; index < maxHubSubscribers; index++ {
		if _, _, _, ok := hub.add(); !ok {
			t.Fatalf("subscriber %d was unexpectedly rejected", index)
		}
	}
	if _, _, _, ok := hub.add(); ok {
		t.Fatalf("subscriber %d exceeded bounded viewer capacity", maxHubSubscribers+1)
	}
}

func TestSubscriberWriteDeadlineShedsNonReadingViewer(t *testing.T) {
	log, err := sessionlog.Open(filepath.Join(t.TempDir(), "S-slow.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	if _, err := log.Append("", FrameConnection, map[string]string{"state": "connected"}); err != nil {
		t.Fatal(err)
	}
	hub := newHub(log.LastSequence())
	worker := &Worker{log: log, hub: hub, subscriberWriteTimeout: 20 * time.Millisecond}
	server, client := net.Pipe()
	defer client.Close()
	done := make(chan struct{})
	go func() {
		worker.subscribe(context.Background(), server, 0)
		_ = server.Close()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("non-reading viewer was not shed after its write deadline")
	}
	hub.mu.Lock()
	defer hub.mu.Unlock()
	if len(hub.subscribers) != 0 {
		t.Fatalf("shed viewer retained a subscriber slot: %#v", hub.subscribers)
	}
}
