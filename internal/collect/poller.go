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
	"time"

	"golang.org/x/crypto/ssh"

	"pmss-fleet-manager/internal/dockerx"
	"pmss-fleet-manager/internal/probe"
	"pmss-fleet-manager/internal/qbt"
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
	guard *qbt.AuthGuard // outlives p.qbt; see qbt.AuthGuard

	prevCPU   int64
	prevCPUAt time.Time

	duMu      sync.Mutex
	duRunning bool
	duForce   bool
	lastDu    time.Time

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
}

func (p *poller) noteAction() {
	p.actionMu.Lock()
	p.actionAt = time.Now()
	p.actionMu.Unlock()
}

// actionSince reports whether a stop/restart happened after t.
func (p *poller) actionSince(t time.Time) bool {
	p.actionMu.Lock()
	defer p.actionMu.Unlock()
	return p.actionAt.After(t)
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
		p.box.HostKey = cli.HostKey()
		if err := p.m.st.SetHostKey(ctx, p.box.ID, p.box.Host, p.box.Port, cli.HostKey()); err != nil {
			p.m.log.Warn("pin host key", "box", p.box.Name, "err", err)
		}
	}
	p.ssh = cli
	return cli, nil
}

func sshConfig(b *store.Box) sshx.Config {
	return sshx.Config{
		Host: b.Host, Port: b.Port, User: b.User, AuthMethod: b.AuthMethod,
		KeyPath: b.KeyPath, Password: b.Password, HostKey: b.HostKey, Timeout: 15 * time.Second,
	}
}

func (p *poller) sshDialTCP(addr string) qbt.Dialer {
	return func(ctx context.Context) (net.Conn, error) {
		p.mu.Lock()
		cli := p.ssh
		p.mu.Unlock()
		if cli == nil {
			return nil, ErrUnavailable
		}
		return cli.DialTCP(ctx, addr)
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
	st, _ := p.m.State(p.box.ID)
	st.LastPoll = start
	st.HostKeyFP = fingerprintOrEmpty(p.hostKey())

	cli, err := p.conn(ctx)
	if err != nil {
		p.fail(ctx, &st, err, start)
		return
	}
	tag := probe.NewTag()
	pctx, cancel := context.WithTimeout(ctx, probeTimeout)
	out, stderr, err := cli.Run(pctx, "sh", strings.NewReader(probe.ScriptWithTag(tag)))
	cancel()
	if err != nil && len(out) == 0 {
		p.mu.Lock()
		if p.ssh != nil {
			p.ssh.Close()
			p.ssh = nil
		}
		p.mu.Unlock()
		p.fail(ctx, &st, fmt.Errorf("probe: %w %s", explainSessionError(err), strings.TrimSpace(string(stderr))), start)
		return
	}
	snap, err := probe.ParseTagged(out, tag, time.Now())
	if err != nil {
		p.fail(ctx, &st, err, start)
		return
	}
	p.apply(&st, snap)

	if p.box.QbtEnabled {
		p.pollQbt(ctx, &st, snap)
	} else {
		// Monitoring off: still report what the probe saw, including whether
		// the binary on disk is behind the latest release.
		qs := QbtState{Enabled: false, Installed: snap.Qbt.BinaryPresent, ProcessUp: snap.Qbt.Running, Port: qbtPortFor(p.box, snap.Qbt.ConfPort),
			BinaryVersion: clip(snap.Qbt.BinaryVersion, 64), StartHelper: snap.Qbt.StartHelper, CronInstalled: snap.Qbt.CronEntries > 0}
		qs.LatestVersion = p.m.latest.get(ctx)
		qs.UpdateAvail = qs.BinaryVersion != "" && qs.LatestVersion != "" && versionLess(qs.BinaryVersion, qs.LatestVersion)
		st.Qbt = qs
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
	if p.actionSince(start) {
		// stop/restart ran while this poll was in flight: the ownership it
		// verified is from before the port was freed.
		st.Qbt.PortOK, st.Qbt.Reachable = false, false
	}
	st.Unpolled = false
	st.Failures = 0
	st.NextPoll, st.Backoff = time.Time{}, false
	st.LastError = ""
	st.LastOK = time.Now()
	st.PollMs = time.Since(start).Milliseconds()
	st.Status, st.StatusReason = classify(&st)

	st.Sanitize() // before the cache write as well as the in-memory store
	p.record(ctx, &st)
	p.m.setState(&st)
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
	st.Docker.Reachable = false
	p.m.log.Warn("poll failed", "box", p.box.Name, "failures", st.Failures, "err", err)
	// Persist the failure as well: a restart must not restore a down box as
	// online (the last successful poll) and walk it through "degraded".
	st.Sanitize()
	p.record(ctx, st)
	p.m.setState(st)
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
		}
		p.prevCPU, p.prevCPUAt = c.CPUUsageNS, snap.At
	}

	// Identity strings are the box's own text: bound them like every other
	// remote string, or one hostile box bloats the cache row and every SSE event.
	h := snap.Host
	st.HostSys = HostState{UptimeSec: h.UptimeSec, Load1: h.Load1, Load5: h.Load5, Load15: h.Load15,
		MemTotal: h.MemTotal, MemAvail: h.MemAvail, NCPU: h.NCPU, Kernel: clip(h.Kernel, 128), Debian: clip(h.Debian, 64), PMSSVersion: clip(h.PMSSVersion, 64),
		HomeFSType: clip(snap.HomeFS.Type, 32), HomeSize: snap.HomeFS.Size, HomeUsed: snap.HomeFS.Used, HomeAvail: snap.HomeFS.Avail,
		CPUModel: clip(h.CPUModel, 128), CPUShort: clip(h.CPUShort, 64), CPUFlags: clip(strings.Join(h.CPUFlags, " "), 2048),
		GPUPresent: h.GPU.Present, GPUDevice: clip(h.GPU.Device, 128), GPUVendor: h.GPU.Vendor, GPUAccess: h.GPU.Accessible, HWEncode: hwEncodeLabel(h.GPU)}

	a := snap.Account
	st.Account = AccountState{OK: a.OK, QuotaGiB: a.QuotaGiB, RAMMiB: a.RAMMiB, DockerEnabled: a.DockerEnabled, Suspended: a.Suspended}
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
	p.qbt = qbt.NewWithGuard(p.sshDialTCP(hp), hp, p.box.QbtUser, p.box.QbtPass, p.guard)
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
		LocalBypass: snap.Qbt.LocalHostAuth, SubnetBypass: snap.Qbt.SubnetBypass, SubnetList: clip(snap.Qbt.SubnetList, 1024), ConfUser: clip(snap.Qbt.Username, 64),
		HasCreds:      p.box.QbtUser != "" || p.box.QbtPass != "",
		BinaryVersion: clip(snap.Qbt.BinaryVersion, 64), StartHelper: snap.Qbt.StartHelper, CronInstalled: snap.Qbt.CronEntries > 0}
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
	qs.Reachable = true
	ss := md.ServerState
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
			Hash: clip(t.Hash, 64), Name: clip(t.Name, 400), Size: t.Size, Progress: t.Progress, UpBps: t.UPSpeed, DownBps: t.DLSpeed,
			Ratio: t.Ratio, State: t.State, Kind: kind, AddedOn: t.AddedOn, Category: clip(t.Category, 100),
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

// qbtNeedsCreds returns a reason to skip the Web API when the config shows
// authentication is required and no credentials are configured.
func qbtNeedsCreds(snap *probe.Snapshot, b *store.Box) string {
	if b.QbtUser != "" || b.QbtPass != "" {
		return ""
	}
	if !snap.Qbt.ConfFound || snap.Qbt.LocalHostAuth || snap.Qbt.SubnetBypass {
		return ""
	}
	return "qBittorrent requires a WebUI login (localhost bypass is off in qBittorrent.conf) and no credentials are set for this box; add them under edit, or enable 'Bypass authentication for clients on localhost'"
}

func (p *poller) dockerClient(uid int) *dockerx.Client {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.dk == nil {
		p.dk = dockerx.New(p.sshDialUnix(fmt.Sprintf("/run/user/%d/docker.sock", uid)))
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
				cv.Ports = append(cv.Ports, fmt.Sprintf("%s%d->%d/%s", ip, pt.PublicPort, pt.PrivatePort, pt.Type))
			} else {
				cv.Ports = append(cv.Ports, fmt.Sprintf("%d/%s", pt.PrivatePort, pt.Type))
			}
		}
		sort.Strings(cv.Ports)
		if c.State == "running" {
			ds.Running++
			running = append(running, c.ID)
		} else {
			ds.Stopped++
		}
		ds.Containers = append(ds.Containers, cv)
	}
	if len(running) > 0 && len(running) <= 12 {
		sctx, scancel := context.WithTimeout(ctx, 20*time.Second)
		stats := cli.StatsAll(sctx, running, 4)
		scancel()
		for i := range ds.Containers {
			if s, ok := stats[ds.Containers[i].ID]; ok {
				ds.Containers[i].CPUPct, ds.Containers[i].MemUsage, ds.Containers[i].HasStats = s.CPUPct, s.MemUsage, true
			}
		}
	}
	sort.Slice(ds.Containers, func(i, j int) bool {
		a, b := ds.Containers[i], ds.Containers[j]
		if (a.State == "running") != (b.State == "running") {
			return a.State == "running"
		}
		return a.Name < b.Name
	})
	st.Docker = ds
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
	var cached DirsState
	if at, err := p.m.st.GetCache(ctx, p.box.ID, "du", &cached); err == nil && (len(st.Dirs.Entries) == 0 || at.After(st.Dirs.At)) {
		cached.At = at
		cached.Enabled = p.box.DuEnabled
		cached.Running = false
		st.Dirs = cached
	}
	if !p.box.DuEnabled {
		return
	}
	p.duMu.Lock()
	defer p.duMu.Unlock()
	if p.duRunning {
		st.Dirs.Running = true
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
	if !p.duForce && time.Since(last) < time.Duration(hours)*time.Hour {
		return
	}
	p.duForce = false
	p.duRunning = true
	st.Dirs.Running = true
	go p.runDu(ctx)
}

func (p *poller) runDu(ctx context.Context) {
	defer func() {
		p.duMu.Lock()
		p.duRunning = false
		p.lastDu = time.Now()
		p.duMu.Unlock()
		p.wake()
	}()
	p.mu.Lock()
	cli := p.ssh
	p.mu.Unlock()
	if cli == nil {
		return
	}
	dctx, cancel := context.WithTimeout(ctx, duTimeout)
	defer cancel()
	out, _, err := cli.Run(dctx, "sh", strings.NewReader(probe.DuScript))
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
	res := DirsState{Enabled: true, At: time.Now()}
	if errors.Is(err, context.Canceled) || ctx.Err() != nil {
		// Retired by an edit/removal/shutdown mid-run: leave the cache and
		// the (new poller's) state alone; nothing was measured.
		return
	}
	// du exits non-zero for unreadable entries but its output is still a
	// full measurement; any other failure (timeout, SSH drop, closed
	// session) means the per-directory lines stopped part-way.
	var exitErr *ssh.ExitError
	finished := errors.As(err, &exitErr) && exitErr.ExitStatus() < 128 // du's own status 1 (unreadable entries); >128 = killed
	interrupted := err != nil && (len(out) == 0 || !finished)
	if interrupted {
		// Interrupted (SSH drop, timeout, killed): du prints one line per
		// directory as it goes, so partial output is NOT a measurement.
		// Keep the last good one and only attach the error.
		var prev DirsState
		if at, gerr := p.m.st.GetCache(sctx, p.box.ID, "du", &prev); gerr == nil {
			res.Entries, res.At = prev.Entries, at
		}
		res.Error = describeErr(err)
	} else {
		for _, d := range probe.ParseDu(out) {
			res.Entries = append(res.Entries, DirEntry{Name: clip(d.Name, 255), Bytes: d.Bytes})
		}
	}
	if err := p.m.st.SetCache(sctx, p.box.ID, "du", res); err != nil {
		p.m.log.Warn("cache du", "box", p.box.Name, "err", err)
	}
	// Merge into the live state without waiting for the next poll.
	p.m.mu.Lock()
	if cur, ok := p.m.states[p.box.ID]; ok {
		cp := *cur
		cp.Dirs = res
		p.m.states[p.box.ID] = &cp
	}
	p.m.mu.Unlock()
}

func (p *poller) record(ctx context.Context, st *BoxState) {
	sctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := p.m.st.SetCache(sctx, p.box.ID, "state", st.Summary()); err != nil {
		p.m.log.Warn("cache state", "box", p.box.Name, "err", err)
	}
	// The auth hold must survive restarts, or every restart would retry a
	// rejected login and walk the box toward qBittorrent's IP ban.
	if err := p.m.st.SetCache(sctx, p.box.ID, authCacheKind, p.guard.State()); err != nil {
		p.m.log.Warn("cache auth hold", "box", p.box.Name, "err", err)
	}
}

// authCacheKind is the box_cache kind holding the persisted qbt.AuthState.
const authCacheKind = "qbt_auth"

func (p *poller) qbtAction(ctx context.Context, action string) error {
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	switch action {
	case "start":
		cli, err := p.conn(actx)
		if err != nil {
			return err
		}
		cmd := p.startCommand()
		_, stderr, err := cli.Run(actx, cmd, nil)
		if err != nil {
			return fmt.Errorf("start qbittorrent: %v %s", err, strings.TrimSpace(string(stderr)))
		}
		return nil
	case "stop", "pause", "resume":
		if !p.box.QbtEnabled {
			return errors.New("qBittorrent is not enabled for this box")
		}
		sshc, err := p.conn(actx)
		if err != nil {
			return err
		}
		st, _ := p.m.State(p.box.ID)
		if !st.Qbt.PortOK {
			return errors.New("the WebUI port has not been verified as owned by this account yet (see the client error on the overview); not sending credentials")
		}
		port := qbtPortFor(p.box, st.Qbt.Port)
		// The poll's verification is seconds to minutes old; on a shared
		// host the port can change hands the moment our client exits.
		// Re-check the socket table right before anything that would send
		// credentials, over the connection we already hold.
		if err := verifyPortNow(actx, sshc, port); err != nil {
			return err
		}
		cli := p.qbtClient(port)
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

func (p *poller) containerAction(ctx context.Context, cid, action string) error {
	if !p.box.DockerEnabled {
		return errors.New("docker is not enabled for this box")
	}
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	if _, err := p.conn(actx); err != nil {
		return err
	}
	st, _ := p.m.State(p.box.ID)
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
