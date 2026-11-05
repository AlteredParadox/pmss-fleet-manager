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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"sort"
	"strings"
	"time"

	"pmss-fleet-manager/internal/pmdb"
	"pmss-fleet-manager/internal/store"
)

// The pmdb.fyi integration (opt-in, Settings.PMDBEnabled): every box runs
// on a shared Pulsed Media host, and the community catalogue at pmdb.fyi
// lists those hosts' hardware, one row per host that any contributor's
// submission replaces whole, under a contributor identifier
// (Settings.PMDBUsername) the catalogue uses to count plans and
// contributors per host. When enabled, the dashboard submits each host
// from the facts the probe already reads: hostname, CPU model, core
// count, nominal clock, RAM, swap, home filesystem size, whether the host
// is a KVM guest, its distribution, kernel and architecture, the box's
// plan (when the catalogue's own plan list has it) and the number of user
// accounts on the host. Nothing about usage, torrents or the dashboard
// goes out.
//
// A host is submitted once and then again only when the row this
// dashboard builds for it changes, when the refresh interval has passed,
// or when the identifier changes: each accepted submission leaves a
// receipt (the row's hash, the identifier, the time) in the store, so a
// restart posts nothing. The catalogue's list says only whether a host is
// listed, for the view; it never decides a submission, so a list that
// lies cannot make the fleet post again.
//
// Because the catalogue replaces a row whole and stores an omitted
// optional as unknown, a submission that lacks a value (an unknown clock,
// no swap figure, no virtualisation answer) would erase what another
// contributor supplied. So a row with any optional unknown first reads the
// catalogue's current row for that host and repeats its value: a number
// or boolean as it is, a string only when it passes the catalogue's own
// text rule, else the host is skipped rather than posted with a hole.
// Best effort: another contributor can post between that read and our
// write.
//
// Several boxes can share a host, so the work is fleet-level: the hosts
// are deduplicated by catalogue key, one list fetch covers them all, and
// submissions are spent against a rolling window (pmdbWindowMax per
// pmdbWindow, the catalogue's own limit with room to spare) that a reset
// does not reopen. An inventory wake fetches the list only when a host is
// due and then at most once per pmdbListInterval; the six-hourly timer
// and a check the user asks for always fetch (unless a hold applies), so
// the standing shown is never older than that and the automatic cost is
// bounded whatever the catalogue sends.

// pmdbStatus is one host's standing with the catalogue.
type pmdbStatus struct {
	Hostname string `json:"hostname"`
	// Status: submitted (a receipt stands and nothing is due), pending (a
	// submission is due or waiting), error (the last attempt failed; the
	// detail says how, retried after pmdbRetry) or skipped (the host
	// cannot be posted as it is; the detail says why).
	Status      string    `json:"status"`
	Listed      bool      `json:"listed"` // in the public list at the last fetch
	Detail      string    `json:"detail,omitempty"`
	Plan        string    `json:"plan,omitempty"`         // the plan name the row carries, or would
	SubmittedAt string    `json:"submitted_at,omitempty"` // the receipt's time, RFC 3339
	At          time.Time `json:"at"`
	BoxIDs      []int64   `json:"box_ids"`
	retryAt     time.Time
}

// PMDBView is what GET /api/pmdb returns.
type PMDBView struct {
	Enabled    bool         `json:"enabled"`
	Configured bool         `json:"configured"` // enabled with an identifier
	Hosts      []pmdbStatus `json:"hosts"`
	CheckedAt  string       `json:"checked_at,omitempty"`
	Error      string       `json:"error,omitempty"` // the last list fetch or service problem
}

type pmdbState struct {
	hosts     map[string]*pmdbStatus
	checkedAt time.Time
	err       string
	gen       uint64             // bumped by every reset; a run from an older generation stores nothing
	cancel    context.CancelFunc // the run in flight, cancelled by a reset
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

// pmdbRetry is how long a failed submission waits before another try,
// pmdbInterval how often the loop fetches the list afresh on its own (a
// new box's first inventory wakes it sooner, but such a wake fetches only
// when a host is due), and pmdbRefresh how long a receipt stands before
// the host is posted again with unchanged facts, so the account count and
// plan it carries do not go stale.
const (
	pmdbRetry    = time.Hour
	pmdbInterval = 6 * time.Hour
	pmdbRefresh  = 30 * 24 * time.Hour
)

// pmdbWindowMax is the most submission attempts (answered or not) in any
// pmdbWindow, from this process: the catalogue allows 30 per address per
// ten minutes and counts failures, and other clients on the same address
// may be spending the same budget. The window is a var for the tests.
const pmdbWindowMax = 25

var pmdbWindow = 10 * time.Minute

// pmdbListInterval is the least time between two automatic list fetches;
// a check the user asks for always fetches (a var for the tests).
var pmdbListInterval = 10 * time.Minute

// pmdbRunBudget bounds one whole run, list fetch and every submission: a
// catalogue that answers slowly cannot hold the loop for long (a var for
// the tests).
var pmdbRunBudget = 5 * time.Minute

// The fixed texts a host's detail can show. Nothing the catalogue sent is
// ever among them.
const (
	pmdbSkipForeign  = "not a pulsedmedia.com hostname"
	pmdbSkipCPU      = "the CPU model name is not one the catalogue accepts"
	pmdbWaitWindow   = "waiting for the submission window"
	pmdbWaitPlans    = "the catalogue's plan list could not be read; trying later"
	pmdbPlanUnknown  = "plan not in the catalogue's list"
	pmdbReadFailed   = "could not read the catalogue's row for this host; not replacing it"
	pmdbChanged      = "the boxes changed during the check; trying later"
	pmdbCannotRepeat = "the catalogue holds a value for this host this dashboard cannot repeat; not replacing it"
)

// pmdbCandidate is one host as the fleet sees it: the boxes on it, the row
// they make (the dashboard's own facts, nothing from the catalogue), the
// hash that stands for those facts in a receipt, the plan name the boxes
// claim (sent only when the catalogue lists it) and, when the host cannot
// be posted as it is, the fixed reason.
type pmdbCandidate struct {
	ids  []int64
	row  pmdb.Row
	hash string
	plan string
	skip string
}

// pmdbPlanOf is the plan name a box claims: the one typed on the box,
// else the Pulsed Media service it matched (the same precedence as the
// page).
func pmdbPlanOf(st *BoxState) string {
	if st.Plan != "" {
		return st.Plan
	}
	if st.Service != nil {
		return st.Service.Product
	}
	return ""
}

// pmdbRow builds the catalogue row for one host from the boxes on it, in
// box ID order so the result is the same whatever order the states came
// in. The required hardware comes from the first box whose inventory has
// it (false when none has); every optional from the first box that knows
// it. Strings go through the catalogue's own text rule and are unknown
// when they fail it, never cut. Swap is sent to the byte: a small swap
// rounded to two decimals would reach the catalogue as none.
func pmdbRow(name string, states []*BoxState) (pmdbCandidate, bool) {
	c := pmdbCandidate{}
	var hw *HostState
	for _, st := range states {
		c.ids = append(c.ids, st.ID)
		if h := &st.HostSys; hw == nil && h.CPUModel != "" && h.NCPU > 0 && h.MemTotal > 0 && h.HomeSize > 0 {
			hw = h
		}
	}
	if hw == nil {
		return c, false
	}
	row := pmdb.Row{ServerName: name, CoreCount: hw.NCPU, RAMGiB: round2(float64(hw.MemTotal) / (1 << 30)), StorageTiB: round2(float64(hw.HomeSize) / (1 << 40))}
	if !strings.HasSuffix(name, ".pulsedmedia.com") {
		c.skip = pmdbSkipForeign // the catalogue is Pulsed Media's hosts; a box whose hostname is something else would make a stray row
	}
	if p, ok := pmdb.CleanText(hw.CPUModel, pmdb.MaxProcessorName); ok {
		row.ProcessorName = p
	} else if c.skip == "" {
		c.skip = pmdbSkipCPU
	}
	var sys pmdb.System
	for _, st := range states {
		h := &st.HostSys
		if row.ProcessorMHz == nil && h.CPUMHz > 0 {
			mhz := float64(h.CPUMHz)
			row.ProcessorMHz = &mhz
		}
		if row.SwapGiB == nil && h.SwapKnown {
			swap := float64(h.SwapTotal) / (1 << 30)
			row.SwapGiB = &swap
		}
		if sys.VM == "" {
			if vm, ok := pmdb.CleanText(strings.ToUpper(h.Virt), pmdb.MaxVM); ok {
				sys.VM = vm // as YABS reports it
				switch vm {
				case "KVM":
					t := true
					row.KVM = &t
				case "NONE":
					f := false
					row.KVM = &f
				}
			}
		}
		if sys.Distro == "" {
			sys.Distro, _ = pmdb.CleanText(h.Distro, pmdb.MaxDistro)
		}
		if sys.Kernel == "" {
			sys.Kernel, _ = pmdb.CleanText(h.Kernel, pmdb.MaxKernel)
		}
		if sys.Arch == "" {
			sys.Arch, _ = pmdb.CleanText(h.Arch, pmdb.MaxArch)
		}
		if row.UserCount == nil && h.UsersKnown && h.Users <= 10_000_000 {
			n := h.Users
			row.UserCount = &n
		}
		if c.plan == "" {
			c.plan, _ = pmdb.CleanText(pmdbPlanOf(st), pmdb.MaxPlan)
		}
	}
	if sys != (pmdb.System{}) {
		row.System = &sys
	}
	c.row = row
	// The hash stands for the facts as this dashboard holds them: the row
	// with the plan it claims (whether or not the catalogue takes it) and
	// without the identifier (a receipt records that beside the hash).
	// Nothing read from the catalogue is in it, so the catalogue cannot
	// make a host due by changing its own numbers.
	hashed := row
	if c.plan != "" {
		p := c.plan
		hashed.Plan = &p
	}
	b, _ := json.Marshal(hashed)
	sum := sha256.Sum256(b)
	c.hash = hex.EncodeToString(sum[:])
	return c, true
}

func round2(f float64) float64 { return math.Round(f*100) / 100 }

// pmdbCandidates groups the current box states by catalogue key.
func (m *Manager) pmdbCandidates() map[string]pmdbCandidate {
	groups := map[string][]*BoxState{}
	states := m.States()
	for i := range states {
		st := &states[i]
		if !st.Ready {
			continue
		}
		name, ok := pmdb.Normalize(st.Hostname)
		if !ok {
			continue
		}
		groups[name] = append(groups[name], st)
	}
	out := map[string]pmdbCandidate{}
	for name, sts := range groups {
		sort.Slice(sts, func(i, j int) bool { return sts[i].ID < sts[j].ID })
		if c, ok := pmdbRow(name, sts); ok {
			out[name] = c
		}
	}
	return out
}

// pmdbDue says whether a host is to be submitted, given its receipt (found
// false when it has none), and why in a few fixed words.
func pmdbDue(rec store.PMDBSubmission, found bool, username, hash string, now time.Time) (bool, string) {
	switch {
	case !found:
		return true, "not submitted yet"
	case rec.Username != username:
		return true, "new identifier"
	case rec.RowHash != hash:
		return true, "facts changed"
	case !now.Before(rec.SubmittedAt.Add(pmdbRefresh)):
		return true, "30 days since the last submission"
	}
	return false, ""
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
		if s := m.Settings(); s.PMDBEnabled && s.PMDBUsername != "" {
			m.syncPMDB(m.ctx, fresh)
		}
	}
}

// SyncPMDB is the check the user asks for: the list is fetched afresh and
// every due host submitted. Holds (a rate limit, a service failure) still
// apply. The six-hourly timer does the same.
func (m *Manager) SyncPMDB(ctx context.Context) { m.syncPMDB(ctx, true) }

// syncPMDB submits every box's host that is due. Calls are serialised. A
// host whose submission failed is retried after pmdbRetry. A run after an
// inventory (fresh false) asks the catalogue for its list only when a
// host is due, and reuses a list fetched within pmdbListInterval.
// The run belongs to the settings it started under: when the opt-in is
// withdrawn or the identifier replaced meanwhile (resetPMDB), its context
// is cancelled, no further host is posted, and a list it already fetched
// is not rebuilt into the view. An acceptance that does arrive is a
// receipt whatever happened to the run (pmdbRecord).
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
	// The plan list is needed once per run, and only when a due host
	// claims a plan. Unreadable, the hosts that claim one wait for a later
	// run (a receipt written without the plan would not make the host due
	// again once the list can be read); the others go ahead.
	var cfg *pmdb.Config
	plans := false
	for _, name := range todo {
		if cands[name].plan != "" {
			plans = true
			break
		}
	}
	if plans {
		cctx, cancel := context.WithTimeout(run.ctx, 2*time.Minute)
		c, err := m.pmdb.GetConfig(cctx)
		cancel()
		switch {
		case err != nil && ctx.Err() != nil:
			return // the caller went away: nothing is recorded
		case err != nil:
			m.log.Warn("pmdb config", "err", err) // err is our own fixed text or a status code, never the catalogue's words
		case !c.UploadsEnabled:
			m.pmdbHold(run, "pmdb.fyi has submissions switched off; trying again in an hour")
			m.Publish(Event{Type: "pmdb"})
			return
		default:
			cfg = &c
		}
	}
hosts:
	for i, name := range todo {
		if !m.pmdbCurrent(run) {
			return
		}
		// The row is built from the boxes as they are now: one removed or
		// moved since the list fetch must not be posted from stale facts.
		// Only this host's boxes are looked at, not the whole fleet again.
		cand, still := m.pmdbRowFor(name, cands[name].ids)
		switch {
		case !still:
			m.pmdbDrop(run, name)
			continue
		case cand.skip != "":
			m.pmdbMark(run, name, "skipped", cand.skip, false)
			continue
		case cand.plan != "" && cfg == nil:
			m.pmdbMark(run, name, "pending", pmdbWaitPlans, false)
			continue
		}
		row, detail := cand.row, ""
		if cand.plan != "" {
			if slices.Contains(cfg.Plans, cand.plan) {
				p := cand.plan
				row.Plan = &p
			} else {
				detail = pmdbPlanUnknown // left out: the catalogue keeps the plan it has for this identifier
			}
		}
		m.pmdbMu.Lock()
		room, wait := m.pmdbWindowRoom(time.Now())
		m.pmdbMu.Unlock()
		if !room {
			// The rest wait for the window; the loop is woken when it opens.
			for _, rest := range todo[i:] {
				m.pmdbMark(run, rest, "pending", pmdbWaitWindow, false)
			}
			time.AfterFunc(wait, m.pmdbWake)
			break
		}
		if pmdbIncomplete(row) {
			// A value this dashboard does not know would reach the catalogue
			// as unknown and replace what it holds: read its row first.
			nctx, ncancel := context.WithTimeout(run.ctx, time.Minute)
			node, err := m.pmdb.Node(nctx, name)
			ncancel()
			switch {
			case err == nil:
				if !pmdbFill(&row, node) {
					m.pmdbMark(run, name, "error", pmdbCannotRepeat, true)
					continue
				}
			case errors.Is(err, pmdb.ErrNotListed):
				// Nothing there to preserve.
			case ctx.Err() != nil:
				return // the caller went away
			default:
				if m.pmdbReadFailed(run, name, err) {
					break hosts
				}
				continue
			}
			// The lookup took time, and a box removal or edit does not
			// cancel this run: the boxes may not make this row any more.
			// The row goes only if they still make the same one; else the
			// next run decides the host afresh.
			switch again, still := m.pmdbRowFor(name, cands[name].ids); {
			case !still:
				m.pmdbDrop(run, name)
				continue
			case again.skip != "":
				m.pmdbMark(run, name, "skipped", again.skip, false)
				continue
			case again.hash != cand.hash:
				m.pmdbMark(run, name, "pending", pmdbChanged, false)
				continue
			}
		}
		row.Username = run.username
		m.pmdbMu.Lock()
		m.pmdbAttempts = append(m.pmdbAttempts, time.Now()) // counted before it is sent: the catalogue counts attempts, answered or not
		m.pmdbMu.Unlock()
		sctx, scancel := context.WithTimeout(run.ctx, time.Minute)
		err := m.pmdb.Submit(sctx, row)
		scancel()
		if err != nil && ctx.Err() != nil {
			// The caller went away (a manual check whose page was closed,
			// or shutdown) and the submission failed with it: not the
			// catalogue's doing, so nothing is recorded and no hold is
			// set. An answer that did arrive still settles the host below,
			// or a later run would post it again. The run's own budget is
			// on run.ctx, not ctx, and still counts as a service failure.
			return
		}
		stop := m.pmdbRecord(run, name, cand.hash, detail, err)
		if ctx.Err() != nil {
			return
		}
		if stop {
			break
		}
	}
	m.Publish(Event{Type: "pmdb"})
}

// pmdbIncomplete reports whether a row lacks any optional value, which
// the catalogue would store as unknown in place of what it holds.
func pmdbIncomplete(row pmdb.Row) bool {
	return row.ProcessorMHz == nil || row.SwapGiB == nil || row.KVM == nil || row.System == nil ||
		row.System.Distro == "" || row.System.Kernel == "" || row.System.Arch == "" || row.System.VM == ""
}

// pmdbFill repeats, into the row's unknowns, what the catalogue holds for
// the host: a number or boolean as it is, a string the catalogue's text
// rule let through. A string that is there but did not pass is the one
// case the row cannot be posted: false, and the host is skipped rather
// than posted with that value missing. Nothing the dashboard knows is
// replaced.
func pmdbFill(row *pmdb.Row, n pmdb.NodeRow) bool {
	if row.ProcessorMHz == nil && n.ProcessorMHz != nil {
		v := *n.ProcessorMHz
		row.ProcessorMHz = &v
	}
	if row.SwapGiB == nil && n.SwapBytes != nil {
		v := float64(*n.SwapBytes) / (1 << 30) // to the byte: the catalogue rounds the GiB back to bytes
		row.SwapGiB = &v
	}
	if row.KVM == nil && n.KVM != nil {
		v := *n.KVM
		row.KVM = &v
	}
	var sys pmdb.System
	if row.System != nil {
		sys = *row.System
	}
	for key, dst := range map[string]*string{"distro": &sys.Distro, "kernel": &sys.Kernel, "arch": &sys.Arch, "vm": &sys.VM} {
		if *dst != "" {
			continue
		}
		switch t := n.System[key]; t.State {
		case pmdb.TextOK:
			*dst = t.Value
		case pmdb.TextRejected:
			return false
		}
	}
	if sys != (pmdb.System{}) {
		row.System = &sys
	}
	return true
}

// pmdbRowFor rebuilds the catalogue row for one host from the current
// state of the boxes the plan attributed to it, or false when none of them
// is still that host. One map lookup per box, under the states lock.
func (m *Manager) pmdbRowFor(name string, ids []int64) (pmdbCandidate, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var sts []*BoxState
	for _, id := range ids {
		st := m.states[id]
		if st == nil || !st.Ready {
			continue
		}
		if n, ok := pmdb.Normalize(st.Hostname); !ok || n != name {
			continue
		}
		cp := *st
		sts = append(sts, &cp)
	}
	if len(sts) == 0 {
		return pmdbCandidate{}, false
	}
	sort.Slice(sts, func(i, j int) bool { return sts[i].ID < sts[j].ID })
	return pmdbRow(name, sts)
}

// pmdbDrop forgets a host whose boxes went away during the run.
func (m *Manager) pmdbDrop(run *pmdbRun, name string) {
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	if m.pmdbSt.gen == run.gen {
		delete(m.pmdbSt.hosts, name)
	}
}

// pmdbRun is one SyncPMDB in flight: the identifier it posts under, the
// state generation it started in and a context resetPMDB cancels.
type pmdbRun struct {
	ctx      context.Context
	cancel   context.CancelFunc
	username string
	gen      uint64
}

// pmdbBeginHook and pmdbAfterListHook, when set, run at the top of
// pmdbBegin and right after the list fetch returns (tests only).
var pmdbBeginHook, pmdbAfterListHook func()

// pmdbBegin opens a run under the current settings, or false when the
// integration is off or a hold stands. The settings are read under
// pmdbMu, after any reset that was in progress has finished: read before
// the lock, a run could start under settings an opt-out had already
// replaced, adopt the new generation and fetch the list once more after
// the user had said no.
func (m *Manager) pmdbBegin(ctx context.Context) (*pmdbRun, bool) {
	if pmdbBeginHook != nil {
		pmdbBeginHook()
	}
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	s := m.Settings()
	if !s.PMDBEnabled || s.PMDBUsername == "" {
		return nil, false
	}
	if time.Now().Before(m.pmdbSt.holdUntil) {
		return nil, false // the view still shows why; a hold passes
	}
	run := &pmdbRun{username: s.PMDBUsername, gen: m.pmdbSt.gen}
	run.ctx, run.cancel = context.WithTimeout(ctx, pmdbRunBudget)
	m.pmdbSt.cancel = run.cancel
	return run, true
}

// pmdbCurrent reports whether the run's settings still stand: the opt-in
// is on, the identifier is the one it started with and no reset happened
// since. Checked before anything is stored or posted.
func (m *Manager) pmdbCurrent(run *pmdbRun) bool {
	if run.ctx.Err() != nil {
		return false
	}
	s := m.Settings()
	m.pmdbMu.Lock()
	gen := m.pmdbSt.gen
	m.pmdbMu.Unlock()
	return s.PMDBEnabled && s.PMDBUsername == run.username && gen == run.gen
}

// pmdbPlan decides every host against its receipt, merges the standing
// (and the catalogue list, fetched or the copy of the last fetch, for what
// it says about who is listed), and returns the hosts to submit, sorted.
// false means nothing is to be done: the list could not be fetched
// (recorded in the view) or the run is stale.
func (m *Manager) pmdbPlan(run *pmdbRun, fresh bool) (todo []string, cands map[string]pmdbCandidate, ok bool) {
	cands = m.pmdbCandidates()
	listed, fetch := m.pmdbListFor(cands, run.username, fresh, time.Now())
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
		return nil, nil, false // stale, or the caller went away: nothing to record
	}
	now := time.Now()
	m.pmdbMu.Lock()
	if m.pmdbSt.gen != run.gen {
		m.pmdbMu.Unlock()
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
		if m.pmdbMerge(name, c, listed, run.username, now) {
			todo = append(todo, name)
		}
	}
	sort.Strings(todo)
	return todo, cands, true
}

// pmdbListFor says which list a run decides against: the copy of the last
// fetch (which may be nil: none yet), or none and fetch true. A check the
// user asks for, and the periodic one, always fetch. A run after an
// inventory fetches only when some candidate is due and not backing off
// (a fleet with nothing due asks nothing), and then not within
// pmdbListInterval of the last fetch; a list failure on view is re-asked
// once its hold has passed.
func (m *Manager) pmdbListFor(cands map[string]pmdbCandidate, username string, fresh bool, now time.Time) (map[string]bool, bool) {
	if fresh {
		return nil, true
	}
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	if m.pmdbSt.err != "" {
		return nil, true
	}
	due := false
	for name, c := range cands {
		if c.skip != "" {
			continue
		}
		if h := m.pmdbSt.hosts[name]; h != nil && h.Status == "error" && now.Before(h.retryAt) {
			continue
		}
		rec, found := m.pmdbDone[name]
		if d, _ := pmdbDue(rec, found, username, c.hash, now); d {
			due = true
			break
		}
	}
	if !due {
		return m.pmdbSt.listed, false
	}
	if !m.pmdbSt.listedAt.IsZero() && now.Before(m.pmdbSt.listedAt.Add(pmdbListInterval)) {
		return m.pmdbSt.listed, false
	}
	return nil, true
}

// pmdbMerge updates one host's standing from its receipt and the list (nil
// when none has been fetched: what it said last stands) and reports
// whether it is to be submitted. Called under pmdbMu.
func (m *Manager) pmdbMerge(name string, c pmdbCandidate, listed map[string]bool, username string, now time.Time) bool {
	h := m.pmdbSt.hosts[name]
	if h == nil {
		h = &pmdbStatus{Hostname: name}
		m.pmdbSt.hosts[name] = h
	}
	h.BoxIDs = append([]int64(nil), c.ids...)
	h.Plan = c.plan
	if listed != nil {
		h.Listed = listed[name]
	}
	rec, found := m.pmdbDone[name]
	if found {
		h.SubmittedAt = rec.SubmittedAt.UTC().Format(time.RFC3339)
	}
	if c.skip != "" {
		h.Status, h.Detail, h.At = "skipped", c.skip, now
		return false
	}
	due, why := pmdbDue(rec, found, username, c.hash, now)
	if !due {
		if h.Status != "submitted" {
			h.Status, h.Detail, h.At = "submitted", "", now
		}
		return false
	}
	if h.Status == "error" && now.Before(h.retryAt) {
		return false // backing off
	}
	if h.Status != "pending" {
		h.Status, h.Detail, h.At = "pending", why, now
	}
	return true
}

// pmdbWindowRoom reports whether another submission may be attempted now
// and, when not, how long until one may. Attempts older than the window
// are forgotten here. Called under pmdbMu.
func (m *Manager) pmdbWindowRoom(now time.Time) (bool, time.Duration) {
	keep := m.pmdbAttempts[:0]
	for _, t := range m.pmdbAttempts {
		if now.Sub(t) < pmdbWindow {
			keep = append(keep, t)
		}
	}
	m.pmdbAttempts = keep
	if len(keep) < pmdbWindowMax {
		return true, 0
	}
	return false, keep[0].Add(pmdbWindow).Sub(now)
}

// pmdbMark sets a host's standing to a fixed text, with the hour's backoff
// when it is an error. Stale runs set nothing.
func (m *Manager) pmdbMark(run *pmdbRun, name, status, detail string, retry bool) {
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	h := m.pmdbSt.hosts[name]
	if h == nil || m.pmdbSt.gen != run.gen {
		return
	}
	now := time.Now()
	h.Status, h.Detail, h.At = status, detail, now
	if retry {
		h.retryAt = now.Add(pmdbRetry)
	}
}

// pmdbHold holds every later run for pmdbRetry with the reason on view.
// Called with pmdbMu not held.
func (m *Manager) pmdbHold(run *pmdbRun, why string) {
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	if m.pmdbSt.gen != run.gen {
		return
	}
	m.pmdbSt.err = why
	m.pmdbSt.holdUntil = time.Now().Add(pmdbRetry)
}

// pmdbReadFailed notes that a host's catalogue row could not be read, so
// the host was not posted (it would have replaced that row blind). It
// returns true when the rest of the run is pointless: the service, not
// this host, is the problem.
func (m *Manager) pmdbReadFailed(run *pmdbRun, name string, err error) (stop bool) {
	m.log.Warn("pmdb node", "host", name, "err", err) // err is our own fixed text or a status code
	m.pmdbMark(run, name, "error", pmdbReadFailed, true)
	switch {
	case errors.Is(err, pmdb.ErrRateLimited):
		m.pmdbHold(run, "pmdb.fyi is rate-limiting requests; trying again in an hour")
		return true
	case pmdbServiceDown(err):
		m.pmdbHold(run, "pmdb.fyi is not answering properly; trying again in an hour")
		return true
	}
	return false
}

// pmdbRecord notes a submission's outcome. An acceptance is a receipt
// whatever happened to the run meanwhile (a reset, an opt-out, the caller
// gone): the catalogue has the row, and a re-enable must not post it
// again. It returns true when the rest of the run is pointless: the run is
// stale, or the catalogue is rate-limiting this address or unwell.
func (m *Manager) pmdbRecord(run *pmdbRun, name, hash, detail string, err error) (stop bool) {
	now := time.Now()
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	if err == nil {
		rec := store.PMDBSubmission{Hostname: name, Username: run.username, RowHash: hash, SubmittedAt: now}
		if m.pmdbDone == nil {
			m.pmdbDone = map[string]store.PMDBSubmission{}
		}
		m.pmdbDone[name] = rec
		// Its own context: a receipt is worth writing during a shutdown too.
		wctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		if perr := m.st.PutPMDBSubmission(wctx, rec); perr != nil {
			m.log.Warn("pmdb receipt", "host", name, "err", perr) // kept in memory; the host is posted once more after a restart
		}
		cancel()
		m.log.Info(pmdbSubmitLog, "host", name)
	}
	h := m.pmdbSt.hosts[name]
	if h == nil || m.pmdbSt.gen != run.gen {
		return true // reset while this ran: the display state belongs to nobody
	}
	switch {
	case err == nil:
		h.Status, h.Detail, h.At, h.SubmittedAt = "submitted", detail, now, now.UTC().Format(time.RFC3339)
	default:
		// err is our own fixed wording or a status code, never the
		// catalogue's words (see package pmdb).
		h.Status, h.Detail, h.At, h.retryAt = "error", err.Error(), now, now.Add(pmdbRetry)
		m.log.Warn(pmdbSubmitLog, "host", name, "err", err)
		switch {
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

// pmdbServiceDown reports a failure that says the service, not this host's
// row, is the problem: anything but a 4xx about the row (and the rate
// limit, the name conflict and the missing row, which have their own
// handling). Unreachable, timed out, oversized, malformed, a redirect, a
// 3xx or a 5xx all mean the next host would meet the same, so the run
// ends on the first.
func pmdbServiceDown(err error) bool {
	var he *pmdb.HTTPError
	if errors.As(err, &he) {
		return he.Code < 400 || he.Code >= 500
	}
	return !errors.Is(err, pmdb.ErrNameConflict) && !errors.Is(err, pmdb.ErrRateLimited) && !errors.Is(err, pmdb.ErrNotListed)
}

// resetPMDB forgets every host's standing and cancels a run in flight, so
// a changed identifier or a re-enable starts from the receipts again and an
// opt-out posts nothing more. Receipts and the submission window survive
// it: a host the catalogue accepted is not posted again for being
// re-enabled, and a reset does not buy more submissions.
func (m *Manager) resetPMDB() {
	m.pmdbMu.Lock()
	defer m.pmdbMu.Unlock()
	if m.pmdbSt.cancel != nil {
		m.pmdbSt.cancel()
	}
	m.pmdbSt = pmdbState{gen: m.pmdbSt.gen + 1}
}

// PMDB returns the catalogue standing of every host, sorted by hostname.
// A host no run has looked at yet (right after a start) shows its receipt,
// so a restart does not look like a fleet never submitted.
func (m *Manager) PMDB() PMDBView {
	s := m.Settings()
	v := PMDBView{Enabled: s.PMDBEnabled, Configured: s.PMDBEnabled && s.PMDBUsername != "", Hosts: []pmdbStatus{}}
	if !v.Enabled {
		return v
	}
	cands := m.pmdbCandidates() // before pmdbMu: it takes the states lock
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
	for name, c := range cands {
		if _, seen := m.pmdbSt.hosts[name]; seen {
			continue
		}
		h := pmdbStatus{Hostname: name, BoxIDs: append([]int64(nil), c.ids...), Plan: c.plan, At: time.Now()}
		rec, found := m.pmdbDone[name]
		switch {
		case c.skip != "":
			h.Status, h.Detail = "skipped", c.skip
		case found:
			h.Status, h.SubmittedAt, h.At = "submitted", rec.SubmittedAt.UTC().Format(time.RFC3339), rec.SubmittedAt
		default:
			h.Status, h.Detail = "pending", "not checked yet"
		}
		v.Hosts = append(v.Hosts, h)
	}
	sort.Slice(v.Hosts, func(i, j int) bool { return v.Hosts[i].Hostname < v.Hosts[j].Hostname })
	return v
}
