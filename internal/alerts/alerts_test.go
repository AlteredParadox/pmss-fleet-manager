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
	"time"

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
}

// Renewal dates are civil dates: the count rolls at local midnight, is 0 on
// the day itself and -1 the day after, and a DST change does not shift it.
func TestDaysUntilDate(t *testing.T) {
	chi, err := time.LoadLocation("America/Chicago")
	if err != nil {
		t.Skip("no tz database")
	}
	cases := []struct {
		now  time.Time
		ymd  string
		want int
	}{
		{time.Date(2026, 10, 17, 19, 30, 0, 0, chi), "2026-10-17", 0}, // evening of the due day, already past UTC midnight
		{time.Date(2026, 10, 17, 23, 59, 0, 0, chi), "2026-10-17", 0},
		{time.Date(2026, 10, 18, 0, 1, 0, 0, chi), "2026-10-17", -1},
		{time.Date(2026, 10, 17, 1, 0, 0, 0, chi), "2026-10-24", 7},
		{time.Date(2026, 10, 30, 12, 0, 0, 0, chi), "2026-11-02", 3}, // across the DST change on Nov 1
		{time.Date(2026, 3, 7, 12, 0, 0, 0, chi), "2026-03-09", 2},   // across the DST change on Mar 8
		{time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC), "2026-10-17", 0},
	}
	for _, c := range cases {
		got, ok := daysUntilDate(c.ymd, c.now)
		if !ok || got != c.want {
			t.Errorf("daysUntilDate(%s, %s) = %d,%v; want %d", c.ymd, c.now, got, ok, c.want)
		}
	}
	if _, ok := daysUntilDate("soon", time.Now()); ok {
		t.Error("garbage date accepted")
	}
}

func TestWantApps(t *testing.T) {
	s := collect.BoxState{Name: "seed-01"}
	s.Apps.Watched = []string{"radarr", "jellyfin"}
	s.Apps.Apps = []collect.AppView{{Key: "radarr", Name: "Radarr", Running: true, Watched: true}, {Key: "jellyfin", Name: "Jellyfin", Watched: true}, {Key: "sonarr", Name: "Sonarr"}}
	if got := wantApps(&s); len(got) != 0 {
		t.Fatalf("no alert without a readable process list: %+v", got)
	}
	s.Apps.OK = true
	got := wantApps(&s)
	if len(got) != 1 || got[0].key != "app:jellyfin" || got[0].sev != "warn" || got[0].title != "seed-01 — Jellyfin is not running" {
		t.Fatalf("alerts: %+v", got)
	}
	// Resolvable only when the list was read this poll, or the app was unwatched.
	s.Apps.OK = false
	if recheckable(&s, "app:jellyfin") {
		t.Error("unreadable process list must not resolve the alert")
	}
	if !recheckable(&s, "app:sonarr") {
		t.Error("an unwatched app's alert must resolve")
	}
	s.Apps.OK = true
	if !recheckable(&s, "app:jellyfin") {
		t.Error("read this poll: resolvable")
	}
}
