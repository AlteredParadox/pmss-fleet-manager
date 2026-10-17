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

// Package probe runs a shell probe on a PMSS box and parses the result into
// a Snapshot of everything a tenant can observe about their own account.
package probe

import (
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"pmss-fleet-manager/internal/phpserial"
)

// Snapshot is one probe result.
type Snapshot struct {
	At       time.Time
	UID      int
	User     string
	Hostname string

	Host    HostInfo
	HomeFS  FSInfo
	Quota   QuotaInfo
	Traffic TrafficInfo
	Cgroup  CgroupInfo
	Account AccountInfo
	Qbt     QbtInfo
	Docker  DockerInfo

	// Listeners maps a listening TCP port to the uid that owns the socket,
	// from /proc/net/tcp{,6}. On a shared host any tenant can bind a free
	// loopback port, so before sending WebUI credentials the poller checks
	// the port is ours. ListenersKnown is false when /proc/net/tcp could not
	// be read, in which case nothing can be verified.
	Listeners      map[int]int
	ListenersKnown bool
}

// PortOwner reports who listens on port: our uid, another uid, or nobody.
// ok is true only when the port is verifiably ours or ownership cannot be
// determined at all; reason explains a refusal.
func (s *Snapshot) PortOwner(port int) (ok bool, reason string) {
	if !s.ListenersKnown {
		return true, ""
	}
	uid, found := s.Listeners[port]
	switch {
	case !found:
		return false, fmt.Sprintf("nothing is listening on port %d", port)
	case uid != s.UID:
		return false, fmt.Sprintf("port %d is owned by uid %d, not by this account; refusing to connect or send credentials to it", port, uid)
	}
	return true, ""
}

// parseListeners reads /proc/net/tcp and tcp6 rows in state 0A (LISTEN).
func parseListeners(text string) (map[int]int, bool) {
	out := map[int]int{}
	known := false
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) < 8 || f[0] == "sl" {
			continue
		}
		known = true
		if f[3] != "0A" {
			continue
		}
		i := strings.LastIndexByte(f[1], ':')
		if i < 0 {
			continue
		}
		port, err := strconv.ParseInt(f[1][i+1:], 16, 32)
		if err != nil {
			continue
		}
		uid, err := strconv.Atoi(f[7])
		if err != nil {
			continue
		}
		if _, dup := out[int(port)]; !dup {
			out[int(port)] = uid
		}
	}
	return out, known
}

// HostInfo is shared-host information from /proc and /etc.
type HostInfo struct {
	UptimeSec   float64
	Load1       float64
	Load5       float64
	Load15      float64
	MemTotal    int64
	MemAvail    int64
	NCPU        int
	Kernel      string
	Debian      string
	PMSSVersion string
	CPUModel    string   // raw "model name"
	CPUShort    string   // trimmed for tags, e.g. "N100", "Xeon E5-2680 v4"
	CPUFlags    []string // subset: avx2, avx512f, sse4_2, aes
	GPU         GPUInfo
}

// GPUInfo describes the first DRM render node, which is what ffmpeg/Jellyfin
// hardware encoding needs (VAAPI/QSV on Intel, VAAPI on AMD).
type GPUInfo struct {
	Present    bool
	Device     string // /dev/dri/renderD128
	Accessible bool   // this account can open it read/write
	Vendor     string // intel | amd | nvidia | other | ""
	VendorID   string
}

// FSInfo is df output for the filesystem holding the home.
type FSInfo struct {
	Size  int64
	Used  int64
	Avail int64
	Type  string
}

// QuotaInfo is the user's disk quota. Hard limit is usually 1.25x soft.
type QuotaInfo struct {
	OK        bool
	UsedBytes int64
	SoftBytes int64
	HardBytes int64
	FilesUsed int64
	FilesSoft int64
	Source    string // "quota" or "file"
	Age       time.Duration
}

// TrafficSeries is one of PMSS's serialized traffic snapshots.
type TrafficSeries struct {
	OK       bool
	MonthMiB float64 // rolling 30 days
	DayMiB   float64
	Min15MiB float64
	Daily    map[string]float64 // "2006/01/02" -> MiB
}

// TrafficInfo bundles egress, ingress and the limit state.
type TrafficInfo struct {
	Out               TrafficSeries
	In                TrafficSeries
	LimitGiB          float64
	BonusGiB          float64
	EffectiveLimitGiB float64 // 0 = unlimited
	Throttled         bool
}

// CgroupInfo is the user's systemd slice accounting. Zero means unknown.
type CgroupInfo struct {
	MemUsage   int64
	MemLimit   int64 // 0 = unlimited/unknown
	CPUUsageNS int64
	CPUQuota   float64 // fraction of CPUs allowed, 0 = unlimited/unknown
	Pids       int64
}

// AccountInfo mirrors ~/.config/pmss-user.json.
type AccountInfo struct {
	OK             bool
	QuotaGiB       float64
	RAMMiB         float64
	DockerEnabled  bool
	TrafficCapMbit float64
	Suspended      bool
}

// QbtInfo is what the probe can tell about a user-installed qbittorrent-nox.
type QbtInfo struct {
	BinaryPresent bool
	Running       bool
	ConfPort      int
	LocalHostAuth bool // true = auth is bypassed for localhost (WebUI\LocalHostAuth=false)
	ConfFound     bool
	SubnetBypass  bool   // WebUI\AuthSubnetWhitelistEnabled=true
	SubnetList    string // WebUI\AuthSubnetWhitelist
	Username      string
	BinaryVersion string // "v5.2.3" from ~/bin/qbittorrent-nox --version
	StartHelper   bool   // ~/bin/qbt-start exists
	CronEntries   int    // crontab lines mentioning qbt-start
}

// DockerInfo reports the rootless daemon's presence.
type DockerInfo struct {
	SocketPresent bool
}

// Sections splits untagged probe output into named sections (tests and
// diagnostics). Production runs use SectionsTagged.
func Sections(raw []byte) map[string]string { return SectionsTagged(raw, "") }

// SectionsTagged splits raw probe output into named sections. A header is a
// line "@@<tag><name>"; with the per-run tag, content a co-tenant can
// influence cannot forge one. The first occurrence of a section wins, so a
// forged duplicate can never overwrite a real section either.
func SectionsTagged(raw []byte, tag string) map[string]string {
	out := map[string]string{}
	prefix := "@@" + tag
	var cur string
	var buf strings.Builder
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	sc.Buffer(make([]byte, 1024*1024), 8*1024*1024)
	flush := func() {
		if cur != "" {
			if _, dup := out[cur]; !dup {
				out[cur] = strings.TrimRight(buf.String(), "\n")
			}
		}
		buf.Reset()
	}
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(line, prefix) && len(line) > len(prefix) && !strings.ContainsAny(line, " \t") {
			flush()
			cur = line[len(prefix):]
			continue
		}
		buf.WriteString(line)
		buf.WriteByte('\n')
	}
	flush()
	return out
}

// Parse turns untagged probe output into a Snapshot (tests and diagnostics).
func Parse(raw []byte, now time.Time) (*Snapshot, error) { return ParseTagged(raw, "", now) }

// ParseTagged turns raw probe output produced by ScriptWithTag(tag) into a
// Snapshot.
func ParseTagged(raw []byte, tag string, now time.Time) (*Snapshot, error) {
	s := SectionsTagged(raw, tag)
	if _, ok := s["end"]; !ok {
		return nil, fmt.Errorf("probe: output truncated (%d bytes)", len(raw))
	}
	snap := &Snapshot{At: now}
	snap.UID, _ = strconv.Atoi(strings.TrimSpace(s["uid"]))
	snap.User = strings.TrimSpace(s["user"])
	snap.Hostname = strings.TrimSpace(s["hostname"])
	if ts, err := strconv.ParseInt(strings.TrimSpace(s["now"]), 10, 64); err == nil && ts > 0 {
		snap.At = time.Unix(ts, 0)
	}
	snap.Host = parseHost(s)
	snap.HomeFS = parseDF(s["df_home"])
	snap.Quota = parseQuota(s, snap.At)
	snap.Traffic = parseTraffic(s)
	snap.Cgroup = parseCgroup(s["cgroup"])
	snap.Account = parseAccount(s["pmss_user"])
	snap.Qbt = parseQbt(s)
	snap.Listeners, snap.ListenersKnown = parseListeners(s["listen"])
	snap.Docker = DockerInfo{
		SocketPresent: strings.TrimSpace(s["docker_sock"]) == "1",
	}
	return snap, nil
}

func parseHost(s map[string]string) HostInfo {
	h := HostInfo{
		Kernel:      strings.TrimSpace(s["kernel"]),
		Debian:      strings.TrimSpace(s["debian"]),
		PMSSVersion: strings.TrimSpace(s["pmss_version"]),
	}
	if f := strings.Fields(s["uptime"]); len(f) > 0 {
		h.UptimeSec, _ = strconv.ParseFloat(f[0], 64)
	}
	if f := strings.Fields(s["loadavg"]); len(f) >= 3 {
		h.Load1, _ = strconv.ParseFloat(f[0], 64)
		h.Load5, _ = strconv.ParseFloat(f[1], 64)
		h.Load15, _ = strconv.ParseFloat(f[2], 64)
	}
	for _, line := range strings.Split(s["meminfo"], "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.ParseInt(f[1], 10, 64)
		switch f[0] {
		case "MemTotal:":
			h.MemTotal = kb * 1024
		case "MemAvailable:":
			h.MemAvail = kb * 1024
		}
	}
	h.NCPU, _ = strconv.Atoi(strings.TrimSpace(s["ncpu"]))
	h.CPUModel = strings.Join(strings.Fields(s["cpu_model"]), " ")
	h.CPUShort = ShortCPU(h.CPUModel)
	h.CPUFlags = strings.Fields(s["cpu_flags"])
	for _, line := range strings.Split(s["dri"], "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		g := GPUInfo{Present: true, Device: f[0], Accessible: f[1] == "rw"}
		if len(f) > 2 {
			g.VendorID = f[2]
		}
		if len(f) > 3 {
		}
		switch strings.ToLower(g.VendorID) {
		case "0x8086":
			g.Vendor = "intel"
		case "0x1002":
			g.Vendor = "amd"
		case "0x10de":
			g.Vendor = "nvidia"
		case "?", "":
		default:
			g.Vendor = "other"
		}
		h.GPU = g
		break // first render node only
	}
	return h
}

var cpuNoise = regexp.MustCompile(`(?i)\b(Intel|AMD)?\(R\)|\(TM\)|\bCPU\b|\bProcessor\b|@\s*[0-9.]+\s*GHz|\b\d+-Core\b|\bwith Radeon Graphics\b|\bGenuine\b|\bCore\b`)

// ShortCPU trims vendor boilerplate from a cpuinfo model name for compact display.
func ShortCPU(model string) string {
	if model == "" {
		return ""
	}
	out := cpuNoise.ReplaceAllString(model, " ")
	out = strings.Join(strings.Fields(out), " ")
	out = strings.TrimPrefix(out, "Intel ")
	return out
}

func parseDF(line string) FSInfo {
	f := strings.Fields(line)
	if len(f) < 3 {
		return FSInfo{}
	}
	fs := FSInfo{}
	fs.Size, _ = strconv.ParseInt(f[0], 10, 64)
	fs.Used, _ = strconv.ParseInt(f[1], 10, 64)
	fs.Avail, _ = strconv.ParseInt(f[2], 10, 64)
	if len(f) > 3 {
		fs.Type = f[3]
	}
	return fs
}

var numTok = regexp.MustCompile(`(?i)^([0-9]+(?:\.[0-9]+)?)([KMGTP]?)\*?$`)

// parseQuotaLine finds six numeric columns (blocks used/soft/hard, files
// used/soft/hard) in quota output. Plain numbers in the first three columns
// are KiB; K/M/G/T suffixes (quota -s output) are honoured.
func parseQuotaLine(text string) (vals [6]int64, ok bool) {
	for _, line := range strings.Split(text, "\n") {
		parts := strings.Fields(line)
		if len(parts) < 6 {
			continue
		}
		for i := range parts {
			if !numTok.MatchString(parts[i]) {
				continue
			}
			var got []int64
			j := i
			for j < len(parts) && len(got) < 6 {
				m := numTok.FindStringSubmatch(parts[j])
				if m == nil {
					// grace column ("6days", "none") sits after the first three and after the last three
					if len(got) != 3 {
						break
					}
					j++
					continue
				}
				n, _ := strconv.ParseFloat(m[1], 64)
				mult := 1.0
				switch {
				case m[2] == "":
					if len(got) < 3 {
						mult = 1024 // blocks are KiB
					}
				case m[2] >= "a": // quota-tools: inode counts use lowercase suffixes on a 1000 base
					mult = math.Pow(1000, float64(strings.Index(" kmgtp", m[2])))
				default: // space uses uppercase suffixes on a 1024 base
					mult = float64(int64(1) << (10 * uint(strings.Index(" KMGTP", m[2]))))
				}
				got = append(got, int64(n*mult))
				j++
			}
			if len(got) == 6 {
				copy(vals[:], got)
				return vals, true
			}
			break
		}
	}
	return vals, false
}

func parseQuota(s map[string]string, now time.Time) QuotaInfo {
	q := QuotaInfo{}
	if v, ok := parseQuotaLine(s["quota"]); ok {
		q.OK, q.Source = true, "quota"
		q.UsedBytes, q.SoftBytes, q.HardBytes = v[0], v[1], v[2]
		q.FilesUsed, q.FilesSoft = v[3], v[4]
		return q
	}
	if v, ok := parseQuotaLine(s["quota_file"]); ok {
		q.OK, q.Source = true, "file"
		q.UsedBytes, q.SoftBytes, q.HardBytes = v[0], v[1], v[2]
		q.FilesUsed, q.FilesSoft = v[3], v[4]
		if mt, err := strconv.ParseInt(strings.TrimSpace(s["quota_mtime"]), 10, 64); err == nil && mt > 0 {
			q.Age = now.Sub(time.Unix(mt, 0))
		}
	}
	return q
}

func parseSeries(raw string) TrafficSeries {
	ts := TrafficSeries{Daily: map[string]float64{}}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ts
	}
	v, err := phpserial.Unserialize([]byte(raw))
	if err != nil {
		return ts
	}
	arr, ok := v.(*phpserial.Array)
	if !ok {
		return ts
	}
	r := arr.Array("raw")
	if r == nil {
		return ts
	}
	ts.OK = true
	ts.MonthMiB, _ = r.Float("month")
	ts.DayMiB, _ = r.Float("day")
	ts.Min15MiB, _ = r.Float("15min")
	if d := arr.Array("daily"); d != nil {
		for _, k := range d.Keys {
			ks, ok := k.(string)
			if !ok {
				continue
			}
			if f, ok := d.Float(ks); ok {
				ts.Daily[ks] = f
			}
		}
	}
	return ts
}

func parseGiB(raw string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64)
	if err != nil || f < 0 {
		return 0
	}
	return f
}

func parseTraffic(s map[string]string) TrafficInfo {
	t := TrafficInfo{
		Out:       parseSeries(s["traffic"]),
		In:        parseSeries(s["traffic_in"]),
		LimitGiB:  parseGiB(s["traffic_limit"]),
		BonusGiB:  parseGiB(s["bonus_traffic"]),
		Throttled: strings.TrimSpace(s["throttle"]) == "1",
	}
	// Same clamp PMSS applies: bonus never exceeds the base limit.
	if t.LimitGiB > 0 {
		if t.BonusGiB > t.LimitGiB {
			t.BonusGiB = t.LimitGiB
		}
		t.EffectiveLimitGiB = t.LimitGiB + t.BonusGiB
	}
	return t
}

func parseCgroup(raw string) CgroupInfo {
	c := CgroupInfo{}
	kv := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	c.MemUsage, _ = strconv.ParseInt(kv["mem_usage"], 10, 64)
	if lim, err := strconv.ParseInt(kv["mem_limit"], 10, 64); err == nil && lim > 0 && lim < 1<<60 {
		c.MemLimit = lim
	}
	c.CPUUsageNS, _ = strconv.ParseInt(kv["cpu_usage_ns"], 10, 64)
	if q, err := strconv.ParseFloat(kv["cpu_quota_us"], 64); err == nil && q > 0 {
		if p, err := strconv.ParseFloat(kv["cpu_period_us"], 64); err == nil && p > 0 {
			c.CPUQuota = q / p
		}
	}
	if m := strings.Fields(kv["cpu_max"]); len(m) == 2 && m[0] != "max" {
		q, _ := strconv.ParseFloat(m[0], 64)
		p, _ := strconv.ParseFloat(m[1], 64)
		if q > 0 && p > 0 {
			c.CPUQuota = q / p
		}
	}
	c.Pids, _ = strconv.ParseInt(kv["pids"], 10, 64)
	return c
}

func parseAccount(raw string) AccountInfo {
	a := AccountInfo{}
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return a
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		return a
	}
	num := func(k string) float64 {
		switch v := m[k].(type) {
		case float64:
			return v
		case string:
			f, _ := strconv.ParseFloat(v, 64)
			return f
		}
		return 0
	}
	a.OK = true
	a.QuotaGiB = num("quota")
	a.RAMMiB = num("ramMiB")
	if a.RAMMiB == 0 {
		a.RAMMiB = num("rtorrentRam")
	}
	a.TrafficCapMbit = num("trafficCapMbit")
	a.DockerEnabled, _ = m["dockerEnabled"].(bool)
	a.Suspended, _ = m["suspended"].(bool)
	return a
}

func parseQbt(s map[string]string) QbtInfo {
	q := QbtInfo{BinaryPresent: strings.TrimSpace(s["qbt_bin"]) != ""}
	q.Running = len(strings.Fields(s["qbt_pid"])) > 0
	q.BinaryVersion = qbtVersionString(s["qbt_bin_version"])
	q.StartHelper = strings.TrimSpace(s["qbt_start_helper"]) == "1"
	q.CronEntries, _ = strconv.Atoi(strings.TrimSpace(s["qbt_cron"]))
	for _, line := range strings.Split(s["qbt_conf"], "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok {
			continue
		}
		q.ConfFound = true
		switch k {
		case `WebUI\Port`:
			q.ConfPort, _ = strconv.Atoi(v)
		case `WebUI\LocalHostAuth`:
			q.LocalHostAuth = strings.EqualFold(v, "false")
		case `WebUI\AuthSubnetWhitelistEnabled`:
			q.SubnetBypass = strings.EqualFold(v, "true")
		case `WebUI\AuthSubnetWhitelist`:
			q.SubnetList = v
		case `WebUI\Username`:
			q.Username = v
		}
	}
	return q
}

// qbtVersionString extracts "v5.2.3" from "qBittorrent v5.2.3" style output.
func qbtVersionString(raw string) string {
	for _, f := range strings.Fields(raw) {
		if strings.HasPrefix(f, "v") && len(f) > 1 && f[1] >= '0' && f[1] <= '9' {
			return f
		}
	}
	return ""
}

// DirUsage is one du row.
type DirUsage struct {
	Name  string
	Bytes int64
}

// ParseDu parses `du -xs -B1` output, largest first.
func ParseDu(raw []byte) []DirUsage {
	var out []DirUsage
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.SplitN(strings.TrimSpace(line), "\t", 2)
		if len(f) != 2 {
			f = strings.Fields(line)
			if len(f) < 2 {
				continue
			}
			f = []string{f[0], strings.Join(f[1:], " ")}
		}
		n, err := strconv.ParseInt(f[0], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, DirUsage{Name: f[1], Bytes: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}
