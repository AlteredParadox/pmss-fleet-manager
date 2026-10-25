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
	"fmt"
	"math"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"

	"pmss-fleet-manager/internal/dockerx"
	"pmss-fleet-manager/internal/probe"
	"pmss-fleet-manager/internal/qbt"
	"pmss-fleet-manager/internal/rtorrent"
	"pmss-fleet-manager/internal/sshx"
	"pmss-fleet-manager/internal/store"
)

const (
	probeTimeout  = 45 * time.Second
	apiTimeout    = 30 * time.Second
	duTimeout     = 20 * time.Minute
	defaultQbtCmd = `cd "$HOME" && ( "$HOME/bin/qbittorrent-nox" -d </dev/null >/dev/null 2>&1 )`
)

type poller struct {
	m   *Manager
	box *store.Box

	mu    sync.Mutex
	ssh   *sshx.Client
	qbt   *qbt.Client
	qbtP  int
	dk    *dockerx.Client
	dkUID int            // the UID dk was dialled for
	guard *qbt.AuthGuard // outlives p.qbt; see qbt.AuthGuard

	prevCPU     int64
	prevCPUAt   time.Time
	prevTicks   probe.CPUTicks // last /proc/stat reading, for the host-wide busy figure
	prevTicksAt time.Time
	procPrev    map[int]procSample // last poll's per-pid CPU readings, see appsFromSnapshot
	// qbtBinStat/qbtBinVersion remember the installed binary's stat and the
	// version it answered, so the probe can skip --version (which writes to
	// the user's qBittorrent log) while the binary is unchanged.
	qbtBinStat, qbtBinVersion string
	// inv is the last probe that ran the inventory sections (trimmed to
	// them) and invAt when; fast polls carry it (every inventoryInterval
	// the sections run again). invWant asks the next poll to run them
	// regardless of age: a manual refresh, an install or an autostart
	// change, or the logs tab finding its listing stale.
	inv     *probe.Snapshot
	invAt   time.Time
	invWant atomic.Bool
	// qbtProcKey identifies the running qbittorrent-nox process(es) by pid
	// and start tick, so a restart between two polls that both saw the
	// client up is noticed (see carriedFirewalledSince).
	qbtProcKey   string
	qbtRestarted bool
	pollStart    time.Time // when the current poll began (dashboard clock), for takeRestartBefore
	// dialTCP builds the dialer the WebUI client uses; nil means the SSH
	// tunnel (sshDialTCP). Tests point it at a fake WebUI so the real
	// pollQbt orchestration runs without a box.
	dialTCP func(addr string) qbt.Dialer
	// rt is the rTorrent client for rtEP (see rtClient); dialRt is the test
	// hook that replaces the SSH channel to the SCGI endpoint.
	rt       *rtorrent.Client
	rtEP     string
	rtSSH    *sshx.Client            // the transport rt's dialer is bound to; a new one means a new client
	rtLabels []rtorrent.PendingLabel // owed magnet labels restored from the cache, handed to the first client
	dialRt   func(ep string) rtorrent.Dialer

	duMu      sync.Mutex
	duRunning bool
	duForce   bool
	lastDu    time.Time      // when a measurement last completed
	duRetryAt time.Time      // earliest automatic attempt after a failed one (see duRetry)
	duWG      sync.WaitGroup // the runDu goroutine; stopAndWait joins it

	wakeCh chan struct{}
	stopCh chan struct{}
	doneCh chan struct{}
	once   sync.Once

	// ctx is cancelled by stopAndWait so an in-flight poll, du or action
	// aborts promptly instead of blocking a box edit/removal for minutes.
	ctx    context.Context
	cancel context.CancelFunc

	actionMu sync.Mutex
	actionAt time.Time // last stop/restart, see QbtAction
	// restartAt is when a stop/restart action last SUCCEEDED and has not
	// yet been seen by a poll: the first readable process poll started
	// after it treats whatever client is up as a new process, which
	// matters when the poller has no identity baseline yet (a fresh
	// poller after a dashboard start or a box edit).
	restartAt time.Time
	discWant  map[string]discWrite // per-key SetDiscovery writes, see noteDiscovery
}

// discWrite is one value SetDiscovery wrote and when.
type discWrite struct {
	v  bool
	at time.Time
}

func (p *poller) noteAction() {
	p.actionMu.Lock()
	p.actionAt = time.Now()
	p.actionMu.Unlock()
}

// noteRestartDone records a stop/restart that succeeded (see restartAt).
func (p *poller) noteRestartDone() {
	p.actionMu.Lock()
	p.restartAt = time.Now()
	p.actionMu.Unlock()
}

// takeRestartBefore consumes a recorded successful restart if it happened
// before pollStart, i.e. the poll that started then observed the box
// after it. A later restart stays recorded for the next poll.
func (p *poller) takeRestartBefore(pollStart time.Time) bool {
	p.actionMu.Lock()
	defer p.actionMu.Unlock()
	if p.restartAt.IsZero() || !p.restartAt.Before(pollStart) {
		return false
	}
	p.restartAt = time.Time{}
	return true
}

// actionSince reports whether a stop/restart happened after t.
func (p *poller) actionSince(t time.Time) bool {
	p.actionMu.Lock()
	defer p.actionMu.Unlock()
	return p.actionAt.After(t)
}

// noteDiscovery records a successful app/setPreferences write so a poll
// that read the preferences before it cannot publish the old values. Each
// key keeps its own write time: a poll that straddles two writes sees both,
// and a key written before the poll started is left to what the poll read.
func (p *poller) noteDiscovery(want map[string]bool) {
	p.actionMu.Lock()
	if p.discWant == nil {
		p.discWant = map[string]discWrite{}
	}
	now := time.Now()
	for k, v := range want {
		p.discWant[k] = discWrite{v: v, at: now}
	}
	p.actionMu.Unlock()
}

// overlayDiscovery applies the values written by SetDiscovery after the
// poll started on top of what the poll read, so the UI never flips back.
func (p *poller) overlayDiscovery(start time.Time, d *Discovery) {
	p.actionMu.Lock()
	defer p.actionMu.Unlock()
	if !d.Known {
		return
	}
	for k, w := range p.discWant {
		if !w.at.After(start) {
			continue
		}
		switch k {
		case "dht":
			d.DHT = w.v
		case "pex":
			d.PeX = w.v
		case "lsd":
			d.LSD = w.v
		}
	}
}

// afterPoll reconciles the poll's result with what happened while it was
// in flight: a stop/restart or discovery write must not be undone by what
// the poll read. It runs once before classify (so the status reflects it)
// and again under the store lock as the state is published (for anything
// that landed in between, even during record()).
func (p *poller) afterPoll(start time.Time) func(*BoxState) {
	return func(s *BoxState) {
		if p.actionSince(start) {
			s.Qbt.PortOK, s.Qbt.Reachable = false, false
		}
		p.overlayDiscovery(start, &s.Qbt.Discovery)
	}
}

// storeFix adapts afterPoll to setStateWith and keeps the freshest Dirs: a
// du result merged into the stored state while the poll was in flight is
// newer than anything the poll carries. The failure path (keepDirs) takes
// the stored Dirs wholesale, since that poll measured nothing; the success
// path takes them only when they are newer (a du this poll just started
// shares the stored At and is kept as Running).
func (p *poller) storeFix(start time.Time, keepDirs bool) func(s, stored *BoxState) {
	fix := p.afterPoll(start)
	return func(s, stored *BoxState) {
		fix(s)
		if stored == nil {
			return
		}
		if keepDirs || stored.Dirs.At.After(s.Dirs.At) {
			s.Dirs = stored.Dirs
		}
		// The watch list is edited through the store, never by a poll: a
		// toggle that landed while this poll was in flight wins.
		s.Apps.Watched = stored.Apps.Watched
		s.Apps.markWatched()
		// Readiness is the generation's, not the poll's: a failed poll
		// that started before the probe that set it must not unset it.
		s.Ready = s.Ready || stored.Ready
	}
}

func newPoller(m *Manager, b *store.Box) *poller {
	return &poller{m: m, box: b, guard: &qbt.AuthGuard{}, wakeCh: make(chan struct{}, 1), stopCh: make(chan struct{}), doneCh: make(chan struct{})}
}

func (p *poller) wake() {
	select {
	case p.wakeCh <- struct{}{}:
	default:
	}
}

// inventoryInterval is how often a poll re-reads the inventory sections on
// its own (see probe.Script): a quarter of an hour, so a change made on
// the PMSS side (a client update, a crontab rewrite, a new log) is seen
// soon enough without paying for it every 30 s.
const inventoryInterval = 15 * time.Minute

// inventoryDue reports whether the poll starting at now should run the
// inventory sections: never run yet, inventoryInterval old, or asked for.
// It consumes the request, so one posted while that poll is in flight is
// kept for the next poll rather than wiped by this one's result.
func (p *poller) inventoryDue(now time.Time) bool {
	asked := p.invWant.Swap(false)
	return asked || p.inv == nil || now.Sub(p.invAt) >= inventoryInterval
}

// inventoryFailed re-arms a consumed request when the poll that took it
// produced no snapshot, so the next poll honours it.
func (p *poller) inventoryFailed(requested bool) {
	if requested {
		p.invWant.Store(true)
	}
}

// wakeInventory polls now with the inventory sections included.
func (p *poller) wakeInventory() {
	p.invWant.Store(true)
	p.wake()
}

func (p *poller) stopAndWait() {
	p.once.Do(func() {
		close(p.stopCh)
		if p.cancel != nil {
			p.cancel()
		}
	})
	if p.box.Enabled {
		<-p.doneCh
	}
	p.closeConns()
	// The du worker checks for cancellation once after its command returns;
	// one that passed the check is writing. Wait for it (closing the
	// connection has ended its command), so the replacement poller of an
	// edit never races it for the cache or the live state.
	p.duWG.Wait()
}

// generation is the box's current generation (see generation in state.go):
// the host key is read under the lock, since conn pins it.
func (p *poller) generation() string {
	return generation(p.box.Host, p.box.Port, p.box.User, fingerprintOrEmpty(p.hostKey()))
}

// sameGeneration reports whether a measurement taken under gen is this
// box's (see sameGeneration in state.go).
func (p *poller) sameGeneration(gen string) bool {
	b := *p.box
	b.HostKey = p.hostKey()
	return sameGeneration(gen, &b)
}

// readyState returns the live state once a complete probe of the current
// generation has filled it (BoxState.Ready); until then an action that
// would use the UID, a container ID or the log list gets ErrNotReady.
func (p *poller) readyState() (BoxState, error) {
	st, ok := p.m.State(p.box.ID)
	if !ok || !st.Ready {
		return BoxState{}, ErrNotReady
	}
	return st, nil
}

// stopped reports whether this poller was retired by an edit or removal;
// an action that still holds it must not open new connections on it.
func (p *poller) stopped() bool {
	return p.ctx != nil && p.ctx.Err() != nil
}

// hostKey reads the pinned key under the lock (conn() writes it).
func (p *poller) hostKey() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.box.HostKey
}

func (p *poller) closeConns() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.qbt != nil {
		p.qbt.Close()
		p.qbt = nil
	}
	if p.dk != nil {
		p.dk.Close()
		p.dk = nil
	}
	p.rt, p.rtSSH = nil, nil
	if p.ssh != nil {
		p.ssh.Close()
		p.ssh = nil
	}
}

func (p *poller) run(ctx context.Context) {
	defer close(p.doneCh)
	// Stagger start a little so a fleet does not hammer at once.
	select {
	case <-time.After(time.Duration(p.box.ID%7) * 300 * time.Millisecond):
	case <-ctx.Done():
		return
	case <-p.stopCh:
		return
	}
	for {
		p.safePoll(ctx)
		interval := time.Duration(p.m.Settings().PollIntervalSec) * time.Second
		if interval < 5*time.Second {
			interval = 5 * time.Second
		}
		// Back off a box that keeps failing (see retryInterval); a manual
		// refresh (wake) still polls immediately.
		if st, ok := p.m.State(p.box.ID); ok {
			interval = retryInterval(interval, st.Failures)
		}
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-time.After(interval):
		case <-p.wakeCh:
		}
	}
}

// conn returns a live SSH client, connecting if needed.
func (p *poller) conn(ctx context.Context) (*sshx.Client, error) {
	if p.stopped() {
		return nil, errStopped
	}
	// An action runs on its HTTP request ctx; tie the dial/ping to the
	// poller's ctx as well so stopAndWait aborts it instead of waiting.
	if p.ctx != nil {
		var cancel context.CancelFunc
		ctx, cancel = context.WithCancel(ctx)
		defer cancel()
		defer context.AfterFunc(p.ctx, cancel)()
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.ssh != nil {
		pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		err := p.ssh.Ping(pctx)
		cancel()
		if err == nil {
			return p.ssh, nil
		}
		if ctx.Err() != nil {
			// The caller gave up (browser navigated away mid-action),
			// the connection did not fail: keep it for the next poll.
			if p.stopped() {
				return nil, errStopped // the box was edited/removed: say so
			}
			return nil, ctx.Err()
		}
		p.ssh.Close()
		p.ssh = nil
		if p.qbt != nil {
			p.qbt.Close()
			p.qbt = nil
		}
		if p.dk != nil {
			p.dk.Close()
			p.dk = nil
		}
		p.rt, p.rtSSH = nil, nil // its dialer held the closed transport (Codex on PR 123)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cli, err := sshx.Dial(cctx, sshConfig(p.box))
	if err != nil {
		return nil, err
	}
	if p.stopped() {
		// Retired while dialling (box edited/removed): never keep this
		// connection or pin its key onto the edited record.
		cli.Close()
		return nil, errStopped
	}
	if p.box.HostKey == "" && cli.HostKey() != "" {
		// First contact: the pin must be on disk before any work runs over
		// this connection, or a restart would trust whatever answers next
		// time. Not on the caller's context: an action's request going away
		// must not be what loses the pin.
		if err := p.persistPin(ctx, cli.HostKey()); err != nil {
			cli.Close()
			return nil, err
		}
		p.box.HostKey = cli.HostKey()
	}
	p.ssh = cli
	return cli, nil
}

// persistPin writes a first-contact host key for this box's endpoint and
// confirms it is what the record now holds (the wizard may have pinned the
// same key a moment earlier; a different one, or an endpoint edited
// meanwhile, refuses).
func (p *poller) persistPin(ctx context.Context, key string) error {
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	pinned, err := p.m.st.SetHostKey(sctx, p.box.ID, p.box.Host, p.box.Port, key)
	if err != nil {
		return fmt.Errorf("could not persist the host key pin for %s (%w); not working on an unpinned connection", p.box.Host, err)
	}
	if pinned {
		return nil
	}
	b, err := p.m.st.GetBox(sctx, p.box.ID)
	if err != nil || b.HostKey != key {
		return fmt.Errorf("the host key pin for %s was not stored (the box was edited or pinned to another key meanwhile); retry the action", p.box.Host)
	}
	return nil
}

func sshConfig(b *store.Box) sshx.Config {
	return sshx.Config{
		Host: b.Host, Port: b.Port, User: b.User, AuthMethod: b.AuthMethod,
		KeyPath: b.KeyPath, Password: b.Password, HostKey: b.HostKey, Timeout: 15 * time.Second,
	}
}

// sshDialTCP dials the WebUI through the current SSH connection, with the
// post-connect owner check (verifiedDial) on every new connection.
func (p *poller) sshDialTCP(addr string) qbt.Dialer {
	return func(ctx context.Context) (net.Conn, error) {
		p.mu.Lock()
		cli := p.ssh
		p.mu.Unlock()
		if cli == nil {
			return nil, ErrUnavailable
		}
		port := 0
		if _, ps, err := net.SplitHostPort(addr); err == nil {
			port, _ = strconv.Atoi(ps)
		}
		return verifiedDial(cli, addr, port)(ctx)
	}
}

func (p *poller) sshDialUnix(path string) dockerx.Dialer {
	return func(ctx context.Context) (net.Conn, error) {
		p.mu.Lock()
		cli := p.ssh
		p.mu.Unlock()
		if cli == nil {
			return nil, ErrUnavailable
		}
		return cli.DialUnix(ctx, path)
	}
}

// explainSessionError turns the two errors a box produces when sshd accepts
// the login and then drops the session (seen when the tenant's slice is out
// of memory and the session process is killed as it starts) into something
// a user can act on; other errors pass through.
func explainSessionError(err error) error {
	msg := err.Error()
	if strings.Contains(msg, "unexpected packet in response to channel open") || strings.Contains(msg, "connection reset by peer") {
		return fmt.Errorf("SSH login succeeded but the box dropped the session (%w); the account's slice may be out of memory or sessions may be blocked for this user, check it from the Pulsed Media web interface", err)
	}
	return err
}

var errStopped = errors.New("this box was just edited or removed; retry the action")

// safePoll turns a panic in a poll (a parser bug on hostile box output) into
// a failed poll for that one box instead of taking the whole dashboard down.
func (p *poller) safePoll(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			p.m.log.Error("poll panicked", "box", p.box.Name, "panic", fmt.Sprint(r))
			st, _ := p.m.State(p.box.ID)
			p.fail(ctx, &st, fmt.Errorf("internal error: %v", r), time.Now())
		}
	}()
	p.poll(ctx)
}

func (p *poller) poll(ctx context.Context) {
	start := time.Now()
	p.pollStart = start
	st, _ := p.m.State(p.box.ID)
	st.LastPoll = start

	cli, err := p.conn(ctx)
	if err != nil {
		p.fail(ctx, &st, err, start)
		return
	}
	st.HostKeyFP = fingerprintOrEmpty(p.hostKey()) // after the dial: the first connect pins it
	tag := probe.NewTag()
	inventory := p.inventoryDue(start) // consumes a pending request: one that lands from here on is for the next poll
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	out, stderr, err := cli.Run(pctx, "sh", strings.NewReader(probe.ScriptWith(tag, p.qbtBinStat, inventory)))
	cancel()
	if err != nil && len(out) == 0 {
		p.mu.Lock()
		if p.ssh != nil {
			p.ssh.Close()
			p.ssh = nil
		}
		p.rt, p.rtSSH = nil, nil
		p.mu.Unlock()
		p.inventoryFailed(inventory)
		p.fail(ctx, &st, fmt.Errorf("probe: %w %s", explainSessionError(err), strings.TrimSpace(string(stderr))), start)
		return
	}
	snap, err := probe.ParseTagged(out, tag, time.Now())
	if err != nil {
		p.inventoryFailed(inventory)
		p.fail(ctx, &st, err, start)
		return
	}
	if snap.Inventory {
		p.inv, p.invAt = snap.InventoryOnly(), start
	} else {
		snap.CarryInventory(p.inv)
	}
	p.qbtBinStat, p.qbtBinVersion, snap.Qbt.BinaryVersion = resolveQbtVersion(p.qbtBinStat, p.qbtBinVersion, snap.Qbt)
	p.apply(&st, snap)
	if snap.UID > 0 {
		st.Ready = true // the facts below are this generation's; actions may use them
	}

	if p.box.QbtEnabled {
		p.pollQbt(ctx, &st, snap)
	} else {
		// Monitoring off: still report what the probe saw, including whether
		// the binary on disk is behind the latest release.
		qs := QbtState{Enabled: false, Installed: snap.Qbt.BinaryPresent, ProcessUp: snap.Qbt.Running, Port: qbtPortFor(p.box, snap.Qbt.ConfPort),
			BinaryVersion: clip(snap.Qbt.BinaryVersion, 64), StartHelper: snap.Qbt.StartHelper, StartHelperStale: snap.Qbt.StartHelperStale, CronInstalled: snap.Qbt.CronEntries > 0, PMSSManaged: snap.Qbt.PMSSManaged}
		qs.LatestVersion = p.m.latest.get(ctx)
		qs.UpdateAvail = qs.BinaryVersion != "" && qs.LatestVersion != "" && versionLess(qs.BinaryVersion, qs.LatestVersion)
		st.Qbt = qs
	}
	if p.box.RtEnabled {
		p.pollRt(ctx, &st, snap)
	} else {
		running, known := snap.RtProcess()
		st.Rt = RtState{Enabled: false, ProcessUp: running, ProcessKnown: known, ConfFound: snap.Rt.ConfFound, Disabled: snap.Rt.Disabled,
			Endpoint: clip(rtEndpoint(rtBox{p.box.RtSCGI}, snap), 600)}
	}
	if p.box.DockerEnabled {
		p.pollDocker(ctx, &st, snap)
	} else {
		st.Docker = DockerState{Enabled: false, SocketUp: snap.Docker.SocketPresent}
	}
	if ctx.Err() != nil {
		// Cancelled by an edit/removal/shutdown part-way through: whatever
		// the qbt/docker stages reported is an artefact of the cancel, not
		// a real failure. Do not schedule a du, classify, record or alert.
		return
	}
	p.maybeDu(ctx, &st)
	// stop/restart ran while this poll was in flight: the ownership it
	// verified is from before the port was freed. Applied here so classify
	// sees it, and again under the store lock (afterPoll) for anything that
	// lands between here and the publish.
	p.afterPoll(start)(&st)
	st.Unpolled = false
	st.Failures = 0
	st.NextPoll, st.Backoff = time.Time{}, false
	st.LastError = ""
	st.LastOK = time.Now()
	st.PollMs = time.Since(start).Milliseconds()
	st.Status, st.StatusReason = classify(&st)

	st.Sanitize() // before the cache write as well as the in-memory store
	p.record(ctx, &st)
	p.m.setStateWith(&st, p.storeFix(start, false))
	if snap.Inventory {
		// Fresh host facts are in the stored state now: let the catalogue
		// check see them (a no-op unless the integration is on). After the
		// commit, or a slow qbt/docker stage would let the check run
		// before this box's facts were there to read.
		p.m.pmdbWake()
	}
}

// retryInterval is the poll interval for a box with the given number of
// consecutive failures. The first few retries run at the normal rate (a
// box rebooting or a transient drop recovers in minutes); after that the
// interval doubles per failure up to ten minutes, so a box that is down
// for the night or has a wrong password is not hit with an SSH handshake
// every interval. On a shared host a fail2ban-style guard would otherwise
// ban the dashboard for every tenant. A manual retry bypasses it.
func retryInterval(base time.Duration, failures int) time.Duration {
	const backoffAfter, ceiling = 5, 10 * time.Minute
	if failures < backoffAfter {
		return base
	}
	back := base << uint(min(failures-backoffAfter+1, 10))
	if back > ceiling {
		back = ceiling
	}
	if back < base {
		return base
	}
	return back
}

func (p *poller) fail(ctx context.Context, st *BoxState, err error, start time.Time) {
	if ctx.Err() != nil {
		return // aborted by stop/edit, not a failure of the box
	}
	st.Failures++
	base := time.Duration(max(p.m.Settings().PollIntervalSec, 5)) * time.Second
	next := retryInterval(base, st.Failures)
	st.NextPoll, st.Backoff = time.Now().Add(next), next > base
	st.Unpolled = false // a failed poll is still a verified observation
	st.LastError = describeErr(err)
	st.PollMs = time.Since(start).Milliseconds()
	// A box already offline stays offline. The state survives an edit in
	// memory and a restart through the cache (failed polls are recorded
	// too); one "degraded" poll in between would resolve the unreachable
	// alert only to raise it again, unacknowledged.
	if st.Failures >= 2 || st.LastOK.IsZero() || st.Status == "offline" {
		st.Status = "offline"
	} else {
		st.Status = "degraded"
	}
	st.StatusReason = st.LastError
	st.Rates = RateState{}
	st.Qbt.Reachable = false
	st.Qbt.PortOK = false // must be re-verified by the next successful probe
	st.Rt.Reachable, st.Rt.PortOK = false, false
	st.Docker.Reachable = false
	p.m.log.Warn("poll failed", "box", p.box.Name, "failures", st.Failures, "err", describeErr(err))
	// st is the snapshot from the poll's start: a discovery toggle that
	// landed while this poll was in flight must survive the failure too.
	p.afterPoll(start)(st)
	// Persist the failure as well: a restart must not restore a down box as
	// online (the last successful poll) and walk it through "degraded".
	st.Sanitize()
	p.record(ctx, st)
	p.m.setStateWith(st, p.storeFix(start, true))
}

// applyHostCPU turns this poll's /proc/stat reading and the previous one
// into host-wide busy, iowait and steal percentages of the interval, the
// figures PMSS's own "node load vs. your share" panel uses, so the two
// agree about the host. A first reading, or counters that went backwards
// (a reboot), leave CPUOK false for this poll and become the new baseline.
func (p *poller) applyHostCPU(st *BoxState, snap *probe.Snapshot) {
	cur := snap.Host.CPUTicks
	if !cur.Known {
		return
	}
	prev := p.prevTicks
	if prev.Known && snap.At.After(p.prevTicksAt) && cur.Total > prev.Total &&
		cur.Busy >= prev.Busy && cur.IOWait >= prev.IOWait && cur.Steal >= prev.Steal {
		dt := float64(cur.Total - prev.Total)
		pctOf := func(d uint64) float64 { return math.Min(float64(d)/dt*100, 100) }
		st.HostSys.CPUOK = true
		st.HostSys.CPUBusyPct = pctOf(cur.Busy - prev.Busy)
		st.HostSys.CPUIOWaitPct = pctOf(cur.IOWait - prev.IOWait)
		st.HostSys.CPUStealPct = pctOf(cur.Steal - prev.Steal)
	}
	p.prevTicks, p.prevTicksAt = cur, snap.At
}

func classify(st *BoxState) (string, string) {
	var reasons []string
	if st.Account.Suspended {
		reasons = append(reasons, "account suspended")
	}
	if st.Traffic.Throttled {
		reasons = append(reasons, "traffic throttled")
	}
	if st.Disk.OK && st.Disk.QuotaBytes > 0 && st.Disk.UsedBytes >= st.Disk.QuotaBytes {
		reasons = append(reasons, "over disk quota")
	}
	if st.Qbt.Enabled && !st.Qbt.Reachable {
		reasons = append(reasons, "qBittorrent not responding")
	}
	if st.Rt.Enabled && !st.Rt.Reachable {
		reasons = append(reasons, "rTorrent not responding")
	}
	if st.Docker.Enabled && !st.Docker.Reachable {
		reasons = append(reasons, "docker not responding")
	}
	if len(reasons) > 0 {
		return "degraded", strings.Join(reasons, "; ")
	}
	return "online", ""
}

func (p *poller) apply(st *BoxState, snap *probe.Snapshot) {
	st.Hostname = clip(snap.Hostname, 128)
	st.UID = snap.UID

	q := snap.Quota
	st.Disk = DiskState{OK: q.OK, UsedBytes: q.UsedBytes, QuotaBytes: q.SoftBytes, HardBytes: q.HardBytes,
		FilesUsed: q.FilesUsed, FilesSoft: q.FilesSoft, Source: q.Source, AgeSec: int64(q.Age.Seconds())}
	if st.Disk.QuotaBytes == 0 && snap.Account.QuotaGiB > 0 {
		st.Disk.QuotaBytes = gibToBytes(snap.Account.QuotaGiB)
	}
	st.Disk.Pct = pct(st.Disk.UsedBytes, st.Disk.QuotaBytes)

	t := snap.Traffic
	const mib = 1 << 20
	st.Traffic = TrafficState{
		OK:             t.Out.OK,
		OutMonthBytes:  mibToBytes(t.Out.MonthMiB),
		InMonthBytes:   mibToBytes(t.In.MonthMiB),
		OutDayBytes:    mibToBytes(t.Out.DayMiB),
		InDayBytes:     mibToBytes(t.In.DayMiB),
		LimitBytes:     gibToBytes(t.EffectiveLimitGiB),
		BaseLimitBytes: gibToBytes(t.LimitGiB),
		BonusBytes:     gibToBytes(t.BonusGiB),
		Throttled:      t.Throttled,
		CapMbit:        snap.Account.TrafficCapMbit,
		Daily:          dailySeries(t.Out.Daily, t.In.Daily, snap.At.In(snap.Location)),
	}
	st.Traffic.Pct = pct(st.Traffic.OutMonthBytes, st.Traffic.LimitBytes)

	// Fallback bandwidth estimate from PMSS's 15-minute egress counter (refreshed twice an hour).
	st.Rates = RateState{}
	if t.Out.OK {
		st.Rates = RateState{UpBps: t.Out.Min15MiB * mib / 900, DownBps: t.In.Min15MiB * mib / 900, Source: "pmss-15min"}
	}

	c := snap.Cgroup
	ram := RAMState{OK: c.MemUsage > 0, UsedBytes: c.MemUsage, RSSBytes: c.MemRSS, CacheBytes: c.MemCache, LimitBytes: c.MemLimit, Pids: c.Pids,
		OOMKills: c.OOMKills, OOMKnown: c.OOMKnown, LastOOM: st.RAM.LastOOM}
	// Only a rise against a KNOWN baseline is a kill: a state cached by an
	// older build or a box that only now reports the counter has no
	// baseline, and the lifetime count would alert on upgrade.
	if st.RAM.OK && st.RAM.OOMKnown && c.OOMKnown && c.OOMKills > st.RAM.OOMKills {
		ram.LastOOM = time.Now().UTC().Format(time.RFC3339)
	}
	st.RAM = ram
	if st.RAM.LimitBytes == 0 && snap.Account.RAMMiB > 0 {
		st.RAM.LimitBytes = mibToBytes(snap.Account.RAMMiB)
	}
	// Pressure is process memory against the limit. The charged figure
	// includes page cache, which on a seedbox is simply every torrent byte
	// the kernel still has around and is reclaimed, not OOM-killed, when
	// the limit is reached.
	pressure := st.RAM.RSSBytes
	if pressure == 0 {
		pressure = st.RAM.UsedBytes
	}
	st.RAM.Pct = pct(pressure, st.RAM.LimitBytes)

	st.CPU = CPUState{QuotaCPUs: c.CPUQuota, HostCPUs: snap.Host.NCPU}
	if c.CPUUsageNS > 0 {
		if p.prevCPU > 0 && snap.At.After(p.prevCPUAt) && c.CPUUsageNS >= p.prevCPU {
			elapsed := snap.At.Sub(p.prevCPUAt).Seconds()
			cpus := float64(c.CPUUsageNS-p.prevCPU) / 1e9 / elapsed
			denom := c.CPUQuota
			if denom <= 0 {
				denom = float64(max(snap.Host.NCPU, 1))
			}
			st.CPU.OK = true
			st.CPU.Pct = math.Min(cpus/denom*100, 100000) // a corrupt quota must not produce Inf
			if snap.Host.NCPU > 0 {
				st.CPU.HostPct = math.Min(cpus/float64(snap.Host.NCPU)*100, 100000)
			}
		}
		p.prevCPU, p.prevCPUAt = c.CPUUsageNS, snap.At
	}

	// Identity strings are the box's own text: bound them like every other
	// remote string, or one hostile box bloats the cache row and every SSE event.
	h := snap.Host
	st.HostSys = HostState{UptimeSec: h.UptimeSec, Load1: h.Load1, Load5: h.Load5, Load15: h.Load15,
		MemTotal: h.MemTotal, MemAvail: h.MemAvail, SwapTotal: h.SwapTotal, SwapKnown: h.SwapKnown, NCPU: h.NCPU, CPUMHz: h.CPUMHz, Kernel: clip(h.Kernel, 128), Debian: clip(h.Debian, 64), PMSSVersion: clip(h.PMSSVersion, 64),
		HomeFSType: clip(snap.HomeFS.Type, 32), HomeSize: snap.HomeFS.Size, HomeUsed: snap.HomeFS.Used, HomeAvail: snap.HomeFS.Avail,
		CPUModel: clip(h.CPUModel, 128), CPUShort: clip(h.CPUShort, 64), CPUFlags: clip(strings.Join(h.CPUFlags, " "), 2048),
		GPUPresent: h.GPU.Present, GPUDevice: clip(h.GPU.Device, 128), GPUVendor: h.GPU.Vendor, GPUAccess: h.GPU.Accessible, HWEncode: hwEncodeLabel(h.GPU)}
	p.applyHostCPU(st, snap)

	a := snap.Account
	st.Account = AccountState{OK: a.OK, QuotaGiB: a.QuotaGiB, RAMMiB: a.RAMMiB, DockerEnabled: a.DockerEnabled, Suspended: a.Suspended}

	st.Apps, p.procPrev = appsFromSnapshot(snap, p.box.User, p.procPrev, st.Apps.Watched)
	p.qbtRestarted, p.qbtProcKey = trackQbtProcess(p.qbtProcKey, snap)
	// A stop/restart the dashboard completed before this poll started:
	// whatever client this poll found is a new process, baseline or not.
	// Consumed only by a readable list that shows a client, so an
	// unreadable or client-less poll leaves it for the next one.
	if snap.ProcsKnown && p.qbtProcKey != "" && p.takeRestartBefore(p.pollStart) {
		p.qbtRestarted = true
	}
	if p.qbtRestarted {
		// The pooled connections led to the previous process; whatever
		// answers the port now is verified afresh on the next connect.
		p.mu.Lock()
		if p.qbt != nil {
			p.qbt.Close()
		}
		p.mu.Unlock()
	}

	st.Live = liveView(snap.Live, snap.LiveKnown)
	st.Logs = LogsState{OK: snap.LogsKnown, At: p.invAt}
	for _, f := range snap.Logs {
		st.Logs.Files = append(st.Logs.Files, LogFileView{Path: f.Path, Size: f.Size, MTime: f.MTime})
	}
}

// LogTailLines is how many lines ReadLog fetches (the script also caps bytes).
const LogTailLines = 300

// readLog tails one of the log files the last probe listed.
func (p *poller) readLog(ctx context.Context, path string) (string, error) {
	st, err := p.readyState()
	if err != nil {
		return "", err
	}
	if !st.Logs.has(path) {
		return "", fmt.Errorf("%q is not one of the log files found on the box", path)
	}
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	cli, err := p.conn(actx)
	if err != nil {
		return "", err
	}
	out, stderr, err := cli.Run(actx, "sh", strings.NewReader(probe.LogTailScript(path, LogTailLines)))
	if err != nil {
		return "", fmt.Errorf("read log: %v %s", err, strings.TrimSpace(string(stderr)))
	}
	return strings.ToValidUTF8(string(out), "\uFFFD"), nil
}

// containerLogs fetches the tail of a container's stdout/stderr.
func (p *poller) containerLogs(ctx context.Context, cid string) (string, error) {
	if !p.box.DockerEnabled {
		return "", errors.New("docker is not enabled for this box")
	}
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	st, err := p.readyState()
	if err != nil {
		return "", err
	}
	if !st.Docker.has(cid) {
		return "", errContainerUnknown
	}
	if _, err := p.conn(actx); err != nil {
		return "", err
	}
	if st.UID == 0 {
		return "", ErrUnavailable
	}
	out, err := p.dockerClient(st.UID).Logs(actx, cid, LogTailLines)
	if err != nil {
		return "", err
	}
	return strings.ToValidUTF8(string(out), "\uFFFD"), nil
}

// hwEncodeLabel summarises hardware encoding availability for tags.
func hwEncodeLabel(g probe.GPUInfo) string {
	if !g.Present {
		return ""
	}
	if !g.Accessible {
		return "GPU no access"
	}
	switch g.Vendor {
	case "intel":
		return "QSV"
	case "amd":
		return "VAAPI"
	case "nvidia":
		return "NVENC"
	}
	return "GPU"
}

// dailySeries builds the last 30 days, oldest first, zero-filled. now must
// be in the box's zone: PMSS writes the keys with PHP date('Y/m/d') there.
func dailySeries(out, in map[string]float64, now time.Time) []DayPoint {
	pts := make([]DayPoint, 0, 30)
	for i := 29; i >= 0; i-- {
		d := now.AddDate(0, 0, -i)
		key := d.Format("2006/01/02")
		pts = append(pts, DayPoint{Date: d.Format("2006-01-02"), OutMiB: out[key], InMiB: in[key]})
	}
	return pts
}

// nonNeg clamps qBittorrent's -1 "unknown" (or anything hostile) to 0.
func nonNeg(n int) int {
	if n < 0 {
		return 0
	}
	return n
}

func nonNeg64(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// unit clamps a fraction from the WebUI to [0, 1].
func unit(f float64) float64 {
	if f < 0 || math.IsNaN(f) {
		return 0
	}
	if f > 1 {
		return 1
	}
	return f
}

// torrentHash keeps a hash only when it is one. A hostile WebUI could emit
// ".." and the browser would then POST /qbt/torrents/../stop, which the mux
// cleans into the box-level shutdown route; without a hash the row simply
// has no buttons.
func torrentHash(h string) string {
	if !qbt.ValidHash(h) {
		return ""
	}
	return strings.ToLower(h)
}

func (p *poller) qbtClient(port int) *qbt.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.qbt != nil && p.qbtP == port {
		return p.qbt
	}
	if p.qbt != nil {
		p.qbt.Close()
	}
	hp := fmt.Sprintf("127.0.0.1:%d", port)
	dial := p.sshDialTCP
	if p.dialTCP != nil {
		dial = p.dialTCP
	}
	p.qbt = qbt.NewWithGuard(dial(hp), hp, p.box.QbtUser, p.box.QbtPass, p.guard)
	p.qbtP = port
	return p.qbt
}

func (p *poller) qbtPort(snap *probe.Snapshot) int {
	if p.box.QbtPort > 0 {
		return p.box.QbtPort
	}
	return qbtPortFor(p.box, snap.Qbt.ConfPort)
}

// qbtPortFor is the WebUI port to use: the box setting, else the port from
// qBittorrent.conf, else qBittorrent's default.
func qbtPortFor(b *store.Box, confPort int) int {
	if b.QbtPort > 0 {
		return b.QbtPort
	}
	if confPort > 0 {
		return confPort
	}
	return 8080
}

func (p *poller) pollQbt(ctx context.Context, st *BoxState, snap *probe.Snapshot) {
	port := p.qbtPort(snap)
	qs := QbtState{Enabled: true, Installed: snap.Qbt.BinaryPresent, ProcessUp: snap.Qbt.Running, Port: port, Version: st.Qbt.Version,
		FirewalledSince: carriedFirewalledSince(st.Qbt, snap.Qbt.Running, p.qbtRestarted), ListenPort: st.Qbt.ListenPort,
		LocalBypass: snap.Qbt.LocalHostAuth, SubnetBypass: snap.Qbt.SubnetBypass, SubnetList: clip(snap.Qbt.SubnetList, 1024), ConfUser: clip(snap.Qbt.Username, 64),
		HasCreds:      p.box.QbtUser != "" || p.box.QbtPass != "",
		BinaryVersion: clip(snap.Qbt.BinaryVersion, 64), StartHelper: snap.Qbt.StartHelper, StartHelperStale: snap.Qbt.StartHelperStale, CronInstalled: snap.Qbt.CronEntries > 0, PMSSManaged: snap.Qbt.PMSSManaged}
	versionOK := false // this poll read the running version (it is otherwise carried over for display)
	defer func() {     // runs after st.Qbt is set below
		q := &st.Qbt
		// Compare only a version this poll confirmed, or a restarted client
		// whose version call failed would keep "restart to apply" lit.
		q.RestartNeeded = versionOK && q.ProcessUp && q.Version != "" && q.BinaryVersion != "" && q.Version != q.BinaryVersion
		// Last, so a slow GitHub lookup (cached six hours) cannot eat into
		// the time budget of the WebUI calls above.
		q.LatestVersion = p.m.latest.get(ctx)
		q.UpdateAvail = q.BinaryVersion != "" && q.LatestVersion != "" && versionLess(q.BinaryVersion, q.LatestVersion)
	}()
	if !snap.Qbt.Running {
		qs.Error = "qbittorrent-nox is not running"
		st.Qbt = qs
		return
	}
	if reason := qbtNeedsCreds(snap, p.box); reason != "" {
		// Do not even try: a failed login counts toward qBittorrent's IP ban,
		// and the banned IP is the box's loopback (shared with the PMSS web path).
		qs.Error = reason
		st.Qbt = qs
		return
	}
	if ok, reason := snap.PortOwner(port); !ok {
		// Shared host: a co-tenant can bind our WebUI port while our client
		// is down and harvest the credentials the login would send.
		qs.Error = reason
		st.Qbt = qs
		return
	}
	qs.PortOK = true
	cli := p.qbtClient(port)
	if err, held := cli.AuthBlocked(); held {
		qs.Error, qs.AuthHold = describeErr(err), true
		st.Qbt = qs
		return
	}
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	md, err := cli.MainData(actx)
	if err != nil {
		qs.Error = describeErr(err)
		_, qs.AuthHold = cli.AuthBlocked()
		st.Qbt = qs
		return
	}
	// Always re-read the running version: it is how "restart to apply" clears
	// after an update, and the call is one tiny request.
	if v, err := cli.Version(actx); err == nil {
		qs.Version = clip(v, 64)
		versionOK = true
	}
	// Peer discovery settings decide where private-tracker torrents may go,
	// so they are shown per box; another small request per poll.
	if pr, err := cli.Preferences(actx); err == nil {
		qs.Discovery = Discovery{Known: true, DHT: pr.DHT, PeX: pr.PeX, LSD: pr.LSD}
		if pr.ListenPort > 0 && pr.ListenPort <= 65535 {
			qs.ListenPort = pr.ListenPort
		}
	}
	qs.Reachable = true
	ss := md.ServerState
	qs.Connection = connectionStatus(ss.ConnectionStatus)
	// From qs's own carried value, not st.Qbt's: the carry already dropped
	// it when the process was gone or restarted (fred review on PR 87).
	qs.FirewalledSince = firewalledSince(qs.FirewalledSince, qs.Connection, time.Now())
	if f, err := strconv.ParseFloat(strings.TrimSpace(ss.GlobalRatio), 64); err == nil && !math.IsNaN(f) && !math.IsInf(f, 0) && f >= 0 && f < 1e9 {
		qs.GlobalRatio = f // qBittorrent sends "∞" for undefined, which parses as 0 here
	}
	st.Rates = RateState{UpBps: float64(ss.UPInfoSpeed), DownBps: float64(ss.DLInfoSpeed), Source: "qbt"}

	qs.Torrents = make([]TorrentView, 0, len(md.Torrents))
	for _, t := range md.Torrents {
		kind := qbt.StateKind(t.State)
		switch kind {
		case "seeding":
			qs.Seeding++
		case "leeching":
			qs.Leeching++
		case "paused":
			qs.Paused++
		case "error":
			qs.Errored++
		}
		qs.Torrents = append(qs.Torrents, TorrentView{
			Hash: torrentHash(t.Hash), Client: "qbt", Name: clip(t.Name, 400), Size: nonNeg64(t.Size), Progress: unit(t.Progress), UpBps: t.UPSpeed, DownBps: t.DLSpeed,
			Ratio: t.Ratio, State: clip(t.State, 32), Kind: kind, AddedOn: t.AddedOn, Category: clip(t.Category, 100),
			Uploaded: nonNeg64(t.Uploaded), Downloaded: nonNeg64(t.Downloaded),
			Seeds: nonNeg(t.Seeds), SeedsSwarm: nonNeg(t.SeedsSwarm), Peers: nonNeg(t.Peers), PeersSwarm: nonNeg(t.PeersSwarm),
		})
	}
	qs.Total = len(qs.Torrents)
	sort.Slice(qs.Torrents, func(i, j int) bool {
		a, b := qs.Torrents[i], qs.Torrents[j]
		if (a.UpBps+a.DownBps > 0) != (b.UpBps+b.DownBps > 0) {
			return a.UpBps+a.DownBps > 0
		}
		if a.UpBps+a.DownBps != b.UpBps+b.DownBps {
			return a.UpBps+a.DownBps > b.UpBps+b.DownBps
		}
		if a.AddedOn != b.AddedOn {
			return a.AddedOn > b.AddedOn
		}
		return a.Name < b.Name
	})
	st.Qbt = qs
}

// connectionStatus keeps only the three values libtorrent reports.
func connectionStatus(s string) string {
	switch s {
	case "connected", "firewalled", "disconnected":
		return s
	}
	return ""
}

// carriedFirewalledSince is the FirewalledSince a poll starts from: the
// previous value while the same client process is still up, so a Web API
// failure (a login hold, a timeout, a port check that refused) does not
// restart the ten-minute clock and clear an alert the moment the API
// answers again; nothing once the process is gone or was replaced between
// two polls (restarted), since a fresh process starts its own clock.
func carriedFirewalledSince(prev QbtState, processUp, restarted bool) string {
	if !processUp || restarted {
		return ""
	}
	return prev.FirewalledSince
}

// trackQbtProcess compares the running qbittorrent-nox process identity
// with the one from the previous readable poll: restarted is true when
// both are known and differ. An unreadable process list keeps the last
// identity rather than forgetting it, so a restart that happens before the
// next readable poll is still noticed.
func trackQbtProcess(prevKey string, snap *probe.Snapshot) (restarted bool, key string) {
	if !snap.ProcsKnown {
		return false, prevKey
	}
	key, known := qbtProcKey(snap.Procs)
	if !known {
		return false, prevKey // a client without a start tick this poll is not evidence of a replacement
	}
	if key == "" && snap.Qbt.Running {
		// The pid section (taken earlier in the probe) saw a client that the
		// process listing no longer shows: an inconsistent snapshot, not an
		// absence. Keep the baseline so the replacement is still noticed.
		return false, prevKey
	}
	return key != "" && prevKey != "" && key != prevKey, key
}

// qbtProcKey names the qbittorrent-nox processes in a process list by pid
// and start tick ("pid:tick,…", sorted): "" when there are none. known is
// false when one of them has no start tick (its /proc stat line was not
// read this poll), since "pid:0" would read as a different process.
func qbtProcKey(procs []probe.ProcInfo) (key string, known bool) {
	var parts []string
	for _, p := range procs {
		if p.Comm != "qbittorrent-nox" {
			continue
		}
		if p.StartTick == 0 {
			return "", false
		}
		parts = append(parts, fmt.Sprintf("%d:%d", p.PID, p.StartTick))
	}
	sort.Strings(parts)
	return strings.Join(parts, ","), true
}

// firewalledSince carries the time the client was first seen not
// connected: kept while it stays firewalled or disconnected, cleared when
// it is connected or the status is unknown.
func firewalledSince(prev, conn string, now time.Time) string {
	if conn != "firewalled" && conn != "disconnected" {
		return ""
	}
	if _, err := time.Parse(time.RFC3339, prev); err == nil {
		return prev
	}
	return now.UTC().Format(time.RFC3339)
}

// resolveQbtVersion keeps the installed binary's version across polls that
// skipped the --version call. A fresh answer (the probe asked because the
// binary's stat differed from seenStat, or nothing was cached) replaces
// the cache; an unchanged binary reuses the cached answer; a changed
// binary whose call failed, or a missing binary, yields nothing and the
// next probe asks again.
func resolveQbtVersion(seenStat, seenVersion string, q probe.QbtInfo) (stat, version, current string) {
	switch {
	case q.BinaryVersion != "": // a fresh answer always wins, whatever the stat said
		return q.BinaryStat, q.BinaryVersion, q.BinaryVersion
	case !q.BinaryPresent || q.BinaryStat == "":
		return "", "", ""
	case q.BinaryStat == seenStat && seenVersion != "":
		return seenStat, seenVersion, seenVersion
	}
	return "", "", ""
}

// qbtNeedsCreds returns a reason to skip the Web API when the config shows
// authentication is required and no credentials are configured.
func qbtNeedsCreds(snap *probe.Snapshot, b *store.Box) string {
	if b.QbtUser != "" || b.QbtPass != "" {
		return ""
	}
	if !snap.Qbt.ConfFound || snap.Qbt.LocalHostAuth || snap.Qbt.SubnetBypass {
		return ""
	}
	return "qBittorrent requires a WebUI login (localhost bypass is off in qBittorrent.conf) and no credentials are set for this box; add them under edit"
}

// dockerClient returns the client for the account's rootless daemon,
// rebuilt when the UID differs from the one it was dialled for.
func (p *poller) dockerClient(uid int) *dockerx.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dk != nil && p.dkUID != uid {
		p.dk.Close()
		p.dk = nil
	}
	if p.dk == nil {
		p.dk = dockerx.New(p.sshDialUnix(fmt.Sprintf("/run/user/%d/docker.sock", uid)))
		p.dkUID = uid
	}
	return p.dk
}

func (p *poller) pollDocker(ctx context.Context, st *BoxState, snap *probe.Snapshot) {
	ds := DockerState{Enabled: true, SocketUp: snap.Docker.SocketPresent, Version: st.Docker.Version}
	if !snap.Docker.SocketPresent {
		ds.Error = "docker socket not present (daemon not running?)"
		st.Docker = ds
		return
	}
	cli := p.dockerClient(snap.UID)
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	if ds.Version == "" {
		if v, err := cli.Version(actx); err == nil {
			ds.Version = clip(v.Version, 64)
		}
	}
	cs, err := cli.Containers(actx)
	if err != nil {
		ds.Error = describeErr(err)
		st.Docker = ds
		return
	}
	ds.Reachable = true
	var running []string
	for _, c := range cs {
		cv := containerView(c)
		if c.State == "running" {
			ds.Running++
			running = append(running, c.ID)
		} else {
			ds.Stopped++
		}
		ds.Containers = append(ds.Containers, cv)
	}
	mergeContainerStats(ctx, cli, ds.Containers, running)
	sort.Slice(ds.Containers, func(i, j int) bool {
		a, b := ds.Containers[i], ds.Containers[j]
		if (a.State == "running") != (b.State == "running") {
			return a.State == "running"
		}
		return a.Name < b.Name
	})
	st.Docker = ds
}

// containerView builds the dashboard view of one container, every
// daemon-supplied string clipped, with its port list sorted.
func containerView(c dockerx.Container) ContainerView {
	cv := ContainerView{ID: clip(c.ID, 64), Name: clip(c.Name(), 200), Image: clip(c.Image, 300), State: clip(c.State, 32), Status: clip(c.Status, 100)}
	for _, pt := range c.Ports {
		if pt.PublicPort > 0 {
			ip := pt.IP
			if ip == "0.0.0.0" || ip == "::" {
				ip = ""
			}
			if ip != "" {
				ip += ":"
			}
			cv.Ports = append(cv.Ports, fmt.Sprintf("%s%d->%d/%s", clip(ip, 64), pt.PublicPort, pt.PrivatePort, clip(pt.Type, 16)))
		} else {
			cv.Ports = append(cv.Ports, fmt.Sprintf("%d/%s", pt.PrivatePort, clip(pt.Type, 16)))
		}
	}
	sort.Strings(cv.Ports)
	return cv
}

// mergeContainerStats fills CPU and memory usage into the views of the
// running containers; it makes no stats call when nothing runs or when more
// than twelve containers do.
func mergeContainerStats(ctx context.Context, cli *dockerx.Client, views []ContainerView, running []string) {
	if len(running) > 0 && len(running) <= 12 {
		sctx, scancel := context.WithTimeout(ctx, 20*time.Second)
		stats := cli.StatsAll(sctx, running, 4)
		scancel()
		for i := range views {
			if s, ok := stats[views[i].ID]; ok {
				views[i].CPUPct, views[i].MemUsage, views[i].HasStats = s.CPUPct, s.MemUsage, true
			}
		}
	}
}

func (p *poller) forceDu() {
	p.duMu.Lock()
	p.duForce = true
	p.duMu.Unlock()
	p.wake()
}

func (p *poller) maybeDu(ctx context.Context, st *BoxState) {
	if ctx.Err() != nil {
		return
	}
	st.Dirs.Enabled = p.box.DuEnabled
	// The persisted state keeps Dirs.At but drops the entries (Summary), and
	// a du finishing mid-poll is overwritten by that poll's stale copy, so
	// reload the cached result whenever the live state has none or the
	// cache is newer.
	// A result from another generation (the record moved, the process
	// died before the row was dropped) is not this machine's. One without
	// a generation predates the tag and cannot be placed either: it is
	// dropped too, at the cost of one measurement after the upgrade.
	var cached DirsState
	if at, err := p.m.st.GetCache(ctx, p.box.ID, "du", &cached); err == nil && (len(st.Dirs.Entries) == 0 || at.After(st.Dirs.At)) && p.sameGeneration(cached.Gen) {
		cached.At = at
		cached.Enabled = p.box.DuEnabled
		cached.Running = false
		cached.Entries = capEntries(cached.Entries)
		st.Dirs = cached
	}
	if !p.box.DuEnabled {
		return
	}
	p.duMu.Lock()
	defer p.duMu.Unlock()
	// Running mirrors the goroutine, not the last stored value: a du that
	// died before it measured anything (no connection) must not leave
	// "measuring…" on screen until the next interval.
	st.Dirs.Running = p.duRunning
	if p.duRunning {
		return
	}
	hours := p.m.Settings().DuIntervalHours
	if hours <= 0 && !p.duForce {
		return
	}
	last := st.Dirs.At
	if p.lastDu.After(last) {
		last = p.lastDu
	}
	if !p.duForce && (time.Since(last) < time.Duration(hours)*time.Hour || time.Now().Before(p.duRetryAt)) {
		return
	}
	p.duForce = false
	p.duRunning = true
	st.Dirs.Running = true
	p.duWG.Add(1)
	go p.runDu(ctx)
}

// duRetry spaces automatic attempts after a measurement that did not
// complete (no connection, timeout, SSH drop): sooner than the configured
// interval, which is for completed measurements, but not every poll.
const duRetry = time.Hour

// maxDuEntries bounds the per-box directory list kept in state and cache;
// anything beyond the largest entries is summed under duRestName.
const (
	maxDuEntries = 500
	duRestName   = "(smaller entries)"
)

// foldDu turns du's largest-first listing into state entries: names clipped,
// and everything past the top maxDuEntries summed into one entry, or a
// hostile home with millions of top-level entries makes a cache row of
// 100 MB that every poll re-reads.
func foldDu(ds []probe.DirUsage) []DirEntry {
	out := make([]DirEntry, 0, min(len(ds), maxDuEntries+1))
	for _, d := range ds {
		out = append(out, DirEntry{Name: clip(d.Name, 255), Bytes: d.Bytes})
	}
	return capEntries(out)
}

// capEntries keeps the first maxDuEntries entries (the list is largest
// first) and sums the rest into one; also applied to rows restored from the
// cache, which an older build may have written unbounded.
func capEntries(es []DirEntry) []DirEntry {
	// A row an older build cached may carry sizes ParseDu now rejects.
	kept := es[:0:0]
	for _, e := range es {
		if e.Bytes >= 0 && e.Bytes <= 1<<60 {
			kept = append(kept, e)
		}
	}
	es = kept
	if len(es) <= maxDuEntries {
		return es
	}
	rest := DirEntry{Name: duRestName}
	for _, e := range es[maxDuEntries:] {
		if e.Bytes > 0 && rest.Bytes > math.MaxInt64-e.Bytes {
			rest.Bytes = math.MaxInt64 // saturate rather than wrap on forged sizes
			break
		}
		rest.Bytes += e.Bytes
	}
	return append(es[:maxDuEntries:maxDuEntries], rest)
}

func (p *poller) runDu(ctx context.Context) {
	defer p.duWG.Done()
	p.mu.Lock()
	cli := p.ssh
	p.mu.Unlock()
	if cli == nil {
		p.duDone(false)
		return
	}
	dctx, cancel := context.WithTimeout(ctx, duTimeout)
	defer cancel()
	out, _, err := cli.Run(dctx, "sh", strings.NewReader(probe.DuScript))
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		// Retired by an edit/removal/shutdown mid-run: leave the cache and
		// the (new poller's) state alone; nothing was measured.
		p.duDone(false)
		return
	}
	p.duDone(p.finishDu(ctx, out, err))
}

// duDone records how a measurement ended: a completed one starts the
// configured interval, any other waits duRetry before the next automatic
// attempt (a failure is not a completed interval).
func (p *poller) duDone(completed bool) {
	p.duMu.Lock()
	p.duRunning = false
	if completed {
		p.lastDu = time.Now()
	} else {
		p.duRetryAt = time.Now().Add(duRetry)
	}
	p.duMu.Unlock()
	p.wake()
}

// finishDu turns the du command's output into the box's Dirs and reports
// whether a measurement completed. The cache and the live state are only
// written by the poller that is still the box's (checked under the
// manager's lock for the state; the cache row carries the generation, so a
// row from another machine is refused when read).
func (p *poller) finishDu(ctx context.Context, out []byte, err error) bool {
	sctx, scancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second) // the write outlives a retired poll
	defer scancel()
	// du exits non-zero for unreadable entries but its output is still a
	// full measurement; any other failure (timeout, SSH drop, closed
	// session) means the per-directory lines stopped part-way.
	var exitErr *ssh.ExitError
	finished := errors.As(err, &exitErr) && exitErr.ExitStatus() < 128 // du's own status 1 (unreadable entries); >128 = killed
	// An overrun stdout is never a measurement, whatever du's exit status
	// (sshx.Run does not wrap the exit error into that case; checked here
	// explicitly all the same).
	if errors.Is(err, sshx.ErrOutputLimit) || err != nil && (len(out) == 0 || !finished) {
		// Interrupted (SSH drop, timeout, killed): du prints one line per
		// directory as it goes, so partial output is NOT a measurement.
		// The last good one stays in the cache untouched (its row time is
		// its measurement time); only the live state learns of the error.
		why := describeErr(err)
		p.m.commit(p.box.ID, func(cur *BoxState) *BoxState {
			if p.m.pollers[p.box.ID] != p { // fn runs under m.mu
				return nil // retired meanwhile: the state is another generation's
			}
			cur.Dirs.Error, cur.Dirs.Running = why, false
			return cur
		}, commitOpts{})
		return false
	}
	res := DirsState{Enabled: true, At: time.Now(), Gen: p.generation(), Entries: foldDu(probe.ParseDu(out))}
	// stopAndWait joins this goroutine before an edit replaces the poller,
	// and the generation tag refuses the row to any other machine; the
	// check here only spares a pointless write after a removal.
	if p.m.poller(p.box.ID) != p {
		return true
	}
	if err := p.m.st.SetCache(sctx, p.box.ID, "du", res); err != nil {
		p.m.log.Warn("cache du", "box", p.box.Name, "err", err)
	}
	// Merge into the live state without waiting for the next poll (which
	// reports it to the UI).
	p.m.commit(p.box.ID, func(cur *BoxState) *BoxState {
		if p.m.pollers[p.box.ID] != p { // fn runs under m.mu
			return nil
		}
		res.Enabled = cur.Dirs.Enabled // an edit may have switched measurement off meanwhile
		cur.Dirs = res
		return cur
	}, commitOpts{})
	return true
}

func (p *poller) record(ctx context.Context, st *BoxState) {
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := p.m.st.SetCache(sctx, p.box.ID, "state", st.Summary()); err != nil {
		p.m.log.Warn("cache state", "box", p.box.Name, "err", err)
	}
	// The auth hold must survive restarts, or every restart would retry a
	// rejected login and walk the box toward qBittorrent's IP ban.
	auth := p.guard.State()
	auth.Endpoint = AuthEndpoint(p.box) // a restore applies the hold to this endpoint only
	if err := p.m.st.SetCache(sctx, p.box.ID, authCacheKind, auth); err != nil {
		p.m.log.Warn("cache auth hold", "box", p.box.Name, "err", err)
	}
	// Labels owed to magnets whose metadata has not arrived yet must
	// survive a restart too, or a deploy in that window drops them.
	p.recordRtLabels(sctx)
}

// authCacheKind is the box_cache kind holding the persisted qbt.AuthState.
const authCacheKind = "qbt_auth"

func (p *poller) qbtAction(ctx context.Context, action string) error {
	if _, err := p.readyState(); err != nil {
		return err // start runs the box's own start command; the rest send credentials
	}
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	switch action {
	case "start":
		cli, err := p.conn(actx)
		if err != nil {
			return err
		}
		cmd := p.startCommand()
		_, stderr, err := cli.Run(actx, startViaStdin, strings.NewReader(cmd+"\n"))
		if err != nil {
			return fmt.Errorf("start qbittorrent: %v %s", err, strings.TrimSpace(string(stderr)))
		}
		return nil
	case "stop", "pause", "resume":
		cli, err := p.qbtWebUI(actx)
		if err != nil {
			return err
		}
		switch action {
		case "stop":
			return cli.Shutdown(actx)
		case "pause":
			return cli.PauseAll(actx)
		default:
			return cli.ResumeAll(actx)
		}
	}
	return fmt.Errorf("unknown action %q", action)
}

// qbtWebUI returns a client for the box's WebUI after every check that must
// precede sending its credentials: monitoring on, SSH up, the port verified
// as this account's by the last poll and re-verified right now.
func (p *poller) qbtWebUI(ctx context.Context) (*qbt.Client, error) {
	if !p.box.QbtEnabled {
		return nil, errors.New("qBittorrent is not enabled for this box")
	}
	st, err := p.readyState()
	if err != nil {
		return nil, err
	}
	sshc, err := p.conn(ctx)
	if err != nil {
		return nil, err
	}
	if !st.Qbt.PortOK {
		return nil, errors.New("the WebUI port has not been verified as owned by this account yet (see the client error on the overview); not sending credentials")
	}
	port := qbtPortFor(p.box, st.Qbt.Port)
	// The poll's verification is seconds to minutes old; on a shared
	// host the port can change hands the moment our client exits.
	// Re-check the socket table right before anything that would send
	// credentials, over the connection we already hold.
	if err := verifyPortNow(ctx, sshc, port); err != nil {
		return nil, err
	}
	cli := p.qbtClient(port)
	if err, held := cli.AuthBlocked(); held {
		// Same hold the poll honours: during a ban every request would
		// re-arm it, so none is sent.
		return nil, err
	}
	return cli, nil
}

// torrentAction runs one qbt.TorrentActions entry against a single torrent.
func (p *poller) torrentAction(ctx context.Context, hash, action string) error {
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	cli, err := p.qbtWebUI(actx)
	if err != nil {
		return err
	}
	return cli.TorrentAction(actx, action, hash)
}

// setDiscovery writes the given DHT/PeX/LSD switches (keys dht, pex, lsd).
func (p *poller) setDiscovery(ctx context.Context, prefs map[string]any) error {
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	cli, err := p.qbtWebUI(actx)
	if err != nil {
		return err
	}
	return cli.SetPreferences(actx, prefs)
}

// addTorrents uploads links and files to the box's WebUI after the same
// preflight as every other credential-sending action.
func (p *poller) addTorrents(ctx context.Context, req qbt.AddRequest) (qbt.AddResult, error) {
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	cli, err := p.qbtWebUI(actx)
	if err != nil {
		return qbt.AddResult{}, err
	}
	return cli.Add(actx, req)
}

// errContainerUnknown refuses a container ID the last poll did not list.
var errContainerUnknown = errors.New("that container is not in the box's current container list; refresh and retry")

func (p *poller) containerAction(ctx context.Context, cid, action string) error {
	if !p.box.DockerEnabled {
		return errors.New("docker is not enabled for this box")
	}
	st, err := p.readyState()
	if err != nil {
		return err
	}
	if !st.Docker.has(cid) {
		return errContainerUnknown
	}
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	if _, err := p.conn(actx); err != nil {
		return err
	}
	if st.UID == 0 {
		return ErrUnavailable
	}
	cli := p.dockerClient(st.UID)
	switch action {
	case "start":
		return cli.Start(actx, cid)
	case "stop":
		return cli.Stop(actx, cid)
	case "restart":
		return cli.Restart(actx, cid)
	}
	return fmt.Errorf("unknown action %q", action)
}

func (p *poller) rawProbe(ctx context.Context) (map[string]string, error) {
	cli, err := p.conn(ctx)
	if err != nil {
		return nil, err
	}
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	tag := probe.NewTag()
	out, stderr, err := cli.Run(pctx, "sh", strings.NewReader(probe.ScriptWithTag(tag)))
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("probe: %v %s", err, strings.TrimSpace(string(stderr)))
	}
	secs := probe.SectionsTagged(out, tag)
	// The account config is shown as the fields the parser uses, not the
	// raw file: PMSS may add fields there that are not ours to display.
	if raw, ok := secs["pmss_user"]; ok {
		secs["pmss_user"] = probe.AccountSummary(raw)
	}
	for k, v := range secs {
		secs[k] = clip(v, 4096)
	}
	if len(stderr) > 0 {
		secs["_stderr"] = clip(strings.TrimSpace(string(stderr)), 4096)
	}
	return secs, nil
}
