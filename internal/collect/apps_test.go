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
	"testing"
	"time"

	"pmss-fleet-manager/internal/probe"
)

func TestClassifyProc(t *testing.T) {
	cases := []struct{ comm, args, key string }{
		{"qbittorrent-nox", "/home/u/bin/qbittorrent-nox -d", "qbittorrent"},
		{"rtorrent main", "rtorrent main", "rtorrent"},
		{"transmission-da", "", "transmission"},
		{"Radarr", "/home/u/.bin/Radarr/Radarr -nobrowser", "radarr"},
		{"dotnet", "dotnet /opt/Sonarr/Sonarr.dll -nobrowser", "sonarr"},
		{"mono", "mono --debug /opt/NzbDrone/NzbDrone.exe", "sonarr"},
		{"python3", "/home/u/.bin/sabnzbd/venv/bin/python3 SABnzbd.py -f cfg", "sabnzbd"},
		{"python3", "python3 -m flexget daemon start", "flexget"},
		{"python", "/home/u/.pyload/venv/bin/python /home/u/.pyload/pyLoadCore.py", "pyload"},
		{"jellyfin", "/home/u/.bin/jellyfin/jellyfin --ffmpeg=/home/u/.bin/ffmpeg", "jellyfin"},
		{"ffmpeg", "ffmpeg -i x", "ffmpeg"},
		{"lighttpd", "lighttpd -D -f /home/u/.lighttpd/lighttpd.conf", "lighttpd"},
		{"php-cgi", "/usr/bin/php-cgi", "php"},
		{"php", "php ./.rtorrentExecute.php", "rtorrent"},
		{"exe", "/proc/self/exe --state-dir=/run/user/1047/dockerd-rootless --net=slirp4netns", "docker"},
		{"containerd-shim", "", "docker"},
		{"rootlesskit", "", "docker"},
		{"sshd", "sshd: u@notty", "shell"},
		{"bash", "-bash", "shell"},
		{"SCREEN", "SCREEN -dmS qbt", "screen"},
		{"(sd-pam)", "", "systemd"},
		{"rclone", "rclone mount gd: /home/u/mnt", "rclone"},
	}
	for _, c := range cases {
		r := classifyProc(c.comm, c.args)
		if r == nil || r.key != c.key {
			got := "<nil>"
			if r != nil {
				got = r.key
			}
			t.Errorf("%q %q: got %s, want %s", c.comm, c.args, got, c.key)
		}
	}
	if classifyProc("myscript", "/home/u/myscript --flag") != nil {
		t.Error("unknown binary classified")
	}
	// An interpreter's comm says nothing: an unknown script stays unknown and
	// is named after itself.
	if classifyProc("python3", "python3 /home/u/bot.py") != nil {
		t.Error("unknown python script classified")
	}
	if k, n := otherKey("python3", "python3 /home/u/bot.py"); k != "other:python3 bot.py" || n != "python3 bot.py" {
		t.Errorf("other key: %q %q", k, n)
	}
	if k, _ := otherKey("python3", "python3 -m mybot --x"); k != "other:python3 mybot" {
		t.Errorf("module key: %q", k)
	}
}

func TestValidAppKey(t *testing.T) {
	for _, k := range []string{"radarr", "other:myscript", "other:python3 bot.py"} {
		if !ValidAppKey(k) {
			t.Errorf("%q should be valid", k)
		}
	}
	for _, k := range []string{"", "Radarr", "other:", "other:Upper", "other:a\nb", "nosuchapp", "other:" + string(make([]byte, 70))} {
		if ValidAppKey(k) {
			t.Errorf("%q should be invalid", k)
		}
	}
	if AppName("radarr") != "Radarr" || AppName("other:foo") != "foo" || AppName("zzz") != "zzz" {
		t.Error("AppName")
	}
}

func TestAppsFromSnapshot(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	snap := &probe.Snapshot{At: t0, ProcsKnown: true,
		Procs: []probe.ProcInfo{
			{PID: 10, PPID: 1, RSS: 800 << 20, CPUSec: 100, StartTick: 1000, ElapsedSec: 5000, Comm: "qbittorrent-nox", Args: "/home/u/bin/qbittorrent-nox -d"},
			{PID: 20, PPID: 1, RSS: 200 << 20, CPUSec: 50, StartTick: 2000, ElapsedSec: 400, Comm: "Radarr", Args: "/home/u/.bin/Radarr/Radarr"},
			{PID: 21, PPID: 20, RSS: 1 << 20, CPUSec: 0, ElapsedSec: 10, Comm: "sleep", Args: "sleep 5"},
			{PID: 30, PPID: 1, RSS: 5 << 20, CPUSec: 1, ElapsedSec: 99999, Comm: "sshd", Args: "sshd: u@notty"},
		},
		ListenPIDs: map[int][]int{8080: {10}, 7878: {20}, 9999: {555}}}
	st, prev := appsFromSnapshot(snap, "ragnar", nil, []string{"radarr", "jellyfin"})
	if !st.OK || st.Procs != 4 || st.RSS != (800+200+1+5)<<20 {
		t.Fatalf("totals: %+v", st)
	}
	for _, p := range st.Processes {
		if p.PID == 21 && p.App != "radarr" {
			t.Fatalf("Radarr's sleep must count under Radarr: %+v", p)
		}
	}
	byKey := map[string]AppView{}
	for _, a := range st.Apps {
		byKey[a.Key] = a
	}
	if q := byKey["qbittorrent"]; !q.Running || q.Procs != 1 || len(q.Ports) != 1 || q.Ports[0] != 8080 || q.CPUKnown || q.WebPath != "/user-ragnar/qbittorrent" || q.UptimeSec != 5000 {
		t.Fatalf("qbt: %+v", q)
	}
	if r := byKey["radarr"]; !r.Watched || !r.Running || r.Procs != 2 || r.Ports[0] != 7878 || r.WebPath != "/public-ragnar/radarr/" {
		t.Fatalf("radarr: %+v", r)
	}
	if j, ok := byKey["jellyfin"]; !ok || j.Running || !j.Watched || j.Name != "Jellyfin" || j.Category != "media" {
		t.Fatalf("watched but absent app must be listed: %+v", j)
	}
	if st.Apps[len(st.Apps)-1].Key != "jellyfin" {
		t.Fatalf("not-running app must sort last: %v", st.Apps)
	}
	if len(prev) != 4 {
		t.Fatalf("samples: %v", prev)
	}
	// Second poll 60 s later: qbt used 30 CPU seconds (50%), Radarr's pid was
	// reused by a process with another start tick (no rate), sshd has no
	// start tick and is matched by its age, sleep is gone.
	snap2 := &probe.Snapshot{At: t0.Add(60 * time.Second), ProcsKnown: true,
		Procs: []probe.ProcInfo{
			{PID: 10, PPID: 1, RSS: 800 << 20, CPUSec: 130.5, StartTick: 1000, ElapsedSec: 5060, Comm: "qbittorrent-nox"},
			{PID: 20, PPID: 1, RSS: 200 << 20, CPUSec: 60, StartTick: 7777, ElapsedSec: 460, Comm: "Radarr"},
			{PID: 30, PPID: 1, RSS: 5 << 20, CPUSec: 1, ElapsedSec: 100059, Comm: "sshd"},
		}}
	st2, _ := appsFromSnapshot(snap2, "ragnar", prev, nil)
	for _, a := range st2.Apps {
		switch a.Key {
		case "qbittorrent":
			if !a.CPUKnown || a.CPUPct < 50.8 || a.CPUPct > 50.9 {
				t.Fatalf("qbt cpu: %+v", a)
			}
		case "radarr":
			if a.CPUKnown || a.Watched {
				t.Fatalf("reused pid must not get a rate, unwatched must not be watched: %+v", a)
			}
		case "shell":
			if !a.CPUKnown || a.CPUPct != 0 {
				t.Fatalf("idle sshd: %+v", a)
			}
		}
	}
	// ps unavailable: nothing is known, the watch list is still carried.
	st3, _ := appsFromSnapshot(&probe.Snapshot{At: t0}, "ragnar", nil, []string{"radarr"})
	if st3.OK || len(st3.Apps) != 0 || len(st3.Watched) != 1 {
		t.Fatalf("unknown procs: %+v", st3)
	}
}

func TestMarkWatched(t *testing.T) {
	a := AppsState{OK: true, Apps: []AppView{{Key: "radarr", Running: true}, {Key: "sonarr", Running: false, Watched: true}}, Watched: []string{"sonarr"}}
	a.Watched = []string{"radarr", "jellyfin"}
	a.markWatched()
	got := map[string]AppView{}
	for _, v := range a.Apps {
		got[v.Key] = v
	}
	if _, ok := got["sonarr"]; ok {
		t.Fatal("unwatched placeholder row must go")
	}
	if !got["radarr"].Watched || !got["jellyfin"].Watched || got["jellyfin"].Running || got["jellyfin"].Name != "Jellyfin" {
		t.Fatalf("marks: %+v", got)
	}
}
