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
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

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

	// Procs are the account's own processes, the probe's own shell and its
	// children left out. ProcsKnown is false when ps printed nothing (a
	// box without procps, or ps failing), which is different from an
	// account with no processes: sshd's session process is always there.
	Procs      []ProcInfo
	ProcsKnown bool
	// ArgsKnown is false when the command lines were not read this poll
	// (the proc_args ps was cut by its timeout) although the process list
	// was: an interpreter-run application (python3 bazarr.py) can then not
	// be told from any other python3, so app classification is incomplete.
	ArgsKnown bool
	// Logs lists the log files found in the catalogued places under the
	// home (see logDirs); LogsKnown is false when the section is missing.
	Logs      []LogFile
	LogsKnown bool
	// Inventory is true when the probe ran its inventory sections (host
	// identity, the installed client, the crontab, the log catalogue; see
	// Script): the poller asks for them every quarter hour and carries the
	// last answer over the polls in between (CarryInventory).
	Inventory bool
	// ListenPIDs maps a listening TCP port to the pids of our processes
	// that hold it, from ss -tlnp. Display only: PortOwner stays the gate.
	ListenPIDs map[int][]int

	// Listeners maps a listening TCP port to the uid that owns the socket,
	// from /proc/net/tcp{,6} or, where PMSS has made those root-only, from
	// ss -tlnp (unknownOwner for a socket ss cannot attribute to us). On a
	// shared host any tenant can bind a free loopback port, so before
	// sending WebUI credentials the poller checks the port is ours.
	// ListenersKnown is false when neither source listed anything, in
	// which case nothing can be verified.
	Listeners      map[int]int
	ListenersKnown bool

	// Location is the box's local time zone (a fixed offset from `date +%z`).
	// PMSS keys the daily traffic series by the box's local date, so the
	// 30-day window must be built in that zone, not the dashboard's.
	Location *time.Location
}

// PortOwner reports who listens on port: our uid, another uid, or nobody.
// ok is true only when the port is verifiably ours; an unreadable table,
// an unknown own uid, a foreign or contested owner all refuse, and reason
// says why.
func (s *Snapshot) PortOwner(port int) (ok bool, reason string) {
	if !s.ListenersKnown {
		// sshd's own listener is always in the table, so an empty section
		// means the probe could not read it; do not send credentials blind.
		return false, "could not read the listening sockets on the box (neither /proc/net/tcp nor ss -tlnp listed any); not sending credentials unverified"
	}
	if s.UID <= 0 {
		return false, "own uid unknown; cannot verify who owns the port"
	}
	uid, found := s.Listeners[port]
	switch {
	case !found:
		return false, fmt.Sprintf("nothing is listening on port %d", port)
	case uid == contestedPort:
		return false, fmt.Sprintf("port %d has listeners owned by more than one account; refusing to connect or send credentials to it", port)
	case uid == unknownOwner:
		return false, fmt.Sprintf("port %d is bound by a process that is not this account's (its owner is not visible from here); refusing to connect or send credentials to it", port)
	case uid != s.UID:
		return false, fmt.Sprintf("port %d is owned by uid %d, not by this account; refusing to connect or send credentials to it", port, uid)
	}
	return true, ""
}

// loopbackReachable reports whether a listener's local address is one that
// a connection to 127.0.0.1 reaches: either a /proc/net/tcp{,6} address
// (hex, little-endian words) or the textual form ss prints.
func loopbackReachable(addr string) bool {
	switch addr {
	case "0.0.0.0", "127.0.0.1", "*", // ss: v4 any, v4 loopback, any
		"[::]", "::", // ss: v6 any (dual-stack reaches 127.0.0.1 too)
		"[::ffff:127.0.0.1]", "::ffff:127.0.0.1":
		return true
	}
	switch strings.ToUpper(addr) {
	case "00000000", "0100007F", // v4 any, v4 loopback
		"00000000000000000000000000000000", // v6 any (dual-stack reaches 127.0.0.1 too)
		"0000000000000000FFFF00000100007F": // ::ffff:127.0.0.1
		return true
	}
	// ::1 is deliberately absent: a 127.0.0.1 dial never reaches it, so a
	// co-tenant on [::1]:port is neither a threat nor a reason to refuse.
	return false
}

// contestedPort marks a port with listeners (v4 and v6, or several
// addresses) owned by different uids: whichever one we would reach, it is
// not safely ours.
const contestedPort = -1

// unknownOwner is the uid recorded for a listener that ss could not
// attribute to one of our own processes (the listen section's "x"): some
// other account's or a system service's, never ours.
const unknownOwner = -2

// AccountSummary renders the parsed fields of ~/.config/pmss-user.json for
// diagnostics, so the raw file (which may grow fields we do not use) is
// never shown verbatim.
func AccountSummary(raw string) string {
	a := parseAccount(raw)
	if !a.OK {
		return "(not present or not parsable)"
	}
	return fmt.Sprintf("quota=%g GiB ram=%g MiB docker=%v trafficCap=%g Mbit suspended=%v", a.QuotaGiB, a.RAMMiB, a.DockerEnabled, a.TrafficCapMbit, a.Suspended)
}

// parseTZ turns a `date +%z` offset ("+0300", "-0500") into a fixed zone;
// unparsable input yields UTC.
func parseTZ(z string) *time.Location {
	if len(z) != 5 || (z[0] != '+' && z[0] != '-') {
		return time.UTC
	}
	hh, err1 := strconv.Atoi(z[1:3])
	mm, err2 := strconv.Atoi(z[3:5])
	if err1 != nil || err2 != nil || hh > 14 || mm > 59 {
		return time.UTC
	}
	off := hh*3600 + mm*60
	if z[0] == '-' {
		off = -off
	}
	return time.FixedZone("box"+z, off)
}

// finite drops NaN/Inf (which encoding/json refuses, turning the whole fleet
// response into a 500) and magnitudes no real PMSS value approaches. The
// unit conversions in collect clamp separately (mibToBytes/gibToBytes), and
// BoxState.Sanitize is the final guard before serialisation.
func finite(f float64, _ bool) float64 {
	if math.IsNaN(f) || math.IsInf(f, 0) || math.Abs(f) > 1e12 {
		return 0
	}
	return f
}

// parseFinite is strconv.ParseFloat for box-supplied text: "nan"/"inf"
// parse successfully there and must not reach the state.
func parseFinite(s string) float64 {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil {
		return 0
	}
	return finite(f, true)
}

// lastSection returns the name of the last section header in raw.
func lastSection(raw []byte, tag string) string {
	prefix := "@@" + tag
	last := ""
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.HasPrefix(line, prefix) && len(line) > len(prefix) && !strings.ContainsAny(line, " \t") {
			last = line[len(prefix):]
		}
	}
	return last
}

// PortOwnerNow evaluates ListenScript's output (a "uid N" line, the listen
// lines, and a closing "end" marker) for one port, with the same rules as
// Snapshot.PortOwner.
func PortOwnerNow(out []byte, port int) (ok bool, reason string) {
	// The script ends with a marker line: a session that was cut off (or
	// a Run that returned partial output with an error) must not pass on
	// a table missing the co-tenant's socket.
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if strings.TrimSpace(lines[len(lines)-1]) != "end" {
		return false, "the box's socket table came back incomplete"
	}
	uid := -1
	for _, l := range lines {
		if n, found := strings.CutPrefix(strings.TrimSpace(l), "uid "); found {
			if v, err := strconv.Atoi(strings.TrimSpace(n)); err == nil {
				uid = v
			}
			break
		}
	}
	if uid < 0 {
		return false, "could not read the account's uid on the box"
	}
	s := &Snapshot{UID: uid}
	s.Listeners, s.ListenersKnown = parseListeners(strings.Join(lines, "\n"))
	return s.PortOwner(port)
}

// EstablishedOwner evaluates EstablishedScript's output for port. known is
// false only for a box that hides non-listening sockets by policy
// ("privacy"); the caller then has the pre-connect listener check alone. A
// failed inspection, a cut-off output, or an output in which this
// account's own tunnel connection to the port does not appear refuses. ok
// requires every tunnel connection of this account whose peer is the
// loopback port to have an accepting socket owned by this uid, in any
// TCP state (a half-closed acceptor still reads); connections other
// accounts hold to the same port, or ours to other addresses, are not
// this connection and do not count either way.
func EstablishedOwner(out []byte, port int) (ok, known bool, reason string) {
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	if strings.TrimSpace(lines[len(lines)-1]) != "end" {
		return false, true, "the box's socket table came back incomplete"
	}
	uid, base := -1, 0
	acceptors := map[string]int{} // our ephemeral port (as printed) -> uid of the accepting socket
	var clients []string          // our ephemeral ports, for connections to the loopback port
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "uid":
			if len(f) == 2 {
				uid, _ = strconv.Atoi(f[1])
			}
		case "privacy":
			return false, false, "the box hides its non-listening sockets (socket-table privacy)"
		case "failed":
			return false, true, "could not inspect the box's socket table (" + strings.Join(f[1:], " ") + ")"
		case "base":
			if len(f) == 2 && f[1] == "hex" {
				base = 16
			} else {
				base = 10
			}
		case "c", "s":
			if len(f) != 5 || base == 0 {
				continue
			}
			p, owner, good := listenerPortUID([]string{map[int]string{16: "h", 10: "d"}[base], f[1], f[2]})
			if !good {
				continue
			}
			_ = p
			if f[0] == "c" {
				// Our tunnel: this account's socket whose peer is the loopback port.
				if owner == uid && loopbackReachable(f[4]) {
					clients = append(clients, f[1])
				}
				continue
			}
			if !loopbackReachable(f[4]) {
				continue // accepted on another address: not reachable by our dial, not ours to judge
			}
			if prev, dup := acceptors[f[1]]; dup && prev != owner {
				acceptors[f[1]] = contestedPort
			} else {
				acceptors[f[1]] = owner
			}
		}
	}
	if uid < 0 {
		return false, true, "could not read the account's uid on the box"
	}
	if len(clients) == 0 {
		return false, true, fmt.Sprintf("the connection to port %d is not in the box's socket table", port)
	}
	for _, eph := range clients {
		owner, found := acceptors[eph]
		switch {
		case !found:
			return false, true, fmt.Sprintf("the socket that accepted the connection to port %d is not visible; refusing to use it", port)
		case owner != uid:
			return false, true, fmt.Sprintf("the connection to port %d was accepted by another account (uid %d); refusing to use it", port, owner)
		}
	}
	return true, true, ""
}

// parseListeners reads the probe's listen section: one listening socket per
// line as "h <hexport> <uid> [<hexaddr>]" from /proc/net/tcp{,6}, or as
// "d <port> <uid|x> [<addr>]" from ss (see listenShell). Only
// sockets a 127.0.0.1 dial can reach count (v4 any/loopback, v6 any,
// v4-mapped loopback; not ::1): a socket bound to a LAN address is neither
// ours to use nor a threat, and a co-tenant on 127.0.0.2 or ::1 must not
// turn our port "contested". A port listed with different reachable owners
// becomes contestedPort.
func parseListeners(text string) (map[int]int, bool) {
	out := map[int]int{}
	known := false
	for _, line := range strings.Split(text, "\n") {
		f := strings.Fields(line)
		if len(f) != 3 && len(f) != 4 {
			continue
		}
		if len(f) == 4 && !loopbackReachable(f[3]) {
			known = true
			continue
		}
		port, uid, ok := listenerPortUID(f)
		if !ok {
			continue
		}
		known = true
		addListener(out, port, uid)
	}
	return out, known
}

// listenerPortUID parses the port (hex after "h", decimal after "d") and
// the owning uid of one listener line ("x" is unknownOwner); ok is false
// for any other prefix, a port outside 1..65535 or any other uid token.
func listenerPortUID(f []string) (port, uid int, ok bool) {
	base := 10
	if f[0] == "h" {
		base = 16
	} else if f[0] != "d" {
		return 0, 0, false
	}
	p, err := strconv.ParseInt(f[1], base, 32)
	if err != nil || p <= 0 || p > 65535 {
		return 0, 0, false
	}
	if f[2] == "x" {
		return int(p), unknownOwner, true
	}
	uid, err = strconv.Atoi(f[2])
	if err != nil || uid < 0 {
		return 0, 0, false
	}
	return int(p), uid, true
}

// addListener records a port's owner; a second listing with a different
// owner marks the port contested.
func addListener(out map[int]int, port, uid int) {
	if prev, dup := out[port]; dup {
		if prev != uid {
			out[port] = contestedPort
		}
		return
	}
	out[port] = uid
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
	MemUsage   int64 // charged to the slice, cache included
	MemRSS     int64 // process memory (v1 total_rss / v2 anon); 0 = unknown
	MemCache   int64 // page cache charged to the slice
	OOMKills   int64 // cumulative kernel OOM kills in the slice
	OOMKnown   bool  // the counter was reported (needed as a baseline before comparing)
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
	BinaryVersion string // "v5.2.3" from ~/bin/qbittorrent-nox --version; empty when the probe skipped the call (see ScriptWith)
	BinaryStat    string // "size mtime ctime inode" of ~/bin/qbittorrent-nox, the key the poller caches BinaryVersion under
	StartHelper   bool   // ~/bin/qbt-start exists
	CronEntries   int    // crontab lines that run only bin/qbt-start (QbtCronPattern)
	PMSSManaged   bool   // ~/.qbittorrentEnable: the PMSS panel runs and configures qBittorrent
}

// DockerInfo reports the rootless daemon's presence.
type DockerInfo struct {
	SocketPresent bool
}

// LogFile is one log file under the home.
type LogFile struct {
	Path  string // relative to the home, e.g. ".logs/trackerCleaner.log"
	Size  int64
	MTime time.Time
}

// maxLogFiles bounds the listing kept from one probe.
const maxLogFiles = 200

// ValidLogPath accepts the relative paths the listing produces and the
// tail script will open: valid UTF-8 (a name with other bytes would be
// listed as U+FFFD and never match on the way back), no leading slash, no
// ".." component, no control characters, bounded length.
func ValidLogPath(p string) bool {
	if p == "" || len(p) > 300 || !utf8.ValidString(p) || strings.HasPrefix(p, "/") || strings.HasSuffix(p, "/") {
		return false
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// parseLogs reads the logs section ("mtime<TAB>size<TAB>path" per line),
// newest first, capped at maxLogFiles. The mtime bound is written so that
// NaN fails it: int64(NaN) is a time encoding/json refuses.
func parseLogs(raw string) []LogFile {
	var out []LogFile
	for _, line := range strings.Split(raw, "\n") {
		f := strings.SplitN(line, "\t", 3)
		if len(f) != 3 {
			continue
		}
		mt, err2 := strconv.ParseFloat(strings.TrimSpace(f[0]), 64)
		size, err1 := strconv.ParseInt(strings.TrimSpace(f[1]), 10, 64)
		p := strings.TrimPrefix(f[2], "./")
		if err1 != nil || err2 != nil || size < 0 || size > 1<<50 || !(mt > 0 && mt <= 1e11) || !ValidLogPath(p) {
			continue
		}
		out = append(out, LogFile{Path: p, Size: size, MTime: time.Unix(int64(mt), 0)})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].MTime.After(out[j].MTime) })
	if len(out) > maxLogFiles {
		out = out[:maxLogFiles]
	}
	return out
}

// ProcInfo is one of the account's processes as ps reported it.
type ProcInfo struct {
	PID        int
	PPID       int
	RSS        int64   // bytes
	CPUSec     float64 // cumulative CPU time (10 ms resolution from /proc/<pid>/stat, else ps's whole seconds)
	StartTick  int64   // start time in clock ticks since boot (0 = unknown); identifies the process across polls
	ElapsedSec int64   // since the process started
	Comm       string  // the kernel's 15-byte name
	Args       string  // command line, clipped on the box
}

// maxProcs bounds the process list kept from one probe: PMSS caps a slice
// at 4096 tasks, and a fork bomb's worth of rows must not bloat the state.
const maxProcs = 2000

// procStat is one process's /proc/<pid>/stat reading: CPU seconds and start tick.
type procStat struct {
	cpuSec float64
	start  int64
}

// parseProcStat reads the proc_stat section: CLK_TCK on the first line,
// then "pid utime stime starttime" in ticks per process.
func parseProcStat(raw string) map[int]procStat {
	out := map[int]procStat{}
	lines := strings.Split(raw, "\n")
	if len(lines) == 0 {
		return out
	}
	hz, err := strconv.ParseFloat(strings.TrimSpace(lines[0]), 64)
	if err != nil || !(hz >= 1 && hz <= 1e6) { // CLK_TCK is a small integer; a tiny or NaN rate would make the seconds infinite
		return out // without the tick rate the numbers mean nothing
	}
	for _, line := range lines[1:] {
		f := strings.Fields(line)
		if len(f) != 4 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ut, e2 := strconv.ParseInt(f[1], 10, 64)
		st, e3 := strconv.ParseInt(f[2], 10, 64)
		start, e4 := strconv.ParseInt(f[3], 10, 64)
		if e1 != nil || e2 != nil || e3 != nil || e4 != nil || pid <= 0 || ut < 0 || st < 0 || start <= 0 || ut > 1<<50 || st > 1<<50 {
			continue
		}
		out[pid] = procStat{cpuSec: float64(ut+st) / hz, start: start}
	}
	return out
}

// parseProcs reads the procs section ("pid ppid rss times etimes comm",
// comm possibly containing spaces), the proc_args section ("pid args") and
// the proc_stat section, dropping the probe's own shell and its children
// (ps, cut, awk).
func parseProcs(procs, args, stat string, probePID int) (out []ProcInfo, known, argsKnown bool) {
	stats := parseProcStat(stat)
	// The args listing is complete only when its "end" marker arrived
	// (procArgsShell): a listing cut by the ps timeout is ignored whole,
	// since nothing says which processes it missed. ps prefixes every
	// row with a pid, so a process whose argv is "end" cannot forge it.
	args = strings.TrimRight(args, "\n")
	argsComplete := false
	if i := strings.LastIndex(args, "\n"); strings.TrimSpace(args[i+1:]) == "end" {
		argsComplete = true
		args = args[:i+1]
	}
	argv := map[int]string{}
	for _, line := range strings.Split(args, "\n") {
		if !argsComplete {
			break
		}
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		if pid, err := strconv.Atoi(f[0]); err == nil && pid > 0 {
			argv[pid] = strings.Join(f[1:], " ")
		}
	}
	var rows []ProcInfo
	for _, line := range strings.Split(procs, "\n") {
		f := strings.Fields(line)
		if len(f) < 6 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		rss, err3 := strconv.ParseInt(f[2], 10, 64)
		cpu, err4 := strconv.ParseFloat(f[3], 64)
		el, err5 := strconv.ParseInt(f[4], 10, 64)
		if err1 != nil || err2 != nil || err3 != nil || err4 != nil || err5 != nil || pid <= 0 {
			continue
		}
		known = true
		if rss < 0 || rss > 1<<40 { // KiB; a forged value must not skew totals
			rss = 0
		}
		if cpu < 0 || math.IsNaN(cpu) || math.IsInf(cpu, 0) || cpu > 1e10 {
			cpu = 0
		}
		if el < 0 || el > 1e10 {
			el = 0
		}
		pi := ProcInfo{PID: pid, PPID: ppid, RSS: rss * 1024, CPUSec: cpu, ElapsedSec: el,
			Comm: strings.Join(f[5:], " "), Args: argv[pid]}
		if ps, ok := stats[pid]; ok {
			pi.CPUSec, pi.StartTick = ps.cpuSec, ps.start
		}
		rows = append(rows, pi)
	}
	// Drop the probe's own process tree: its shell, the ps/awk/xargs it
	// runs, and their children (timeout wraps ps, so ps is a grandchild).
	ppidOf := make(map[int]int, len(rows))
	for _, r := range rows {
		ppidOf[r.PID] = r.PPID
	}
	out = make([]ProcInfo, 0, len(rows))
	for _, r := range rows {
		if probePID > 0 && underPID(r.PID, probePID, ppidOf) {
			continue
		}
		out = append(out, r)
		if len(out) >= maxProcs {
			break
		}
	}
	return out, known, known && argsComplete
}

// underPID reports whether pid is root or one of its descendants in the
// parent map, walking at most a few levels (a cycle in forged data stops).
func underPID(pid, root int, ppidOf map[int]int) bool {
	for depth := 0; depth < 8 && pid > 0; depth++ {
		if pid == root {
			return true
		}
		pid = ppidOf[pid]
	}
	return false
}

// parseListenPIDs reads "<port> <pid>" lines from the listen_pids section.
func parseListenPIDs(raw string) map[int][]int {
	out := map[int][]int{}
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 {
			continue
		}
		port, err1 := strconv.Atoi(f[0])
		pid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil || port <= 0 || port > 65535 || pid <= 0 {
			continue
		}
		if !slices.Contains(out[port], pid) {
			out[port] = append(out[port], pid)
		}
	}
	return out
}

// Sections splits untagged probe output into named sections. Test support:
// production runs use SectionsTagged.
func Sections(raw []byte) map[string]string { return SectionsTagged(raw, "") }

// SectionsTagged splits raw probe output into named sections. A header is a
// line "@@<tag><name>"; with the per-run tag, content a co-tenant can
// influence cannot forge one. The first occurrence of a section wins, so a
// forged duplicate can never overwrite a real section either.
func SectionsTagged(raw []byte, tag string) map[string]string {
	out, complete := sectionsTagged(raw, tag)
	if !complete {
		out["_truncated"] = "a section line exceeded the 8 MiB limit; output after it is missing"
	}
	return out
}

// sectionsTagged also reports whether the whole input was scanned (false
// when a single line exceeded the scanner's limit).
func sectionsTagged(raw []byte, tag string) (map[string]string, bool) {
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
	return out, sc.Err() == nil
}

// Parse turns untagged probe output into a Snapshot. Test support: production
// runs use ParseTagged.
func Parse(raw []byte, now time.Time) (*Snapshot, error) { return ParseTagged(raw, "", now) }

// ParseTagged turns raw probe output produced by ScriptWithTag(tag) into a
// Snapshot.
func ParseTagged(raw []byte, tag string, now time.Time) (*Snapshot, error) {
	s, complete := sectionsTagged(raw, tag)
	if !complete {
		return nil, fmt.Errorf("probe: a section line exceeded the 8 MiB limit (%d bytes total)", len(raw))
	}
	if _, ok := s["end"]; !ok {
		// Name the last section that did arrive: it points at the command
		// that hung when the probe hit its timeout on a slow box.
		return nil, fmt.Errorf("probe: output truncated after section %q (%d bytes)", lastSection(raw, tag), len(raw))
	}
	snap := &Snapshot{At: now}
	snap.UID, _ = strconv.Atoi(strings.TrimSpace(s["uid"]))
	snap.User = strings.TrimSpace(s["user"])
	snap.Hostname = hostnameField(strings.TrimSpace(s["hostname"]))
	if ts, err := strconv.ParseInt(strings.TrimSpace(s["now"]), 10, 64); err == nil && ts > 0 {
		// The box's clock, within a year of ours: a forged or broken epoch
		// would otherwise become a time encoding/json refuses, taking the
		// fleet endpoint down with it.
		if t := time.Unix(ts, 0); t.Sub(now).Abs() < 366*24*time.Hour {
			snap.At = t
		}
	}
	snap.Location = parseTZ(strings.TrimSpace(s["tz"]))
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
	probePID, _ := strconv.Atoi(strings.TrimSpace(s["probe_pid"]))
	snap.Procs, snap.ProcsKnown, snap.ArgsKnown = parseProcs(s["procs"], s["proc_args"], s["proc_stat"], probePID)
	snap.ListenPIDs = parseListenPIDs(s["listen_pids"])
	_, snap.LogsKnown = s["logs"]
	snap.Logs = parseLogs(s["logs"])
	_, snap.Inventory = s["inventory"]
	return snap, nil
}

// CarryInventory fills the inventory fields of a snapshot whose probe
// skipped those sections from prev, the last snapshot that ran them. A
// snapshot that ran them itself, or no previous one, leaves s as it is.
func (s *Snapshot) CarryInventory(prev *Snapshot) {
	if s.Inventory || prev == nil {
		return
	}
	h, ph := &s.Host, &prev.Host
	h.NCPU, h.Kernel, h.Debian, h.PMSSVersion = ph.NCPU, ph.Kernel, ph.Debian, ph.PMSSVersion
	h.CPUModel, h.CPUShort, h.CPUFlags, h.GPU = ph.CPUModel, ph.CPUShort, append([]string(nil), ph.CPUFlags...), ph.GPU
	q, pq := &s.Qbt, &prev.Qbt
	q.BinaryPresent, q.BinaryVersion, q.BinaryStat = pq.BinaryPresent, pq.BinaryVersion, pq.BinaryStat
	q.StartHelper, q.CronEntries, q.PMSSManaged = pq.StartHelper, pq.CronEntries, pq.PMSSManaged
	s.Logs, s.LogsKnown = append([]LogFile(nil), prev.Logs...), prev.LogsKnown
}

// InventoryOnly returns a copy of s holding just what CarryInventory
// copies, for a poller to keep between inventory runs without the process
// list and the rest.
func (s *Snapshot) InventoryOnly() *Snapshot {
	out := &Snapshot{At: s.At}
	out.CarryInventory(s)
	out.Inventory = true
	return out
}

func parseHost(s map[string]string) HostInfo {
	h := HostInfo{
		Kernel:      strings.TrimSpace(s["kernel"]),
		Debian:      strings.TrimSpace(s["debian"]),
		PMSSVersion: strings.TrimSpace(s["pmss_version"]),
	}
	parseLoad(&h, s)
	parseMeminfo(&h, s["meminfo"])
	parseCPUInfo(&h, s)
	h.GPU = parseDRI(s["dri"])
	return h
}

// parseLoad fills uptime and the three load averages.
func parseLoad(h *HostInfo, s map[string]string) {
	if f := strings.Fields(s["uptime"]); len(f) > 0 {
		h.UptimeSec = parseFinite(f[0])
	}
	if f := strings.Fields(s["loadavg"]); len(f) >= 3 {
		h.Load1 = parseFinite(f[0])
		h.Load5 = parseFinite(f[1])
		h.Load15 = parseFinite(f[2])
	}
}

// parseMeminfo fills MemTotal and MemAvail from /proc/meminfo lines (kB).
func parseMeminfo(h *HostInfo, raw string) {
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.ParseInt(f[1], 10, 64)
		if kb < 0 || kb > 1<<50 { // a forged value: no host has an exabyte, and kb*1024 must not overflow
			kb = 0
		}
		switch f[0] {
		case "MemTotal:":
			h.MemTotal = kb * 1024
		case "MemAvailable:":
			h.MemAvail = kb * 1024
		}
	}
}

// parseCPUInfo fills the core count, model name (raw and short) and flags.
func parseCPUInfo(h *HostInfo, s map[string]string) {
	h.NCPU, _ = strconv.Atoi(strings.TrimSpace(s["ncpu"]))
	h.CPUModel = strings.Join(strings.Fields(s["cpu_model"]), " ")
	h.CPUShort = ShortCPU(h.CPUModel)
	h.CPUFlags = strings.Fields(s["cpu_flags"])
}

// parseDRI returns the first render node listed in the dri section, or the
// zero GPUInfo when there is none.
func parseDRI(raw string) GPUInfo {
	for _, line := range strings.Split(raw, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		g := GPUInfo{Present: true, Device: f[0], Accessible: f[1] == "rw"}
		if len(f) > 2 {
			g.VendorID = f[2]
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
		return g // first render node only
	}
	return GPUInfo{}
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
			got := quotaColumns(parts, i)
			if len(got) == 6 {
				copy(vals[:], got)
				return vals, true
			}
			break
		}
	}
	return vals, false
}

// quotaColumns reads up to six numeric columns starting at parts[i],
// stepping over the grace column that may follow the first three. It stops
// at the first column that is neither, so fewer than six means no match.
func quotaColumns(parts []string, i int) []int64 {
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
		b, ok := quotaValue(m, len(got))
		if !ok {
			break
		}
		got = append(got, b)
		j++
	}
	return got
}

// quotaValue converts one numTok match to an integer: bytes for the first
// three columns (plain numbers are KiB), counts for the inode columns. ok is
// false for a negative or absurd value.
func quotaValue(m []string, col int) (int64, bool) {
	n, _ := strconv.ParseFloat(m[1], 64)
	mult := 1.0
	switch {
	case m[2] == "":
		if col < 3 {
			mult = 1024 // blocks are KiB
		}
	case m[2] >= "a": // quota-tools: inode counts use lowercase suffixes on a 1000 base
		mult = math.Pow(1000, float64(strings.Index(" kmgtp", m[2])))
	default: // space uses uppercase suffixes on a 1024 base
		mult = float64(int64(1) << (10 * uint(strings.Index(" KMGTP", m[2]))))
	}
	b := n * mult
	if math.IsNaN(b) || b < 0 || b > 1<<62 { // negative or absurd: unparsable column
		return 0, false
	}
	return int64(b), true
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
	ts.MonthMiB = finite(r.Float("month"))
	ts.DayMiB = finite(r.Float("day"))
	ts.Min15MiB = finite(r.Float("15min"))
	if d := arr.Array("daily"); d != nil {
		for _, k := range d.Keys {
			ks, ok := k.(string)
			if !ok {
				continue
			}
			if f, ok := d.Float(ks); ok {
				ts.Daily[ks] = finite(f, true)
			}
		}
	}
	return ts
}

func parseGiB(raw string) float64 {
	f := parseFinite(raw)
	if f < 0 {
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
	c.MemRSS, _ = strconv.ParseInt(kv["mem_rss"], 10, 64)
	c.MemCache, _ = strconv.ParseInt(kv["mem_cache"], 10, 64)
	c.OOMKills, _ = strconv.ParseInt(kv["oom_kill"], 10, 64)
	_, c.OOMKnown = kv["oom_kill"]
	if lim, err := strconv.ParseInt(kv["mem_limit"], 10, 64); err == nil && lim > 0 && lim < 1<<60 {
		c.MemLimit = lim
	}
	c.CPUUsageNS, _ = strconv.ParseInt(kv["cpu_usage_ns"], 10, 64)
	if q, err := strconv.ParseFloat(kv["cpu_quota_us"], 64); err == nil && q > 0 {
		if p, err := strconv.ParseFloat(kv["cpu_period_us"], 64); err == nil && p > 0 {
			c.CPUQuota = finite(q/p, true)
		}
	}
	if m := strings.Fields(kv["cpu_max"]); len(m) == 2 && m[0] != "max" {
		q, p := parseFinite(m[0]), parseFinite(m[1])
		if q > 0 && p > 0 {
			c.CPUQuota = finite(q/p, true)
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
			return finite(v, true)
		case string:
			return parseFinite(v)
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
	if st := strings.TrimSpace(s["qbt_bin_stat"]); len(st) <= 80 && binaryStatShape.MatchString(st) {
		q.BinaryStat = st // anything else is not a stat line, and it is echoed back to the box every poll
	}
	q.StartHelper = strings.TrimSpace(s["qbt_start_helper"]) == "1"
	q.CronEntries, _ = strconv.Atoi(strings.TrimSpace(s["qbt_cron"]))
	q.PMSSManaged = strings.TrimSpace(s["qbt_pmss"]) == "1"
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

// binaryStatShape is what `stat -c '%s %Y %Z %i'` prints.
var binaryStatShape = regexp.MustCompile(`^[0-9]+ [0-9]+ [0-9]+ [0-9]+$`)

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
		if err != nil || n < 0 || n > 1<<60 { // du never prints these; a forged line must not skew totals
			continue
		}
		out = append(out, DirUsage{Name: f[1], Bytes: n})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Bytes > out[j].Bytes })
	return out
}

// hostnameField accepts only a DNS-shaped host name. The UI builds https
// links from it, so a value with '@', '/', '#' or quotes from a hostile box
// must not become a link to another origin; such a value is dropped.
func hostnameField(h string) string {
	if h == "" || len(h) > 253 || h[0] == '.' || h[0] == '-' {
		return ""
	}
	for _, r := range h {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '-' {
			return ""
		}
	}
	return h
}
