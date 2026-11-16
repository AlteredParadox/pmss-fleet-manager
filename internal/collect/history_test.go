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
	"errors"
	"testing"
	"time"

	"pmss-fleet-manager/internal/store"
)

// The poller counts each successful poll into its hour and writes the
// hour every historyFlushEvery and when the next hour begins; unknown
// figures are left out, never counted as zero; the reader keeps the
// current generation only.
func TestHistoryHourlyRows(t *testing.T) {
	ctx := context.Background()
	m, st, b := genTestManager(t)
	p := m.poller(b.ID)
	hour := time.Now().UTC().Truncate(time.Hour).Add(-2 * time.Hour)
	poll := func(at time.Time, used int64, cpu bool, io float64) {
		t.Helper()
		s := BoxState{Host: b.Host, Port: b.Port, User: b.User}
		s.Disk = DiskState{OK: used >= 0, UsedBytes: used, QuotaBytes: 1000, FilesUsed: 7}
		s.HostSys.CPUOK, s.HostSys.CPUIOWaitPct, s.HostSys.CPUBusyPct = cpu, io, 50
		p.historyAdd(ctx, &s, at)
	}
	rows := func() []store.HistoryRow {
		t.Helper()
		r, err := st.History(ctx, b.ID, hour.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	poll(hour.Add(time.Minute), 100, true, 4)
	if r := rows(); len(r) != 0 {
		t.Fatalf("written before the flush interval: %+v", r)
	}
	poll(hour.Add(7*time.Minute), 110, true, 12) // flushes the two
	if r := rows(); len(r) != 1 || r[0].Polls != 2 || r[0].DiskUsed != 110 || r[0].IOSum != 16 || r[0].IOMax != 12 {
		t.Fatalf("after the flush: %+v", r)
	}
	poll(hour.Add(8*time.Minute), -1, false, 0) // no disk, no cpu reading: a poll, nothing else
	poll(hour.Add(time.Hour+time.Minute), 130, true, 30)
	r := rows()
	if len(r) != 1 || r[0].Polls != 3 || r[0].CPUN != 2 || r[0].DiskUsed != 110 {
		t.Fatalf("the hour closes when the next begins: %+v", r)
	}
	p.historyFlush(ctx)
	// Another machine's hour under the same record: never shown.
	st.AddHistory(ctx, b.ID, store.HistoryDelta{Hour: hour, Gen: "elsewhere:22/u@SHA256:x", DiskUsed: 9999, DiskQuota: -1, FilesUsed: -1, IOSum: 90, IOMax: 90, CPUN: 1, Polls: 1})

	h, err := m.BoxHistory(ctx, b.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if len(h.Points) != 2 || h.Days != 7 || h.Traffic == nil {
		t.Fatalf("history %+v", h)
	}
	p0, p1 := h.Points[0], h.Points[1]
	if *p0.DiskUsed != 110 || *p0.IOWaitAvg != 8 || *p0.IOWaitPeak != 12 || *p0.BusyAvg != 50 || p0.Polls != 3 || *p0.FilesUsed != 7 {
		t.Fatalf("first hour %+v", p0)
	}
	if *p1.DiskUsed != 130 || *p1.IOWaitAvg != 30 {
		t.Fatalf("second hour %+v", p1)
	}
	sm := h.Summary
	if sm.Hours != 2 || sm.IOWaitHours != 2 || *sm.DiskGrowth != 20 || *sm.IOWaitHourMax != 30 || sm.IOWaitBadHours != 1 || sm.IOWaitSlowPct != 20 || *sm.IOWaitAvg != (4+12+30)/3.0 {
		t.Fatalf("summary %+v", sm)
	}
	// The slow-hour threshold is the setting's: at 5 % both hours count,
	// at 30 % neither (exactly the threshold is not over it).
	for pct, want := range map[int]int{5: 2, 30: 0} {
		st := m.Settings()
		st.IOWaitSlowPct = pct
		m.SetSettings(st)
		if h, err := m.BoxHistory(ctx, b.ID, 7); err != nil || h.Summary.IOWaitBadHours != want || h.Summary.IOWaitSlowPct != pct {
			t.Fatalf("threshold %d %%: %+v %v", pct, h.Summary, err)
		}
	}
	fleet := m.HistorySummaries(ctx, 30)
	if fleet.Days != 30 || len(fleet.Boxes) != 1 || fleet.Boxes[b.ID].Hours != 2 {
		t.Fatalf("fleet summaries %+v", fleet)
	}
	if bad := m.HistorySummaries(ctx, 1); len(bad.Boxes) != 0 {
		t.Fatalf("a window that is not kept summarizes nothing: %+v", bad)
	}
	if _, err := m.BoxHistory(ctx, b.ID, 5); err == nil {
		t.Fatal("a 5-day window")
	}
	if _, err := m.BoxHistory(ctx, 9999, 7); err == nil {
		t.Fatal("an unknown box")
	}
}

func TestSummarizeUnknowns(t *testing.T) {
	s := summarize([]HistoryPoint{{Polls: 2}, {Polls: 1}}, 20)
	if s.Hours != 2 || s.IOWaitHours != 0 || s.IOWaitAvg != nil || s.IOWaitHourMax != nil || s.DiskGrowth != nil || s.BusyAvg != nil {
		t.Fatalf("nothing measured is not zero: %+v", s)
	}
	one := int64(5)
	if s := summarize([]HistoryPoint{{Polls: 1, DiskUsed: &one}}, 20); s.DiskGrowth != nil {
		t.Fatalf("one disk reading is no growth: %+v", s)
	}
	if s := summarize(nil, 20); s.Hours != 0 {
		t.Fatalf("empty %+v", s)
	}
}

// A write that fails keeps the hour: a closed hour waits in the backlog
// (capped) and goes in with the next write that works, in order.
func TestHistoryFailedWritesAreRetried(t *testing.T) {
	ctx := context.Background()
	m, st, b := genTestManager(t)
	p := m.poller(b.ID)
	failing := true
	real := historyWrite
	historyWrite = func(ctx context.Context, s *store.Store, id int64, d store.HistoryDelta) error {
		if failing {
			return errors.New("disk full")
		}
		return real(ctx, s, id, d)
	}
	t.Cleanup(func() { historyWrite = real })
	hour := time.Now().UTC().Truncate(time.Hour).Add(-30 * time.Hour)
	poll := func(at time.Time) {
		s := BoxState{Host: b.Host, Port: b.Port, User: b.User}
		s.HostSys.CPUOK, s.HostSys.CPUIOWaitPct = true, 1
		p.historyAdd(ctx, &s, at)
	}
	// 27 hours of one poll each, every write failing: the backlog keeps
	// the latest historyBacklogMax closed hours.
	for i := 0; i < 27; i++ {
		poll(hour.Add(time.Duration(i) * time.Hour))
	}
	if n := len(p.hist.backlog); n != historyBacklogMax {
		t.Fatalf("backlog %d", n)
	}
	failing = false
	poll(hour.Add(27 * time.Hour)) // closes hour 26 and writes everything held
	rows, err := st.History(ctx, b.ID, hour.Add(-time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != historyBacklogMax+1 || !rows[0].Hour.Equal(hour.Add(2*time.Hour)) || p.hist.backlog != nil {
		t.Fatalf("rows %d, first %v, backlog %d", len(rows), rows[0].Hour, len(p.hist.backlog))
	}
	for _, r := range rows {
		if r.Polls != 1 || r.CPUN != 1 {
			t.Fatalf("hour %v written twice or partly: %+v", r.Hour, r)
		}
	}
}
