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
	"math"
	"path"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"pmss-fleet-manager/internal/probe"
)

// ProcView is one of the account's processes for the UI.
type ProcView struct {
	PID        int     `json:"pid"`
	Name       string  `json:"name"` // comm
	Args       string  `json:"args,omitempty"`
	RSS        int64   `json:"rss"`
	CPUPct     float64 `json:"cpu_pct"`
	CPUKnown   bool    `json:"cpu_known"` // two samples of this pid were seen, so the rate is real
	ElapsedSec int64   `json:"elapsed_sec"`
	App        string  `json:"app"` // AppView.Key it was grouped under
}

// AppView is one detected application: every process of the account that
// the classifier attributes to it, summed.
type AppView struct {
	Key      string  `json:"key"`      // stable id, e.g. "radarr", "other:myscript"
	Name     string  `json:"name"`     // display name
	Category string  `json:"category"` // torrent | media | download | sync | web | docker | shell | other
	Running  bool    `json:"running"`
	Watched  bool    `json:"watched"` // alert when not running
	Procs    int     `json:"procs"`
	RSS      int64   `json:"rss"`
	CPUPct   float64 `json:"cpu_pct"`
	CPUKnown bool    `json:"cpu_known"`
	// UptimeSec is the longest-running process's age: how long the app has
	// been up, give or take a watchdog restart of a helper.
	UptimeSec int64 `json:"uptime_sec"`
	Ports     []int `json:"ports,omitempty"` // listening TCP ports held by its processes
	// WebPath is where PMSS serves the app on the box's own web server, when
	// the stack has a conventional path for it ("/public-<user>/radarr/").
	WebPath string `json:"web_path,omitempty"`
}

// AppsState is the services view of one box: the account's own processes
// grouped into applications.
type AppsState struct {
	OK      bool      `json:"ok"` // ps listed the account's processes this poll
	At      time.Time `json:"at"`
	Procs   int       `json:"procs"` // process count
	RSS     int64     `json:"rss"`   // summed resident memory
	Watched []string  `json:"watched"`
	Apps    []AppView `json:"apps,omitempty"`
	// Processes is the full list, largest first, for the per-box tab; the
	// fleet summary drops it.
	Processes []ProcView `json:"processes,omitempty"`
}

// appRule maps a process to an application. comm is matched exactly (the
// kernel's 15-byte name, so "transmission-da"), args by a substring of the
// command line for interpreters (python, mono, dotnet, node, java) whose
// comm says nothing.
type appRule struct {
	key, name, category string
	comm                []string
	args                []string // lower-case substrings of the command line
	webPath             string   // PMSS path template: {u} = account name
}

var appRules = []appRule{
	{key: "qbittorrent", name: "qBittorrent", category: "torrent", comm: []string{"qbittorrent-nox", "qbittorrent"}, webPath: "/user-{u}/qbittorrent"},
	{key: "rtorrent", name: "rTorrent", category: "torrent", comm: []string{"rtorrent", "rtorrent main"}, args: []string{"/rtorrent", ".rtorrentexecute.php"}, webPath: "/user-{u}/rutorrent/"},
	{key: "deluge", name: "Deluge", category: "torrent", comm: []string{"deluged", "deluge-web", "deluge"}, args: []string{"/deluged", "/deluge-web", "deluge.ui.web", "deluge.core.daemon"}, webPath: "/deluge-{u}/"},
	{key: "transmission", name: "Transmission", category: "torrent", comm: []string{"transmission-da", "transmission-daemon"}},
	{key: "autodl", name: "autodl-irssi", category: "torrent", comm: []string{"irssi"}},
	{key: "radarr", name: "Radarr", category: "media", comm: []string{"Radarr", "radarr"}, args: []string{"/radarr", "radarr.exe", "radarr.dll"}, webPath: "/public-{u}/radarr/"},
	{key: "sonarr", name: "Sonarr", category: "media", comm: []string{"Sonarr", "sonarr"}, args: []string{"/sonarr", "nzbdrone.exe", "sonarr.exe", "sonarr.dll"}, webPath: "/public-{u}/sonarr/"},
	{key: "lidarr", name: "Lidarr", category: "media", comm: []string{"Lidarr", "lidarr"}, args: []string{"/lidarr", "lidarr.dll"}, webPath: "/public-{u}/lidarr/"},
	{key: "readarr", name: "Readarr", category: "media", comm: []string{"Readarr", "readarr"}, args: []string{"/readarr", "readarr.dll"}},
	{key: "prowlarr", name: "Prowlarr", category: "media", comm: []string{"Prowlarr", "prowlarr"}, args: []string{"/prowlarr", "prowlarr.dll"}, webPath: "/public-{u}/prowlarr/"},
	{key: "bazarr", name: "Bazarr", category: "media", comm: []string{"bazarr"}, args: []string{"bazarr.py", "/bazarr/"}},
	{key: "jackett", name: "Jackett", category: "media", comm: []string{"jackett", "Jackett"}, args: []string{"jackett.dll", "/jackett"}},
	{key: "overseerr", name: "Overseerr", category: "media", args: []string{"overseerr", "jellyseerr"}},
	{key: "jellyfin", name: "Jellyfin", category: "media", comm: []string{"jellyfin"}, args: []string{"jellyfin.dll", "/jellyfin"}, webPath: "/public-{u}/jellyfin/"},
	{key: "plex", name: "Plex Media Server", category: "media", comm: []string{"Plex Media Serv", "Plex Media Server", "Plex Script Hos", "Plex Tuner Serv", "Plex Plug-in"}, args: []string{"plex media server"}},
	{key: "emby", name: "Emby", category: "media", comm: []string{"EmbyServer", "emby-server"}, args: []string{"embyserver"}},
	{key: "ffmpeg", name: "FFmpeg", category: "media", comm: []string{"ffmpeg", "ffprobe"}},
	{key: "filebot", name: "FileBot", category: "media", comm: []string{"filebot"}, args: []string{"filebot"}},
	{key: "sabnzbd", name: "SABnzbd", category: "download", comm: []string{"SABnzbd", "sabnzbd"}, args: []string{"sabnzbd.py", "/sabnzbd"}, webPath: "/public-{u}/sabnzbd/"},
	{key: "nzbget", name: "NZBGet", category: "download", comm: []string{"nzbget"}},
	{key: "pyload", name: "pyLoad", category: "download", comm: []string{"pyload", "pyLoadCore", "pyLoadCore.py"}, args: []string{"pyload"}},
	{key: "flexget", name: "FlexGet", category: "download", comm: []string{"flexget"}, args: []string{"flexget"}},
	{key: "cloudplow", name: "Cloudplow", category: "sync", comm: []string{"cloudplow", "cloudplow.py"}, args: []string{"cloudplow"}},
	{key: "rclone", name: "rclone", category: "sync", comm: []string{"rclone"}},
	{key: "syncthing", name: "Syncthing", category: "sync", comm: []string{"syncthing"}},
	{key: "resilio", name: "Resilio Sync", category: "sync", comm: []string{"rslsync", "btsync"}},
	{key: "lighttpd", name: "lighttpd (web UI)", category: "web", comm: []string{"lighttpd"}},
	{key: "php", name: "PHP (web UI)", category: "web", comm: []string{"php-cgi", "php-fpm"}, args: []string{"php-fpm", "php-cgi"}},
	{key: "nginx", name: "nginx", category: "web", comm: []string{"nginx"}},
	{key: "docker", name: "Docker (rootless)", category: "docker", comm: []string{"dockerd", "dockerd-rootles", "rootlesskit", "containerd", "containerd-shim", "docker-proxy", "slirp4netns", "vpnkit", "fuse-overlayfs", "docker-init", "containerd-shim-runc-v2"}, args: []string{"dockerd-rootless", "/containerd-shim", "docker-proxy"}},
	{key: "screen", name: "screen / tmux", category: "shell", comm: []string{"screen", "SCREEN", "tmux", "tmux: server"}},
	{key: "shell", name: "SSH / shell sessions", category: "shell", comm: []string{"sshd", "bash", "sh", "dash", "zsh", "fish", "sftp-server", "internal-sftp", "sleep", "ssh-agent", "sshd-session"}},
	{key: "systemd", name: "systemd user session", category: "shell", comm: []string{"systemd", "(sd-pam)", "dbus-daemon", "dbus-broker", "dbus-broker-lau", "pipewire", "wireplumber", "gpg-agent"}},
}

// interpreters are commands whose comm names the runtime, not the app; the
// script or assembly in the command line names the app instead.
var interpreters = map[string]bool{"python": true, "python3": true, "python2": true, "mono": true, "dotnet": true, "node": true, "nodejs": true, "java": true, "perl": true, "ruby": true, "php": true}

// helpers are commands that belong to whatever started them: a sleep
// forked by Radarr, the cat pipes lighttpd keeps, the sh that execs
// rtorrent. They inherit the parent's app (see inheritApps) unless the
// parent is itself a session or unknown.
var helpers = map[string]bool{"sh": true, "bash": true, "dash": true, "sleep": true, "cat": true, "timeout": true, "nice": true, "ionice": true, "env": true, "tee": true, "xargs": true, "tail": true}

// inheritApps reassigns helper and unknown processes to the app of their
// nearest classified ancestor, so an app's forked helpers count under it.
func inheritApps(procs []ProcView, ppid map[int]int) {
	idx := make(map[int]int, len(procs))
	for i, p := range procs {
		idx[p.PID] = i
	}
	inheritable := func(key string) bool {
		r, ok := ruleByKey[key]
		return ok && r.category != "shell"
	}
	// Parents usually precede children, but after a pid wrap a helper can
	// be listed before its ancestor: repeat until a pass changes nothing,
	// bounded by the longest possible chain.
	for pass := 0; pass <= len(procs); pass++ {
		changed := false
		for i := range procs {
			p := &procs[i]
			if !helpers[p.Name] && !strings.HasPrefix(p.App, otherPrefix) {
				continue
			}
			if j, ok := idx[ppid[p.PID]]; ok && inheritable(procs[j].App) && p.App != procs[j].App {
				p.App = procs[j].App
				changed = true
			}
		}
		if !changed {
			return
		}
	}
}

var ruleByKey = func() map[string]*appRule {
	m := make(map[string]*appRule, len(appRules))
	for i := range appRules {
		m[appRules[i].key] = &appRules[i]
	}
	return m
}()

// classifyProc returns the rule for a process, or nil for an unknown one.
// The command line decides for interpreters, and for any process whose comm
// matched nothing (a renamed binary still says what it is).
func classifyProc(comm, args string) *appRule {
	if !interpreters[comm] {
		if r := matchComm(comm); r != nil {
			return r
		}
	}
	return matchArgs(strings.ToLower(args))
}

func matchComm(comm string) *appRule {
	for i := range appRules {
		if slices.Contains(appRules[i].comm, comm) {
			return &appRules[i]
		}
	}
	return nil
}

func matchArgs(largs string) *appRule {
	for i := range appRules {
		for _, a := range appRules[i].args {
			if a != "" && strings.Contains(largs, a) {
				return &appRules[i]
			}
		}
	}
	return nil
}

// otherPrefix starts the key of an unclassified process.
const otherPrefix = "other:"

// otherKey names an unclassified process by its comm, or for an
// interpreter by its script: "other:python3 myscript.py". Control
// characters (a name set with prctl can carry them) become '?' so the key
// always passes ValidAppKey.
func otherKey(comm, args string) (key, name string) {
	name = comm
	if interpreters[comm] {
		if script := scriptName(args); script != "" {
			name = comm + " " + script
		}
	}
	name = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return '?'
		}
		return r
	}, name)
	return otherPrefix + strings.ToLower(clip(name, 48)), clip(name, 48)
}

// scriptName picks the first argument that looks like a script, jar or
// assembly from an interpreter's command line.
func scriptName(args string) string {
	f := strings.Fields(args)
	for _, a := range f[min(1, len(f)):] {
		if strings.HasPrefix(a, "-") {
			continue
		}
		l := strings.ToLower(a)
		if strings.HasSuffix(l, ".py") || strings.HasSuffix(l, ".js") || strings.HasSuffix(l, ".jar") || strings.HasSuffix(l, ".dll") || strings.HasSuffix(l, ".exe") || strings.HasSuffix(l, ".pl") || strings.HasSuffix(l, ".rb") || strings.Contains(a, "/") {
			return path.Base(a)
		}
		return a // "python3 -m flexget" style: the module
	}
	return ""
}

// AppName is the display name for an app key, for alerts about an app
// that is not currently running (so has no AppView to take it from).
func AppName(key string) string {
	if r, ok := ruleByKey[key]; ok {
		return r.name
	}
	if n, ok := strings.CutPrefix(key, otherPrefix); ok {
		return n
	}
	return key
}

// ValidAppKey accepts the keys the classifier produces: a rule key, or
// "other:" plus a clipped process name (any printable text, up to 48 runes
// plus the clip's ellipsis, so the bound is in runes: a non-ASCII name is
// longer in bytes).
func ValidAppKey(k string) bool {
	if k == "" || utf8.RuneCountInString(k) > 64 {
		return false
	}
	if _, ok := ruleByKey[k]; ok {
		return true
	}
	n, ok := strings.CutPrefix(k, otherPrefix)
	if !ok || n == "" || n != strings.ToLower(n) {
		return false
	}
	for _, r := range n {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// procSample is one pid's cumulative CPU reading, for the rate.
type procSample struct {
	cpuSec  float64
	start   int64 // start tick, 0 when the box did not report it
	elapsed int64
	at      time.Time
}

// sameProcess reports whether a pid's new reading is of the process the
// previous sample measured: the same start tick when known, else an age
// that grew by about the wall time since the last sample.
func sameProcess(s procSample, p probe.ProcInfo, wall float64) bool {
	if s.start != 0 || p.StartTick != 0 {
		return s.start == p.StartTick
	}
	return p.ElapsedSec >= s.elapsed && float64(p.ElapsedSec-s.elapsed) <= wall+5
}

// appsFromSnapshot groups the probe's processes into AppViews. prev holds
// the previous poll's per-pid CPU readings; this poll's are returned for
// the caller to keep; a pid that was seen last time with a consistent start gets a
// rate, anything else is reported with CPUKnown false. watched is the
// box's watch list; a watched app that is not running is listed as such.
func appsFromSnapshot(snap *probe.Snapshot, user string, prev map[int]procSample, watched []string) (AppsState, map[int]procSample) {
	st := AppsState{OK: snap.ProcsKnown, At: snap.At, Watched: append([]string{}, watched...)}
	next := make(map[int]procSample, len(snap.Procs))
	if !snap.ProcsKnown {
		return st, next
	}
	ppid := make(map[int]int, len(snap.Procs))
	for _, p := range snap.Procs {
		ppid[p.PID] = p.PPID
		next[p.PID] = procSample{cpuSec: p.CPUSec, start: p.StartTick, elapsed: p.ElapsedSec, at: snap.At}
		st.Processes = append(st.Processes, procView(p, prev, snap.At))
		st.RSS += p.RSS
	}
	st.Procs = len(st.Processes)
	inheritApps(st.Processes, ppid)
	apps := groupApps(st.Processes, user)
	attachPorts(apps, st.Processes, snap.ListenPIDs)
	st.Apps = make([]AppView, 0, len(apps))
	for _, a := range apps {
		st.Apps = append(st.Apps, *a)
	}
	st.markWatched() // also sorts
	sort.SliceStable(st.Processes, func(i, j int) bool { return st.Processes[i].RSS > st.Processes[j].RSS })
	if len(st.Processes) > maxProcViews {
		st.Processes = st.Processes[:maxProcViews]
	}
	return st, next
}

// procView builds one process's view: clipped strings, the CPU rate
// against the previous sample of the same process, and its app key.
func procView(p probe.ProcInfo, prev map[int]procSample, at time.Time) ProcView {
	pv := ProcView{PID: p.PID, Name: clip(p.Comm, 64), Args: clip(p.Args, 400), RSS: p.RSS, ElapsedSec: p.ElapsedSec}
	if s, ok := prev[p.PID]; ok && at.After(s.at) {
		wall := at.Sub(s.at).Seconds()
		if wall > 0 && sameProcess(s, p, wall) && p.CPUSec >= s.cpuSec {
			pv.CPUPct = (p.CPUSec - s.cpuSec) / wall * 100
			pv.CPUKnown = !math.IsInf(pv.CPUPct, 0) && !math.IsNaN(pv.CPUPct) // a hostile tick rate must not reach the sums
			if !pv.CPUKnown {
				pv.CPUPct = 0
			}
		}
	}
	if r := classifyProc(p.Comm, p.Args); r != nil {
		pv.App = r.key
	} else {
		pv.App, _ = otherKey(p.Comm, p.Args)
	}
	return pv
}

// groupApps sums the processes into one AppView per app key.
func groupApps(procs []ProcView, user string) map[string]*AppView {
	apps := map[string]*AppView{}
	for _, pv := range procs {
		a := apps[pv.App]
		if a == nil {
			a = newAppView(pv, user)
			apps[pv.App] = a
		}
		a.Procs++
		a.RSS += pv.RSS
		if pv.CPUKnown {
			a.CPUPct += pv.CPUPct
			a.CPUKnown = true
		}
		a.UptimeSec = max(a.UptimeSec, pv.ElapsedSec)
	}
	return apps
}

// newAppView names a running app from its rule, or after its first process.
func newAppView(pv ProcView, user string) *AppView {
	a := &AppView{Key: pv.App, Running: true}
	r, ok := ruleByKey[pv.App]
	if !ok {
		_, a.Name = otherKey(pv.Name, pv.Args)
		a.Category = "other"
		return a
	}
	a.Name, a.Category = r.name, r.category
	if r.webPath != "" && user != "" {
		a.WebPath = strings.ReplaceAll(r.webPath, "{u}", user)
	}
	return a
}

// attachPorts lists each app's listening ports once: a port held by several
// of our pids (a parent and its forked children) belongs to their app.
func attachPorts(apps map[string]*AppView, procs []ProcView, listenPIDs map[int][]int) {
	appOf := make(map[int]string, len(procs))
	for _, pv := range procs {
		appOf[pv.PID] = pv.App
	}
	seen := map[string]map[int]bool{}
	for port, pids := range listenPIDs {
		for _, pid := range pids {
			k, ok := appOf[pid]
			if !ok || seen[k][port] {
				continue
			}
			if seen[k] == nil {
				seen[k] = map[int]bool{}
			}
			seen[k][port] = true
			apps[k].Ports = append(apps[k].Ports, port)
		}
	}
	for _, a := range apps {
		sort.Ints(a.Ports)
	}
}

// sortApps orders the views in place: running first, then by category,
// memory, name. Only ever called on a slice markWatched just allocated.
func sortApps(out []AppView) {
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Running != b.Running {
			return a.Running // running apps first; a watched app that is down stands out at the end
		}
		if a.Category != b.Category {
			return categoryRank(a.Category) < categoryRank(b.Category)
		}
		if a.RSS != b.RSS {
			return a.RSS > b.RSS
		}
		return a.Name < b.Name
	})
}

// maxProcViews bounds the per-box process list served to the UI.
const maxProcViews = 300

func categoryOf(key string) string {
	if r, ok := ruleByKey[key]; ok {
		return r.category
	}
	return "other"
}

func categoryRank(c string) int {
	switch c {
	case "torrent":
		return 0
	case "media":
		return 1
	case "download":
		return 2
	case "sync":
		return 3
	case "docker":
		return 4
	case "web":
		return 5
	case "other":
		return 6
	}
	return 7 // shell last: the sessions are the dashboard's own
}

// markWatched recomputes the Watched flags of a state's apps from its watch
// list and adds rows for watched apps that are not running. It builds new
// slices: a state is copied shallowly when it is stored or served, so the
// slices behind it may be shared with readers and are never written to.
func (a *AppsState) markWatched() {
	want := make(map[string]bool, len(a.Watched))
	for _, k := range a.Watched {
		want[k] = true
	}
	out := make([]AppView, 0, len(a.Apps)+len(want))
	seen := make(map[string]bool, len(a.Apps))
	for _, v := range a.Apps {
		v.Watched = want[v.Key]
		seen[v.Key] = true
		if v.Running || v.Watched {
			out = append(out, v) // an unwatched, not-running placeholder row has no reason to stay
		}
	}
	for _, k := range a.Watched {
		if !seen[k] {
			seen[k] = true // a hand-edited list may repeat a key
			out = append(out, AppView{Key: k, Name: AppName(k), Category: categoryOf(k), Watched: true})
		}
	}
	sortApps(out) // the placeholder rows take their category order among the other down apps
	a.Apps = out
}
