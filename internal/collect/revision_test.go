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

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"pmss-fleet-manager/internal/pmapi"
	"pmss-fleet-manager/internal/store"
)

func revTestManager(t *testing.T, boxes int) (*Manager, []*store.Box) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	var out []*store.Box
	for i := 0; i < boxes; i++ {
		b := &store.Box{Name: string(rune('a' + i)), Host: "h.example", User: "u", Port: 22, Enabled: false}
		if err := m.AddBox(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		out = append(out, b)
	}
	return m, out
}

// Every store, every hook evaluation and every state event happens in one
// fleet-wide revision order, however many goroutines commit at once, and a
// snapshot's revision bounds the states in it (external review 3, finding
// 6; the client relies on this to merge a snapshot with the stream).
func TestCommitsLeaveInRevisionOrder(t *testing.T) {
	ctx := context.Background()
	m, boxes := revTestManager(t, 3)
	var hookMu sync.Mutex
	var hookRevs []uint64
	m.OnPoll(func(_ context.Context, s *BoxState) {
		hookMu.Lock()
		hookRevs = append(hookRevs, s.Rev)
		hookMu.Unlock()
	})
	ch, cancel := m.Bus().Subscribe()
	defer cancel()
	var got []Event
	gotDone := make(chan struct{})
	go func() {
		defer close(gotDone)
		for ev := range ch {
			if ev.Type == "state" || ev.Type == "removed" {
				got = append(got, ev)
			}
		}
	}()
	var wg sync.WaitGroup
	for _, b := range boxes {
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(id int64, i int) {
				defer wg.Done()
				if i%2 == 0 {
					if err := m.SetAppWatch(ctx, id, "other:x", i%4 == 0); err != nil {
						t.Error(err)
					}
				} else {
					m.mutateState(id, func(s *BoxState) { s.Failures++ })
				}
				m.reattachServices()
			}(b.ID, i)
		}
	}
	wg.Wait()
	states, rev := m.Snapshot()
	for _, s := range states {
		if s.Rev == 0 || s.Rev > rev {
			t.Fatalf("state rev %d outside the snapshot's %d", s.Rev, rev)
		}
	}
	if err := m.RemoveBox(ctx, boxes[0].ID); err != nil {
		t.Fatal(err)
	}
	cancel()
	<-gotDone
	var last uint64
	for _, ev := range got {
		if ev.Rev <= last {
			t.Fatalf("events out of order: rev %d after %d (%s)", ev.Rev, last, ev.Type)
		}
		if ev.Type == "state" && ev.State.Rev != ev.Rev {
			t.Fatalf("event rev %d carries state rev %d", ev.Rev, ev.State.Rev)
		}
		last = ev.Rev
	}
	if got[len(got)-1].Type != "removed" || got[len(got)-1].Rev <= rev {
		t.Fatalf("removal must be the last event and above the snapshot: %+v", got[len(got)-1])
	}
	hookMu.Lock()
	defer hookMu.Unlock()
	last = 0
	for _, r := range hookRevs {
		if r <= last {
			t.Fatalf("hooks ran out of order: rev %d after %d", r, last)
		}
		last = r
	}
	if len(hookRevs) == 0 {
		t.Fatal("no hook ran")
	}
}

// A state committed while the services feed is being replaced is matched
// against the feed of the moment it is stored, never against the one the
// swap retired: after the last swap and reattach, every box carries the
// final feed's service (external review 3, finding 5).
func TestFeedSwapNeverLeavesStaleService(t *testing.T) {
	m, boxes := revTestManager(t, 2)
	feeds := make([]*pmapi.Feed, 2)
	for i, raw := range []string{
		`{"client_id":1,"currency":"EUR","services":[{"id":11,"product":"A","domain":"h.example","status":"Active","recurring_amount":"1","billing_cycle":"Monthly"}]}`,
		`{"client_id":1,"currency":"EUR","services":[{"id":22,"product":"B","domain":"h.example","status":"Active","recurring_amount":"2","billing_cycle":"Monthly"}]}`,
	} {
		f, err := pmapi.Parse([]byte(raw))
		if err != nil {
			t.Fatal(err)
		}
		feeds[i] = f
	}
	swap := func(f *pmapi.Feed) {
		m.svcMu.Lock()
		m.svc = servicesCache{feed: f, syncedAt: time.Now()}
		m.svcMu.Unlock()
		m.reattachServices()
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	for _, b := range boxes {
		wg.Add(1)
		go func(id int64) {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				m.mutateState(id, func(s *BoxState) { s.Failures++ })
			}
		}(b.ID)
	}
	for i := 0; i < 200; i++ {
		swap(feeds[i%2])
	}
	final := feeds[1]
	swap(final)
	close(stop)
	wg.Wait()
	for _, s := range m.States() {
		if s.Service == nil || s.Service.ID != final.Services[0].ID {
			t.Fatalf("box %s matched against a retired feed: %+v", s.Name, s.Service)
		}
	}
	// And a commit after the swap sees the swapped feed on its own.
	swap(feeds[0])
	m.mutateState(boxes[0].ID, func(s *BoxState) { s.Service = nil })
	if s, _ := m.State(boxes[0].ID); s.Service == nil || s.Service.ID != feeds[0].Services[0].ID {
		t.Fatalf("commit did not decorate against the live feed: %+v", s.Service)
	}
}

// A commit for a box that was removed meanwhile stores nothing, and a du
// merge never resurrects it either.
func TestCommitAfterRemoveStoresNothing(t *testing.T) {
	m, boxes := revTestManager(t, 1)
	if err := m.RemoveBox(context.Background(), boxes[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := m.commit(boxes[0].ID, func(s *BoxState) *BoxState { return &BoxState{ID: boxes[0].ID} }, commitOpts{publish: true}); ok {
		t.Fatal("commit stored a removed box")
	}
	if states, _ := m.Snapshot(); len(states) != 0 {
		t.Fatalf("removed box is back: %+v", states)
	}
}

// Removing a box deletes its alerts with it; the removed event goes out
// in revision order (before the row delete), so a second tab learns the
// corrected alert count from an alerts event after the delete has landed.
func TestRemoveBoxNotifiesAlertsAfterDelete(t *testing.T) {
	ctx := context.Background()
	m, boxes := revTestManager(t, 1)
	if _, err := m.st.RaiseAlert(ctx, boxes[0].ID, "k", "bad", "t"); err != nil {
		t.Fatal(err)
	}
	ch, cancel := m.Bus().Subscribe()
	defer cancel()
	if err := m.RemoveBox(ctx, boxes[0].ID); err != nil {
		t.Fatal(err)
	}
	var got []Event
	for len(got) < 2 {
		select {
		case ev := <-ch:
			got = append(got, ev)
		case <-time.After(5 * time.Second):
			t.Fatalf("after removal: %+v", got)
		}
	}
	if got[0].Type != "removed" || got[0].Unacked != 1 {
		t.Fatalf("first event: %+v", got[0])
	}
	if got[1].Type != "alerts" || got[1].Unacked != 0 {
		t.Fatalf("second event must report the cascaded alerts: %+v", got[1])
	}
}

// Every event and every snapshot read carries a higher alert revision
// than the last, with the count read under that revision, so a client
// keeping the highest-revision count can never regress to an older one,
// however the stream and late snapshots interleave (GPT audit 2026-10-22
// finding 4; Codex on PR 103 for the snapshot reads taking their own).
func TestAlertRevisionOrdersCounts(t *testing.T) {
	ctx := context.Background()
	m, boxes := revTestManager(t, 1)
	ch, cancel := m.Bus().Subscribe()
	defer cancel()
	next := func() Event {
		select {
		case ev := <-ch:
			return ev
		case <-time.After(5 * time.Second):
			t.Fatal("no event")
		}
		return Event{}
	}
	if _, err := m.st.RaiseAlert(ctx, boxes[0].ID, "disk", "warn", "disk"); err != nil {
		t.Fatal(err)
	}
	m.Publish(Event{Type: "alerts"})
	ev := next()
	n, rev := m.AlertsView(ctx) // a read after the event: its own, higher revision
	if ev.Unacked != 1 || ev.AlertsRev == 0 || n != 1 || rev != ev.AlertsRev+1 {
		t.Fatalf("after raise: event %+v view %d@%d", ev, n, rev)
	}
	// A read before a change and a read after it: the later read has the
	// higher revision, so the earlier one cannot restore its count if it
	// lands last; the announcement is higher still.
	if err := m.st.AckAll(ctx); err != nil {
		t.Fatal(err)
	}
	if n, r := m.AlertsView(ctx); n != 0 || r != rev+1 {
		t.Fatalf("after ack, before publish: %d@%d, want 0@%d", n, r, rev+1)
	}
	m.Publish(Event{Type: "alerts"})
	if ev = next(); ev.Unacked != 0 || ev.AlertsRev != rev+2 {
		t.Fatalf("after publish: %+v, want 0@%d", ev, rev+2)
	}
	// Concurrent publishers: revisions on the bus are strictly increasing.
	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			m.Publish(Event{Type: "alerts"})
		}()
	}
	wg.Wait()
	last := ev.AlertsRev
	for i := 0; i < 20; i++ {
		ev = next()
		if ev.AlertsRev <= last {
			t.Fatalf("revision %d after %d", ev.AlertsRev, last)
		}
		last = ev.AlertsRev
	}
}
