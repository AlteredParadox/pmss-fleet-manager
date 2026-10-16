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
	"strings"
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

// Detect connects once with the given box settings and reports what it finds.
// The returned Box has detection-derived fields filled in (host key, qbt port,
// docker flag, name).
func Detect(ctx context.Context, b store.Box) *DetectResult {
	res := &DetectResult{}
	add := func(status, format string, args ...any) {
		res.Steps = append(res.Steps, Step{Status: status, Text: fmt.Sprintf(format, args...)})
	}
	if b.Port == 0 {
		b.Port = 22
	}
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
	out, stderr, err := cli.Run(pctx, "sh", strings.NewReader(probe.Script))
	if err != nil && len(out) == 0 {
		add("fail", "probe failed: %v %s", err, strings.TrimSpace(string(stderr)))
		res.Box = &b
		return res
	}
	snap, err := probe.Parse(out, time.Now())
	if err != nil {
		add("fail", "probe output unreadable: %v", err)
		res.Box = &b
		return res
	}
	res.Facts.Hostname, res.Facts.User, res.Facts.UID = snap.Hostname, snap.User, snap.UID
	res.Facts.PMSSVersion, res.Facts.Debian = snap.Host.PMSSVersion, snap.Host.Debian
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
	if snap.Quota.OK {
		res.Facts.QuotaBytes, res.Facts.DiskUsedBytes = snap.Quota.SoftBytes, snap.Quota.UsedBytes
		add("ok", "disk quota %s · %s used (%s)", fmtBytes(snap.Quota.SoftBytes), fmtBytes(snap.Quota.UsedBytes), snap.Quota.Source)
	} else if snap.Account.QuotaGiB > 0 {
		res.Facts.QuotaBytes = int64(snap.Account.QuotaGiB * (1 << 30))
		add("warn", "quota command unavailable; plan quota %s from account config", fmtBytes(res.Facts.QuotaBytes))
	} else {
		add("warn", "no quota information readable")
	}
	if snap.Traffic.Out.OK {
		res.Facts.TrafficLimit = int64(snap.Traffic.EffectiveLimitGiB * (1 << 30))
		lim := "unlimited"
		if res.Facts.TrafficLimit > 0 {
			lim = fmtBytes(res.Facts.TrafficLimit) + " / 30 days"
		}
		add("ok", "traffic stats present · %s used in last 30 days · limit %s", fmtBytes(int64(snap.Traffic.Out.MonthMiB*(1<<20))), lim)
	} else {
		add("warn", "no ~/.trafficData yet (PMSS writes it twice an hour)")
	}
	if snap.Cgroup.MemLimit > 0 || snap.Cgroup.CPUQuota > 0 {
		res.Facts.RAMLimit, res.Facts.CPUQuota = snap.Cgroup.MemLimit, snap.Cgroup.CPUQuota
		add("ok", "resource slice: %s RAM · %.1f CPUs", fmtBytes(snap.Cgroup.MemLimit), snap.Cgroup.CPUQuota)
	} else {
		add("info", "cgroup limits not readable; CPU/RAM will show as unknown")
	}

	// qBittorrent
	if b.Type == "storage" && !b.QbtEnabled && !snap.Qbt.BinaryPresent && !snap.Qbt.Running {
		add("info", "storage box: no torrent client expected")
		b.QbtEnabled = false
	} else {
		port := b.QbtPort
		if port == 0 {
			port = snap.Qbt.ConfPort
		}
		if port == 0 {
			port = 8080
		}
		res.Facts.QbtPort = port
		switch {
		case !snap.Qbt.BinaryPresent && !snap.Qbt.Running:
			add("warn", "qbittorrent-nox not found in ~/bin and not running")
			b.QbtEnabled = b.QbtEnabled && false
		case !snap.Qbt.Running:
			add("warn", "qbittorrent-nox present in ~/bin but not running (WebUI port %d)", port)
			b.QbtEnabled = true
			b.QbtPort = port
		case qbtNeedsCreds(snap, &b) != "":
			add("warn", "qBittorrent on port %d requires a WebUI login (localhost bypass is off) — enter the WebUI user/password, or enable 'Bypass authentication for clients on localhost'; not attempting a login to avoid an IP ban", port)
			b.QbtEnabled = true
			b.QbtPort = port
		default:
			qctx, qcancel := context.WithTimeout(ctx, 15*time.Second)
			hp := fmt.Sprintf("127.0.0.1:%d", port)
			qc := qbt.New(func(ctx context.Context) (netConn, error) { return cli.DialTCP(ctx, hp) }, hp, b.QbtUser, b.QbtPass)
			v, err := qc.Version(qctx)
			qcancel()
			qc.Close()
			if err != nil {
				add("warn", "qbittorrent-nox running but Web API on 127.0.0.1:%d failed: %v", port, err)
			} else {
				res.Facts.QbtVersion = v
				add("ok", "qBittorrent %s responding on port %d", v, port)
			}
			b.QbtEnabled = true
			b.QbtPort = port
		}
	}

	// Docker
	if snap.Docker.SocketPresent {
		dctx, dcancel := context.WithTimeout(ctx, 15*time.Second)
		dc := dockerx.New(func(ctx context.Context) (netConn, error) {
			return cli.DialUnix(ctx, fmt.Sprintf("/run/user/%d/docker.sock", snap.UID))
		})
		v, err := dc.Version(dctx)
		if err == nil {
			cs, _ := dc.Containers(dctx)
			res.Facts.DockerVersion, res.Facts.Containers = v.Version, len(cs)
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

	if b.Name == "" {
		b.Name = strings.SplitN(b.Host, ".", 2)[0]
	}
	res.OK = true
	res.Box = &b
	return res
}

func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 5 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
