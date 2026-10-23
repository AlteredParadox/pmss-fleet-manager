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
	"math"
	"sort"
	"time"

	"pmss-fleet-manager/internal/pmdb"
)

// The pmdb.fyi integration (opt-in, Settings.PMDBEnabled): every box runs
// on a shared Pulsed Media host, and the community catalogue at pmdb.fyi
// lists those hosts' hardware. When enabled, the dashboard looks each
// box's host up in the public list and, if it is missing, submits the
// hardware facts the probe already reads: hostname, CPU model, core count,
// nominal clock, RAM, swap, home filesystem size and whether an Intel GPU
// is present. Nothing about the account, the user or the dashboard goes
// out, and the catalogue is never asked to replace a row it already has.
//
// Several boxes can share a host, so the work is fleet-level: the hosts
// are deduplicated by catalogue key, one list fetch covers them all, and
// each host is decided once per process (a restart re-reads the public
// list, which is what stops a second submission).

// pmdbStatus is one host's standing with the catalogue.
type pmdbStatus struct {
	Hostname string    `json:"hostname"`
	Status   string    `json:"status"` // listed | submitted | pending | error
	Detail   string    `json:"detail,omitempty"`
	At       time.Time `json:"at"`
	BoxIDs   []int64   `json:"box_ids"`
	retryAt  time.Time
}

// PMDBView is what GET /api/pmdb returns.
type PMDBView struct {
	Enabled    bool         `json:"enabled"`
	Configured bool         `json:"configured"` // enabled with an upload key
	Hosts      []pmdbStatus `json:"hosts"`
	CheckedAt  string       `json:"checked_at,omitempty"`
	Error      string       `json:"error,omitempty"` // the last list fetch or key problem
}

type pmdbState struct {
	hosts     map[string]*pmdbStatus
	checkedAt time.Time
	err       string
	gen       uint64             // bumped by every reset; a run from an older generation stores nothing
	cancel    context.CancelFunc // the run in flight, cancelled by a reset
}

// pmdbRetry is how long a failed submission waits before another try, and
// pmdbInterval how often the loop re-checks on its own (a new box's first
// inventory wakes it sooner).
const (
	pmdbRetry    = time.Hour
	pmdbInterval = 6 * time.Hour
)

// pmdbRow builds the catalogue row for a box, or false when the inventory
// has not filled the facts the catalogue requires.
func pmdbRow(st *BoxState) (pmdb.Row, bool) {
	h := st.HostSys
	name, ok := pmdb.Normalize(st.Hostname)
	if !ok || h.CPUModel == "" || h.NCPU <= 0 || h.MemTotal <= 0 || h.HomeSize <= 0 {
		return pmdb.Row{}, false
	}
	row := pmdb.Row{
		ServerName:    name,
		ProcessorName: h.CPUModel,
		CoreCount:     h.NCPU,
		RAMGiB:        round2(float64(h.MemTotal) / (1 << 30)),
		StorageTiB:    round2(float64(h.HomeSize) / (1 << 40)),
		QSV:           h.GPUPresent && h.GPUVendor == "intel",
	}
	if h.CPUMHz > 0 {
		mhz := float64(h.CPUMHz)
		row.ProcessorMHz = &mhz
	}
	if h.SwapKnown {
		swap := round2(float64(h.SwapTotal) / (1 << 30))
		row.SwapGiB = &swap
	}
	return row, true
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// pmdbCandidates groups the current box states by catalogue key.
func (m *Manager) pmdbCandidates() map[string]pmdbCandidate {
	out := map[string]pmdbCandidate{}
	for _, st := range m.States() {
		if !st.Ready {
			continue
		}
		row, ok := pmdbRow(&st)
		if !ok {
			continue
		}
		c := out[row.ServerName]
		if c.ids == nil {
			c.row = row
		}
		c.ids = append(c.ids, st.ID)
		out[row.ServerName] = c
	}
	return out
}

// pmdbWake asks the loop to run a check soon (a fresh inventory landed, or
// the settings changed). Non-blocking: one pending wake is enough.
func (m *Manager) pmdbWake() {
	select {
	case m.pmdbWakeCh <- struct{}{}:
	default:
	}
}

func (m *Manager) pmdbLoop() {
	defer m.wg.Done()
	t := time.NewTicker(pmdbInterval)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
		case <-m.pmdbWakeCh:
			// Inventories of a fleet land within seconds of each other
			// after a start; let them, so one list fetch serves them all.
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
		}
		if s := m.Settings(); s.PMDBEnabled && s.PMDBUploadKey != "" {
			m.SyncPMDB(m.ctx)
		}
	}
}

// SyncPMDB checks every box's host against the catalogue now and submits
// the ones that are missing. Calls are serialised. A host whose submission
// failed is retried after pmdbRetry; a refused key stops the run until the
// key changes. The run belongs to the settings it started under: when the
// opt-in is withdrawn or the key replaced meanwhile (resetPMDB), its
// context is cancelled, a list it already fetched is discarded rather than
// rebuilt into state, and no further host is posted.
func (m *Manager) SyncPMDB(ctx context.Context) {
	m.pmdbSyncMu.Lock()
	defer m.pmdbSyncMu.Unlock()
	run, ok := m.pmdbBegin(ctx)
	if !ok {
		return
	}
	defer run.cancel()
	todo, cands, ok := m.pmdbPlan(run)
	if !ok {
		return
	}
	for _, name := range todo {
		if !m.pmdbCurrent(run) {
			return
		}
		sctx, scancel := context.WithTimeout(run.ctx, time.Minute)
		err := m.pmdb.Submit(sctx, run.key, cands[name].row)
		scancel()
		if m.pmdbRecord(run, name, err) {
			break
		}
	}
	m.Publish(Event{Type: "pmdb"})
}

// pmdbRun is one SyncPMDB in flight: the key it posts with, the state
// generation it started in and a context resetPMDB cancels.
type pmdbRun struct {
	ctx    context.Context
	cancel context.CancelFunc
	key    string
	gen    uint64
}

// pmdbBegin opens a run under the current settings, or false when the
// integration is off.
func (m *Manager) pmdbBegin(ctx context.Context) (*pmdbRun, bool) {
	s := m.Settings()
	if !s.PMDBEnabled || s.PMDBUploadKey == "" {
		return nil, false
	}
	run := &pmdbRun{key: s.PMDBUploadKey}
	run.ctx, run.cancel = context.WithCancel(ctx)
	m.pmdbMu.Lock()
	run.gen, m.pmdbSt.cancel = m.pmdbSt.gen, run.cancel
	m.pmdbMu.Unlock()
	return run, true
}

// pmdbCurrent reports whether the run's settings still stand: the opt-in
// is on, the key is the one it started with and no reset happened since.
// Checked before anything is stored or posted.
func (m *Manager) pmdbCurrent(run *pmdbRun) bool {
	if run.ctx.Err() != nil {
		return false
	}
	s := m.Settings()
	m.pmdbMu.Lock()
	gen := m.pmdbSt.gen
	m.pmdbMu.Unlock()
	return s.PMDBEnabled && s.PMDBUploadKey == run.key && gen == run.gen
}

type pmdbCandidate struct {
	row pmdb.Row
	ids []int64
}

// pmdbPlan fetches the catalogue list and merges it into the hosts' standing,
// returning the hosts to submit, sorted. false means nothing is to be done:
// the list could not be fetched (recorded in the view) or the run is stale.
func (m *Manager) pmdbPlan(run *pmdbRun) (todo []string, cands map[string]pmdbCandidate, ok bool) {
	cands = m.pmdbCandidates()
	now := time.Now()
	fctx, cancel := context.WithTimeout(run.ctx, 2*time.Minute)
	defer cancel()
	listed, err := m.pmdb.List(fctx)
	if !m.pmdbCurrent(run) {
		return nil, nil, false
	}
	m.pmdbMu.Lock()
	if m.pmdbSt.hosts == nil {
		m.pmdbSt.hosts = map[string]*pmdbStatus{}
	}
	if err != nil {
		m.pmdbSt.err = "catalogue list: " + err.Error()
		m.pmdbMu.Unlock()
		m.log.Warn("pmdb list", "err", err)
		m.Publish(Event{Type: "pmdb"})
		return nil, nil, false
	}
	defer m.pmdbMu.Unlock()
	m.pmdbSt.err, m.pmdbSt.checkedAt = "", now
	// Hosts whose boxes are gone drop out of the view.
	for name := range m.pmdbSt.hosts {
		if _, ok := cands[name]; !ok {
			delete(m.pmdbSt.hosts, name)
		}
	}
	for name, c := range cands {
		if m.pmdbMerge(name, c, listed[name], now) {
			todo = append(todo, name)
		}
	}
	sort.Strings(todo)
	return todo, cands, true
}

// pmdbMerge updates one host's standing from the catalogue list and reports
// whether it is to be submitted. Called under pmdbMu.
func (m *Manager) pmdbMerge(name string, c pmdbCandidate, listed bool, now time.Time) bool {
	h := m.pmdbSt.hosts[name]
	if h == nil {
		h = &pmdbStatus{Hostname: name}
		m.pmdbSt.hosts[name] = h
	}
	h.BoxIDs = append([]int64(nil), c.ids...)
	switch {
	case listed:
		if h.Status != "listed" {
			h.Status, h.Detail, h.At = "listed", "", now
		}
		return false
	case h.Status == "submitted":
		// Accepted earlier but not in the list yet: the catalogue may
		// cache; leave it, the next check will see it.
		return false
	case h.Status == "error" && now.Before(h.retryAt):
		return false // backing off
	}
	if h.Status == "" {
		h.Status, h.At = "pending", now
	}
	return true
}

// pmdbRecord notes a submission's outcome. It returns true when the rest
// of the run is pointless: the run is stale, or a refused key would refuse
// every host the same way, so one refusal stops the run and is shown once,
// at the top.
func (m *Manager) pmdbRecord(run *pmdbRun, name string, err error) (stop bool) {
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	h := m.pmdbSt.hosts[name]
	if h == nil || m.pmdbSt.gen != run.gen {
		return true // reset while this ran (opt-out or key change): the outcome belongs to nobody
	}
	now := time.Now()
	switch {
	case err == nil:
		h.Status, h.Detail, h.At = "submitted", "", now
		m.log.Info("pmdb submit", "host", name)
	case errors.Is(err, pmdb.ErrExists):
		h.Status, h.Detail, h.At = "listed", "", now
	default:
		h.Status, h.Detail, h.At, h.retryAt = "error", err.Error(), now, now.Add(pmdbRetry)
		m.log.Warn("pmdb submit", "host", name, "err", err)
		if errors.Is(err, pmdb.ErrAuth) {
			m.pmdbSt.err = "upload key refused by pmdb.fyi; check the key in settings"
			return true
		}
	}
	return false
}

// resetPMDB forgets every host's standing and cancels a run in flight, so
// a changed key or a re-enable starts from the public list again and an
// opt-out posts nothing more.
func (m *Manager) resetPMDB() {
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	if m.pmdbSt.cancel != nil {
		m.pmdbSt.cancel()
	}
	m.pmdbSt = pmdbState{gen: m.pmdbSt.gen + 1}
}

// PMDB returns the catalogue standing of every host, sorted by hostname.
func (m *Manager) PMDB() PMDBView {
	s := m.Settings()
	v := PMDBView{Enabled: s.PMDBEnabled, Configured: s.PMDBEnabled && s.PMDBUploadKey != "", Hosts: []pmdbStatus{}}
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	v.Error = m.pmdbSt.err
	if !m.pmdbSt.checkedAt.IsZero() {
		v.CheckedAt = m.pmdbSt.checkedAt.UTC().Format(time.RFC3339)
	}
	for _, h := range m.pmdbSt.hosts {
		c := *h
		c.BoxIDs = append([]int64(nil), h.BoxIDs...)
		v.Hosts = append(v.Hosts, c)
	}
	sort.Slice(v.Hosts, func(i, j int) bool { return v.Hosts[i].Hostname < v.Hosts[j].Hostname })
	if !v.Enabled {
		v.Hosts = []pmdbStatus{}
	}
	return v
}
