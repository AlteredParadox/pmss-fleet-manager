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
	"testing"
	"time"

	"pmss-fleet-manager/internal/probe"
	"pmss-fleet-manager/internal/store"
)

func TestApplyHostCPU(t *testing.T) {
	p := &poller{}
	at := time.Date(2026, 10, 22, 22, 0, 0, 0, time.UTC)
	snap := func(busy, idle, iowait, steal uint64, dt time.Duration) *probe.Snapshot {
		return &probe.Snapshot{At: at.Add(dt), Host: probe.HostInfo{CPUTicks: probe.CPUTicks{
			Known: true, Busy: busy, Idle: idle, IOWait: iowait, Steal: steal, Total: busy + idle + iowait}}}
	}
	near := func(got, want float64) bool { return math.Abs(got-want) < 0.01 }

	var st BoxState
	p.applyHostCPU(&st, snap(1000, 1000, 0, 100, 0))
	if st.HostSys.CPUOK {
		t.Fatal("first reading has nothing to compare with")
	}
	st = BoxState{}
	// 30 s later: +500 busy (of which +50 steal), +400 idle, +100 iowait → 1000 ticks.
	p.applyHostCPU(&st, snap(1500, 1400, 100, 150, 30*time.Second))
	h := st.HostSys
	if !h.CPUOK || !near(h.CPUBusyPct, 50) || !near(h.CPUIOWaitPct, 10) || !near(h.CPUStealPct, 5) {
		t.Fatalf("host cpu = %+v", h)
	}
	st = BoxState{}
	p.applyHostCPU(&st, snap(10, 20, 0, 0, 60*time.Second)) // reboot: counters went backwards
	if st.HostSys.CPUOK {
		t.Fatal("a reset must not produce a figure")
	}
	st = BoxState{}
	p.applyHostCPU(&st, snap(110, 20, 0, 0, 90*time.Second)) // and the reset reading is the new baseline
	if !st.HostSys.CPUOK || !near(st.HostSys.CPUBusyPct, 100) || !near(st.HostSys.CPUIOWaitPct, 0) {
		t.Fatalf("after reset = %+v", st.HostSys)
	}
	st = BoxState{}
	p.applyHostCPU(&st, &probe.Snapshot{At: at.Add(2 * time.Minute)}) // no /proc/stat this time
	if st.HostSys.CPUOK || !p.prevTicks.Known {
		t.Fatalf("an unknown reading must be skipped and keep the baseline: %+v", st.HostSys)
	}
}

func TestHostShareBesideQuota(t *testing.T) {
	p := &poller{box: &store.Box{User: "u"}}
	at := time.Date(2026, 10, 22, 22, 0, 0, 0, time.UTC)
	snap := func(ns int64, dt time.Duration) *probe.Snapshot {
		return &probe.Snapshot{At: at.Add(dt), Location: time.UTC, UID: 1000, Host: probe.HostInfo{NCPU: 8}, Cgroup: probe.CgroupInfo{CPUUsageNS: ns, CPUQuota: 2}}
	}
	var st BoxState
	p.apply(&st, snap(1e9, 0))
	if st.CPU.OK {
		t.Fatal("first reading has nothing to compare with")
	}
	st = BoxState{}
	p.apply(&st, snap(31e9, 30*time.Second)) // one full core for 30 s
	if !st.CPU.OK || math.Abs(st.CPU.Pct-50) > 0.01 || math.Abs(st.CPU.HostPct-12.5) > 0.01 {
		t.Fatalf("cpu = %+v, want 50%% of the 2-core quota and 12.5%% of the 8-core host", st.CPU)
	}
}
