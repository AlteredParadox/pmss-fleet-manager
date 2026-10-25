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

// Package collect polls every box over SSH and keeps the latest BoxState.
package collect

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"pmss-fleet-manager/internal/pmapi"
	"pmss-fleet-manager/internal/pmdb"
	"pmss-fleet-manager/internal/qbt"
	"pmss-fleet-manager/internal/rtorrent"
	"pmss-fleet-manager/internal/sshx"
	"pmss-fleet-manager/internal/store"
)

// Hook runs with the new state after each successful or failed poll, when a
// disabled box is (re)started, and after a services sync re-attaches plans.
type Hook func(ctx context.Context, s *BoxState)

// ErrDisabled is returned by actions on a box whose polling is switched off.
var ErrDisabled = errors.New("polling is disabled for this box; enable it before running actions")

// ErrNotReady is returned by actions that would use facts read from the box
// (its UID, container IDs, log list) before a complete probe of the box's
// current generation has read them (BoxState.Ready).
var ErrNotReady = errors.New("the box has not been probed since it was added, moved or the dashboard started; wait for the first poll")

// Manager owns one poller per box.
type Manager struct {
	st       *store.Store
	bus      *Bus
	log      *slog.Logger
	settings atomic.Pointer[store.Settings]

	mu      sync.RWMutex
	states  map[int64]*BoxState
	pollers map[int64]*poller
	// rev counts every commit and removal, fleet-wide. Each stored state
	// carries the rev it was stored under and each state/removed event the
	// rev it reports, so a client can tell a stale event from a newer
	// snapshot (see commit and Snapshot).
	rev uint64

	hooks  []Hook
	latest latestRelease

	boxMu sync.Mutex // serialises AddBox/UpdateBox/RemoveBox

	// Destructive jobs (remove-with-data's erase and deletion) run under a
	// context of their own, not the one Start was given: that one is the
	// process's shutdown signal, cancelled before Stop even runs, and a
	// deletion admitted before shutdown must finish (daybreak blue
	// re-review of PR 123). jobMu guards admission; once closed, nothing new
	// starts, and Stop drains jobWG before cancelling jobCtx.
	jobCtx     context.Context
	jobCancel  context.CancelFunc
	jobWG      sync.WaitGroup
	jobMu      sync.Mutex
	jobsClosed bool
	// rtLeases are per-box mutation leases for rTorrent (add, actions,
	// remove-with-data for its whole life): one mutation at a time per box,
	// whatever client or poller carries it. labelGuards serialise the
	// snapshot-and-write of a box's owed labels.
	leaseMu     sync.Mutex
	rtLeases    map[int64]chan struct{}
	labelGuards map[int64]*sync.Mutex
	// watchMu serialises SetAppWatch's read-modify-write of a box's watch
	// list: two toggles at once would otherwise each save a list missing
	// the other's change.
	watchMu sync.Mutex
	// hooksMu keeps the alert hooks and the state events in store order:
	// commit holds it from the moment a state is stored until its hooks
	// have run and its event is on the bus, so a stale evaluation can never
	// undo the alert change of a newer state and a client never sees an
	// older state after a newer one.
	// Lock order: hooksMu before mu before svcMu; hooksMu before pubMu.
	hooksMu sync.Mutex
	// pubMu keeps state events in store order without holding hooksMu
	// while the event is built (the unacknowledged-alert count is a
	// database read): commit takes it before releasing hooksMu, so the
	// next box's store and alert evaluation proceed meanwhile.
	pubMu sync.Mutex
	// alertMu serialises reading the unacknowledged count with taking the
	// next alert revision and handing the event to the bus, so revisions
	// and counts leave in one order; AlertsView reads both under it too.
	// Lock order: pubMu before alertMu; nothing is taken under alertMu.
	alertMu  sync.Mutex
	alertRev uint64
	pmapi    *pmapi.Client
	svcMu    sync.RWMutex
	svc      servicesCache
	syncMu   sync.Mutex // one SyncServices at a time: an older fetch must not land after a newer one

	// The pmdb.fyi catalogue integration (pmdb.go): pmdbMu guards pmdbSt,
	// pmdbSyncMu serialises SyncPMDB, pmdbWakeCh nudges the loop.
	pmdb       *pmdb.Client
	pmdbMu     sync.Mutex
	pmdbSyncMu sync.Mutex
	pmdbSt     pmdbState
	// pmdbSettled is the process-lifetime record of decided hosts (see
	// pmdbStatus.decided), keyed by catalogue name with the standing to
	// show ("listed" or "submitted"). Outside pmdbState on purpose: a reset
	// (a new key, a re-enable) and a host dropping out of the candidates
	// clear display, error and backoff state, never a decision, or a later
	// stale or hostile list could have a host posted a second time.
	pmdbSettled map[string]string // under pmdbMu
	pmdbWakeCh  chan struct{}

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a Manager. Call Start to begin polling.
func New(st *store.Store, log *slog.Logger) *Manager {
	m := &Manager{st: st, bus: NewBus(), log: log, states: map[int64]*BoxState{}, pollers: map[int64]*poller{},
		pmapi:       &pmapi.Client{URL: os.Getenv("PMSS_PMAPI_URL")}, // URL overrides are for testing against a mock
		pmdb:        &pmdb.Client{URL: os.Getenv("PMSS_PMDB_URL")},
		pmdbWakeCh:  make(chan struct{}, 1),
		rtLeases:    map[int64]chan struct{}{},
		labelGuards: map[int64]*sync.Mutex{}}
	m.jobCtx, m.jobCancel = context.WithCancel(context.Background())
	def := store.DefaultSettings()
	m.settings.Store(&def)
	return m
}

// Bus exposes the event bus.
func (m *Manager) Bus() *Bus { return m.bus }

// Publish sends an event with the current unacknowledged alert count and
// the next alert revision attached. Every read of the count, here and in
// AlertsView, takes its own revision under one lock, so a higher revision
// always carries a count read later: a client keeping the count with the
// highest revision can never go back to an older one, and equal revisions
// never occur. (Alert writes themselves happen outside the lock; a read
// after a write sees it, and the write's own Publish follows anyway.) The
// revision restarts with the process; a page that reconnects after a
// restart reloads itself on the changed instance id before it could refuse
// the low numbers.
func (m *Manager) Publish(e Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m.alertMu.Lock()
	defer m.alertMu.Unlock()
	e.Unacked, _ = m.st.CountUnacked(ctx)
	m.alertRev++
	e.AlertsRev = m.alertRev
	m.bus.Publish(e)
}

// AlertsView returns the unacknowledged alert count under a fresh alert
// revision (see Publish): a snapshot is ordered against events and against
// other snapshots by it.
func (m *Manager) AlertsView(ctx context.Context) (int, uint64) {
	m.alertMu.Lock()
	defer m.alertMu.Unlock()
	n, _ := m.st.CountUnacked(ctx)
	m.alertRev++
	return n, m.alertRev
}

// Settings returns the current settings snapshot.
func (m *Manager) Settings() store.Settings { return *m.settings.Load() }

// SetSettings swaps the settings used by pollers; a changed poll interval
// wakes them so they re-sleep with it. Thresholds apply at the next poll.
func (m *Manager) SetSettings(s store.Settings) {
	prev := m.settings.Swap(&s)
	if prev.PMAPIKey != s.PMAPIKey && m.ctx != nil {
		// The cached feed belongs to the old key: drop it first, so a
		// failed sync for the new key cannot leave the old account's
		// services, costs and renewal dates on the boxes. A removed key
		// needs nothing more (SyncServices would only forget again); a new
		// one is fetched by a goroutine the shutdown wait group tracks.
		m.forgetServices(m.ctx)
		if s.PMAPIKey != "" && m.ctx.Err() == nil {
			m.wg.Add(1)
			go func() {
				defer m.wg.Done()
				_ = m.SyncServices(m.ctx)
			}()
		}
	}
	if prev.PMDBUploadKey != s.PMDBUploadKey || prev.PMDBEnabled != s.PMDBEnabled {
		// A new key or a re-enable starts over from the public list: a
		// "key refused" standing must not outlive the key it was for.
		m.resetPMDB()
		if s.PMDBEnabled && s.PMDBUploadKey != "" {
			m.pmdbWake()
		}
	}
	if prev.PollIntervalSec == s.PollIntervalSec {
		return // thresholds and keys are read on the next poll anyway
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.pollers {
		p.wake() // re-sleep with the new interval
	}
}

// OnPoll registers a hook run after every poll.
func (m *Manager) OnPoll(h Hook) { m.hooks = append(m.hooks, h) }

// Start loads boxes and settings and starts polling.
func (m *Manager) Start(ctx context.Context) error {
	m.ctx, m.cancel = context.WithCancel(ctx)
	s, err := m.st.GetSettings(ctx)
	if err != nil {
		return err
	}
	m.settings.Store(&s)
	m.loadServicesCache(ctx)
	boxes, err := m.st.ListBoxes(ctx)
	if err != nil {
		return err
	}
	for _, b := range boxes {
		m.startPoller(b, nil)
	}
	m.wg.Add(3)
	go m.housekeeping()
	go m.servicesLoop()
	go m.pmdbLoop()
	return nil
}

// Stop ends all pollers and waits for them.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	// No new destructive job from here on; the admitted ones finish below.
	m.jobMu.Lock()
	m.jobsClosed = true
	m.jobMu.Unlock()
	m.mu.Lock()
	ps := make([]*poller, 0, len(m.pollers))
	for _, p := range m.pollers {
		ps = append(ps, p)
	}
	m.mu.Unlock()
	for _, p := range ps {
		p.stopAndWait()
		// The labels owed to magnets live in the poller's memory until a
		// poll records them; a poll cancelled by this stop did not. Write
		// them now, under a fresh bounded context (the manager's is gone).
		fctx, fcancel := context.WithTimeout(context.Background(), 10*time.Second)
		p.recordRtLabels(fctx)
		fcancel()
	}
	m.wg.Wait()
	// Admitted destructive jobs complete under their own deadlines before
	// their context goes: a deletion after an erase is never cut by a
	// shutdown, only by a hard kill of the process (documented).
	m.jobWG.Wait()
	m.jobCancel()
}

// ErrShuttingDown refuses a destructive job once Stop began.
var ErrShuttingDown = errors.New("the dashboard is shutting down; nothing was changed")

// detached runs fn under a context the manager owns (bounded by timeout and
// by the destructive-job lifetime, never by an HTTP request or the
// shutdown signal) and returns the channel its result arrives on. Stop
// drains these jobs before cancelling their context. Once Stop began, the
// channel carries ErrShuttingDown and fn does not run.
func (m *Manager) detached(timeout time.Duration, fn func(ctx context.Context) error) (res <-chan error, admitted bool) {
	ch := make(chan error, 1)
	m.jobMu.Lock()
	if m.jobsClosed {
		m.jobMu.Unlock()
		ch <- ErrShuttingDown
		return ch, false
	}
	m.jobWG.Add(1)
	m.jobMu.Unlock()
	ctx, cancel := context.WithTimeout(m.jobCtx, timeout)
	go func() {
		defer m.jobWG.Done()
		defer cancel()
		ch <- fn(ctx)
	}()
	return ch, true
}

// jobBase is the destructive-job context itself, for a phase that needs a
// deadline of its own rather than the remainder of its job's.
func (m *Manager) jobBase() context.Context { return m.jobCtx }

// rtLease is the per-box mutation lease for rTorrent: one add, action or
// remove-with-data at a time per box, whatever client or poller carries it,
// so a removal's erase-and-delete cannot interleave with an add of the same
// hash (daybreak blue re-review of PR 123). Acquire honours ctx.
func (m *Manager) rtLease(id int64) *boxLease {
	m.leaseMu.Lock()
	defer m.leaseMu.Unlock()
	ch, ok := m.rtLeases[id]
	if !ok {
		ch = make(chan struct{}, 1)
		ch <- struct{}{}
		m.rtLeases[id] = ch
	}
	return &boxLease{ch: ch}
}

type boxLease struct{ ch chan struct{} }

// Acquire takes the lease or gives up with ctx; the returned release must
// be called exactly once.
func (l *boxLease) Acquire(ctx context.Context) (release func(), err error) {
	select {
	case <-l.ch:
		var once sync.Once
		return func() { once.Do(func() { l.ch <- struct{}{} }) }, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("another rTorrent change on this box is still running: %w", ctx.Err())
	}
}

// labelGuard serialises the snapshot-and-write of a box's owed labels, so
// an older snapshot can never be written after a newer one.
func (m *Manager) labelGuard(id int64) *sync.Mutex {
	m.leaseMu.Lock()
	defer m.leaseMu.Unlock()
	g, ok := m.labelGuards[id]
	if !ok {
		g = &sync.Mutex{}
		m.labelGuards[id] = g
	}
	return g
}

func (m *Manager) housekeeping() {
	defer m.wg.Done()
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			ctx, cancel := context.WithTimeout(m.ctx, time.Minute)
			if err := m.st.PruneAlerts(ctx, 30*24*time.Hour); err != nil {
				m.log.Warn("prune alerts", "err", err)
			}
			cancel()
		}
	}
}

// startPoller builds the box's state (restored from the cache, or carried
// over from prev when an edit replaces a running poller), stores it and
// starts the run loop. prev is merged here, before the poller exists, so
// its first poll cannot start from a state that is about to be replaced.
func (m *Manager) startPoller(bp *store.Box, prev *BoxState) {
	if m.ctx.Err() != nil {
		return // shutting down: never add a poller after Stop began waiting
	}
	b := new(store.Box)
	*b = *bp // the poller owns its own copy (conn() writes HostKey into it)
	st := fromBox(b)
	// Resume from the last persisted state so a restart does not blank the
	// UI (an edit carries the live state instead and skips the read).
	var cached BoxState
	if prev == nil {
		// A cache from another generation (the record was moved and the
		// process died before UpdateBox dropped the row) is not this
		// machine's data: leave it to be overwritten by the first poll.
		if _, err := m.st.GetCache(m.ctx, b.ID, "state", &cached); err == nil && cached.ID == b.ID && sameGeneration(cached.generation(), b) {
			keep := st
			st = cached
			st.adoptIdentity(keep)
			st.resetVolatile()
		}
	}
	if prev != nil {
		// Keep the last measured data so the UI does not blank out on edit.
		keep := *prev
		keep.adoptIdentity(st)
		keep.resetVolatile() // port/creds may have changed: re-verify before any action
		// Same generation (UpdateBox starts a moved box from nothing): the
		// facts a complete probe read are still this machine's.
		keep.Ready = prev.Ready && b.Enabled && prev.Status != "disabled"
		if keep.Status == "disabled" && b.Enabled {
			keep.Status, keep.StatusReason, keep.LastError, keep.Failures = "pending", "", "", 0 // re-enabled: the first poll decides
		}
		st = keep
	}
	// The retry schedule belongs to the poller that set it; this one polls
	// at once. The failure count stays: it feeds the offline/degraded
	// decision, and a box down for hours must not dip to "degraded".
	st.NextPoll, st.Backoff = time.Time{}, false
	auth, authKnown := m.restoredAuth(b)
	rtLabels := m.restoredRtLabels(b)
	if !b.Enabled {
		st.Status, st.StatusReason, st.Failures = "disabled", "", 0 // nothing to retry; the reason belonged to the last poll
	}
	p := newPoller(m, b)
	p.rtLabels = rtLabels
	p.ctx, p.cancel = context.WithCancel(m.ctx)
	if authKnown {
		p.guard.Restore(auth)
	}
	// A login the add/edit wizard already saw rejected must not be retried
	// by the first poll; seed before the run loop can start.
	if ds, ok := detectGuardState(b); ok {
		p.guard.Restore(ds)
	}
	// No poll will ever run the hooks of a disabled box: let the alert
	// rules see the disabled status now so open alerts for it resolve.
	m.commit(b.ID, func(*BoxState) *BoxState {
		m.pollers[b.ID] = p // fn runs under m.mu: state and poller appear together
		return &st
	}, commitOpts{insert: true, hooks: !b.Enabled, publish: true})
	if b.Enabled {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			p.run(p.ctx)
		}()
	}
}

// restoredAuth loads the persisted WebUI hold for a box, if it was recorded
// for the box's current SSH endpoint (one recorded before a move does not
// apply).
func (m *Manager) restoredAuth(b *store.Box) (qbt.AuthState, bool) {
	var auth qbt.AuthState
	if _, err := m.st.GetCache(m.ctx, b.ID, authCacheKind, &auth); err != nil || auth.Endpoint != AuthEndpoint(b) {
		return qbt.AuthState{}, false
	}
	return auth, true
}

// runHooks evaluates the hooks on a copy of a state outside any lock.
func (m *Manager) runHooks(st BoxState) {
	for _, h := range m.hooks {
		h(m.ctx, &st)
	}
}

// States returns a full copy of every box state sorted by name.
func (m *Manager) States() []BoxState {
	states, _ := m.Snapshot()
	return states
}

// Snapshot is States with the revision it was taken at: every stored
// state has a Rev at or below it, and every later commit or removal a Rev
// above it, so a client can merge the snapshot with the events around it.
func (m *Manager) Snapshot() ([]BoxState, uint64) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]BoxState, 0, len(m.states))
	for _, s := range m.states {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Name != out[j].Name {
			return out[i].Name < out[j].Name
		}
		return out[i].ID < out[j].ID
	})
	return out, m.rev
}

// State returns a full copy of one box's state.
func (m *Manager) State(id int64) (BoxState, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.states[id]
	if !ok {
		return BoxState{}, false
	}
	return *s, true
}

// commitOpts selects what commit does around the store.
type commitOpts struct {
	insert  bool // store even when the box has no state yet (startPoller)
	hooks   bool // run the alert hooks on the stored state
	publish bool // send the state event
}

// commit is the only write path for a box's live state. fn runs under
// hooksMu and mu with a copy of the stored state (nil when the box has
// none) and returns the state to store, or nil to store nothing (a worker
// finding under the lock that its poller was retired); commit then attaches the service matched against the feed of that very moment,
// assigns the next fleet-wide revision, stores, runs the hooks, and hands
// the event to the bus in that same order (pubMu, taken before hooksMu is
// released). Stores, alert evaluations and events therefore happen in one
// order for every box, a feed swap can never leave a state matched against
// the feed it replaced (reattachServices re-commits every box after the
// swap), and ok is false when the box is gone.
func (m *Manager) commit(id int64, fn func(stored *BoxState) *BoxState, o commitOpts) (BoxState, bool) {
	m.hooksMu.Lock()
	m.mu.Lock()
	stored, ok := m.states[id]
	if !ok && !o.insert {
		m.mu.Unlock()
		m.hooksMu.Unlock()
		return BoxState{}, false
	}
	var base *BoxState
	if ok {
		cp := *stored // the stored object may be in a hook's hands on another goroutine
		base = &cp
	}
	next := fn(base)
	if next == nil {
		m.mu.Unlock()
		m.hooksMu.Unlock()
		return BoxState{}, false
	}
	m.decorate(next)
	m.rev++
	next.Rev = m.rev
	cp := *next
	m.states[id] = &cp
	m.mu.Unlock()
	if o.hooks {
		m.runHooks(cp)
	}
	if !o.publish {
		m.hooksMu.Unlock()
		return cp, true
	}
	m.pubMu.Lock()
	m.hooksMu.Unlock()
	sum := cp.Summary()
	m.Publish(Event{Type: "state", BoxID: id, Rev: cp.Rev, State: &sum})
	m.pubMu.Unlock()
	return cp, true
}

// setStateWith stores a poll result as the box's state; fix runs on it
// under the store lock right before it is stored, with the state being
// replaced, so a concurrent mutateState or du merge cannot slip in between
// the poller's last look and the store.
func (m *Manager) setStateWith(s *BoxState, fix func(s, stored *BoxState)) {
	s.Sanitize()
	m.commit(s.ID, func(stored *BoxState) *BoxState {
		fix(s, stored)
		return s
	}, commitOpts{hooks: true, publish: true})
}

// AddBox persists and starts polling a new box.
func (m *Manager) AddBox(ctx context.Context, b *store.Box) error {
	m.boxMu.Lock()
	defer m.boxMu.Unlock()
	if err := m.st.CreateBox(ctx, b); err != nil {
		return err
	}
	m.startPoller(b, nil) // publishes the new box
	return nil
}

// UpdateBox persists changes and restarts the poller with fresh connections.
func (m *Manager) UpdateBox(ctx context.Context, b *store.Box, clears store.Clears) error {
	m.boxMu.Lock()
	defer m.boxMu.Unlock()
	if err := m.st.UpdateBox(ctx, b, clears); err != nil {
		return err
	}
	m.mu.Lock()
	p := m.pollers[b.ID]
	m.mu.Unlock()
	if p != nil {
		p.stopAndWait()
		// Labels the retiring poller still owes to magnets live only in its
		// memory (a poll cancelled by the stop did not record them): write
		// them for the record as it was, so the replacement poller finds
		// them in the cache (startPoller) unless the box moved, in which
		// case they go with the other caches below (Codex on PR 123).
		p.recordRtLabels(m.ctx)
	}
	// Snapshot the state only now: a poll that was finishing while we
	// waited has written its result, and that is what the UI should keep.
	m.mu.Lock()
	prev := m.states[b.ID]
	m.mu.Unlock()
	// Read the record only after the old poller is gone: its last
	// successful dial could otherwise pin a host key onto the record
	// between our write and this read.
	// From here on the box has no poller: the restart half must not be
	// abandoned because the browser went away mid-request, so it runs on
	// the manager's own context, not the request's.
	sctx, scancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer scancel()
	// A watch toggle landing between this read and startPoller's store
	// would be in the database but not in the new state: keep them apart
	// (after stopAndWait, so a toggle never waits on an in-flight poll).
	m.watchMu.Lock()
	defer m.watchMu.Unlock()
	fresh, err := m.st.GetBox(sctx, b.ID)
	if err != nil {
		if p != nil {
			m.log.Warn("box edit saved but the record could not be re-read; polling continues with the previous settings until the next edit", "box", p.box.Name, "err", err)
			m.startPoller(p.box, prev) // keep polling with the old settings rather than nothing
		}
		return err
	}
	if b.QbtPass != "" || authEndpointChanged(p, fresh) {
		// A password was typed in (an explicit request to try again, even
		// with the same one the hold was recorded for), or the endpoint
		// the hold belongs to changed (host, port, account or WebUI port):
		// drop the hold. After stopAndWait, so the old poller's last
		// record() cannot write it back; the wizard's memory of it goes too.
		_ = m.st.SetCache(sctx, b.ID, authCacheKind, qbt.AuthState{})
		detectGuard(fresh).Clear()
	}
	if p != nil && (AuthEndpoint(p.box) != AuthEndpoint(fresh) || clears.ForgetHostKey) {
		// The record now names another machine (or may, once the forgotten
		// pin is replaced): nothing measured on the old one carries over.
		// The new poller starts from the record alone, its actions wait
		// for the first complete probe (BoxState.Ready), and the cached
		// state and du go, so a restart cannot bring them back either; a
		// row that survives a failed delete is refused by its generation.
		// The old machine's alerts resolve, except the renewal alert, which
		// is the record's and would otherwise come back unacknowledged.
		prev = nil
		if err := m.st.DeleteCache(sctx, b.ID, "state", "du", rtLabelsCacheKind); err != nil {
			m.log.Warn("drop cache of moved box", "box", fresh.Name, "err", err)
		}
		if n, err := m.st.ResolveBoxAlerts(sctx, b.ID, "renewal"); err != nil {
			m.log.Warn("resolve alerts of moved box", "box", fresh.Name, "err", err)
		} else if n > 0 {
			defer m.Publish(Event{Type: "alerts"}) // after the state event startPoller sends
		}
	}
	// boxMu serialises edits and removals, so this only guards against a
	// RemoveBox whose DB delete failed after the in-memory removal.
	m.mu.Lock()
	_, stillHere := m.states[b.ID]
	m.mu.Unlock()
	if !stillHere {
		return store.ErrNotFound
	}
	m.startPoller(fresh, prev) // carries the last measured data over; runs the hooks if disabled; publishes
	return nil
}

// authEndpointChanged reports whether an edit moved the WebUI the hold was
// recorded for: a different SSH endpoint or a different WebUI port.
func authEndpointChanged(p *poller, fresh *store.Box) bool {
	if p == nil {
		return false
	}
	return AuthEndpoint(p.box) != AuthEndpoint(fresh) || p.box.QbtPort != fresh.QbtPort
}

// RemoveBox stops polling and deletes the box, its alerts and its cache.
func (m *Manager) RemoveBox(ctx context.Context, id int64) error {
	m.boxMu.Lock()
	defer m.boxMu.Unlock()
	// The removal is a commit like any other: the box leaves memory under
	// hooksMu (a commit that stored before this has put its event on the
	// bus first, and none can store after it) and its event goes out under
	// pubMu at its revision, before any later commit's. The row is deleted
	// afterwards: the box is gone from the live view either way, and a
	// failed delete is reported to the caller (and the row resurfaces at
	// the next start).
	m.hooksMu.Lock()
	m.mu.Lock()
	p := m.pollers[id]
	_, had := m.states[id]
	delete(m.pollers, id)
	delete(m.states, id)
	m.rev++
	rev := m.rev
	m.mu.Unlock()
	m.pubMu.Lock()
	m.hooksMu.Unlock()
	if had {
		m.Publish(Event{Type: "removed", BoxID: id, Rev: rev})
	}
	m.pubMu.Unlock()
	if p != nil {
		p.stopAndWait()
	}
	// The box is already gone from memory: finish the delete on the
	// manager's context so a closed browser cannot leave the row behind.
	sctx, scancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer scancel()
	if err := m.st.DeleteBox(sctx, id); err != nil {
		return err
	}
	// The removed event counted the box's alerts before the delete
	// cascaded them away: tell every tab the count and list changed.
	m.Publish(Event{Type: "alerts"})
	return nil
}

// Refresh asks one poller to poll now.
func (m *Manager) Refresh(id int64) error {
	p := m.poller(id)
	if p == nil {
		return store.ErrNotFound
	}
	if !p.box.Enabled {
		return ErrDisabled
	}
	p.wakeInventory() // a person asked: re-read the inventory too (logs, crontab, binary)
	return nil
}

func (m *Manager) poller(id int64) *poller {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return m.pollers[id]
}

// ErrUnavailable is returned when an action needs a connection that is down.
var ErrUnavailable = errors.New("box not reachable")

// QbtAction performs pause|resume|start|stop|restart|install|autostart on the
// box's qBittorrent and returns any output worth showing.
func (m *Manager) QbtAction(ctx context.Context, id int64, action string) (string, error) {
	p := m.poller(id)
	if p == nil {
		return "", store.ErrNotFound
	}
	if !p.box.Enabled {
		return "", ErrDisabled
	}
	var out string
	var err error
	switch action {
	case "install":
		out, err = p.qbtInstall(ctx, m.Settings().QbtDownloadURL)
	case "restart":
		out, err = p.qbtRestart(ctx)
		if err == nil {
			p.guard.Clear() // a fresh qbittorrent-nox has a fresh failed-login counter
			detectGuard(p.box).Clear()
		}
	case "autostart":
		out, err = p.qbtAutostart(ctx)
	default:
		// "start" is not cleared: qbt-start exits 0 when the client is
		// already running, so it does not prove a fresh process.
		err = p.qbtAction(ctx, action)
	}
	if err == nil && (action == "restart" || action == "stop") {
		p.noteRestartDone() // the client that is up at the next poll is a new one (see restartAt)
	}
	if action == "restart" || action == "stop" {
		// The port may be free until the client is back, whether or not
		// the action reported success (a restart can stop the client and
		// then fail to start it): whoever binds it next must be verified
		// again before any credential-sending action. The poller also notes
		// the time, so a poll that was already in flight cannot store the
		// verification it made before the port was freed.
		m.mutateState(id, func(s *BoxState) {
			p.noteAction() // under the store lock: a poll's store is either before both or after both
			s.Qbt.PortOK, s.Qbt.Reachable = false, false
			// FirewalledSince is deliberately left alone: the action may have
			// failed before touching the client. A client that did stop or
			// come back as a new process is noticed by the next poll
			// (process gone, a changed pid/start tick, or the successful
			// action recorded above), which starts the new clock then.
		})
	}
	if action == "install" || action == "autostart" {
		p.wakeInventory() // the binary or the crontab changed: the inventory sections must see it
	} else {
		p.wake()
	}
	return out, err
}

// mutateState applies fn to a copy of the stored state and stores the
// copy, without hooks or an event (the next poll reports the result).
func (m *Manager) mutateState(id int64, fn func(*BoxState)) {
	m.commit(id, func(s *BoxState) *BoxState {
		fn(s)
		return s
	}, commitOpts{})
}

// SetAppWatch adds or removes one detected application from the box's
// watch list, persists it, and re-evaluates the alert rules at once so an
// "app not running" alert appears or resolves without waiting for a poll.
func (m *Manager) SetAppWatch(ctx context.Context, id int64, key string, on bool) error {
	if !ValidAppKey(key) {
		return fmt.Errorf("invalid app key %q", key)
	}
	p := m.poller(id)
	if p == nil {
		return store.ErrNotFound
	}
	m.watchMu.Lock()
	defer m.watchMu.Unlock()
	st, ok := m.State(id)
	if !ok {
		return store.ErrNotFound
	}
	keys := make([]string, 0, len(st.Apps.Watched)+1)
	for _, k := range st.Apps.Watched {
		if k != key {
			keys = append(keys, k)
		}
	}
	if on {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if err := m.st.SetAppsWatch(ctx, id, keys); err != nil {
		return err
	}
	// Evaluate skips an Unpolled state itself; the first poll raises the
	// app alert then.
	if _, ok := m.commit(id, func(s *BoxState) *BoxState {
		s.Apps.Watched = keys
		s.Apps.markWatched()
		return s
	}, commitOpts{hooks: true, publish: true}); !ok {
		return store.ErrNotFound
	}
	return nil
}

// ContainerAction performs start|stop|restart on a container.
func (m *Manager) ContainerAction(ctx context.Context, id int64, cid, action string) error {
	p := m.poller(id)
	if p == nil {
		return store.ErrNotFound
	}
	if !p.box.Enabled {
		return ErrDisabled
	}
	err := p.containerAction(ctx, cid, action)
	p.wake()
	return err
}

// TorrentAction performs one qbt.TorrentActions entry on a single torrent
// and wakes the poller so the list reflects it promptly.
func (m *Manager) TorrentAction(ctx context.Context, id int64, hash, action string) error {
	p := m.poller(id)
	if p == nil {
		return store.ErrNotFound
	}
	if !p.box.Enabled {
		return ErrDisabled
	}
	err := p.torrentAction(ctx, hash, action)
	p.wake()
	return err
}

// RtTorrentAction performs one RtTorrentActions entry on a torrent in the
// box's rTorrent and wakes the poller so the list reflects it promptly.
func (m *Manager) RtTorrentAction(ctx context.Context, id int64, hash, action string) error {
	if !ValidRtAction(action) {
		return fmt.Errorf("unknown rTorrent action %q", action)
	}
	p := m.poller(id)
	if p == nil {
		return store.ErrNotFound
	}
	if !p.box.Enabled {
		return ErrDisabled
	}
	err := p.rtTorrentAction(ctx, hash, action)
	p.wake()
	return err
}

// RtAddTorrents adds links and/or .torrent files to the box's rTorrent.
func (m *Manager) RtAddTorrents(ctx context.Context, id int64, req qbt.AddRequest) (rtorrent.AddResult, error) {
	p := m.poller(id)
	if p == nil {
		return rtorrent.AddResult{}, store.ErrNotFound
	}
	if !p.box.Enabled {
		return rtorrent.AddResult{}, ErrDisabled
	}
	res, err := p.rtAddTorrents(ctx, req)
	p.wake()
	return res, err
}

// DiscoveryKeys are the preferences SetDiscovery may change.
var DiscoveryKeys = map[string]bool{"dht": true, "pex": true, "lsd": true}

// SetDiscovery flips peer-discovery switches on the box's qBittorrent and
// reflects them in the cached state at once; the next poll re-reads them.
func (m *Manager) SetDiscovery(ctx context.Context, id int64, want map[string]bool) error {
	p := m.poller(id)
	if p == nil {
		return store.ErrNotFound
	}
	if !p.box.Enabled {
		return ErrDisabled
	}
	prefs := make(map[string]any, len(want))
	for k, v := range want {
		if !DiscoveryKeys[k] {
			return fmt.Errorf("unknown discovery setting %q", k)
		}
		prefs[k] = v
	}
	if len(prefs) == 0 {
		return errors.New("nothing to change")
	}
	err := p.setDiscovery(ctx, prefs)
	if err == nil {
		m.mutateState(id, func(s *BoxState) {
			p.noteDiscovery(want) // under the store lock, see QbtAction
			if !s.Qbt.Discovery.Known {
				return
			}
			for k, v := range want {
				switch k {
				case "dht":
					s.Qbt.Discovery.DHT = v
				case "pex":
					s.Qbt.Discovery.PeX = v
				case "lsd":
					s.Qbt.Discovery.LSD = v
				}
			}
		})
	}
	p.wake()
	return err
}

// AddTorrents adds links and/or .torrent files to the box's qBittorrent.
func (m *Manager) AddTorrents(ctx context.Context, id int64, req qbt.AddRequest) (qbt.AddResult, error) {
	p := m.poller(id)
	if p == nil {
		return qbt.AddResult{}, store.ErrNotFound
	}
	if !p.box.Enabled {
		return qbt.AddResult{}, ErrDisabled
	}
	res, err := p.addTorrents(ctx, req)
	p.wake()
	return res, err
}

// ReadLog returns the tail of one of the box's listed log files.
func (m *Manager) ReadLog(ctx context.Context, id int64, path string) (string, error) {
	p := m.poller(id)
	if p == nil {
		return "", store.ErrNotFound
	}
	if !p.box.Enabled {
		return "", ErrDisabled
	}
	return p.readLog(ctx, path)
}

// ContainerLogs returns the tail of a container's output.
func (m *Manager) ContainerLogs(ctx context.Context, id int64, cid string) (string, error) {
	p := m.poller(id)
	if p == nil {
		return "", store.ErrNotFound
	}
	if !p.box.Enabled {
		return "", ErrDisabled
	}
	return p.containerLogs(ctx, cid)
}

// RunDu forces a directory usage measurement.
func (m *Manager) RunDu(id int64) error {
	p := m.poller(id)
	if p == nil {
		return store.ErrNotFound
	}
	if !p.box.Enabled {
		return ErrDisabled
	}
	p.forceDu()
	return nil
}

func fingerprintOrEmpty(hostKey string) string {
	if hostKey == "" {
		return ""
	}
	return sshx.Fingerprint(hostKey)
}

func describeErr(err error) string {
	if err == nil {
		return ""
	}
	return clip(err.Error(), 300)
}

func pct(used, total int64) float64 {
	if total <= 0 {
		return 0
	}
	return float64(used) / float64(total) * 100
}

// RawProbe runs the probe once over the box's connection and returns the raw
// sections, for diagnostics. Values are truncated to keep responses small.
func (m *Manager) RawProbe(ctx context.Context, id int64) (map[string]string, error) {
	p := m.poller(id)
	if p == nil {
		return nil, store.ErrNotFound
	}
	if !p.box.Enabled {
		return nil, ErrDisabled
	}
	return p.rawProbe(ctx)
}
