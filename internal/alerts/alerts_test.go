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

package alerts

import (
	"testing"

	"pmss-fleet-manager/internal/collect"
)

// A good poll whose quota, traffic, memory or docker source was unavailable
// must keep the alerts that depend on that source; everything else stays
// resolvable.
func TestRecheckable(t *testing.T) {
	var s collect.BoxState // monitoring on, every source unavailable this poll
	s.Docker.Enabled = true
	for _, k := range []string{"disk", "traffic", "ram", "suspended", "container:plex"} {
		if recheckable(&s, k) {
			t.Errorf("%s: resolvable although its source was not read", k)
		}
	}
	for _, k := range []string{"unreachable", "renewal", "qbt", "docker", "throttled", "oom"} {
		if !recheckable(&s, k) {
			t.Errorf("%s: must stay resolvable without a source flag", k)
		}
	}
	s.Disk.OK, s.Traffic.OK, s.RAM.OK, s.Account.OK, s.Docker.Reachable = true, true, true, true, true
	for _, k := range []string{"disk", "traffic", "ram", "suspended", "container:plex"} {
		if !recheckable(&s, k) {
			t.Errorf("%s: source read this poll, must be resolvable", k)
		}
	}
	// Docker monitoring switched off for the box: the daemon is never read
	// again, so a container alert must resolve rather than stay forever.
	off := collect.BoxState{}
	off.Docker.Enabled, off.Docker.Reachable = false, false
	if !recheckable(&off, "container:plex") {
		t.Error("container alert stranded after docker monitoring was turned off")
	}
	on := collect.BoxState{}
	on.Docker.Enabled, on.Docker.Reachable = true, false // monitoring on, daemon blip
	if recheckable(&on, "container:plex") {
		t.Error("container alert resolved on a daemon blip")
	}
}
