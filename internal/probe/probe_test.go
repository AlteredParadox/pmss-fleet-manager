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
	"testing"
	"time"
)

const sample = `@@uid
1001
@@user
ragnar
@@home
/home/ragnar
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
@@resource
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
@@docker_pid
4300
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
	if !q.OK || q.Source != "quota" || q.UsedBytes != 1739461632*1024 || q.SoftBytes != 2097152000*1024 || q.FilesUsed != 12345 || q.FilesHard != 1250000 {
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
	if !snap.Docker.SocketPresent || !snap.Docker.Running {
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
	if v[0] != 1659<<30 || v[1] != 2000<<30 || v[3] != 12345 || v[4] != 1000<<10 {
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
