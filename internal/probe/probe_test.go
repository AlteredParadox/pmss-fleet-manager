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
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
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
@@ncpu
16
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
@@qbt_cron
2
@@qbt_pmss
1
@@docker_sock
1
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
	if !snap.Qbt.Running || snap.Qbt.ConfPort != 8089 || !snap.Qbt.LocalHostAuth || !snap.Qbt.BinaryPresent || snap.Qbt.CronEntries != 2 || !snap.Qbt.PMSSManaged {
		t.Fatalf("qbt: %+v", snap.Qbt)
	}
	if !snap.Docker.SocketPresent {
		t.Fatalf("docker: %+v", snap.Docker)
	}
	// The probe's own shell (777) and its child ps (778) are dropped; the
	// rest keep their args, and a comm with a space survives.
	if !snap.ProcsKnown || len(snap.Procs) != 4 {
		t.Fatalf("procs: %+v", snap.Procs)
	}
	if q := snap.Procs[0]; q.PID != 4242 || q.Comm != "qbittorrent-nox" || q.RSS != 812340*1024 || q.CPUSec != 6172 || q.ElapsedSec != 438873 || q.Args != "/home/ragnar/bin/qbittorrent-nox -d" {
		t.Fatalf("qbt proc: %+v", q)
	}
	if r := snap.Procs[3]; r.Comm != "rtorrent main" || r.PPID != 1 {
		t.Fatalf("rtorrent proc: %+v", r)
	}
	if got := snap.ListenPIDs[8089]; len(got) != 1 || got[0] != 4242 {
		t.Fatalf("listen pids (deduplicated): %v", snap.ListenPIDs)
	}
	if got := snap.ListenPIDs[7878]; len(got) != 1 || got[0] != 5001 {
		t.Fatalf("listen pids: %v", snap.ListenPIDs)
	}
}

func TestParseProcsHostileAndMissing(t *testing.T) {
	procs, known := parseProcs("", "", 0)
	if known || len(procs) != 0 {
		t.Fatalf("empty ps must be unknown, got %v %v", known, procs)
	}
	raw := "1 0 -5 nan -1 bad\n2 0 99999999999999 1e99 5 huge\nx y z\n3 0 10 1 1 ok"
	procs, known = parseProcs(raw, "3 cmd with args\nnotanumber args", 0)
	if !known || len(procs) != 3 {
		t.Fatalf("procs: %+v", procs)
	}
	if procs[0].RSS != 0 || procs[0].CPUSec != 0 || procs[0].ElapsedSec != 0 {
		t.Fatalf("negative/nan values must clamp to 0: %+v", procs[0])
	}
	if procs[1].RSS != 0 || procs[1].CPUSec != 0 {
		t.Fatalf("absurd values must clamp to 0: %+v", procs[1])
	}
	if procs[2].Args != "cmd with args" {
		t.Fatalf("args: %+v", procs[2])
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
	if !strings.HasPrefix(src, "PMSS_TAG='"+tag+"'\n#!/bin/sh") {
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
