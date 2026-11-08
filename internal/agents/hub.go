package agents

import "sync"

// hub wakes the inbox and outbox calls waiting on an agent's tasks.
type hub struct {
	mu   sync.Mutex
	subs map[string]map[chan struct{}]struct{}
}

func newHub() *hub { return &hub{subs: map[string]map[chan struct{}]struct{}{}} }

func (h *hub) subscribe(keyID string) (<-chan struct{}, func()) {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	if h.subs[keyID] == nil {
		h.subs[keyID] = map[chan struct{}]struct{}{}
	}
	h.subs[keyID][ch] = struct{}{}
	h.mu.Unlock()
	return ch, func() {
		h.mu.Lock()
		delete(h.subs[keyID], ch)
		if len(h.subs[keyID]) == 0 {
			delete(h.subs, keyID)
		}
		h.mu.Unlock()
	}
}

// wake never blocks: a waiter that has not read its last wake-up needs no second.
func (h *hub) wake(keyID string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs[keyID] {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}
