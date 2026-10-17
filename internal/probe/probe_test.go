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
	"os/exec"
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
@@docker_sock
1
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
	if !snap.Qbt.Running || snap.Qbt.ConfPort != 8089 || !snap.Qbt.LocalHostAuth || !snap.Qbt.BinaryPresent {
		t.Fatalf("qbt: %+v", snap.Qbt)
	}
	if !snap.Docker.SocketPresent {
		t.Fatalf("docker: %+v", snap.Docker)
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
	// Unreadable /proc/net/tcp: nothing can be verified, do not block.
	snap2, _ := Parse([]byte("@@uid\n1001\n@@listen\n@@end\n"), time.Now())
	if ok, _ := snap2.PortOwner(8080); !ok || snap2.ListenersKnown {
		t.Fatal("unknown listeners must not refuse")
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
	c := parseCgroup("mem_usage=15000000000\nmem_limit=15700000000\nmem_rss=165000000\nmem_cache=13900000000\nmem_inactive_file=13800000000\noom_kill=2\n")
	if c.MemRSS != 165000000 || c.MemCache != 13900000000 || c.MemInactiveFile != 13800000000 || c.OOMKills != 2 || c.MemUsage != 15000000000 {
		t.Fatalf("%+v", c)
	}
}
