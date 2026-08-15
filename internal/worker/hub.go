package worker

import (
	"sync"

	"github.com/yty-build/acp-gateway/internal/sessionlog"
)

type subscriber struct {
	lines chan sessionlog.Line
}

type hub struct {
	mu          sync.Mutex
	nextID      uint64
	lastSeq     uint64
	subscribers map[uint64]*subscriber
	closed      bool
}

func newHub(lastSequence uint64) *hub {
	return &hub{lastSeq: lastSequence, subscribers: make(map[uint64]*subscriber)}
}

func (h *hub) add() (uint64, uint64, <-chan sessionlog.Line, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, h.lastSeq, nil, false
	}
	h.nextID++
	subscriber := &subscriber{lines: make(chan sessionlog.Line, 256)}
	h.subscribers[h.nextID] = subscriber
	return h.nextID, h.lastSeq, subscriber.lines, true
}

func (h *hub) remove(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subscribers, id)
}

func (h *hub) broadcast(line sessionlog.Line) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.lastSeq = line.Seq
	for id, subscriber := range h.subscribers {
		select {
		case subscriber.lines <- line:
		default:
			close(subscriber.lines)
			delete(h.subscribers, id)
		}
	}
}

func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, subscriber := range h.subscribers {
		close(subscriber.lines)
		delete(h.subscribers, id)
	}
}
