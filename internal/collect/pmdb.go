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
// list, which is what stops a second submission). An inventory wake
// fetches the list only when a host still needs deciding and then at most
// once per pmdbListInterval; the six-hourly timer and a check the user
// asks for always fetch (unless a hold applies), so the standing shown is
// never older than that and the automatic cost is bounded whatever the
// catalogue sends.

// pmdbStatus is one host's standing with the catalogue.
type pmdbStatus struct {
	Hostname string    `json:"hostname"`
	Status   string    `json:"status"` // listed | submitted | pending | error
	Detail   string    `json:"detail,omitempty"`
	At       time.Time `json:"at"`
	BoxIDs   []int64   `json:"box_ids"`
	retryAt  time.Time
	// decided is set once this process has settled the host (seen listed,
	// accepted, or told it exists) and is never cleared short of a reset:
	// whatever a later list says, the host is not submitted again. A
	// catalogue that alternated its answers could otherwise make every
	// check resubmit the whole fleet.
	decided bool
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
	// keyRefused holds every later run once the catalogue refused the key:
	// retrying a rejected key host by host is pointless and noisy. Only a
	// reset (a new key, a re-enable) lifts it.
	keyRefused bool
	// holdUntil holds every later run after a rate limit (which is per
	// address, not per host) or a service failure, until it passes.
	holdUntil time.Time
	// listed is the last list fetched and listedAt when: an automatic run
	// within pmdbListInterval of it decides from this copy instead of
	// asking again, so inventories landing every few minutes across the
	// fleet cannot be turned into a list fetch each (a valid list is a
	// success and never holds, and the catalogue chooses its size).
	listed   map[string]bool
	listedAt time.Time
}

// pmdbSubmitLog is the log message of every submission outcome.
const pmdbSubmitLog = "pmdb submit"

// pmdbRetry is how long a failed submission waits before another try, and
// pmdbInterval how often the loop fetches the list afresh on its own (a
// new box's first inventory wakes it sooner, but such a wake fetches only
// when a host needs deciding).
const (
	pmdbRetry    = time.Hour
	pmdbInterval = 6 * time.Hour
)

// pmdbListInterval is the least time between two automatic list fetches;
// a check the user asks for always fetches (a var for the tests).
var pmdbListInterval = 10 * time.Minute

// pmdbMaxSubmits bounds the submissions of one run: a fleet has a few
// dozen hosts at most, and a catalogue that made every one look missing
// should not get more than this out of a single check.
const pmdbMaxSubmits = 50

// pmdbRunBudget bounds one whole run, list fetch and every submission: a
// catalogue that answers slowly cannot hold the loop for long (a var for
// the tests).
var pmdbRunBudget = 5 * time.Minute

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
		fresh := false
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			fresh = true // the periodic look: always asks, so the view is never older than this
		case <-m.pmdbWakeCh:
			// Inventories of a fleet land within seconds of each other
			// after a start; let them, so one list fetch serves them all.
			select {
			case <-m.ctx.Done():
				return
			case <-time.After(10 * time.Second):
			}
			// A wake that arrived during the wait is covered by this run:
			// drop it, or it would fetch the catalogue a second time.
			select {
			case <-m.pmdbWakeCh:
			default:
			}
		}
		if s := m.Settings(); s.PMDBEnabled && s.PMDBUploadKey != "" {
			m.syncPMDB(m.ctx, fresh)
		}
	}
}

// SyncPMDB is the check the user asks for: the list is fetched afresh and
// every box's host decided against it, the missing ones submitted. Holds
// (a refused key, a rate limit, a service failure) still apply. The
// six-hourly timer does the same.
func (m *Manager) SyncPMDB(ctx context.Context) { m.syncPMDB(ctx, true) }

// syncPMDB checks every box's host against the catalogue and submits the
// ones that are missing. Calls are serialised. A host whose submission
// failed is retried after pmdbRetry; a refused key stops the run until the
// key changes. A run after an inventory (fresh false) asks the catalogue
// only when a host needs deciding, and reuses a list fetched within
// pmdbListInterval.
// The run belongs to the settings it started under: when the opt-in is
// withdrawn or the key replaced meanwhile (resetPMDB), its context is
// cancelled, no further host is posted, and a list it already fetched
// is not rebuilt into the view (the hosts it names are still settled,
// see pmdbSalvage).
func (m *Manager) syncPMDB(ctx context.Context, fresh bool) {
	m.pmdbSyncMu.Lock()
	defer m.pmdbSyncMu.Unlock()
	run, ok := m.pmdbBegin(ctx)
	if !ok {
		return
	}
	defer run.cancel()
	todo, cands, ok := m.pmdbPlan(run, fresh)
	if !ok {
		return
	}
	if len(todo) > pmdbMaxSubmits {
		todo = todo[:pmdbMaxSubmits] // the rest stay pending for the next run
	}
	for _, name := range todo {
		if !m.pmdbCurrent(run) {
			return
		}
		// The row is built from the box as it is now: one removed or moved
		// since the list fetch must not be posted from stale facts. Only
		// this host's boxes are looked at, not the whole fleet again.
		row, still := m.pmdbRowFor(name, cands[name].ids)
		if !still {
			m.pmdbDrop(run, name)
			continue
		}
		sctx, scancel := context.WithTimeout(run.ctx, time.Minute)
		err := m.pmdb.Submit(sctx, run.key, row)
		scancel()
		if err != nil && !errors.Is(err, pmdb.ErrExists) && ctx.Err() != nil {
			// The caller went away (a manual check whose page was closed,
			// or shutdown) and the submission failed with it: not the
			// catalogue's doing, so nothing is recorded and no hold is
			// set. An answer that did arrive (accepted, or exists, which
			// is an answer though it travels as an error) still settles
			// the host below, or a later run would post it again. The
			// run's own budget is on run.ctx, not ctx, and still counts
			// as a service failure.
			return
		}
		stop := m.pmdbRecord(run, name, err)
		if ctx.Err() != nil {
			return
		}
		if stop {
			break
		}
	}
	m.Publish(Event{Type: "pmdb"})
}

// pmdbRowFor rebuilds the catalogue row for one host from the current
// state of the boxes the plan attributed to it, or false when none of them
// is still that host. One map lookup per box, under the states lock.
func (m *Manager) pmdbRowFor(name string, ids []int64) (pmdb.Row, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, id := range ids {
		st := m.states[id]
		if st == nil || !st.Ready {
			continue
		}
		cp := *st
		if row, ok := pmdbRow(&cp); ok && row.ServerName == name {
			return row, true
		}
	}
	return pmdb.Row{}, false
}

// pmdbDrop forgets a host whose boxes went away during the run.
func (m *Manager) pmdbDrop(run *pmdbRun, name string) {
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	if m.pmdbSt.gen == run.gen {
		delete(m.pmdbSt.hosts, name)
	}
}

// pmdbRun is one SyncPMDB in flight: the key it posts with, the state
// generation it started in and a context resetPMDB cancels.
type pmdbRun struct {
	ctx    context.Context
	cancel context.CancelFunc
	key    string
	gen    uint64
}

// pmdbBeginHook and pmdbAfterListHook, when set, run at the top of
// pmdbBegin and right after the list fetch returns (tests only).
var pmdbBeginHook, pmdbAfterListHook func()

// pmdbBegin opens a run under the current settings, or false when the
// integration is off or the catalogue has refused the current key. The
// settings are read under pmdbMu, after any reset that was in progress
// has finished: read before the lock, a run could start under settings an
// opt-out had already replaced, adopt the new generation and fetch the
// list once more after the user had said no.
func (m *Manager) pmdbBegin(ctx context.Context) (*pmdbRun, bool) {
	if pmdbBeginHook != nil {
		pmdbBeginHook()
	}
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	s := m.Settings()
	if !s.PMDBEnabled || s.PMDBUploadKey == "" {
		return nil, false
	}
	if m.pmdbSt.keyRefused || time.Now().Before(m.pmdbSt.holdUntil) {
		return nil, false // the view still shows why; a new key resets it, a hold passes
	}
	run := &pmdbRun{key: s.PMDBUploadKey, gen: m.pmdbSt.gen}
	run.ctx, run.cancel = context.WithTimeout(ctx, pmdbRunBudget)
	m.pmdbSt.cancel = run.cancel
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

// pmdbPlan decides every host against the catalogue list (fetched, or the
// copy of the last fetch) and merges the standing, returning the hosts to
// submit, sorted. false means nothing is to be done: the list could not be
// fetched (recorded in the view) or the run is stale.
func (m *Manager) pmdbPlan(run *pmdbRun, fresh bool) (todo []string, cands map[string]pmdbCandidate, ok bool) {
	cands = m.pmdbCandidates()
	listed, fetch := m.pmdbListFor(cands, fresh, time.Now())
	var err error
	if fetch {
		fctx, cancel := context.WithTimeout(run.ctx, 2*time.Minute)
		listed, err = m.pmdb.List(fctx)
		cancel()
		if pmdbAfterListHook != nil {
			pmdbAfterListHook()
		}
		// The boxes as they are after the fetch: one removed or moved
		// while the list was fetching is not a candidate any more. (With
		// no fetch the snapshot the decision was made on is the one
		// used: a host that landed in between would otherwise be decided
		// from the copy without the fetch its arrival calls for.)
		cands = m.pmdbCandidates()
	}
	if errors.Is(run.ctx.Err(), context.DeadlineExceeded) {
		err = pmdb.ErrTimeout // the run's own budget ran out: the service is too slow, which counts as down
	} else if !m.pmdbCurrent(run) {
		m.pmdbSalvage(listed, cands, err)
		return nil, nil, false // stale, or the caller went away: nothing to record
	}
	now := time.Now()
	m.pmdbMu.Lock()
	if m.pmdbSt.gen != run.gen {
		m.pmdbMu.Unlock()
		m.pmdbSalvage(listed, cands, err)
		return nil, nil, false // reset meanwhile: the outcome belongs to nobody
	}
	if m.pmdbSt.hosts == nil {
		m.pmdbSt.hosts = map[string]*pmdbStatus{}
	}
	if err != nil {
		m.pmdbSt.err = "catalogue list: " + err.Error()
		// Every list failure is the catalogue's, not a host's: another run
		// straight away would only ask the same question again.
		m.pmdbSt.holdUntil = now.Add(pmdbRetry)
		m.pmdbMu.Unlock()
		m.log.Warn("pmdb list", "err", err) // err is our own fixed text or a status code, never the catalogue's words
		m.Publish(Event{Type: "pmdb"})
		return nil, nil, false
	}
	defer m.pmdbMu.Unlock()
	if fetch {
		m.pmdbSt.err, m.pmdbSt.checkedAt = "", now
		m.pmdbSt.listed, m.pmdbSt.listedAt = listed, now
	}
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

// pmdbSalvage keeps what a list that did arrive says about the hosts it
// was fetched for, when the run it belongs to is stale (reset, or the
// caller gone): a host the catalogue lists is a fact of this process like
// an accepted submission is, and is settled the same way, or a re-enable
// met by a hostile empty list could post it.
func (m *Manager) pmdbSalvage(listed map[string]bool, cands map[string]pmdbCandidate, err error) {
	if err != nil || listed == nil {
		return
	}
	now := time.Now()
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	for name := range cands {
		if listed[name] {
			if _, done := m.pmdbSettled[name]; !done {
				m.pmdbSettle(&pmdbStatus{Hostname: name}, "listed", now)
			}
		}
	}
}

// pmdbListFor says which list a run decides against: the copy of the last
// fetch (which may be nil: nothing listed), or none yet and fetch true.
// A check the user asks for, and the periodic one, always fetch. A run
// after an inventory fetches only when some candidate is undecided and
// not backing off (a decided host is decided whatever the list says, so a
// fleet of decided hosts asks nothing), and then not within
// pmdbListInterval of the last fetch; a list failure on view is re-asked
// once its hold has passed.
func (m *Manager) pmdbListFor(cands map[string]pmdbCandidate, fresh bool, now time.Time) (map[string]bool, bool) {
	if fresh {
		return nil, true
	}
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	if m.pmdbSt.err != "" {
		return nil, true
	}
	undecided := false
	for name := range cands {
		if _, ok := m.pmdbSettled[name]; ok {
			continue
		}
		if h := m.pmdbSt.hosts[name]; h != nil && h.Status == "error" && now.Before(h.retryAt) {
			continue
		}
		undecided = true
		break
	}
	if !undecided {
		return m.pmdbSt.listed, false
	}
	if !m.pmdbSt.listedAt.IsZero() && now.Before(m.pmdbSt.listedAt.Add(pmdbListInterval)) {
		return m.pmdbSt.listed, false
	}
	return nil, true
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
	if standing, ok := m.pmdbSettled[name]; ok && !h.decided {
		// Decided earlier in this process, before a reset or while the
		// host had no box: still decided.
		h.decided = true
		if h.Status == "" {
			h.Status, h.At = standing, now
		}
	}
	if listed && h.Status != "listed" {
		m.pmdbSettle(h, "listed", now)
	}
	switch {
	case h.decided:
		// Settled in this process (listed, accepted, or told it exists):
		// never again, whatever today's list says.
		return false
	case h.Status == "error" && now.Before(h.retryAt):
		return false // backing off
	}
	if h.Status == "" {
		h.Status, h.At = "pending", now
	}
	return true
}

// pmdbSettle records a host as decided for the life of the process, with
// the standing to show. Called under pmdbMu.
func (m *Manager) pmdbSettle(h *pmdbStatus, standing string, now time.Time) {
	h.Status, h.Detail, h.At, h.decided = standing, "", now, true
	if m.pmdbSettled == nil {
		m.pmdbSettled = map[string]string{}
	}
	m.pmdbSettled[h.Hostname] = standing
}

// pmdbRecord notes a submission's outcome. It returns true when the rest
// of the run is pointless: the run is stale, a refused key would refuse
// every host the same way (one refusal stops the run and is shown once, at
// the top), or the catalogue is rate-limiting this address.
func (m *Manager) pmdbRecord(run *pmdbRun, name string, err error) (stop bool) {
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	now := time.Now()
	h := m.pmdbSt.hosts[name]
	if h == nil || m.pmdbSt.gen != run.gen {
		// Reset while this ran (opt-out or key change): the display state
		// belongs to nobody, but a decision the catalogue did make is a
		// fact of this process and is kept, or a re-enable could post the
		// host again.
		switch {
		case err == nil:
			m.pmdbSettle(&pmdbStatus{Hostname: name}, "submitted", now)
			m.log.Info(pmdbSubmitLog, "host", name)
		case errors.Is(err, pmdb.ErrExists):
			m.pmdbSettle(&pmdbStatus{Hostname: name}, "listed", now)
		}
		return true
	}
	switch {
	case err == nil:
		m.pmdbSettle(h, "submitted", now)
		m.log.Info(pmdbSubmitLog, "host", name)
	case errors.Is(err, pmdb.ErrExists):
		m.pmdbSettle(h, "listed", now)
	default:
		// err is our own fixed wording or a status code, never the
		// catalogue's words (see package pmdb).
		h.Status, h.Detail, h.At, h.retryAt = "error", err.Error(), now, now.Add(pmdbRetry)
		m.log.Warn(pmdbSubmitLog, "host", name, "err", err)
		switch {
		case errors.Is(err, pmdb.ErrAuth):
			m.pmdbSt.err = "upload key refused by pmdb.fyi; check the key in settings"
			m.pmdbSt.keyRefused = true
			return true
		case errors.Is(err, pmdb.ErrRateLimited):
			// Per address, not per host: hold every run for a while, not
			// just this host's retry.
			m.pmdbSt.err = "pmdb.fyi is rate-limiting submissions; trying again in an hour"
			m.pmdbSt.holdUntil = now.Add(pmdbRetry)
			return true
		case pmdbServiceDown(err):
			// The service is unwell: the remaining hosts would only add
			// requests to it. Their turn comes with the next run.
			m.pmdbSt.err = "pmdb.fyi is not answering properly; trying again in an hour"
			m.pmdbSt.holdUntil = now.Add(pmdbRetry)
			return true
		}
	}
	return false
}

// pmdbServiceDown reports a submission failure that says the service, not
// this host's row, is the problem: anything but a 4xx about the row (and
// the refused key and the rate limit, which have their own handling).
// Unreachable, timed out, oversized, a redirect, a 3xx or a 5xx all mean
// the next host would meet the same, so the run ends on the first.
func pmdbServiceDown(err error) bool {
	var he *pmdb.HTTPError
	if errors.As(err, &he) {
		return he.Code < 400 || he.Code >= 500
	}
	return !errors.Is(err, pmdb.ErrExists) && !errors.Is(err, pmdb.ErrAuth) && !errors.Is(err, pmdb.ErrRateLimited)
}

// resetPMDB forgets every host's standing and cancels a run in flight, so
// a changed key or a re-enable starts from the public list again and an
// opt-out posts nothing more. Decisions survive it (pmdbSettled): a host
// this process has seen listed or had accepted is never posted again.
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
