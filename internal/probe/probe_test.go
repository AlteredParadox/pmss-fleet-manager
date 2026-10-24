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

package probe

import (
	"bytes"
	"compress/gzip"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

const sample = `@@uid
1001
@@user
ragnar
@@hostname
odin.pulsedmedia.com
@@now
1760572800
@@uptime
15724800.12 123.4
@@loadavg
3.21 2.87 2.50 4/1234 56789
@@meminfo
MemTotal:       65536000 kB
MemAvailable:   12345678 kB
SwapTotal:       8388608 kB
@@ncpu
16
@@cpu_mhz
 2100.000
@@kernel
6.1.0-18-amd64
@@debian
12.5
@@pmss_version
git/main:2026-10-01
@@df_home
15000000000000 9000000000000 6000000000000 ext4
@@quota
Disk quotas for user ragnar (uid 1001): 
     Filesystem  blocks   quota   limit   grace   files   quota   limit   grace
      /dev/md2 1739461632* 2097152000 2621440000   6days   12345 1000000 1250000        
@@quota_file
@@quota_mtime
@@traffic
a:2:{s:3:"raw";a:5:{s:5:"month";d:6963.25;s:4:"week";i:1200;s:3:"day";d:0;s:4:"hour";d:12.5;s:5:"15min";d:3;}s:5:"daily";a:2:{s:10:"2026/10/15";d:220.5;s:10:"2026/10/16";i:40;}}
@@traffic_in
@@traffic_limit
10240
@@bonus_traffic
20000
@@throttle
0
@@pmss_user
{"quota": 2000, "quotaBurst": 2500, "ramMiB": 8192, "dockerEnabled": true, "trafficCapMbit": 1000}
@@cgroup
mem_usage=4080218931
mem_limit=8589934592
cpu_usage_ns=123456789000
cpu_quota_us=400000
cpu_period_us=100000
pids=142
@@qbt_conf
WebUI\Port=8089
WebUI\LocalHostAuth=false
@@qbt_pid
4242
@@qbt_bin
/home/ragnar/bin/qbittorrent-nox
@@qbt_bin_stat
30515200 1760500000 1760500001 4242
@@qbt_cron
2
@@qbt_pmss
1
@@docker_sock
1
@@logs
1760572000.5	1234	.logs/trackerCleaner.log
1760572700	99	.local/share/qBittorrent/logs/qbittorrent.log
1760570000	5	../etc/passwd
1	x	.logs/bad.log
1760571000	7	./.lighttpd/error.log
@@probe_pid
777
@@procs
    777       1  1200        0       1 sh
    778     777   900        0       0 ps
   4242       1 812340     6172  438873 qbittorrent-nox
   5001       1 212000      300   86400 Radarr
   5002    5001   100        1       1 sleep
   5003       1  5000        2    3600 rtorrent main
@@proc_args
777 sh
778 ps -ww -u 1001 -o pid=,ppid=,rss=,times=,etimes=,comm=
4242 /home/ragnar/bin/qbittorrent-nox -d
5001 /home/ragnar/.bin/Radarr/Radarr -nobrowser -data=/home/ragnar/.config/Radarr
5003 rtorrent main
end
@@proc_stat
100
4242 500000 117250 1234567
5001 30000 100 99
@@listen_pids
8089 4242
8089 4242
7878 5001
@@end
`

func TestParse(t *testing.T) {
	snap, err := Parse([]byte(sample), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if snap.UID != 1001 || snap.User != "ragnar" || snap.Hostname != "odin.pulsedmedia.com" {
		t.Fatalf("identity: %+v", snap)
	}
	if snap.Host.NCPU != 16 || snap.Host.MemTotal != 65536000*1024 || snap.Host.Load1 != 3.21 {
		t.Errorf("host: %+v", snap.Host)
	}
	if !snap.Host.SwapKnown || snap.Host.SwapTotal != 8388608*1024 || snap.Host.CPUMHz != 2100 {
		t.Fatalf("host: %+v", snap.Host)
	}
	if snap.HomeFS.Type != "ext4" || snap.HomeFS.Used != 9000000000000 {
		t.Fatalf("fs: %+v", snap.HomeFS)
	}
	q := snap.Quota
	if !q.OK || q.Source != "quota" || q.UsedBytes != 1739461632*1024 || q.SoftBytes != 2097152000*1024 || q.FilesUsed != 12345 {
		t.Fatalf("quota: %+v", q)
	}
	tr := snap.Traffic
	if !tr.Out.OK || tr.Out.MonthMiB != 6963.25 || tr.Out.Daily["2026/10/16"] != 40 {
		t.Fatalf("traffic out: %+v", tr.Out)
	}
	if tr.LimitGiB != 10240 || tr.BonusGiB != 10240 || tr.EffectiveLimitGiB != 20480 {
		t.Fatalf("traffic limit: %+v", tr)
	}
	if snap.Cgroup.MemLimit != 8589934592 || snap.Cgroup.CPUQuota != 4 || snap.Cgroup.Pids != 142 {
		t.Fatalf("cgroup: %+v", snap.Cgroup)
	}
	if !snap.Account.OK || snap.Account.QuotaGiB != 2000 || !snap.Account.DockerEnabled {
		t.Fatalf("account: %+v", snap.Account)
	}
	if !snap.Qbt.Running || snap.Qbt.ConfPort != 8089 || !snap.Qbt.LocalHostAuth || !snap.Qbt.BinaryPresent || snap.Qbt.CronEntries != 2 || !snap.Qbt.PMSSManaged || snap.Qbt.BinaryStat != "30515200 1760500000 1760500001 4242" {
		t.Fatalf("qbt: %+v", snap.Qbt)
	}
	if !snap.Docker.SocketPresent {
		t.Fatalf("docker: %+v", snap.Docker)
	}
	// The probe's own shell (777) and its child ps (778) are dropped; the
	// rest keep their args, and a comm with a space survives.
	if !snap.ProcsKnown || !snap.ArgsKnown || len(snap.Procs) != 4 {
		t.Fatalf("procs: %+v", snap.Procs)
	}
	// CPU comes from /proc stat ticks where present (6172.5 s), ps's whole
	// seconds otherwise; the start tick identifies the process.
	if q := snap.Procs[0]; q.PID != 4242 || q.Comm != "qbittorrent-nox" || q.RSS != 812340*1024 || q.CPUSec != 6172.5 || q.StartTick != 1234567 || q.ElapsedSec != 438873 || q.Args != "/home/ragnar/bin/qbittorrent-nox -d" {
		t.Fatalf("qbt proc: %+v", q)
	}
	if r := snap.Procs[3]; r.Comm != "rtorrent main" || r.PPID != 1 || r.CPUSec != 2 || r.StartTick != 0 {
		t.Fatalf("rtorrent proc: %+v", r)
	}
	if got := snap.ListenPIDs[8089]; len(got) != 1 || got[0] != 4242 {
		t.Fatalf("listen pids (deduplicated): %v", snap.ListenPIDs)
	}
	if got := snap.ListenPIDs[7878]; len(got) != 1 || got[0] != 5001 {
		t.Fatalf("listen pids: %v", snap.ListenPIDs)
	}
}

func TestParseLogs(t *testing.T) {
	snap, err := Parse([]byte(sample), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !snap.LogsKnown || len(snap.Logs) != 3 {
		t.Fatalf("logs: %+v", snap.Logs)
	}
	// Newest first, the "./" prefix stripped, the escape and the junk line dropped.
	if snap.Logs[0].Path != ".local/share/qBittorrent/logs/qbittorrent.log" || snap.Logs[1].Path != ".logs/trackerCleaner.log" || snap.Logs[2].Path != ".lighttpd/error.log" || snap.Logs[1].Size != 1234 || snap.Logs[1].MTime.Unix() != 1760572000 {
		t.Fatalf("logs: %+v", snap.Logs)
	}
	for _, p := range []string{"", "/etc/passwd", "../x", "a/../b", "a/..", "./a", "a//b", "a\nb", "ok/"} {
		if ValidLogPath(p) {
			t.Errorf("%q should be invalid", p)
		}
	}
	for _, p := range []string{".logs/x.log", "logs/a b.txt", ".config/Radarr/logs/radarr.txt"} {
		if !ValidLogPath(p) {
			t.Errorf("%q should be valid", p)
		}
	}
	if files := parseLogs("NaN\t10\tnan.log\n1e12\t1\tbig.log\n1760572000\t5\tok.log"); len(files) != 1 || files[0].Path != "ok.log" {
		t.Fatalf("NaN / out-of-range mtimes must be dropped: %+v", files)
	}
	if ValidLogPath(".logs/caf\xe9.log") {
		t.Error("a non-UTF-8 name would be listed as U+FFFD and never match on the way back")
	}
	// The listing shell keeps the newest files when a directory holds more
	// than the cap: 45 rotated copies older than the live log must not push
	// it out.
	home := t.TempDir()
	os.MkdirAll(home+"/.logs", 0o700)
	for i := 0; i < 45; i++ {
		name := fmt.Sprintf("%s/.logs/old.log.%d", home, i)
		os.WriteFile(name, []byte("x"), 0o600)
		os.Chtimes(name, time.Unix(1700000000+int64(i), 0), time.Unix(1700000000+int64(i), 0))
	}
	os.WriteFile(home+"/.logs/live.log", []byte("live"), 0o600)
	os.Chtimes(home+"/.logs/live.log", time.Unix(1800000000, 0), time.Unix(1800000000, 0))
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader("home=" + home + "\n" + logsListShell)
	lsOut, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	files := parseLogs(string(lsOut))
	if len(files) != 40 || files[0].Path != ".logs/live.log" || files[0].Size != 4 {
		t.Fatalf("newest-first cap: %d files, first %+v", len(files), files[:min(2, len(files))])
	}
	// LogTailScript refuses on the box what ValidLogPath refuses here, even
	// for a path that slipped past (the function quotes whatever it is given).
	for _, p := range []string{"/etc/passwd", "../x", "a/../b", "", "it's/../x"} {
		out, stderr, code := runSh(t, LogTailScript(p, 5))
		if code != 4 || len(out) != 0 || !strings.Contains(stderr, "refused") {
			t.Errorf("%q: code %d out %q err %q", p, code, out, stderr)
		}
	}
	dir := t.TempDir()
	os.WriteFile(dir+"/it's a.log", []byte("1\n2\n3\n4\n5\n6\n7\n"), 0o600)
	t.Setenv("HOME", dir)
	out, _, code := runSh(t, LogTailScript("it's a.log", 3))
	if code != 0 || out != "5\n6\n7\n" {
		t.Fatalf("tail: code %d out %q", code, out)
	}
	if _, stderr, code := runSh(t, LogTailScript("missing.log", 3)); code != 5 || !strings.Contains(stderr, "not a regular file") {
		t.Fatalf("missing: code %d err %q", code, stderr)
	}
	// A newline-free blob (a gzip bomb, a rotated binary) must not make tail
	// buffer it whole: the byte cap sits before the line count, so only the
	// last 256 KiB come back and memory stays bounded.
	var blob bytes.Buffer
	bw := gzip.NewWriter(&blob)
	bw.Write(bytes.Repeat([]byte{'x'}, 3<<20))
	bw.Close()
	os.WriteFile(dir+"/blob.log.gz", blob.Bytes(), 0o600)
	if out, _, code := runSh(t, LogTailScript("blob.log.gz", 300)); code != 0 || len(out) != 262144 {
		t.Fatalf("blob tail: code %d len %d", code, len(out))
	}
	os.WriteFile(dir+"/blob.log", bytes.Repeat([]byte{'y'}, 5<<20), 0o600)
	if out, _, code := runSh(t, LogTailScript("blob.log", 300)); code != 0 || len(out) != 262144 {
		t.Fatalf("plain blob tail: code %d len %d", code, len(out))
	}
	// A gzip-rotated copy is decompressed on the box, never served as bytes.
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	zw.Write([]byte("a\nb\nc\nd\n"))
	zw.Close()
	os.WriteFile(dir+"/old.log.2.gz", gz.Bytes(), 0o600)
	if out, _, code := runSh(t, LogTailScript("old.log.2.gz", 2)); code != 0 || out != "c\nd\n" {
		t.Fatalf("gz tail: code %d out %q", code, out)
	}
}

// runSh runs a probe-style script the way the SSH session does, on sh's
// stdin, and returns stdout, stderr and the exit code.
func runSh(t *testing.T, script string) (string, string, int) {
	t.Helper()
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(script)
	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return out.String(), errb.String(), code
}

// More processes than the list keeps: the kept prefix is reported as
// truncated, so a watched app beyond it is not judged absent (GPT audit
// 2026-10-22 finding 9).
func TestParseProcsTruncated(t *testing.T) {
	var sb strings.Builder
	for i := 1; i <= maxProcs+1; i++ {
		fmt.Fprintf(&sb, "%d 1 10 1 1 p%d\n", i, i)
	}
	procs, known, _, truncated := parseProcs(sb.String(), "", "", 0)
	if !known || !truncated || len(procs) != maxProcs {
		t.Fatalf("known %v truncated %v kept %d", known, truncated, len(procs))
	}
	if _, _, _, truncated := parseProcs("1 1 10 1 1 p1\n2 1 10 1 1 p2", "", "", 0); truncated {
		t.Fatal("a short list reported truncated")
	}
}

func TestParseProcsHostileAndMissing(t *testing.T) {
	procs, known, argsKnown, _ := parseProcs("", "", "", 0)
	if known || argsKnown || len(procs) != 0 {
		t.Fatalf("empty ps must be unknown, got %v %v", known, procs)
	}
	raw := "1 0 -5 nan -1 bad\n2 0 99999999999999 1e99 5 huge\nx y z\n3 0 10 1 1 ok"
	procs, known, argsKnown, _ = parseProcs(raw, "3 cmd with args\nnotanumber args\nend", "0\n3 100 100 5", 0) // CLK_TCK 0: stat ignored
	if !known || !argsKnown || len(procs) != 3 {
		t.Fatalf("procs: %+v", procs)
	}
	// A readable process list with a cut (empty) args listing: the
	// processes are known, their command lines are not.
	if _, known, argsKnown, _ := parseProcs("3 0 10 1 1 python3", "", "", 0); !known || argsKnown {
		t.Fatalf("args cut: known %v argsKnown %v", known, argsKnown)
	}
	if procs[0].RSS != 0 || procs[0].CPUSec != 0 || procs[0].ElapsedSec != 0 {
		t.Fatalf("negative/nan values must clamp to 0: %+v", procs[0])
	}
	if procs[1].RSS != 0 || procs[1].CPUSec != 0 {
		t.Fatalf("absurd values must clamp to 0: %+v", procs[1])
	}
	if procs[2].Args != "cmd with args" || procs[2].CPUSec != 1 || procs[2].StartTick != 0 {
		t.Fatalf("args / stat fallback: %+v", procs[2])
	}
	if st := parseProcStat("100\n3 100 50 7\n4 -1 0 7\n5 1 1\nx"); len(st) != 1 || st[3].cpuSec != 1.5 || st[3].start != 7 {
		t.Fatalf("proc stat: %+v", st)
	}
	for _, hz := range []string{"0.5", "1e-300", "NaN", "0", "-1", "1e9"} {
		if st := parseProcStat(hz + "\n3 100 50 7"); len(st) != 0 {
			t.Errorf("CLK_TCK %s must be rejected: %+v", hz, st)
		}
	}
	// The probe's whole tree is dropped: timeout wraps ps, so ps is a grandchild.
	tree := "777 1 100 0 1 sh\n778 777 100 0 1 timeout\n779 778 100 0 1 ps\n780 779 100 0 1 cat\n900 1 100 0 1 radarr"
	if procs, _, _, _ := parseProcs(tree, "", "", 777); len(procs) != 1 || procs[0].Comm != "radarr" {
		t.Fatalf("probe tree exclusion: %+v", procs)
	}
	if got := parseListenPIDs("80 1\n70000 2\nabc 3\n80 1\n81"); len(got) != 1 || len(got[80]) != 1 {
		t.Fatalf("listen pids: %v", got)
	}
}

func TestQuotaHumanized(t *testing.T) {
	text := `Disk quotas for user foo (uid 1001): 
     Filesystem   space   quota   limit   grace   files   quota   limit   grace
      /dev/md2   1659G   2000G   2500G           12345   1000k   1250k        
`
	v, ok := parseQuotaLine(text)
	if !ok {
		t.Fatal("no parse")
	}
	if v[0] != 1659<<30 || v[1] != 2000<<30 || v[3] != 12345 || v[4] != 1000000 || v[5] != 1250000 {
		t.Fatalf("%v", v)
	}
}

// The version call runs only when the binary's stat differs from the one
// the poller already has an answer for; a fake qbittorrent-nox records
// each invocation.
func TestQbtVersionSkippedWhileUnchanged(t *testing.T) {
	home := t.TempDir()
	os.Mkdir(home+"/bin", 0o700)
	fake := "#!/bin/sh\necho run >> \"$HOME/calls\"\necho 'qBittorrent v5.2.4'\n"
	os.WriteFile(home+"/bin/qbittorrent-nox", []byte(fake), 0o700)
	t.Setenv("HOME", home)
	st, err := os.Stat(home + "/bin/qbittorrent-nox")
	if err != nil {
		t.Fatal(err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no Stat_t")
	}
	seen := fmt.Sprintf("%d %d %d %d", st.Size(), st.ModTime().Unix(), sys.Ctim.Sec, sys.Ino)
	run := func(seenVal string) string {
		cmd := exec.Command("sh")
		cmd.Stdin = strings.NewReader("home=$HOME; PMSS_QBT_SEEN='" + seenVal + "'\n" + qbtVersionShell)
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(out))
	}
	if got := run(""); got != "qBittorrent v5.2.4" {
		t.Fatalf("first call: %q", got)
	}
	if got := run(seen); got != "" {
		t.Fatalf("unchanged binary must skip the call, got %q", got)
	}
	if got := run("1 2 3 4"); got != "qBittorrent v5.2.4" {
		t.Fatalf("changed binary: %q", got)
	}
	// A replacement that keeps size and mtime still changes ctime and inode.
	os.WriteFile(home+"/bin/qbittorrent-nox.new", []byte(fake), 0o700)
	os.Chtimes(home+"/bin/qbittorrent-nox.new", st.ModTime(), st.ModTime())
	os.Rename(home+"/bin/qbittorrent-nox.new", home+"/bin/qbittorrent-nox")
	if got := run(seen); got != "qBittorrent v5.2.4" {
		t.Fatalf("timestamp-preserving replacement must be noticed, got %q", got)
	}
	// A symlinked binary: the key describes the target, so replacing the
	// target under an unchanged link is noticed (fred review on PR 86).
	os.Rename(home+"/bin/qbittorrent-nox", home+"/bin/qbt-real")
	os.Symlink(home+"/bin/qbt-real", home+"/bin/qbittorrent-nox")
	lst, _ := os.Stat(home + "/bin/qbt-real")
	lsys := lst.Sys().(*syscall.Stat_t)
	linkSeen := fmt.Sprintf("%d %d %d %d", lst.Size(), lst.ModTime().Unix(), lsys.Ctim.Sec, lsys.Ino)
	if got := run(linkSeen); got != "" {
		t.Fatalf("unchanged target behind a link must skip, got %q", got)
	}
	os.WriteFile(home+"/bin/qbt-real.new", []byte(fake), 0o700)
	os.Rename(home+"/bin/qbt-real.new", home+"/bin/qbt-real")
	if got := run(linkSeen); got != "qBittorrent v5.2.4" {
		t.Fatalf("target replaced behind an unchanged link must be noticed, got %q", got)
	}
	calls, _ := os.ReadFile(home + "/calls")
	if strings.Count(string(calls), "run") != 4 {
		t.Fatalf("calls: %q", calls)
	}
	if s := ScriptWith("abc", "12 34'; rm -rf /", true); !strings.HasPrefix(s, "PMSS_TAG='abc'\nPMSS_QBT_SEEN='12 34   '\nPMSS_INVENTORY=1\n") {
		t.Fatalf("seen must be digits and spaces only: %q", s[:60])
	}
}

func TestShortCPU(t *testing.T) {
	cases := map[string]string{
		"Intel(R) N100": "N100",
		"Intel(R) Xeon(R) CPU E5-2680 v4 @ 2.40GHz": "Xeon E5-2680 v4",
		"AMD Ryzen 9 7950X 16-Core Processor":       "AMD Ryzen 9 7950X",
		"Intel(R) Core(TM) i7-14700K":               "i7-14700K",
		"AMD EPYC 7443P 24-Core Processor":          "AMD EPYC 7443P",
		"":                                          "",
	}
	for in, want := range cases {
		if got := ShortCPU(in); got != want {
			t.Errorf("ShortCPU(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseGPU(t *testing.T) {
	s := Sections([]byte("@@cpu_model\n Intel(R) N100\n@@dri\n/dev/dri/renderD128 rw 0x8086 0x46d1\n@@end\n"))
	h := parseHost(s)
	if h.CPUShort != "N100" || !h.GPU.Present || !h.GPU.Accessible || h.GPU.Vendor != "intel" || h.GPU.Device != "/dev/dri/renderD128" {
		t.Fatalf("%+v", h)
	}
}

func TestParseDu(t *testing.T) {
	rows := ParseDu([]byte("1024\tdata\n4096\twatch dir\n20\t.config\nbad\tx\n"))
	if len(rows) != 3 || rows[0].Name != "watch dir" || rows[2].Name != ".config" {
		t.Fatalf("%+v", rows)
	}
}

func TestTruncated(t *testing.T) {
	if _, err := Parse([]byte("@@uid\n1\n"), time.Now()); err == nil {
		t.Fatal("want error")
	}
}

func TestSectionsTaggedRejectsForgedHeaders(t *testing.T) {
	// A co-tenant's process argument lands inside a section; without the
	// tag it must not open a new section, and a duplicate must not overwrite.
	raw := "\n@@t0kuid\n1001\n@@qbt_conf\nWebUI\\Port=9999\n@@t0kuid\n6666\n@@t0kend\n"
	s := SectionsTagged([]byte(raw), "t0k")
	// The forged lines stay inside the uid section as content; the later
	// genuine-looking duplicate "@@t0kuid" must not overwrite the first.
	if !strings.HasPrefix(s["uid"], "1001") || strings.Contains(s["uid"], "6666") {
		t.Fatalf("uid = %q, want first occurrence kept", s["uid"])
	}
	if _, ok := s["qbt_conf"]; ok {
		t.Fatal("untagged header must not open a section")
	}
	if _, ok := s["end"]; !ok {
		t.Fatal("end missing")
	}
	if _, err := ParseTagged([]byte("\n@@end\n"), "t0k", time.Now()); err == nil {
		t.Fatal("untagged output must not parse as tagged")
	}
}

func TestScriptWithTag(t *testing.T) {
	tag := NewTag()
	if len(tag) != 24 || strings.Trim(tag, "0123456789abcdef") != "" {
		t.Fatalf("bad tag %q", tag)
	}
	src := ScriptWithTag(tag)
	if !strings.HasPrefix(src, "PMSS_TAG='"+tag+"'\nPMSS_QBT_SEEN=''\nPMSS_INVENTORY=1\n#!/bin/sh") {
		t.Fatalf("tag line missing: %q", src[:60])
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(src)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("script: %v", err)
	}
	snap, err := ParseTagged(out, tag, time.Now())
	if err != nil || snap.UID == 0 {
		t.Fatalf("tagged parse: %v %+v", err, snap)
	}
	if _, err := ParseTagged(out, "wrong", time.Now()); err == nil {
		t.Fatal("wrong tag must not parse")
	}
}

func TestPortOwner(t *testing.T) {
	// Mixed: ss-derived decimal lines and /proc-derived hex lines; a bogus line.
	listen := `d 37849 1001
h 1F90 1002
garbage line here
d 80 0`
	snap, err := Parse([]byte("@@uid\n1001\n@@listen\n"+listen+"\n@@end\n"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !snap.ListenersKnown || snap.Listeners[37849] != 1001 || snap.Listeners[8080] != 1002 || snap.Listeners[80] != 0 {
		t.Fatalf("listeners = %+v", snap.Listeners)
	}
	if ok, _ := snap.PortOwner(37849); !ok {
		t.Fatal("own port should be ok")
	}
	if ok, reason := snap.PortOwner(8080); ok || !strings.Contains(reason, "uid 1002") {
		t.Fatalf("foreign port must be refused: %v %q", ok, reason)
	}
	if ok, reason := snap.PortOwner(9999); ok || !strings.Contains(reason, "nothing is listening") {
		t.Fatalf("unbound port must be refused: %v %q", ok, reason)
	}
	// Unreadable /proc/net/tcp: sshd's listener is always there, so an empty
	// section means the probe could not read it; fail closed.
	snap2, _ := Parse([]byte("@@uid\n1001\n@@listen\n@@end\n"), time.Now())
	if ok, reason := snap2.PortOwner(8080); ok || snap2.ListenersKnown || !strings.Contains(reason, "could not read") {
		t.Fatalf("unknown listeners must refuse: %v %q", ok, reason)
	}
}

func TestTruncatedNamesLastSection(t *testing.T) {
	_, err := Parse([]byte("@@uid\n1\n@@quota\n"), time.Now())
	if err == nil || !strings.Contains(err.Error(), `after section "quota"`) {
		t.Fatalf("err = %v", err)
	}
}

// A box epoch far from the dashboard's clock is ignored in favour of the
// local time; time.Unix(1<<63-1) would not even encode as JSON.
func TestNowBounded(t *testing.T) {
	now := time.Date(2026, 10, 22, 0, 0, 0, 0, time.UTC)
	for _, ts := range []string{"9223372036854775807", "1", "4102444800"} { // max, 1970, 2100
		snap, err := Parse([]byte("@@now\n"+ts+"\n@@end\n"), now)
		if err != nil || !snap.At.Equal(now) {
			t.Errorf("now=%s: %v %v", ts, snap.At, err)
		}
	}
	snap, _ := Parse([]byte("@@now\n1761091200\n@@end\n"), now) // 2026-10-22, within a year
	if snap.At.Unix() != 1761091200 {
		t.Fatalf("sane epoch not taken: %v", snap.At)
	}
}

func TestParseTZ(t *testing.T) {
	for in, want := range map[string]int{"+0300": 3 * 3600, "-0500": -5 * 3600, "+0530": 5*3600 + 1800, "Z": 0, "": 0, "+9900": 0} {
		if _, got := time.Now().In(parseTZ(in)).Zone(); got != want {
			t.Errorf("%q: got %d want %d", in, got, want)
		}
	}
}

func TestListenersContestedAndUnknownUID(t *testing.T) {
	snap, _ := Parse([]byte("@@uid\n1001\n@@listen\nh 1F90 1001\nh 1F90 1002\nh 0016 0\n@@end\n"), time.Now())
	if ok, reason := snap.PortOwner(8080); ok || !strings.Contains(reason, "more than one account") {
		t.Fatalf("contested port must be refused: %v %q", ok, reason)
	}
	snap2, _ := Parse([]byte("@@listen\nh 1F90 0\n@@end\n"), time.Now())
	if ok, _ := snap2.PortOwner(8080); ok {
		t.Fatal("unknown own uid must refuse")
	}
	if _, err := Parse([]byte("@@uid\n1\n@@traffic\na:1:{s:3:\"raw\";a:1:{s:5:\"15min\";d:1.0E+308;}}\n@@end\n"), time.Now()); err != nil {
		t.Fatal(err)
	}
}

func TestFiniteTraffic(t *testing.T) {
	snap, err := Parse([]byte("@@uid\n1\n@@traffic\na:2:{s:3:\"raw\";a:2:{s:5:\"15min\";d:1.0E+308;s:5:\"month\";d:NAN;}s:5:\"daily\";a:1:{s:10:\"2026/10/16\";d:INF;}}\n@@end\n"), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ts := snap.Traffic.Out
	if ts.Min15MiB != 0 || ts.MonthMiB != 0 || ts.Daily["2026/10/16"] != 0 {
		t.Fatalf("non-finite values must become 0: %+v", ts)
	}
}

func TestCgroupMemorySplit(t *testing.T) {
	c := parseCgroup("mem_usage=15000000000\nmem_limit=15700000000\nmem_rss=165000000\nmem_cache=13900000000\noom_kill=2\n")
	if c.MemRSS != 165000000 || c.MemCache != 13900000000 || c.OOMKills != 2 || !c.OOMKnown || c.MemUsage != 15000000000 {
		t.Fatalf("%+v", c)
	}
	if c2 := parseCgroup("mem_usage=1\n"); c2.OOMKnown {
		t.Fatal("oom counter absent must not count as known")
	}
}

func TestListenersAddressAware(t *testing.T) {
	// Ours on v4-any; a co-tenant on 127.0.0.2 and ::1 with the same port
	// must not make it contested; a co-tenant on 0.0.0.0 must.
	raw := "@@uid\n1001\n@@listen\nh 1F90 1001 00000000\nh 1F90 1002 0200007F\nh 1F90 1002 00000000000000000000000001000000\nh 0016 0 00000000\nh 1F91 1001 0A00000A\n@@end\n"
	snap, _ := Parse([]byte(raw), time.Now())
	if ok, reason := snap.PortOwner(8080); !ok {
		t.Fatalf("non-reachable co-tenant sockets must not contest: %q", reason)
	}
	if ok, reason := snap.PortOwner(8081); ok || !strings.Contains(reason, "nothing is listening") {
		t.Fatalf("our LAN-only socket is not reachable from 127.0.0.1: %v %q", ok, reason)
	}
	raw2 := "@@uid\n1001\n@@listen\nh 1F90 1001 0100007F\nh 1F90 1002 00000000000000000000000000000000\n@@end\n"
	snap2, _ := Parse([]byte(raw2), time.Now())
	if ok, reason := snap2.PortOwner(8080); ok || !strings.Contains(reason, "more than one") {
		t.Fatalf("v6-any co-tenant socket is reachable and must contest: %v %q", ok, reason)
	}
}

func TestNonFiniteEverywhere(t *testing.T) {
	raw := "@@uid\n1\n@@uptime\nnan 1\n@@loadavg\ninf 0.1 0.2 1/2 3\n@@traffic_limit\nNaN\n@@pmss_user\n{\"quota\":\"NaN\",\"ramMiB\":\"Inf\",\"trafficCapMbit\":1e308}\n@@traffic\na:2:{s:3:\"raw\";a:1:{s:5:\"month\";d:1.0E+14;}s:5:\"daily\";a:1:{s:10:\"2026/10/16\";s:3:\"NaN\";}}\n@@end\n"
	snap, err := Parse([]byte(raw), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if snap.Host.UptimeSec != 0 || snap.Host.Load1 != 0 || snap.Traffic.LimitGiB != 0 || snap.Account.QuotaGiB != 0 || snap.Account.RAMMiB != 0 || snap.Account.TrafficCapMbit != 0 || snap.Traffic.Out.MonthMiB != 0 || snap.Traffic.Out.Daily["2026/10/16"] != 0 {
		t.Fatalf("non-finite or absurd values survived: host=%+v acct=%+v traffic=%+v", snap.Host, snap.Account, snap.Traffic.Out)
	}
}

func TestListenersViaSS(t *testing.T) {
	// The ss fallback: our socket carries our uid, anything ss could not
	// attribute to us is "x" and must be refused, and a port with both is
	// contested. Textual addresses decide reachability like the hex ones.
	raw := "@@uid\n1001\n@@listen\nvia ss\nd 8080 1001 0.0.0.0\nd 22 x 0.0.0.0\nd 22 x [::]\nd 8081 x 127.0.0.1\nd 8082 1001 [::]\nd 8082 x 127.0.0.2\nd 8083 1001 127.0.0.1\nd 8083 x [::]\nd 8084 1001 10.0.0.5\nd 8085 1001 [::1]\n@@end\n"
	snap, err := Parse([]byte(raw), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !snap.ListenersKnown {
		t.Fatal("ss lines must count as a readable table")
	}
	if ok, reason := snap.PortOwner(8080); !ok {
		t.Fatalf("own port via ss: %q", reason)
	}
	if ok, reason := snap.PortOwner(22); ok || !strings.Contains(reason, "not this account") {
		t.Fatalf("unattributed listener must be refused: %v %q", ok, reason)
	}
	if ok, reason := snap.PortOwner(8081); ok || !strings.Contains(reason, "not this account") {
		t.Fatalf("foreign loopback listener must be refused: %v %q", ok, reason)
	}
	if ok, reason := snap.PortOwner(8082); !ok {
		t.Fatalf("a co-tenant on 127.0.0.2 is not reachable and must not contest: %q", reason)
	}
	if ok, reason := snap.PortOwner(8083); ok || !strings.Contains(reason, "more than one") {
		t.Fatalf("ours on 127.0.0.1 plus a foreign v6-any listener is contested: %v %q", ok, reason)
	}
	for _, port := range []int{8084, 8085} {
		if ok, reason := snap.PortOwner(port); ok || !strings.Contains(reason, "nothing is listening") {
			t.Fatalf("port %d is not reachable from 127.0.0.1: %v %q", port, ok, reason)
		}
	}
	// A negative or non-numeric uid token other than x is not a listener.
	if ls, known := parseListeners("d 8080 -5\nd 8081 abc\n"); known || len(ls) != 0 {
		t.Fatalf("bad uid tokens must be ignored: %v %v", ls, known)
	}
}

// TestListenViaSSLocal runs the real ss branch of the listen shell against
// this machine: a socket this test opens must come back as ours on
// 127.0.0.1, and the columns must split as the awk expects.
func TestListenViaSSLocal(t *testing.T) {
	if _, err := exec.LookPath("ss"); err != nil {
		t.Skip("no ss on this machine")
	}
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this machine")
	}
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Skip("cannot listen:", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	cmd := exec.Command("sh", "-c", "uid=$(id -u); "+listenViaSS)
	raw, err := cmd.Output()
	if err != nil {
		t.Skip("ss branch did not run here:", err)
	}
	want := fmt.Sprintf("d %d %d 127.0.0.1", port, os.Getuid())
	lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
	found := false
	for _, l := range lines {
		f := strings.Fields(l)
		if len(f) != 4 || f[0] != "d" {
			t.Fatalf("unexpected ss line %q", l)
		}
		if _, _, ok := listenerPortUID(f); !ok {
			t.Fatalf("unparsable ss line %q", l)
		}
		if l == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("own listener %q not in ss output:\n%s", want, raw)
	}
}

func TestPortOwnerNow(t *testing.T) {
	out := []byte("uid 1001\nh 1F90 1001 0100007F\nh 0016 0 00000000\nend\n")
	if ok, reason := PortOwnerNow(out, 8080); !ok {
		t.Fatalf("own port: %q", reason)
	}
	if ok, _ := PortOwnerNow(out, 22); ok {
		t.Fatal("root's port must be refused")
	}
	if ok, reason := PortOwnerNow([]byte("garbage\nend\n"), 8080); ok || !strings.Contains(reason, "uid") {
		t.Fatalf("bad uid line: %v %q", ok, reason)
	}
	if ok, _ := PortOwnerNow([]byte("uid 1001\nend\n"), 8080); ok {
		t.Fatal("empty table must refuse")
	}
	if ok, _ := PortOwnerNow([]byte("uid 1001\nvia ss\nend\n"), 8080); ok {
		t.Fatal("ss that listed nothing must refuse")
	}
	if ok, reason := PortOwnerNow([]byte("uid 1001\nvia ss\nd 8080 1001 0.0.0.0\nd 22 x 0.0.0.0\nend\n"), 8080); !ok {
		t.Fatalf("own port via ss: %q", reason)
	}
	if ok, _ := PortOwnerNow([]byte("uid 1001\nvia ss\nd 8080 1001 0.0.0.0\nd 22 x 0.0.0.0\nend\n"), 22); ok {
		t.Fatal("unattributed port via ss must refuse")
	}
	// Cut off before the marker: the co-tenant's line may be the missing one.
	if ok, reason := PortOwnerNow([]byte("uid 1001\nh 1F90 1001 0100007F\n"), 8080); ok || !strings.Contains(reason, "incomplete") {
		t.Fatalf("truncated table: %v %q", ok, reason)
	}
	if ok, _ := PortOwnerNow(nil, 8080); ok {
		t.Fatal("no output must refuse")
	}
	// The live script must produce exactly this shape locally.
	if _, err := exec.LookPath("sh"); err == nil {
		cmd := exec.Command("sh")
		cmd.Stdin = strings.NewReader(ListenScript)
		raw, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(strings.TrimSpace(string(raw)), "\n")
		if _, err := strconv.Atoi(strings.TrimPrefix(lines[0], "uid ")); err != nil || !strings.HasPrefix(lines[0], "uid ") {
			t.Fatalf("first line should be the uid: %q", lines[0])
		}
		if lines[len(lines)-1] != "end" {
			t.Fatalf("last line should be the marker: %q", lines[len(lines)-1])
		}
	}
}

// The UI builds https links from the hostname, so only DNS-shaped names pass.
func TestHostnameField(t *testing.T) {
	for in, want := range map[string]string{
		"odin.pulsedmedia.com": "odin.pulsedmedia.com", "lt5-1-57.example": "lt5-1-57.example", "box": "box",
		"evil.example/#": "", "x@evil.example": "", "a b": "", `a"b`: "", "": "", strings.Repeat("a", 254): "", ".x": "", "-x": "",
	} {
		if got := hostnameField(in); got != want {
			t.Errorf("hostnameField(%q) = %q, want %q", in, got, want)
		}
	}
}

// ~/.qbittorrentEnable is PMSS's marker for a panel-managed qBittorrent; a
// missing section (an older probe) reads as not managed.
// A ps cut by its timeout prints nothing, so the parser reports the list
// as unknown rather than as a complete listing missing its tail.
func TestProcsShellDropsTimedOutListing(t *testing.T) {
	bin := t.TempDir()
	os.WriteFile(bin+"/ps", []byte("#!/bin/sh\necho '1 0 1 0 1 partial'\nexit 124\n"), 0o700)
	os.WriteFile(bin+"/timeout", []byte("#!/bin/sh\nshift\nexec \"$@\"\n"), 0o700)
	run := func() string {
		cmd := exec.Command("sh")
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		cmd.Stdin = strings.NewReader("uid=1\n" + procsShell)
		out, _ := cmd.Output()
		return string(out)
	}
	if out := run(); out != "" {
		t.Fatalf("timed-out ps must print nothing, got %q", out)
	}
	os.WriteFile(bin+"/ps", []byte("#!/bin/sh\necho '1 0 1 0 1 whole'\n"), 0o700)
	if out := run(); out != "1 0 1 0 1 whole\n" {
		t.Fatalf("completed ps: %q", out)
	}
	// Same for the command lines.
	runArgs := func() string {
		cmd := exec.Command("sh")
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
		cmd.Stdin = strings.NewReader("uid=1\n" + procArgsShell)
		out, _ := cmd.Output()
		return string(out)
	}
	// The command lines are streamed (no shell variable holds them all)
	// and a completed ps appends the "end" marker; a cut listing lacks it
	// and the parser ignores it whole.
	os.WriteFile(bin+"/ps", []byte("#!/bin/sh\necho '1 python3 bot.py'\nexit 124\n"), 0o700)
	if out := runArgs(); out != "1 python3 bot.py\n" {
		t.Fatalf("timed-out args ps must print no marker, got %q", out)
	}
	if _, _, argsKnown, _ := parseProcs("1 0 1 0 1 python3", "1 python3 bot.py\n", "", 0); argsKnown {
		t.Fatal("a listing without the marker must not count as known")
	}
	os.WriteFile(bin+"/ps", []byte("#!/bin/sh\necho '1 python3 bot.py'\necho \"2 $(head -c 600 /dev/zero | tr '\\0' x)\"\n"), 0o700)
	out := runArgs()
	if !strings.HasPrefix(out, "1 python3 bot.py\n2 xxxx") || !strings.HasSuffix(out, "\nend\n") || len(strings.Split(strings.TrimSpace(out), "\n")[1]) != 400 {
		t.Fatalf("completed args ps: %q", out)
	}
	procs, _, argsKnown, _ := parseProcs("1 0 1 0 1 python3\n2 0 1 0 1 x\n3 0 1 0 1 end", out+"3 end\nend\n", "", 0)
	if !argsKnown || len(procs) != 3 || procs[0].Args != "python3 bot.py" || len(procs[1].Args) != 398 || procs[2].Args != "end" {
		t.Fatalf("parsed args: known=%v %+v", argsKnown, procs)
	}
	if _, _, argsKnown, _ := parseProcs("1 0 1 0 1 sh", "end\n", "", 0); !argsKnown {
		t.Fatal("a marker alone is a complete (empty) listing")
	}
}

// With PMSS_INVENTORY=0 the probe skips the inventory sections and says so;
// the poller carries the last inventory over such a poll.
func TestInventoryGated(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	run := func(inventory bool) *Snapshot {
		cmd := exec.Command("sh")
		cmd.Stdin = strings.NewReader(ScriptWith("t1", "", inventory))
		out, err := cmd.Output()
		if err != nil {
			t.Fatal(err)
		}
		snap, err := ParseTagged(out, "t1", time.Now())
		if err != nil {
			t.Fatal(err)
		}
		return snap
	}
	full := run(true)
	if !full.Inventory || full.Host.Kernel == "" || full.Host.NCPU == 0 || !full.LogsKnown {
		t.Fatalf("full probe: inventory=%v kernel=%q ncpu=%d logs=%v", full.Inventory, full.Host.Kernel, full.Host.NCPU, full.LogsKnown)
	}
	// The PMSS-managed marker is a safety check for install/autostart: it
	// is read every poll, inventory or not.
	home := t.TempDir()
	os.WriteFile(home+"/.qbittorrentEnable", nil, 0o600)
	t.Setenv("HOME", home)
	fast := run(false)
	if fast.Inventory || fast.Host.Kernel != "" || fast.Host.NCPU != 0 || fast.LogsKnown || fast.Qbt.BinaryStat != "" {
		t.Fatalf("fast probe ran inventory sections: %+v", fast.Host)
	}
	if !fast.Qbt.PMSSManaged {
		t.Fatal("the PMSS-managed marker must be read on a fast poll")
	}
	prevManaged := &Snapshot{Inventory: true, Qbt: QbtInfo{PMSSManaged: false}}
	fast.CarryInventory(prevManaged)
	if !fast.Qbt.PMSSManaged {
		t.Fatal("carry must not overwrite the live marker")
	}
	if fast.Host.UptimeSec <= 0 || fast.Host.MemTotal <= 0 || !fast.ProcsKnown {
		t.Fatalf("fast probe must still measure: %+v procs=%v", fast.Host, fast.ProcsKnown)
	}
	kept := full.InventoryOnly()
	if len(kept.Procs) != 0 || !kept.Inventory || kept.Host.Kernel != full.Host.Kernel || kept.Host.UptimeSec != 0 {
		t.Fatalf("InventoryOnly: %+v", kept.Host)
	}
	fast.CarryInventory(kept)
	if fast.Inventory || fast.Host.Kernel != full.Host.Kernel || fast.Host.NCPU != full.Host.NCPU || fast.LogsKnown != full.LogsKnown || len(fast.Logs) != len(full.Logs) {
		t.Fatalf("carry: kernel=%q ncpu=%d logs=%v", fast.Host.Kernel, fast.Host.NCPU, fast.LogsKnown)
	}
	// A probe that ran the sections itself is never overwritten by a carry.
	prev := &Snapshot{Inventory: true, Host: HostInfo{Kernel: "old"}}
	full.CarryInventory(prev)
	if full.Host.Kernel == "old" {
		t.Fatal("carry overwrote a fresh inventory")
	}
	fast2 := &Snapshot{}
	fast2.CarryInventory(nil)
	if fast2.Host.Kernel != "" {
		t.Fatal("carry from nothing")
	}
}

// The cached binary stat is echoed back to the box every poll: only a
// stat-shaped line is kept (round 20).
func TestParseQbtBinaryStatShape(t *testing.T) {
	for _, raw := range []string{strings.Repeat("1 ", 50), "1 2 3", "a b c d", "1 2 3 4 5", ""} {
		if q := parseQbt(map[string]string{"qbt_bin_stat": raw}); q.BinaryStat != "" {
			t.Errorf("%q accepted as %q", raw[:min(20, len(raw))], q.BinaryStat)
		}
	}
	if q := parseQbt(map[string]string{"qbt_bin_stat": " 30515200 1760500000 1760500001 4242\n"}); q.BinaryStat != "30515200 1760500000 1760500001 4242" {
		t.Fatalf("stat: %q", q.BinaryStat)
	}
}

// A helper keyed on $USER is flagged only when the helper is there at all.
func TestParseQbtStaleHelper(t *testing.T) {
	q := parseQbt(map[string]string{"qbt_start_helper": "1\n", "qbt_start_helper_stale": "1\n"})
	if !q.StartHelper || !q.StartHelperStale {
		t.Fatalf("stale helper: %+v", q)
	}
	q = parseQbt(map[string]string{"qbt_start_helper": "1\n", "qbt_start_helper_stale": "0\n"})
	if !q.StartHelper || q.StartHelperStale {
		t.Fatalf("fixed helper: %+v", q)
	}
	q = parseQbt(map[string]string{"qbt_start_helper": "0\n", "qbt_start_helper_stale": "1\n"})
	if q.StartHelper || q.StartHelperStale {
		t.Fatalf("no helper: %+v", q)
	}
	// Carried across a non-inventory poll like the rest of the inventory.
	prev := Snapshot{Qbt: QbtInfo{StartHelper: true, StartHelperStale: true}}
	cur := Snapshot{}
	cur.CarryInventory(&prev)
	if !cur.Qbt.StartHelperStale {
		t.Fatal("stale flag not carried")
	}
}

func TestParseQbtPMSSManaged(t *testing.T) {
	for in, want := range map[string]bool{"1": true, "1\n": true, "0": false, "": false} {
		if got := parseQbt(map[string]string{"qbt_pmss": in}).PMSSManaged; got != want {
			t.Errorf("qbt_pmss %q: PMSSManaged = %v, want %v", in, got, want)
		}
	}
	if parseQbt(map[string]string{}).PMSSManaged {
		t.Error("no qbt_pmss section must not read as managed")
	}
}

// bounded() skips a command whose previous run still holds its lock (a
// df stuck in the kernel on a hung filesystem), instead of adding another
// stuck process every poll; without a holder it runs the command.
func TestBoundedSkipsWhileLocked(t *testing.T) {
	if _, err := exec.LookPath("flock"); err != nil {
		t.Skip("no flock")
	}
	scratch := t.TempDir()
	prelude := "scratch='" + scratch + "'\n" + boundedShell
	out, _, code := runSh(t, prelude+"bounded 2 echo hello\n")
	if code != 0 || out != "hello\n" {
		t.Fatalf("unlocked: code %d out %q", code, out)
	}
	// Hold the lock the way a stuck df would, then run bounded again.
	holder := exec.Command("flock", scratch+"/.pmss-probe-lock.echo", "sleep", "20")
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { holder.Process.Kill(); holder.Wait() }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		if l, err := exec.Command("flock", "-n", scratch+"/.pmss-probe-lock.echo", "true").CombinedOutput(); err != nil || time.Now().After(deadline) {
			_ = l
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	start := time.Now()
	out, _, code = runSh(t, prelude+"bounded 3 echo hello\n")
	if code != 0 || out != "" {
		t.Fatalf("locked: code %d out %q (must skip, not run)", code, out)
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("locked run must return at once, took %v", time.Since(start))
	}
}

// EstablishedScript, run locally against a real connection: our client
// socket pairs with an accepting socket of our uid, so the check passes.
// Crafted tables cover the refusals and the two fallback decisions.
func TestEstablishedOwner(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close()
		}
	}()
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	out, stderr, code := runSh(t, EstablishedScript(port))
	if code != 0 {
		t.Fatalf("script: code %d err %q", code, stderr)
	}
	ok, known, reason := EstablishedOwner([]byte(out), port)
	if !known {
		t.Skipf("non-listening sockets not visible here: %q", out)
	}
	if !ok {
		t.Fatalf("own connection refused: %s\n%s", reason, out)
	}

	uid := os.Getuid()
	mk := func(rows string) []byte { return []byte(fmt.Sprintf("uid %d\nbase hex\n%send\n", uid, rows)) }
	own := fmt.Sprintf("c BEEF %d 01 0100007F\ns BEEF %d 01 0100007F\n", uid, uid)
	if ok, known, reason := EstablishedOwner(mk(own), port); !ok || !known {
		t.Fatalf("own connection: %v %v %s", ok, known, reason)
	}
	// The acceptor belongs to another account: refuse, whatever its state
	// (a half-closed acceptor still reads).
	for _, state := range []string{"01", "04", "05", "08"} {
		if ok, _, reason := EstablishedOwner(mk(fmt.Sprintf("c BEEF %d 01 0100007F\ns BEEF %d %s 0100007F\n", uid, uid+1, state)), port); ok || !strings.Contains(reason, "another account") {
			t.Fatalf("foreign acceptor in state %s: %v %s", state, ok, reason)
		}
	}
	// No acceptor visible for our connection: refuse.
	if ok, _, reason := EstablishedOwner(mk(fmt.Sprintf("c BEEF %d 01 0100007F\n", uid)), port); ok || !strings.Contains(reason, "not visible") {
		t.Fatalf("missing acceptor: %v %s", ok, reason)
	}
	// Another tenant's own connection to the same port, and our own
	// connection to the port on another address, are not this connection.
	noise := fmt.Sprintf("c CAFE %d 01 0100007F\ns CAFE %d 01 0100007F\nc F00D %d 01 0A0B0C0D\ns F00D %d 01 0A0B0C0D\n", uid+1, uid+1, uid, uid+2)
	if ok, _, reason := EstablishedOwner(mk(own+noise), port); !ok {
		t.Fatalf("unrelated connections must not refuse: %s", reason)
	}
	// Our connection does not appear at all (table readable): refuse.
	if ok, _, reason := EstablishedOwner(mk(noise), port); ok || !strings.Contains(reason, "not in the box's socket table") {
		t.Fatalf("absent connection: %v %s", ok, reason)
	}
	// ss fallback: decimal ports, an unattributable acceptor refuses.
	dec := []byte(fmt.Sprintf("uid %d\nbase dec\nc 51234 %d ESTAB 127.0.0.1\ns 51234 x ESTAB 127.0.0.1\nend\n", uid, uid))
	if ok, _, _ := EstablishedOwner(dec, port); ok {
		t.Fatal("an unattributable acceptor must refuse")
	}
	dec = []byte(fmt.Sprintf("uid %d\nbase dec\nc 51234 %d ESTAB 127.0.0.1\ns 51234 %d FIN-WAIT-2 127.0.0.1\nend\n", uid, uid, uid))
	if ok, _, reason := EstablishedOwner(dec, port); !ok {
		t.Fatalf("own acceptor via ss: %s", reason)
	}
	// Policy fallback versus failure.
	if _, known, _ := EstablishedOwner([]byte(fmt.Sprintf("uid %d\nprivacy\nend\n", uid)), port); known {
		t.Fatal("privacy is the one unknown")
	}
	if ok, known, reason := EstablishedOwner([]byte(fmt.Sprintf("uid %d\nfailed 255\nend\n", uid)), port); ok || !known || !strings.Contains(reason, "could not inspect") {
		t.Fatalf("failed inspection must refuse: %v %v %s", ok, known, reason)
	}
	if ok, known, _ := EstablishedOwner([]byte(fmt.Sprintf("uid %d\nbase hex\n%s", uid, own)), port); ok || !known {
		t.Fatal("a cut-off table must refuse")
	}
	if ok, _, _ := EstablishedOwner([]byte("base hex\n"+own+"end\n"), port); ok {
		t.Fatal("no uid must refuse")
	}
}

// The script's decisions when /proc/net/tcp is unreadable: a failing ss is
// a failure (refuse); a silent ss is the privacy fallback only when the
// policy shows itself, by its marker or by its signature (the table
// present but denied, ss still listing listeners), and a failure
// otherwise; a working ss yields decimal rows.
func TestEstablishedScriptFallbacks(t *testing.T) {
	bin := t.TempDir()
	const absent, denied, readable = "no-such-table", "denied-table", "readable-table"
	os.WriteFile(bin+"/"+denied, nil, 0)
	os.WriteFile(bin+"/"+readable, nil, 0o600)
	run := func(ss string, marker bool, table string) string {
		os.WriteFile(bin+"/ss", []byte(ss), 0o700)
		m := bin + "/absent"
		if marker {
			m = bin + "/marker"
			os.WriteFile(m, nil, 0o600)
		}
		cmd := exec.Command("sh")
		cmd.Stdin = strings.NewReader(EstablishedScript(8089))
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "PMSS_TCP_TABLES="+bin+"/"+table, "PMSS_PRIVACY_MARKER="+m)
		out, _ := cmd.Output()
		return string(out)
	}
	// ss that lists listeners (-l) but no other socket, as the privacy
	// filter leaves it; and one that lists nothing at all.
	silentButListeners := "#!/bin/sh\ncase \"$*\" in *l*) echo 'LISTEN 0 128 0.0.0.0:22 0.0.0.0:* users:((\"sshd\",pid=1,fd=3))';; esac\nexit 0\n"
	silent := "#!/bin/sh\nexit 0\n"
	if out := run("#!/bin/sh\nexit 255\n", false, denied); !strings.Contains(out, "failed 255") {
		t.Fatalf("ss failing must report a failure: %q", out)
	}
	if out := run(silent, false, absent); !strings.Contains(out, "failed 0") {
		t.Fatalf("ss printing nothing, no marker, no table at all: a failure: %q", out)
	}
	if out := run(silent, true, absent); !strings.Contains(out, "privacy") || strings.Contains(out, "failed") {
		t.Fatalf("ss printing nothing with the marker is the privacy fallback: %q", out)
	}
	if os.Getuid() != 0 { // root reads a mode-0 file, so the signature cannot be staged as root
		// The marker out of reach (PMSS 2026-10-22 made /etc/seedbox
		// root-only on some hosts) but the signature complete: privacy.
		if out := run(silentButListeners, false, denied); !strings.Contains(out, "privacy") || strings.Contains(out, "failed") {
			t.Fatalf("denied table + listeners visible + no other socket is the privacy policy: %q", out)
		}
		// Signature incomplete: ss lists nothing at all (broken), or the
		// table is readable (so its emptiness was real and ss should have
		// had rows), or the table does not exist: all failures.
		if out := run(silent, false, denied); !strings.Contains(out, "failed 0") {
			t.Fatalf("ss listing no listeners either is a failure: %q", out)
		}
		if out := run(silentButListeners, false, readable); !strings.Contains(out, "failed 0") {
			t.Fatalf("a readable table is not the privacy policy: %q", out)
		}
		if out := run(silentButListeners, false, absent); !strings.Contains(out, "failed 0") {
			t.Fatalf("no table at all is not the privacy policy: %q", out)
		}
	}
	fake := "#!/bin/sh\necho 'ESTAB 0 0 127.0.0.1:41000 127.0.0.1:8089 users:((\"sshd\",pid=1,fd=9))'\necho 'FIN-WAIT-2 0 0 127.0.0.1:8089 127.0.0.1:41000'\n"
	out := run(fake, false, denied)
	if !strings.Contains(out, "base dec") || !strings.Contains(out, "c 41000 "+strconv.Itoa(os.Getuid())+" ESTAB 127.0.0.1") || !strings.Contains(out, "s 41000 x FIN-WAIT-2 127.0.0.1") {
		t.Fatalf("ss rows: %q", out)
	}
	if ok, known, reason := EstablishedOwner([]byte(out), 8089); ok || !known || !strings.Contains(reason, "another account") {
		t.Fatalf("half-closed foreign acceptor via ss must refuse: %v %v %s", ok, known, reason)
	}
}

// Without flock, bounded() skips the command instead of running it
// unguarded: a stuck earlier run could not be told from none (GPT audit
// 2026-10-22 finding 12).
func TestBoundedSkipsWithoutFlock(t *testing.T) {
	bin := t.TempDir()
	for _, tool := range []string{"sh", "mktemp", "basename", "rm", "cat", "echo", "kill", "sleep", "true"} {
		if p, err := exec.LookPath(tool); err == nil {
			_ = os.Symlink(p, filepath.Join(bin, tool))
		}
	}
	scratch := t.TempDir()
	cmd := exec.Command(filepath.Join(bin, "sh"))
	cmd.Env = []string{"PATH=" + bin}
	cmd.Stdin = strings.NewReader("scratch='" + scratch + "'\n" + boundedShell + "bounded 2 echo hello\necho rc=$?\n")
	out, err := cmd.CombinedOutput()
	if err != nil || string(out) != "rc=0\n" {
		t.Fatalf("without flock: %q %v (want the command skipped and status 0)", out, err)
	}
}

func TestParseStat(t *testing.T) {
	// pm5 on 2026-10-22: idle 1%, iowait 38%; the two guest fields are ignored.
	tk := parseStat("cpu  39752744 922511 72506445 2609519 82162714 0 19085988 0 5 7\n")
	if !tk.Known || tk.Idle != 2609519 || tk.IOWait != 82162714 || tk.Steal != 0 {
		t.Fatalf("ticks = %+v", tk)
	}
	if want := uint64(39752744 + 922511 + 72506445 + 2609519 + 82162714 + 19085988); tk.Total != want || tk.Busy != want-2609519-82162714 {
		t.Fatalf("total %d busy %d, want %d / %d", tk.Total, tk.Busy, want, want-2609519-82162714)
	}
	if tk := parseStat("cpu 10 0 20 30"); !tk.Known || tk.Total != 60 || tk.Busy != 30 || tk.Steal != 0 {
		t.Fatalf("four-field kernel: %+v", tk) // user nice system idle is the oldest shape
	}
	for _, raw := range []string{"", "cpu 10 0 20", "cpu0 10 0 20 30 40", "cpu 10 x 20 30", "cpu 10 0 20 -3", "cpu 99999999999999999999 0 0 0", "intr 5 6 7 8"} {
		if tk := parseStat(raw); tk.Known {
			t.Errorf("%q parsed as %+v", raw, tk)
		}
	}
}

func TestParseLiveRings(t *testing.T) {
	// Shapes as written on the fleet on 2026-10-22; cs arrives with a later
	// PMSS release, so one node slot lacks it. The user ring lacks one slot.
	node := `{"v":1,"samples":[{"t":1792696861,"dt":300,"cb":1300.5,"ct":2472,"mu":8e9,"mt":4.3e10,"tx":3.8e9,"rx":5.4e8},
	 {"t":1792697161,"dt":299,"cb":1307.82,"ct":2472.48,"mu":8539582464,"mt":43358269440,"tx":3866847232,"rx":549797231,"cs":286.81},
	 {"t":1792697161,"dt":299,"cb":1,"ct":2,"mu":1,"mt":1,"tx":1,"rx":1},
	 {"t":-5,"dt":299,"cb":1,"ct":2,"mu":1,"mt":1,"tx":1,"rx":1},
	 {"t":1792696561,"dt":300,"cb":"x","ct":2,"mu":1,"mt":1,"tx":1,"rx":1},
	 {"t":1792696261,"dt":300,"cb":-1,"ct":2,"mu":1,"mt":1,"tx":1,"rx":1},
	 {"t":1792695961,"dt":300,"cb":1,"ct":0,"mu":1,"mt":1,"tx":1,"rx":1}]}`
	user := `{"v":1,"samples":[{"t":1792697161,"cpu":6.72,"mem":521621504,"tx":300469,"rx":333001},{"t":1792690000,"cpu":1e400,"mem":1,"tx":1,"rx":1}]}`
	now := time.Unix(1792697161+600, 0)
	r, known := parseLiveRings(node, user, now)
	if !known || len(r.Node) != 2 || len(r.User) != 1 {
		t.Fatalf("known=%v node=%d user=%d", known, len(r.Node), len(r.User))
	}
	if n := r.Node[1]; n.T != 1792697161 || n.CB != 1307.82 || !n.HasCS || n.CS != 286.81 || r.Node[0].HasCS {
		t.Fatalf("the first of two equal-time slots wins and keeps cs: %+v / %+v", r.Node[0], r.Node[1])
	}
	if u := r.User[0]; u.CPU != 6.72 || u.Mem != 521621504 {
		t.Fatalf("user = %+v", u)
	}
	if r, known := parseLiveRings(node, "", now); !known || len(r.User) != 0 {
		t.Fatal("the node ring alone is enough; the user ring is optional")
	}
	for _, raw := range []string{"", "{", `{"samples":{}}`, `{"samples":[]}`, `[1,2]`} {
		if _, known := parseLiveRings(raw, user, now); known {
			t.Errorf("%q counted as known", raw)
		}
	}
	// A forged ring longer than PMSS ever writes keeps only its newest slots.
	var sb strings.Builder
	sb.WriteString(`{"samples":[`)
	for i := 0; i < 400; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `{"t":%d,"dt":300,"cb":1,"ct":2,"mu":1,"mt":2,"tx":0,"rx":0}`, 1000+i*300)
	}
	sb.WriteString(`]}`)
	if r, _ := parseLiveRings(sb.String(), "", time.Unix(1000+400*300, 0)); len(r.Node) != liveSlots || r.Node[0].T != 1000+112*300 {
		t.Fatalf("trimmed to %d from %d", len(r.Node), 400)
	}
	if _, known := parseLiveRings(`{"pad":"`+strings.Repeat("x", liveMaxBytes)+`",`+node[1:], "", now); known {
		t.Fatal("an oversized ring must be refused")
	}
	// Milliseconds where seconds belong, a slot from the far past and one a
	// week ahead are dropped: a time outside RFC 3339's range would make the
	// whole state unencodable.
	bad := `{"samples":[{"t":1792697161000,"dt":300,"cb":1,"ct":2,"mu":1,"mt":2,"tx":0,"rx":0},{"t":1000,"dt":300,"cb":1,"ct":2,"mu":1,"mt":2,"tx":0,"rx":0},{"t":1793301961,"dt":300,"cb":1,"ct":2,"mu":1,"mt":2,"tx":0,"rx":0}]}`
	if _, known := parseLiveRings(bad, `{"samples":[{"t":1792697161000,"cpu":1,"mem":1,"tx":0,"rx":0}]}`, now); known {
		t.Fatal("out-of-range slot times must be dropped")
	}
}

// Through the section parser: the model name's GHz is the clock when the
// box exposes no base frequency, and the base frequency wins when it does.
func TestParseHostClock(t *testing.T) {
	h := parseHost(Sections([]byte("@@cpu_model\n Intel(R) Xeon(R) CPU E5-2630 0 @ 2.30GHz\n@@cpu_mhz\n\n@@end\n")))
	if h.CPUMHz != 2300 || h.CPUModel != "Intel(R) Xeon(R) CPU E5-2630 0 @ 2.30GHz" {
		t.Fatalf("model fallback: %+v", h)
	}
	h = parseHost(Sections([]byte("@@cpu_model\n Intel(R) Xeon(R) CPU E5-2630 0 @ 2.30GHz\n@@cpu_mhz\n2100\n@@end\n")))
	if h.CPUMHz != 2100 {
		t.Fatalf("base frequency should win: %+v", h)
	}
	if h = parseHost(Sections([]byte("@@cpu_model\n Intel(R) N100\n@@end\n"))); h.CPUMHz != 0 {
		t.Fatalf("unknown clock: %+v", h)
	}
}

func TestNominalMHz(t *testing.T) {
	for _, c := range []struct {
		base, model string
		want        int
	}{
		{" 2100\n", "Intel(R) Core(TM) i7-12700", 2100},
		{"", "Intel(R) Xeon(R) CPU E5-2630 0 @ 2.30GHz", 2300},
		{"", "Intel(R) Xeon(R) CPU L5640 @ 2.27GHz", 2270},
		{"", "AMD EPYC 7551P 32-Core Processor", 0},
		{"", "Intel(R) N100", 0},
		{"0", "Intel(R) N100", 0},
		{"junk", "@ 2.5 GHz", 2500},
		{"", "@ 500GHz", 0},
	} {
		if got := nominalMHz(c.base, c.model); got != c.want {
			t.Errorf("nominalMHz(%q, %q) = %d, want %d", c.base, c.model, got, c.want)
		}
	}
}

// TestQbtConfShell reads the WebUI settings out of both conf layouts: the
// 4.x `WebUI\Key` under [Preferences] and the 5.x bare keys under [WebUI].
// Lines the parser does not read (the password hash above all) never leave
// the box. Runs under the system awk and, when installed, mawk (Debian's
// default awk on the boxes).
func TestQbtConfShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	v4 := "[Preferences]\nWebUI\\Port=8089\nWebUI\\LocalHostAuth=false\nWebUI\\Username=ragnar\nWebUI\\Password_PBKDF2=\"@ByteArray(secret)\"\nConnection\\ResolvePeerCountries=true\n"
	v5 := "[BitTorrent]\nSession\\Port=15143\n\n[Preferences]\nConnection\\ResolvePeerCountries=true\n\n[WebUI]\r\nAddress=127.0.0.1\nAuthSubnetWhitelistEnabled=false\nLocalHostAuth=true\nPassword_PBKDF2=\"@ByteArray(secret)\"\nPort=16209\nUsername=rc5\n"
	awks := []string{""}
	if mawk, err := exec.LookPath("mawk"); err == nil {
		awks = append(awks, mawk)
	}
	for _, awk := range awks {
		for name, conf := range map[string]string{"v4": v4, "v5": v5} {
			home := t.TempDir()
			dir := home + "/.config/qBittorrent"
			os.MkdirAll(dir, 0o700)
			os.WriteFile(dir+"/qBittorrent.conf", []byte(conf), 0o600)
			cmd := exec.Command("sh")
			cmd.Stdin = strings.NewReader("home=" + home + "\n" + qbtConfShell)
			if awk != "" {
				bin := t.TempDir()
				os.Symlink(awk, bin+"/awk")
				cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
			}
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("%s/%s: %v", awk, name, err)
			}
			if strings.Contains(string(out), "secret") || strings.Contains(string(out), "Address") {
				t.Fatalf("%s/%s: leaked a line the parser does not read: %q", awk, name, out)
			}
			q := parseQbt(map[string]string{"qbt_conf": string(out)})
			switch name {
			case "v4":
				if !q.ConfFound || q.ConfPort != 8089 || !q.LocalHostAuth || q.Username != "ragnar" {
					t.Fatalf("%s/v4: %+v from %q", awk, q, out)
				}
			case "v5":
				if !q.ConfFound || q.ConfPort != 16209 || q.LocalHostAuth || q.SubnetBypass || q.Username != "rc5" {
					t.Fatalf("%s/v5: %+v from %q", awk, q, out)
				}
			}
		}
		// No conf at all: an empty section and nothing on stderr (awk's
		// non-zero exit is as harmless as grep's was: the script has no set -e).
		out, stderr, _ := runSh(t, "home="+t.TempDir()+"\n"+qbtConfShell)
		if out != "" || stderr != "" {
			t.Fatalf("%s/missing conf: %q %q", awk, out, stderr)
		}
	}
}
