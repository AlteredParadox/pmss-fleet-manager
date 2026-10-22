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

func TestResolveQbtVersion(t *testing.T) {
	bin := probe.QbtInfo{BinaryPresent: true, BinaryStat: "100 1", BinaryVersion: "v5.2.4"}
	stat, ver, cur := resolveQbtVersion("", "", bin)
	if stat != "100 1" || ver != "v5.2.4" || cur != "v5.2.4" {
		t.Fatalf("fresh answer: %q %q %q", stat, ver, cur)
	}
	// Next poll: the probe skipped the call, the binary is unchanged.
	skipped := probe.QbtInfo{BinaryPresent: true, BinaryStat: "100 1"}
	if stat, ver, cur = resolveQbtVersion(stat, ver, skipped); cur != "v5.2.4" || stat != "100 1" {
		t.Fatalf("unchanged: %q %q %q", stat, ver, cur)
	}
	// A fresh answer wins even when stat printed nothing (round 20, LOW-3).
	if _, ver, cur := resolveQbtVersion("100 1", "v5.2.4", probe.QbtInfo{BinaryPresent: true, BinaryVersion: "v5.2.5"}); cur != "v5.2.5" || ver != "v5.2.5" {
		t.Fatalf("fresh answer without stat: %q %q", ver, cur)
	}
	// Binary replaced but --version failed: nothing, and the cache is dropped
	// so the next probe asks again.
	if stat, ver, cur = resolveQbtVersion(stat, ver, probe.QbtInfo{BinaryPresent: true, BinaryStat: "200 2"}); cur != "" || stat != "" || ver != "" {
		t.Fatalf("changed, no answer: %q %q %q", stat, ver, cur)
	}
	if _, _, cur = resolveQbtVersion("100 1", "v5.2.4", probe.QbtInfo{}); cur != "" {
		t.Fatalf("binary gone: %q", cur)
	}
}

func TestFirewalledSince(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	if got := firewalledSince("", "firewalled", now); got != "2026-10-21T12:00:00Z" {
		t.Fatalf("first sighting: %q", got)
	}
	if got := firewalledSince("2026-10-21T11:00:00Z", "firewalled", now); got != "2026-10-21T11:00:00Z" {
		t.Fatalf("kept: %q", got)
	}
	if got := firewalledSince("2026-10-21T11:00:00Z", "disconnected", now); got != "2026-10-21T11:00:00Z" {
		t.Fatalf("disconnected keeps it: %q", got)
	}
	if got := firewalledSince("garbage", "firewalled", now); got != "2026-10-21T12:00:00Z" {
		t.Fatalf("unparsable previous value restarts the clock: %q", got)
	}
	for _, c := range []string{"connected", ""} {
		if got := firewalledSince("2026-10-21T11:00:00Z", c, now); got != "" {
			t.Fatalf("%q must clear: %q", c, got)
		}
	}
	if connectionStatus("Connected") != "" || connectionStatus("firewalled") != "firewalled" {
		t.Fatal("connectionStatus")
	}
}

// A Web API failure between two firewalled readings must not restart the
// clock: the poll after the failure keeps the original time, so the alert
// neither resolves nor waits another ten minutes. A client process that is
// gone starts afresh.
func TestFirewalledSinceSurvivesAPIFailure(t *testing.T) {
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	prev := QbtState{}
	// Poll 1: firewalled.
	prev.FirewalledSince = firewalledSince(carriedFirewalledSince(prev, true, false), "firewalled", t0)
	if prev.FirewalledSince != "2026-10-21T12:00:00Z" {
		t.Fatalf("poll 1: %q", prev.FirewalledSince)
	}
	// Poll 2: process up, API failed: the state the poll stores starts from
	// the carried value and never reaches firewalledSince.
	failed := QbtState{FirewalledSince: carriedFirewalledSince(prev, true, false)}
	if failed.FirewalledSince != prev.FirewalledSince {
		t.Fatalf("API failure cleared it: %q", failed.FirewalledSince)
	}
	// Poll 3: API back, still firewalled: the original time stands.
	got := firewalledSince(carriedFirewalledSince(failed, true, false), "firewalled", t0.Add(20*time.Minute))
	if got != "2026-10-21T12:00:00Z" {
		t.Fatalf("recovery restarted the clock: %q", got)
	}
	// Process gone, or replaced between two polls that both saw it up: a
	// new process gets its own clock.
	if s := carriedFirewalledSince(failed, false, false); s != "" {
		t.Fatalf("process down must clear: %q", s)
	}
	if s := carriedFirewalledSince(failed, true, true); s != "" {
		t.Fatalf("restart between polls must clear: %q", s)
	}
}

func TestQbtProcKey(t *testing.T) {
	procs := []probe.ProcInfo{{PID: 20, Comm: "sshd"}, {PID: 42, StartTick: 900, Comm: "qbittorrent-nox"}, {PID: 7, StartTick: 100, Comm: "qbittorrent-nox"}}
	if k := qbtProcKey(procs); k != "42:900,7:100" {
		t.Fatalf("key: %q", k)
	}
	if k := qbtProcKey(nil); k != "" {
		t.Fatalf("empty: %q", k)
	}
	// A restart with the same pid is told apart by the start tick.
	if qbtProcKey([]probe.ProcInfo{{PID: 42, StartTick: 901, Comm: "qbittorrent-nox"}}) == qbtProcKey([]probe.ProcInfo{{PID: 42, StartTick: 900, Comm: "qbittorrent-nox"}}) {
		t.Fatal("start tick ignored")
	}
}
