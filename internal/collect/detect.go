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
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"pmss-fleet-manager/internal/dockerx"
	"pmss-fleet-manager/internal/probe"
	"pmss-fleet-manager/internal/qbt"
	"pmss-fleet-manager/internal/sshx"
	"pmss-fleet-manager/internal/store"
)

// Step is one line of the add-box detection log.
type Step struct {
	Status string `json:"status"` // ok | warn | fail | info
	Text   string `json:"text"`
}

// DetectResult is what the wizard shows before confirming.
type DetectResult struct {
	Steps []Step     `json:"steps"`
	OK    bool       `json:"ok"`
	Box   *store.Box `json:"box"`
	Facts Facts      `json:"facts"`
}

// Facts are the detected numbers shown on the confirm step.
type Facts struct {
	Hostname      string  `json:"hostname"`
	User          string  `json:"user"`
	UID           int     `json:"uid"`
	PMSSVersion   string  `json:"pmss_version"`
	Debian        string  `json:"debian"`
	QuotaBytes    int64   `json:"quota_bytes"`
	DiskUsedBytes int64   `json:"disk_used_bytes"`
	TrafficLimit  int64   `json:"traffic_limit_bytes"`
	RAMLimit      int64   `json:"ram_limit_bytes"`
	CPUQuota      float64 `json:"cpu_quota_cpus"`
	QbtVersion    string  `json:"qbt_version"`
	QbtPort       int     `json:"qbt_port"`
	DockerVersion string  `json:"docker_version"`
	Containers    int     `json:"containers"`
	HostKeyFP     string  `json:"host_key_fp"`
}

// detectGuards remembers rejected WebUI logins seen by the add/edit wizard,
// per SSH endpoint (host, port, account) and credentials, so clicking
// "detect" again never retries one (five rejections ban the box's loopback
// IP). Two accounts on one host with the same WebUI password are two
// endpoints: a rejection on one must not suppress a valid login on the
// other. startPoller seeds the box's poller guard from it; a
// dashboard-driven client restart clears it.
var (
	detectMu     sync.Mutex
	detectGuards = map[string]*qbt.AuthGuard{}
)

// AuthEndpoint identifies the SSH endpoint a WebUI hold belongs to.
func AuthEndpoint(b *store.Box) string {
	return strings.ToLower(b.Host) + ":" + strconv.Itoa(b.Port) + "/" + b.User
}

func detectKey(b *store.Box) string {
	return AuthEndpoint(b) + ":" + qbt.CredsKey(b.QbtUser, b.QbtPass)
}

func detectGuard(b *store.Box) *qbt.AuthGuard {
	k := detectKey(b)
	detectMu.Lock()
	defer detectMu.Unlock()
	if len(detectGuards) > 256 {
		detectGuards = map[string]*qbt.AuthGuard{}
	}
	g, ok := detectGuards[k]
	if !ok {
		g = &qbt.AuthGuard{}
		detectGuards[k] = g
	}
	return g
}

// detectGuardState returns the remembered hold for a box's endpoint and
// credentials without creating an entry.
func detectGuardState(b *store.Box) (qbt.AuthState, bool) {
	detectMu.Lock()
	g, ok := detectGuards[detectKey(b)]
	detectMu.Unlock()
	if !ok {
		return qbt.AuthState{}, false
	}
	return g.State(), true
}

// addStep appends one line to the detection log; see Detect for the clipping
// it applies to its arguments.
type addStep func(status, format string, args ...any)

// Detect connects once with the given box settings and reports what it finds.
// The returned Box has detection-derived fields filled in (host key, qbt port,
// docker flag, name).
func Detect(ctx context.Context, b store.Box) *DetectResult {
	res := &DetectResult{}
	var add addStep = func(status, format string, args ...any) {
		// Every string or error here is box- or client-supplied: bound them
		// like the poller does, or one hostile box inflates this response.
		for i, a := range args {
			switch v := a.(type) {
			case string:
				args[i] = clip(v, 200)
			case error:
				args[i] = describeErr(v)
			}
		}
		res.Steps = append(res.Steps, Step{Status: status, Text: fmt.Sprintf(format, args...)})
	}
	if b.Port == 0 {
		b.Port = 22
	}
	b.HasPassword, b.HasQbtPass = b.Password != "", b.QbtPass != "" // shown by the wizard on every path
	cctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	cli, err := sshx.Dial(cctx, sshConfig(&b))
	if err != nil {
		add("fail", "ssh %s@%s:%d: %v", b.User, b.Host, b.Port, err)
		res.Box = &b
		return res
	}
	defer cli.Close()
	b.HostKey = cli.HostKey()
	res.Facts.HostKeyFP = sshx.Fingerprint(b.HostKey)
	add("ok", "ssh auth ok (%s) · host key %s", b.User, res.Facts.HostKeyFP)

	pctx, cancel2 := context.WithTimeout(ctx, probeTimeout)
	defer cancel2()
	tag := probe.NewTag()
	out, stderr, err := cli.Run(pctx, "sh", strings.NewReader(probe.ScriptWithTag(tag)))
	if err != nil && len(out) == 0 {
		add("fail", "probe failed: %v %s", explainSessionError(err), strings.TrimSpace(string(stderr)))
		res.Box = &b
		return res
	}
	snap, err := probe.ParseTagged(out, tag, time.Now())
	if err != nil {
		add("fail", "probe output unreadable: %v", err)
		res.Box = &b
		return res
	}
	detectHostFacts(snap, res, add)
	detectLimitFacts(snap, res, add)
	detectQbt(ctx, cli, snap, &b, res, add)
	detectDocker(ctx, cli, snap, &b, res, add)

	if b.Name == "" {
		b.Name = strings.SplitN(b.Host, ".", 2)[0]
	}
	res.OK = true
	res.Box = &b
	return res
}

// detectHostFacts reports the host identity: PMSS version, Debian release
// and CPU/GPU.
func detectHostFacts(snap *probe.Snapshot, res *DetectResult, add addStep) {
	res.Facts.Hostname, res.Facts.User, res.Facts.UID = clip(snap.Hostname, 128), clip(snap.User, 64), snap.UID
	res.Facts.PMSSVersion, res.Facts.Debian = clip(snap.Host.PMSSVersion, 64), clip(snap.Host.Debian, 64)
	if snap.Host.PMSSVersion != "" {
		add("ok", "PMSS %s on Debian %s (%s)", snap.Host.PMSSVersion, snap.Host.Debian, snap.Hostname)
	} else {
		add("warn", "no PMSS version file readable; is this a Pulsed Media box? (%s, Debian %s)", snap.Hostname, snap.Host.Debian)
	}
	if snap.Host.CPUShort != "" {
		hw := hwEncodeLabel(snap.Host.GPU)
		if hw == "" {
			hw = "no GPU render node"
		}
		add("ok", "cpu: %s · %d cores on host · %s", snap.Host.CPUShort, snap.Host.NCPU, hw)
	}
}

// detectLimitFacts reports the account limits: disk quota, traffic and the
// cgroup resource slice.
func detectLimitFacts(snap *probe.Snapshot, res *DetectResult, add addStep) {
	if snap.Quota.OK {
		res.Facts.QuotaBytes, res.Facts.DiskUsedBytes = snap.Quota.SoftBytes, snap.Quota.UsedBytes
		add("ok", "disk quota %s · %s used (%s)", HumanBytes(snap.Quota.SoftBytes), HumanBytes(snap.Quota.UsedBytes), snap.Quota.Source)
	} else if snap.Account.QuotaGiB > 0 {
		res.Facts.QuotaBytes = gibToBytes(snap.Account.QuotaGiB)
		add("warn", "quota command unavailable; plan quota %s from account config", HumanBytes(res.Facts.QuotaBytes))
	} else {
		add("warn", "no quota information readable")
	}
	if snap.Traffic.Out.OK {
		res.Facts.TrafficLimit = gibToBytes(snap.Traffic.EffectiveLimitGiB)
		lim := "unlimited"
		if res.Facts.TrafficLimit > 0 {
			lim = HumanBytes(res.Facts.TrafficLimit) + " / 30 days"
		}
		add("ok", "traffic stats present · %s used in last 30 days · limit %s", HumanBytes(int64(snap.Traffic.Out.MonthMiB*(1<<20))), lim)
	} else {
		add("warn", "no ~/.trafficData yet (PMSS writes it twice an hour)")
	}
	if snap.Cgroup.MemLimit > 0 || snap.Cgroup.CPUQuota > 0 {
		res.Facts.RAMLimit, res.Facts.CPUQuota = snap.Cgroup.MemLimit, snap.Cgroup.CPUQuota
		add("ok", "resource slice: %s RAM · %.1f CPUs", HumanBytes(snap.Cgroup.MemLimit), snap.Cgroup.CPUQuota)
	} else {
		add("info", "cgroup limits not readable; CPU/RAM will show as unknown")
	}
}

// detectQbt reports the qBittorrent state and sets the box's QbtEnabled and
// QbtPort from what it finds.
func detectQbt(ctx context.Context, cli *sshx.Client, snap *probe.Snapshot, b *store.Box, res *DetectResult, add addStep) {
	// qBittorrent
	if b.Type == "storage" && !b.QbtEnabled && !snap.Qbt.BinaryPresent && !snap.Qbt.Running && !snap.Qbt.PMSSManaged {
		add("info", "storage box: no torrent client expected")
		return
	}
	port := qbtPortFor(b, snap.Qbt.ConfPort)
	res.Facts.QbtPort = port
	switch {
	case snap.Qbt.PMSSManaged && !snap.Qbt.Running:
		// PMSS's watchdog runs the system qbittorrent-nox, not one in ~/bin,
		// and starts it again on its own: keep monitoring it.
		add("warn", "qBittorrent is managed by the Pulsed Media panel and is currently not running (WebUI port %d)", port)
		b.QbtEnabled = true
		b.QbtPort = port
	case !snap.Qbt.BinaryPresent && !snap.Qbt.Running:
		add("warn", "qbittorrent-nox not found in ~/bin and not running")
		b.QbtEnabled = false
	case !snap.Qbt.Running:
		add("warn", "qbittorrent-nox present in ~/bin but not running (WebUI port %d)", port)
		b.QbtEnabled = true
		b.QbtPort = port
	case qbtNeedsCreds(snap, b) != "":
		add("warn", "qBittorrent on port %d requires a WebUI login (localhost bypass is off) — enter the WebUI user/password; not attempting a login to avoid an IP ban", port)
		b.QbtEnabled = true
		b.QbtPort = port
	default:
		if ok, reason := snap.PortOwner(port); !ok {
			add("warn", "not connecting to the WebUI: %s", reason)
			b.QbtEnabled = true
			b.QbtPort = port
			break
		}
		g := detectGuard(b)
		if err, held := g.Blocked(qbt.CredsKey(b.QbtUser, b.QbtPass)); held {
			add("warn", "not retrying the WebUI login: %v", err)
			b.QbtEnabled = true
			b.QbtPort = port
			break
		}
		qctx, qcancel := context.WithTimeout(ctx, 15*time.Second)
		hp := fmt.Sprintf("127.0.0.1:%d", port)
		qc := qbt.NewWithGuard(verifiedDial(cli, hp, port), hp, b.QbtUser, b.QbtPass, g)
		v, err := qc.Version(qctx)
		qcancel()
		qc.Close()
		if err != nil {
			add("warn", "qbittorrent-nox running but Web API on 127.0.0.1:%d failed: %v", port, err)
		} else {
			res.Facts.QbtVersion = clip(v, 64)
			add("ok", "qBittorrent %s responding on port %d", v, port)
		}
		b.QbtEnabled = true
		b.QbtPort = port
	}
}

// detectDocker reports the rootless docker state and sets the box's
// DockerEnabled from what it finds.
func detectDocker(ctx context.Context, cli *sshx.Client, snap *probe.Snapshot, b *store.Box, res *DetectResult, add addStep) {
	// Docker
	if snap.Docker.SocketPresent {
		dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
		dc := dockerx.New(func(ctx context.Context) (netConn, error) {
			return cli.DialUnix(ctx, fmt.Sprintf("/run/user/%d/docker.sock", snap.UID))
		})
		v, err := dc.Version(dctx)
		if err == nil {
			cs, _ := dc.Containers(dctx)
			res.Facts.DockerVersion, res.Facts.Containers = clip(v.Version, 64), len(cs)
			add("ok", "rootless docker %s · %d containers", v.Version, len(cs))
			b.DockerEnabled = true
		} else {
			add("warn", "docker socket present but API failed: %v", err)
		}
		dcancel()
		dc.Close()
	} else if snap.Account.DockerEnabled {
		add("info", "docker enabled on the account but daemon not running")
		b.DockerEnabled = true
	} else {
		add("info", "no rootless docker socket")
	}
}
