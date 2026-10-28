// pmss-fleet-manager — a single-pane dashboard for Pulsed Media seedboxes.
// Copyright (C) 2026 AlteredParadox
//
// This program is free software: you can redistribute it and/or modify it
// under the terms of the GNU Affero General Public License as published by
// the Free Software Foundation, either version 3 of the License, or (at your
// option) any later version.
//
// This program is distributed in the hope that it will be useful, but WITHOUT
// ANY WARRANTY; without even the implied warranty of MERCHANTABILITY or
// FITNESS FOR A PARTICULAR PURPOSE. See the GNU Affero General Public License
// for more details.
//
// You should have received a copy of the GNU Affero General Public License
// along with this program. If not, see <https://www.gnu.org/licenses/>.

package collect

import "sync"

// Event is pushed to subscribers after every poll or box change.
type Event struct {
	Type    string    `json:"type"` // state | removed | alerts | services | settings | pmdb | transfers
	BoxID   int64     `json:"box_id,omitempty"`
	Rev     uint64    `json:"rev,omitempty"` // state and removed: the fleet-wide revision this change was made at
	State   *BoxState `json:"state,omitempty"`
	Unacked int       `json:"alerts_unacked"` // current unacknowledged alert count, on every event
	// AlertsRev orders the counts: every event carries a higher one than
	// the last, and a state snapshot carries the one its count was read
	// under, so a client keeps the count with the highest revision whether
	// it came by the stream or by a snapshot that landed late (see
	// Manager.Publish and Manager.AlertsView).
	AlertsRev uint64 `json:"alerts_rev"`
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

// Publish sends to every subscriber without blocking. A subscriber whose
// buffer is full is dropped and its channel closed: silently losing an
// event would leave that client's view stale until the next poll, whereas
// ending the stream makes the browser reconnect and resync everything.
func (b *Bus) Publish(e Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for ch := range b.subs {
		select {
		case ch <- e:
		default:
			delete(b.subs, ch)
			close(ch)
		}
	}
}
