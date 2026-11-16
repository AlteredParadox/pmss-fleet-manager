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
	if sm.Hours != 2 || *sm.DiskGrowth != 20 || *sm.IOWaitHourMax != 30 || sm.IOWaitBadHours != 1 || *sm.IOWaitAvg != (4+12+30)/3.0 {
		t.Fatalf("summary %+v", sm)
	}
	if _, err := m.BoxHistory(ctx, b.ID, 5); err == nil {
		t.Fatal("a 5-day window")
	}
	if _, err := m.BoxHistory(ctx, 9999, 7); err == nil {
		t.Fatal("an unknown box")
	}
}

func TestSummarizeUnknowns(t *testing.T) {
	s := summarize([]HistoryPoint{{Polls: 2}, {Polls: 1}})
	if s.Hours != 2 || s.IOWaitAvg != nil || s.IOWaitHourMax != nil || s.DiskGrowth != nil || s.BusyAvg != nil {
		t.Fatalf("nothing measured is not zero: %+v", s)
	}
	one := int64(5)
	if s := summarize([]HistoryPoint{{Polls: 1, DiskUsed: &one}}); s.DiskGrowth != nil {
		t.Fatalf("one disk reading is no growth: %+v", s)
	}
	if s := summarize(nil); s.Hours != 0 {
		t.Fatalf("empty %+v", s)
	}
}
