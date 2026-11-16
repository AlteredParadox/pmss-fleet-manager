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

// Longer-term box history: hourly disk, iowait and host CPU figures kept
// for a month (store.HistoryKeep), to tell a box that is always slow from
// one having a bad hour. Traffic needs no store of its own: PMSS keeps a
// daily series for the same 30 days, read every poll (TrafficState.Daily).

import (
	"context"
	"fmt"
	"slices"
	"time"

	"pmss-fleet-manager/internal/store"
)

// historyFlushEvery bounds what a restart loses of the open hour.
const historyFlushEvery = 5 * time.Minute

// historyAcc is the open hour of one poller, not yet written. Only the
// poll goroutine touches it.
type historyAcc struct {
	open    bool
	d       store.HistoryDelta
	flushed time.Time
}

// historyAdd counts one successful poll into its hour and writes the
// hour when it is complete (the next poll is in another hour, or the box
// moved to another generation) or historyFlushEvery after the last write.
func (p *poller) historyAdd(ctx context.Context, st *BoxState, at time.Time) {
	hour, gen := at.UTC().Truncate(time.Hour), st.generation()
	h := &p.hist
	if h.open && (!h.d.Hour.Equal(hour) || h.d.Gen != gen) {
		p.historyFlush(ctx)
		h.open = false
	}
	if !h.open {
		h.d = store.HistoryDelta{Hour: hour, Gen: gen, DiskUsed: -1, DiskQuota: -1, FilesUsed: -1}
		h.open, h.flushed = true, at
	}
	addToDelta(&h.d, st)
	if at.Sub(h.flushed) >= historyFlushEvery {
		p.historyFlush(ctx)
		h.flushed = at
	}
}

// addToDelta adds one poll's figures. Unknown ones are left out, never
// counted as zero: a poll without a host CPU reading (the first after a
// start or a reboot) adds to the poll count only.
func addToDelta(d *store.HistoryDelta, st *BoxState) {
	if st.Disk.OK && st.Disk.QuotaBytes > 0 {
		d.DiskUsed, d.DiskQuota = st.Disk.UsedBytes, st.Disk.QuotaBytes
		if st.Disk.FilesUsed > 0 {
			d.FilesUsed = st.Disk.FilesUsed
		}
	}
	if st.HostSys.CPUOK {
		d.IOSum += st.HostSys.CPUIOWaitPct
		d.IOMax = max(d.IOMax, st.HostSys.CPUIOWaitPct)
		d.BusySum += st.HostSys.CPUBusyPct
		d.CPUN++
	}
	d.Polls++
}

// historyFlush writes the open hour's sums and starts them over; the
// row keeps them (store.AddHistory adds). A failed write keeps them for
// the next attempt.
func (p *poller) historyFlush(ctx context.Context) {
	h := &p.hist
	if !h.open || h.d.Polls == 0 {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := p.m.st.AddHistory(wctx, p.box.ID, h.d); err != nil {
		p.m.log.Warn("history", "box", p.box.Name, "err", err)
		return
	}
	h.d = store.HistoryDelta{Hour: h.d.Hour, Gen: h.d.Gen, DiskUsed: -1, DiskQuota: -1, FilesUsed: -1}
}

// HistoryPoint is one hour of a box's history. A nil field is unknown for
// that hour (no successful poll measured it), not zero.
type HistoryPoint struct {
	T          time.Time `json:"t"` // the hour's start, UTC
	DiskUsed   *int64    `json:"disk_used"`
	DiskQuota  *int64    `json:"disk_quota"`
	FilesUsed  *int64    `json:"files_used"`
	IOWaitAvg  *float64  `json:"iowait_avg"` // mean of the polls' host iowait
	IOWaitPeak *float64  `json:"iowait_peak"`
	BusyAvg    *float64  `json:"busy_avg"`
	Polls      int       `json:"polls"`
	cpuN       int       // polls with a CPU reading: the averages' weight
}

// HistorySummary condenses a window of hours.
type HistorySummary struct {
	Hours int `json:"hours"` // hours with at least one poll
	// IOWaitAvg is the mean over every poll in the window with a CPU
	// reading; IOWaitHourMax the worst hour's mean; IOWaitBadHours how
	// many hours averaged above IOWaitBadPct.
	IOWaitAvg      *float64 `json:"iowait_avg"`
	IOWaitHourMax  *float64 `json:"iowait_hour_max"`
	IOWaitBadHours int      `json:"iowait_bad_hours"`
	BusyAvg        *float64 `json:"busy_avg"`
	// DiskGrowth is the change in disk used from the window's first known
	// hour to its last (nil with fewer than two).
	DiskGrowth *int64 `json:"disk_growth"`
}

// IOWaitBadPct is the hourly iowait mean above which an hour counts as a
// slow one in HistorySummary.
const IOWaitBadPct = 10.0

// BoxHistory is the answer of GET /api/boxes/{id}/history.
type BoxHistory struct {
	Days    int            `json:"days"`
	Since   time.Time      `json:"since"`
	Points  []HistoryPoint `json:"points"`
	Summary HistorySummary `json:"summary"`
	// Traffic is PMSS's own daily series (30 days), as on the traffic tab.
	Traffic []DayPoint `json:"traffic"`
}

// HistoryDays are the windows the history can be read in.
var HistoryDays = []int{7, 30}

// BoxHistory reads box id's last days of history, of its current
// generation only.
func (m *Manager) BoxHistory(ctx context.Context, id int64, days int) (BoxHistory, error) {
	if !slices.Contains(HistoryDays, days) {
		return BoxHistory{}, fmt.Errorf("history window of %d days: only 7 and 30 are kept", days)
	}
	p := m.poller(id)
	if p == nil {
		return BoxHistory{}, store.ErrNotFound
	}
	since := time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(days) * 24 * time.Hour)
	rows, err := m.st.History(ctx, id, since)
	if err != nil {
		return BoxHistory{}, err
	}
	h := BoxHistory{Days: days, Since: since, Points: historyPoints(rows, p.sameGeneration), Traffic: []DayPoint{}}
	h.Summary = summarize(h.Points)
	if st, ok := m.State(id); ok && st.Traffic.OK {
		h.Traffic = st.Traffic.Daily
	}
	return h, nil
}

// historyPoints turns rows into points, keeping those of generations
// same accepts and merging rows of one hour (two generations that both
// match: the same endpoint before and after its key was pinned).
func historyPoints(rows []store.HistoryRow, same func(string) bool) []HistoryPoint {
	type acc struct {
		row  store.HistoryRow
		disk store.HistoryRow // the row whose disk figures are used
	}
	var order []time.Time
	by := map[time.Time]*acc{}
	for _, r := range rows {
		if !same(r.Gen) {
			continue
		}
		a := by[r.Hour]
		if a == nil {
			a = &acc{row: r, disk: r}
			by[r.Hour] = a
			order = append(order, r.Hour)
			continue
		}
		a.row.IOSum += r.IOSum
		a.row.IOMax = max(a.row.IOMax, r.IOMax)
		a.row.BusySum += r.BusySum
		a.row.CPUN += r.CPUN
		a.row.Polls += r.Polls
		if r.DiskUsed >= 0 {
			a.disk = r
		}
	}
	out := make([]HistoryPoint, 0, len(order))
	for _, t := range order {
		a := by[t]
		pt := HistoryPoint{T: t, Polls: a.row.Polls, cpuN: a.row.CPUN}
		if a.disk.DiskUsed >= 0 {
			pt.DiskUsed = new(a.disk.DiskUsed)
		}
		if a.disk.DiskQuota >= 0 {
			pt.DiskQuota = new(a.disk.DiskQuota)
		}
		if a.disk.FilesUsed >= 0 {
			pt.FilesUsed = new(a.disk.FilesUsed)
		}
		if n := float64(a.row.CPUN); n > 0 {
			pt.IOWaitAvg, pt.IOWaitPeak, pt.BusyAvg = new(a.row.IOSum/n), new(a.row.IOMax), new(a.row.BusySum/n)
		}
		out = append(out, pt)
	}
	return out
}

// summarize condenses points (oldest first).
func summarize(pts []HistoryPoint) HistorySummary {
	var s HistorySummary
	var ioSum, busySum float64
	var n int
	var first, last *int64
	for _, pt := range pts {
		if pt.Polls > 0 {
			s.Hours++
		}
		if pt.DiskUsed != nil {
			if first == nil {
				first = pt.DiskUsed
			}
			last = pt.DiskUsed
		}
		if pt.IOWaitAvg == nil {
			continue
		}
		// Weighted by the hour's readings: polls are evenly spaced.
		w := float64(pt.cpuN)
		ioSum += *pt.IOWaitAvg * w
		busySum += *pt.BusyAvg * w
		n += pt.cpuN
		if s.IOWaitHourMax == nil || *pt.IOWaitAvg > *s.IOWaitHourMax {
			s.IOWaitHourMax = new(*pt.IOWaitAvg)
		}
		if *pt.IOWaitAvg > IOWaitBadPct {
			s.IOWaitBadHours++
		}
	}
	if n > 0 {
		s.IOWaitAvg, s.BusyAvg = new(ioSum/float64(n)), new(busySum/float64(n))
	}
	if first != nil && last != nil && first != last {
		s.DiskGrowth = new(*last - *first)
	}
	return s
}
