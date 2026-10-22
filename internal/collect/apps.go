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
	"path"
	"sort"
	"strings"
	"time"

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
	for pass := 0; pass < 3; pass++ { // parents usually precede children, but not always
		for i := range procs {
			p := &procs[i]
			if !helpers[p.Name] && !strings.HasPrefix(p.App, "other:") {
				continue
			}
			if j, ok := idx[ppid[p.PID]]; ok && inheritable(procs[j].App) {
				p.App = procs[j].App
			}
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
func classifyProc(comm, args string) *appRule {
	largs := strings.ToLower(args)
	// The command line decides for interpreters, and for any process whose
	// comm matched nothing (a renamed binary still says what it is).
	if !interpreters[comm] {
		for i := range appRules {
			for _, c := range appRules[i].comm {
				if c == comm {
					return &appRules[i]
				}
			}
		}
	}
	for i := range appRules {
		for _, a := range appRules[i].args {
			if a != "" && strings.Contains(largs, a) {
				return &appRules[i]
			}
		}
	}
	return nil
}

// otherKey names an unclassified process by its comm, or for an
// interpreter by its script: "other:python3:myscript.py".
func otherKey(comm, args string) (key, name string) {
	name = comm
	if interpreters[comm] {
		if script := scriptName(args); script != "" {
			name = comm + " " + script
		}
	}
	return "other:" + strings.ToLower(clip(name, 48)), clip(name, 48)
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
	if n, ok := strings.CutPrefix(key, "other:"); ok {
		return n
	}
	return key
}

// ValidAppKey accepts the keys the classifier produces: a rule key, or
// "other:" plus a clipped process name (any printable text).
func ValidAppKey(k string) bool {
	if k == "" || len(k) > 64 {
		return false
	}
	if _, ok := ruleByKey[k]; ok {
		return true
	}
	n, ok := strings.CutPrefix(k, "other:")
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
// the previous poll's per-pid CPU readings and is replaced with this
// poll's; a pid that was seen last time with a consistent start gets a
// rate, anything else is reported with CPUKnown false. watched is the
// box's watch list; a watched app that is not running is listed as such.
func appsFromSnapshot(snap *probe.Snapshot, user string, prev map[int]procSample, watched []string) (AppsState, map[int]procSample) {
	st := AppsState{OK: snap.ProcsKnown, At: snap.At, Watched: append([]string(nil), watched...)}
	if st.Watched == nil {
		st.Watched = []string{}
	}
	next := make(map[int]procSample, len(snap.Procs))
	if !snap.ProcsKnown {
		return st, next
	}
	byPID := map[int]int{} // pid -> index into st.Processes
	ppid := map[int]int{}
	for _, p := range snap.Procs {
		ppid[p.PID] = p.PPID
		pv := ProcView{PID: p.PID, Name: clip(p.Comm, 64), Args: clip(p.Args, 400), RSS: p.RSS, ElapsedSec: p.ElapsedSec}
		if s, ok := prev[p.PID]; ok && snap.At.After(s.at) {
			wall := snap.At.Sub(s.at).Seconds()
			if wall > 0 && sameProcess(s, p, wall) && p.CPUSec >= s.cpuSec {
				pv.CPUPct = (p.CPUSec - s.cpuSec) / wall * 100
				pv.CPUKnown = true
			}
		}
		next[p.PID] = procSample{cpuSec: p.CPUSec, start: p.StartTick, elapsed: p.ElapsedSec, at: snap.At}
		if r := classifyProc(p.Comm, p.Args); r != nil {
			pv.App = r.key
		} else {
			pv.App, _ = otherKey(p.Comm, p.Args)
		}
		byPID[p.PID] = len(st.Processes)
		st.Processes = append(st.Processes, pv)
		st.Procs++
		st.RSS += p.RSS
	}
	inheritApps(st.Processes, ppid)
	// Ports: a port held by several of our pids (a parent and its forked
	// children) belongs to whichever app those pids are in; list it once.
	portsByApp := map[string]map[int]bool{}
	for port, pids := range snap.ListenPIDs {
		for _, pid := range pids {
			if i, ok := byPID[pid]; ok {
				k := st.Processes[i].App
				if portsByApp[k] == nil {
					portsByApp[k] = map[int]bool{}
				}
				portsByApp[k][port] = true
			}
		}
	}
	apps := map[string]*AppView{}
	for _, pv := range st.Processes {
		a := apps[pv.App]
		if a == nil {
			a = &AppView{Key: pv.App, Running: true}
			if r, ok := ruleByKey[pv.App]; ok {
				a.Name, a.Category = r.name, r.category
				if r.webPath != "" && user != "" {
					a.WebPath = strings.ReplaceAll(r.webPath, "{u}", user)
				}
			} else {
				_, a.Name = otherKey(pv.Name, pv.Args)
				a.Category = "other"
			}
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
	for k, ports := range portsByApp {
		for port := range ports {
			apps[k].Ports = append(apps[k].Ports, port)
		}
		sort.Ints(apps[k].Ports)
	}
	for _, k := range st.Watched {
		if a, ok := apps[k]; ok {
			a.Watched = true
			continue
		}
		apps[k] = &AppView{Key: k, Name: AppName(k), Category: categoryOf(k), Watched: true}
	}
	st.Apps = make([]AppView, 0, len(apps))
	for _, a := range apps {
		st.Apps = append(st.Apps, *a)
	}
	sort.Slice(st.Apps, func(i, j int) bool {
		a, b := st.Apps[i], st.Apps[j]
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
	sort.SliceStable(st.Processes, func(i, j int) bool { return st.Processes[i].RSS > st.Processes[j].RSS })
	if len(st.Processes) > maxProcViews {
		st.Processes = st.Processes[:maxProcViews]
	}
	return st, next
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
// list and adds rows for watched apps that are not running.
func (a *AppsState) markWatched() {
	want := map[string]bool{}
	for _, k := range a.Watched {
		want[k] = true
	}
	seen := map[string]bool{}
	for i := range a.Apps {
		a.Apps[i].Watched = want[a.Apps[i].Key]
		seen[a.Apps[i].Key] = true
	}
	kept := a.Apps[:0]
	for _, v := range a.Apps {
		if v.Running || want[v.Key] {
			kept = append(kept, v) // an unwatched, not-running placeholder row has no reason to stay
		}
	}
	a.Apps = kept
	for k := range want {
		if !seen[k] {
			a.Apps = append(a.Apps, AppView{Key: k, Name: AppName(k), Category: categoryOf(k), Watched: true})
		}
	}
}
