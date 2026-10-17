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
	"net"
	"sort"
	"strings"
	"sync"
	"time"

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

	mu   sync.Mutex
	ssh  *sshx.Client
	qbt  *qbt.Client
	qbtP int
	dk   *dockerx.Client

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
}

func newPoller(m *Manager, b *store.Box) *poller {
	return &poller{m: m, box: b, wakeCh: make(chan struct{}, 1), stopCh: make(chan struct{}), doneCh: make(chan struct{})}
}

func (p *poller) wake() {
	select {
	case p.wakeCh <- struct{}{}:
	default:
	}
}

func (p *poller) stopAndWait() {
	p.once.Do(func() { close(p.stopCh) })
	if p.box.Enabled {
		<-p.doneCh
	}
	p.closeConns()
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
	if p.box.HostKey == "" && cli.HostKey() != "" {
		p.box.HostKey = cli.HostKey()
		if err := p.m.st.SetHostKey(ctx, p.box.ID, cli.HostKey()); err != nil {
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

// safePoll turns a panic in a poll (a parser bug on hostile box output) into
// a failed poll for that one box instead of taking the whole dashboard down.
func (p *poller) safePoll(ctx context.Context) {
	defer func() {
		if r := recover(); r != nil {
			p.m.log.Error("poll panicked", "box", p.box.Name, "panic", fmt.Sprint(r))
			st, _ := p.m.State(p.box.ID)
			p.fail(&st, fmt.Errorf("internal error: %v", r), time.Now())
		}
	}()
	p.poll(ctx)
}

func (p *poller) poll(ctx context.Context) {
	start := time.Now()
	st, _ := p.m.State(p.box.ID)
	st.LastPoll = start
	st.HostKeyFP = fingerprintOrEmpty(p.box.HostKey)

	cli, err := p.conn(ctx)
	if err != nil {
		p.fail(&st, err, start)
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
		p.fail(&st, fmt.Errorf("probe: %v %s", err, strings.TrimSpace(string(stderr))), start)
		return
	}
	snap, err := probe.ParseTagged(out, tag, time.Now())
	if err != nil {
		p.fail(&st, err, start)
		return
	}
	p.apply(&st, snap)

	if p.box.QbtEnabled {
		p.pollQbt(ctx, &st, snap)
	} else {
		st.Qbt = QbtState{Enabled: false, Installed: snap.Qbt.BinaryPresent, ProcessUp: snap.Qbt.Running, Port: snap.Qbt.ConfPort,
			BinaryVersion: snap.Qbt.BinaryVersion, StartHelper: snap.Qbt.StartHelper, CronInstalled: snap.Qbt.CronEntries > 0}
	}
	if p.box.DockerEnabled {
		p.pollDocker(ctx, &st, snap)
	} else {
		st.Docker = DockerState{Enabled: false, SocketUp: snap.Docker.SocketPresent}
	}
	p.maybeDu(ctx, &st)

	st.Failures = 0
	st.LastError = ""
	st.LastOK = time.Now()
	st.PollMs = time.Since(start).Milliseconds()
	st.Status, st.StatusReason = classify(&st)

	p.record(ctx, &st)
	p.m.setState(&st)
}

func (p *poller) fail(st *BoxState, err error, start time.Time) {
	st.Failures++
	st.LastError = describeErr(err)
	st.PollMs = time.Since(start).Milliseconds()
	if st.Failures >= 2 || st.LastOK.IsZero() {
		st.Status = "offline"
	} else {
		st.Status = "degraded"
	}
	st.StatusReason = st.LastError
	st.Rates = RateState{}
	st.Qbt.Reachable = false
	st.Docker.Reachable = false
	p.m.log.Warn("poll failed", "box", p.box.Name, "failures", st.Failures, "err", err)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = p.m.st.AddSample(ctx, p.box.ID, store.Sample{At: start, OK: false})
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
	st.Hostname = snap.Hostname
	st.UID = snap.UID

	q := snap.Quota
	st.Disk = DiskState{OK: q.OK, UsedBytes: q.UsedBytes, QuotaBytes: q.SoftBytes, HardBytes: q.HardBytes,
		FilesUsed: q.FilesUsed, FilesSoft: q.FilesSoft, Source: q.Source, AgeSec: int64(q.Age.Seconds())}
	if st.Disk.QuotaBytes == 0 && snap.Account.QuotaGiB > 0 {
		st.Disk.QuotaBytes = int64(snap.Account.QuotaGiB * (1 << 30))
	}
	st.Disk.Pct = pct(st.Disk.UsedBytes, st.Disk.QuotaBytes)

	t := snap.Traffic
	const mib = 1 << 20
	st.Traffic = TrafficState{
		OK:             t.Out.OK,
		OutMonthBytes:  int64(t.Out.MonthMiB * mib),
		InMonthBytes:   int64(t.In.MonthMiB * mib),
		OutDayBytes:    int64(t.Out.DayMiB * mib),
		InDayBytes:     int64(t.In.DayMiB * mib),
		LimitBytes:     int64(t.EffectiveLimitGiB * (1 << 30)),
		BaseLimitBytes: int64(t.LimitGiB * (1 << 30)),
		BonusBytes:     int64(t.BonusGiB * (1 << 30)),
		Throttled:      t.Throttled,
		CapMbit:        snap.Account.TrafficCapMbit,
		Daily:          dailySeries(t.Out.Daily, t.In.Daily, snap.At),
	}
	st.Traffic.Pct = pct(st.Traffic.OutMonthBytes, st.Traffic.LimitBytes)

	// Fallback bandwidth estimate from PMSS's 15-minute egress counter (refreshed twice an hour).
	st.Rates = RateState{}
	if t.Out.OK {
		st.Rates = RateState{UpBps: t.Out.Min15MiB * mib / 900, DownBps: t.In.Min15MiB * mib / 900, Source: "pmss-15min"}
	}

	c := snap.Cgroup
	st.RAM = RAMState{OK: c.MemUsage > 0, UsedBytes: c.MemUsage, LimitBytes: c.MemLimit, Pids: c.Pids}
	if st.RAM.LimitBytes == 0 && snap.Account.RAMMiB > 0 {
		st.RAM.LimitBytes = int64(snap.Account.RAMMiB * mib)
	}
	st.RAM.Pct = pct(st.RAM.UsedBytes, st.RAM.LimitBytes)

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
			st.CPU.Pct = cpus / denom * 100
		}
		p.prevCPU, p.prevCPUAt = c.CPUUsageNS, snap.At
	}

	h := snap.Host
	st.HostSys = HostState{UptimeSec: h.UptimeSec, Load1: h.Load1, Load5: h.Load5, Load15: h.Load15,
		MemTotal: h.MemTotal, MemAvail: h.MemAvail, NCPU: h.NCPU, Kernel: h.Kernel, Debian: h.Debian, PMSSVersion: h.PMSSVersion,
		HomeFSType: snap.HomeFS.Type, HomeSize: snap.HomeFS.Size, HomeUsed: snap.HomeFS.Used, HomeAvail: snap.HomeFS.Avail,
		CPUModel: h.CPUModel, CPUShort: h.CPUShort, CPUFlags: strings.Join(h.CPUFlags, " "),
		GPUPresent: h.GPU.Present, GPUDevice: h.GPU.Device, GPUVendor: h.GPU.Vendor, GPUAccess: h.GPU.Accessible, HWEncode: hwEncodeLabel(h.GPU)}

	a := snap.Account
	st.Account = AccountState{OK: a.OK, QuotaGiB: a.QuotaGiB, RAMMiB: a.RAMMiB, DockerEnabled: a.DockerEnabled, TrafficCapMbit: a.TrafficCapMbit, Suspended: a.Suspended}
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

// dailySeries builds the last 30 days, oldest first, zero-filled.
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
	p.qbt = qbt.New(p.sshDialTCP(hp), hp, p.box.QbtUser, p.box.QbtPass)
	p.qbtP = port
	return p.qbt
}

func (p *poller) qbtPort(snap *probe.Snapshot) int {
	if p.box.QbtPort > 0 {
		return p.box.QbtPort
	}
	if snap != nil && snap.Qbt.ConfPort > 0 {
		return snap.Qbt.ConfPort
	}
	return 8080
}

func (p *poller) pollQbt(ctx context.Context, st *BoxState, snap *probe.Snapshot) {
	port := p.qbtPort(snap)
	qs := QbtState{Enabled: true, Installed: snap.Qbt.BinaryPresent, ProcessUp: snap.Qbt.Running, Port: port, Version: st.Qbt.Version,
		LocalBypass: snap.Qbt.LocalHostAuth, SubnetBypass: snap.Qbt.SubnetBypass, SubnetList: snap.Qbt.SubnetList, ConfUser: snap.Qbt.Username,
		HasCreds:      p.box.QbtUser != "" || p.box.QbtPass != "",
		BinaryVersion: snap.Qbt.BinaryVersion, StartHelper: snap.Qbt.StartHelper, CronInstalled: snap.Qbt.CronEntries > 0}
	qs.LatestVersion = p.m.latest.get(ctx)
	qs.UpdateAvail = qs.BinaryVersion != "" && qs.LatestVersion != "" && versionLess(qs.BinaryVersion, qs.LatestVersion)
	defer func() { // runs after st.Qbt is set below
		q := &st.Qbt
		q.RestartNeeded = q.ProcessUp && q.Version != "" && q.BinaryVersion != "" && q.Version != q.BinaryVersion
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
	}
	qs.Reachable = true
	ss := md.ServerState
	qs.AllTimeUL, qs.AllTimeDL = ss.AllTimeUL, ss.AllTimeDL
	qs.SessionUL, qs.SessionDL = ss.UPInfoData, ss.DLInfoData
	qs.Connection = ss.ConnectionStatus
	qs.FreeSpaceDisk = ss.FreeSpaceOnDisk
	fmt.Sscanf(ss.GlobalRatio, "%f", &qs.GlobalRatio)
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
		case "checking":
			qs.Checking++
		case "error":
			qs.Errored++
		}
		if t.UPSpeed > 0 || t.DLSpeed > 0 {
			qs.Active++
		}
		qs.Torrents = append(qs.Torrents, TorrentView{
			Hash: clip(t.Hash, 64), Name: clip(t.Name, 400), Size: t.Size, Progress: t.Progress, UpBps: t.UPSpeed, DownBps: t.DLSpeed,
			Ratio: t.Ratio, State: t.State, Kind: kind, ETA: t.ETA, AddedOn: t.AddedOn, Uploaded: t.Uploaded,
			Category: clip(t.Category, 100), Seeds: t.NumSeeds, Leechs: t.NumLeechs,
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
		cv := ContainerView{ID: clip(c.ID, 64), Name: clip(c.Name(), 200), Image: clip(c.Image, 300), State: clip(c.State, 32), Status: clip(c.Status, 100), Created: c.Created}
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
				ds.Containers[i].CPUPct, ds.Containers[i].MemUsage, ds.Containers[i].MemLimit, ds.Containers[i].HasStats = s.CPUPct, s.MemUsage, s.MemLimit, true
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
	st.Dirs.Enabled = p.box.DuEnabled
	if st.Dirs.At.IsZero() {
		var cached DirsState
		if at, err := p.m.st.GetCache(ctx, p.box.ID, "du", &cached); err == nil {
			cached.At = at
			cached.Enabled = p.box.DuEnabled
			cached.Running = false
			st.Dirs = cached
		}
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
	res := DirsState{Enabled: true, At: time.Now()}
	if err != nil && len(out) == 0 {
		res.Error = describeErr(err)
	}
	for _, d := range probe.ParseDu(out) {
		res.Entries = append(res.Entries, DirEntry{Name: clip(d.Name, 255), Bytes: d.Bytes})
	}
	sctx, scancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer scancel()
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
	err := p.m.st.AddSample(sctx, p.box.ID, store.Sample{
		At: st.LastPoll, OK: true, DiskUsed: st.Disk.UsedBytes, DiskQuota: st.Disk.QuotaBytes,
		TrafficOutMiB: float64(st.Traffic.OutMonthBytes) / (1 << 20), TrafficInMiB: float64(st.Traffic.InMonthBytes) / (1 << 20),
		UpBps: st.Rates.UpBps, DownBps: st.Rates.DownBps, CPUPct: st.CPU.Pct, RAMPct: st.RAM.Pct, Torrents: st.Qbt.Total,
	})
	if err != nil {
		p.m.log.Warn("store sample", "box", p.box.Name, "err", err)
	}
	if err := p.m.st.SetCache(sctx, p.box.ID, "state", st.Summary()); err != nil {
		p.m.log.Warn("cache state", "box", p.box.Name, "err", err)
	}
}

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
		if _, err := p.conn(actx); err != nil {
			return err
		}
		st, _ := p.m.State(p.box.ID)
		port := p.box.QbtPort
		if port == 0 {
			port = st.Qbt.Port
		}
		if port == 0 {
			port = 8080
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
	out, stderr, err := cli.Run(pctx, "sh", strings.NewReader(probe.Script))
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("probe: %v %s", err, strings.TrimSpace(string(stderr)))
	}
	secs := probe.Sections(out)
	for k, v := range secs {
		if len(v) > 4096 {
			secs[k] = v[:4096] + "…"
		}
	}
	if len(stderr) > 0 {
		secs["_stderr"] = strings.TrimSpace(string(stderr))
	}
	return secs, nil
}
