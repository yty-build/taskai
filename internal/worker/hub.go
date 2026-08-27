package worker

import (
	"sync"
)

const maxHubSubscribers = 16

type subscriber struct {
	wake chan struct{}
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

func (h *hub) add() (uint64, uint64, <-chan struct{}, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return 0, h.lastSeq, nil, false
	}
	if len(h.subscribers) >= maxHubSubscribers {
		return 0, h.lastSeq, nil, false
	}
	h.nextID++
	// The durable JSONL is the payload queue. The hub retains only one
	// coalescing wake-up per viewer, so slow or staggered viewers cannot pin
	// provider frame bytes in memory.
	subscriber := &subscriber{wake: make(chan struct{}, 1)}
	h.subscribers[h.nextID] = subscriber
	return h.nextID, h.lastSeq, subscriber.wake, true
}

func (h *hub) remove(id uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.subscribers, id)
}

func (h *hub) broadcast(sequence uint64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.lastSeq = sequence
	for _, subscriber := range h.subscribers {
		select {
		case subscriber.wake <- struct{}{}:
		default:
			// A wake-up is already pending. The subscriber reads the latest
			// sequence from the hub and walks the durable suffix once awake.
		}
	}
}

func (h *hub) sequence() uint64 {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.lastSeq
}

func (h *hub) close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closed {
		return
	}
	h.closed = true
	for id, subscriber := range h.subscribers {
		close(subscriber.wake)
		delete(h.subscribers, id)
	}
}
