package collect

import "sync"

// Event is pushed to subscribers after every poll or box change.
type Event struct {
	Type    string    `json:"type"` // state | removed | alerts
	BoxID   int64     `json:"box_id,omitempty"`
	State   *BoxState `json:"state,omitempty"`
	Unacked int       `json:"alerts_unacked"` // current unacknowledged alert count, on every event
}

// Bus fans events out to subscribers; slow subscribers drop events.
type Bus struct {
	mu   sync.Mutex
	subs map[chan Event]struct{}
}

// NewBus creates a Bus.
func NewBus() *Bus { return &Bus{subs: map[chan Event]struct{}{}} }

// Subscribe returns a channel and a cancel func.
func (b *Bus) Subscribe() (<-chan Event, func()) {
	ch := make(chan Event, 64)
	b.mu.Lock()
	b.subs[ch] = struct{}{}
	b.mu.Unlock()
	return ch, func() {
		b.mu.Lock()
		if _, ok := b.subs[ch]; ok {
			delete(b.subs, ch)
			close(ch)
		}
		b.mu.Unlock()
	}
}

// Publish sends to every subscriber without blocking.
func (b *Bus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
		}
	}
}
