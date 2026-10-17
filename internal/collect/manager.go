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
	"log/slog"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"pmss-fleet-manager/internal/pmapi"
	"pmss-fleet-manager/internal/qbt"
	"pmss-fleet-manager/internal/sshx"
	"pmss-fleet-manager/internal/store"
)

// Hook runs after each successful or failed poll with the new state.
type Hook func(ctx context.Context, s *BoxState)

// Manager owns one poller per box.
type Manager struct {
	st       *store.Store
	bus      *Bus
	log      *slog.Logger
	settings atomic.Pointer[store.Settings]

	mu      sync.RWMutex
	states  map[int64]*BoxState
	pollers map[int64]*poller

	hooks  []Hook
	latest latestRelease

	pmapi *pmapi.Client
	svcMu sync.RWMutex
	svc   servicesCache

	ctx    context.Context
	cancel context.CancelFunc
	wg     sync.WaitGroup
}

// New creates a Manager. Call Start to begin polling.
func New(st *store.Store, log *slog.Logger) *Manager {
	m := &Manager{st: st, bus: NewBus(), log: log, states: map[int64]*BoxState{}, pollers: map[int64]*poller{},
		pmapi: &pmapi.Client{URL: os.Getenv("PMSS_PMAPI_URL")}} // URL override is for testing against a mock feed
	def := store.DefaultSettings()
	m.settings.Store(&def)
	return m
}

// Bus exposes the event bus.
func (m *Manager) Bus() *Bus { return m.bus }

// Publish sends an event with the current unacknowledged alert count attached.
func (m *Manager) Publish(e Event) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.Unacked, _ = m.st.CountUnacked(ctx)
	m.bus.Publish(e)
}

// Settings returns the current settings snapshot.
func (m *Manager) Settings() store.Settings { return *m.settings.Load() }

// SetSettings swaps the settings used by pollers and wakes them.
func (m *Manager) SetSettings(s store.Settings) {
	prev := m.settings.Swap(&s)
	if prev.PMAPIKey != s.PMAPIKey && m.ctx != nil {
		go m.SyncServices(m.ctx)
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	for _, p := range m.pollers {
		p.wake()
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
		m.startPoller(b)
	}
	m.wg.Add(2)
	go m.housekeeping()
	go m.servicesLoop()
	return nil
}

// Stop ends all pollers and waits for them.
func (m *Manager) Stop() {
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Lock()
	ps := make([]*poller, 0, len(m.pollers))
	for _, p := range m.pollers {
		ps = append(ps, p)
	}
	m.mu.Unlock()
	for _, p := range ps {
		p.stopAndWait()
	}
	m.wg.Wait()
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

func (m *Manager) startPoller(bp *store.Box) {
	if m.ctx.Err() != nil {
		return // shutting down: never add a poller after Stop began waiting
	}
	b := new(store.Box)
	*b = *bp // the poller owns its own copy (conn() writes HostKey into it)
	st := fromBox(b)
	// Resume from the last persisted state so a restart does not blank the UI.
	var cached BoxState
	if _, err := m.st.GetCache(m.ctx, b.ID, "state", &cached); err == nil && cached.ID == b.ID {
		keep := st
		st = cached
		st.adoptIdentity(keep)
		st.resetVolatile()
	}
	var auth qbt.AuthState
	authKnown := false
	if _, err := m.st.GetCache(m.ctx, b.ID, authCacheKind, &auth); err == nil {
		authKnown = true
	}
	if !b.Enabled {
		st.Status = "disabled"
	}
	m.decorate(&st)
	m.mu.Lock()
	m.states[b.ID] = &st
	p := newPoller(m, b)
	p.ctx, p.cancel = context.WithCancel(m.ctx)
	if authKnown {
		p.guard.Restore(auth)
	}
	// A login the add/edit wizard already saw rejected must not be retried
	// by the first poll; seed before the run loop can start.
	if ds, ok := detectGuardState(b.Host, b.QbtPort, b.QbtUser, b.QbtPass); ok {
		p.guard.Restore(ds)
	}
	m.pollers[b.ID] = p
	m.mu.Unlock()
	if b.Enabled {
		m.wg.Add(1)
		go func() {
			defer m.wg.Done()
			p.run(p.ctx)
		}()
	} else {
		// No poll will ever run the hooks: let the alert rules see the
		// disabled status now so open alerts for this box resolve.
		m.runHooks(st)
	}
}

// runHooks evaluates the hooks on a copy of a state outside any lock.
func (m *Manager) runHooks(st BoxState) {
	for _, h := range m.hooks {
		h(m.ctx, &st)
	}
}

// States returns a full copy of every box state sorted by name.
func (m *Manager) States() []BoxState {
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
	return out
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

func (m *Manager) setState(s *BoxState) {
	m.mu.Lock()
	if _, ok := m.states[s.ID]; !ok { // box was removed mid-poll
		m.mu.Unlock()
		return
	}
	m.mu.Unlock()
	m.decorate(s)
	m.mu.Lock()
	if _, ok := m.states[s.ID]; !ok {
		m.mu.Unlock()
		return
	}
	cp := *s
	m.states[s.ID] = &cp
	m.mu.Unlock()
	for _, h := range m.hooks {
		h(m.ctx, &cp)
	}
	sum := cp.Summary()
	m.Publish(Event{Type: "state", BoxID: cp.ID, State: &sum})
}

// AddBox persists and starts polling a new box.
func (m *Manager) AddBox(ctx context.Context, b *store.Box) error {
	if err := m.st.CreateBox(ctx, b); err != nil {
		return err
	}
	m.startPoller(b)
	if s, ok := m.State(b.ID); ok {
		sum := s.Summary()
		m.Publish(Event{Type: "state", BoxID: b.ID, State: &sum})
	}
	return nil
}

// UpdateBox persists changes and restarts the poller with fresh connections.
func (m *Manager) UpdateBox(ctx context.Context, b *store.Box) error {
	if err := m.st.UpdateBox(ctx, b); err != nil {
		return err
	}
	m.mu.Lock()
	p := m.pollers[b.ID]
	prev := m.states[b.ID]
	m.mu.Unlock()
	if p != nil {
		p.stopAndWait()
	}
	// Read the record only after the old poller is gone: its last
	// successful dial could otherwise pin a host key onto the record
	// between our write and this read.
	fresh, err := m.st.GetBox(ctx, b.ID)
	if err != nil {
		return err
	}
	if b.QbtPass != "" {
		// A password was typed in: treat it as an explicit request to try
		// again, even if it is the same one the hold was recorded for.
		// After stopAndWait, so the old poller's last record() cannot
		// write the hold back.
		_ = m.st.SetCache(ctx, b.ID, authCacheKind, qbt.AuthState{})
	}
	m.startPoller(fresh)
	// Keep the last measured data so the UI does not blank out on edit.
	if prev != nil {
		m.mu.Lock()
		cur := m.states[fresh.ID]
		keep := *prev
		keep.adoptIdentity(*cur)
		keep.Service = cur.Service
		keep.resetVolatile() // port/creds may have changed: re-verify before any action
		if !fresh.Enabled {
			keep.Status = "disabled"
		}
		m.states[fresh.ID] = &keep
		m.mu.Unlock()
		if !fresh.Enabled {
			m.runHooks(keep) // resolve open alerts: nothing will poll this box now
		}
	}
	if s, ok := m.State(fresh.ID); ok {
		sum := s.Summary()
		m.Publish(Event{Type: "state", BoxID: fresh.ID, State: &sum})
	}
	return nil
}

// RemoveBox stops polling and deletes the box, its alerts and its cache.
func (m *Manager) RemoveBox(ctx context.Context, id int64) error {
	m.mu.Lock()
	p := m.pollers[id]
	delete(m.pollers, id)
	delete(m.states, id)
	m.mu.Unlock()
	if p != nil {
		p.stopAndWait()
	}
	if err := m.st.DeleteBox(ctx, id); err != nil {
		return err
	}
	m.Publish(Event{Type: "removed", BoxID: id})
	return nil
}

// Refresh asks one poller to poll now.
func (m *Manager) Refresh(id int64) error {
	p := m.poller(id)
	if p == nil {
		return store.ErrNotFound
	}
	p.wake()
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
		return "", errors.New("polling is disabled for this box; enable it before running actions")
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
			detectGuard(p.box.Host, p.box.QbtPort, p.box.QbtUser, p.box.QbtPass).Clear()
		}
	case "autostart":
		out, err = p.qbtAutostart(ctx)
	default:
		// "start" is not cleared: qbt-start exits 0 when the client is
		// already running, so it does not prove a fresh process.
		err = p.qbtAction(ctx, action)
	}
	if err == nil && (action == "restart" || action == "stop") {
		// The port is free until the client is back: whoever binds it next
		// must be verified again before any credential-sending action.
		m.mutateState(id, func(s *BoxState) { s.Qbt.PortOK, s.Qbt.Reachable = false, false })
	}
	p.wake()
	return out, err
}

// mutateState applies fn to a copy of the stored state and stores the copy.
func (m *Manager) mutateState(id int64, fn func(*BoxState)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if cur, ok := m.states[id]; ok {
		cp := *cur
		fn(&cp)
		m.states[id] = &cp
	}
}

// ContainerAction performs start|stop|restart on a container.
func (m *Manager) ContainerAction(ctx context.Context, id int64, cid, action string) error {
	p := m.poller(id)
	if p == nil {
		return store.ErrNotFound
	}
	if !p.box.Enabled {
		return errors.New("polling is disabled for this box; enable it before running actions")
	}
	err := p.containerAction(ctx, cid, action)
	p.wake()
	return err
}

// RunDu forces a directory usage measurement.
func (m *Manager) RunDu(id int64) error {
	p := m.poller(id)
	if p == nil {
		return store.ErrNotFound
	}
	if !p.box.Enabled {
		return errors.New("polling is disabled for this box; enable it before running actions")
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
	return clip(err.Error(), 200)
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
		return nil, errors.New("polling is disabled for this box; enable it before running actions")
	}
	return p.rawProbe(ctx)
}
