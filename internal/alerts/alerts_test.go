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
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"pmss-fleet-manager/internal/collect"
	"pmss-fleet-manager/internal/store"
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

func TestWantQbtConnection(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	s := collect.BoxState{Name: "seed-01"}
	s.Qbt.Enabled, s.Qbt.Reachable, s.Qbt.Connection, s.Qbt.ListenPort = true, true, "firewalled", 51413
	s.Qbt.FirewalledSince = now.Add(-5 * time.Minute).Format(time.RFC3339)
	if got := wantQbtConnection(&s, now); len(got) != 0 {
		t.Fatalf("inside the grace period: %+v", got)
	}
	s.Qbt.FirewalledSince = now.Add(-15 * time.Minute).Format(time.RFC3339)
	got := wantQbtConnection(&s, now)
	if len(got) != 1 || got[0].key != "qbt-conn" || got[0].title != "seed-01 — qBittorrent: no peer has reached listen port 51413 (firewalled) for 15m; check the port in qBittorrent against the one Pulsed Media assigned" {
		t.Fatalf("alert: %+v", got)
	}
	s.Qbt.Connection = "disconnected"
	s.Qbt.FirewalledSince = now.Add(-90 * time.Minute).Format(time.RFC3339)
	if got := wantQbtConnection(&s, now); len(got) != 1 || got[0].title != "seed-01 — qBittorrent: libtorrent reports no network connection for 1h 30m; check the port in qBittorrent against the one Pulsed Media assigned" {
		t.Fatalf("disconnected: %+v", got)
	}
	s.Qbt.Connection = "connected"
	if got := wantQbtConnection(&s, now); len(got) != 0 {
		t.Fatalf("connected: %+v", got)
	}
	s.Qbt.Connection, s.Qbt.Reachable = "firewalled", false
	if got := wantQbtConnection(&s, now); len(got) != 0 {
		t.Fatalf("unreachable client has no connection status: %+v", got)
	}
	if recheckable(&s, "qbt-conn") {
		t.Error("unreachable: must not resolve")
	}
	s.Qbt.Enabled = false
	if !recheckable(&s, "qbt-conn") {
		t.Error("monitoring off: must resolve")
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
	// Processes read but their command lines cut: Bazarr runs as python3
	// and cannot be told from any other python3, so no verdict this poll.
	if got := wantApps(&s); len(got) != 0 {
		t.Fatalf("no alert without the command lines: %+v", got)
	}
	if recheckable(&s, "app:jellyfin") {
		t.Error("command lines not read: must not resolve the alert either")
	}
	s.Apps.ArgsKnown = true
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

// A state restored from the cache or carried across an edit (Unpolled) has
// its reachability flags reset: evaluating it must raise nothing (round 20
// MEDIUM: a watch toggle before the first poll raised "not responding").
func TestEvaluateSkipsUnpolled(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	b := &store.Box{Name: "x", Host: "h", User: "u", Port: 22, Enabled: true}
	if err := st.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	e := New(st, store.DefaultSettings, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	s := &collect.BoxState{ID: b.ID, Name: "x", Status: "online", Unpolled: true}
	s.Qbt.Enabled, s.Docker.Enabled = true, true // Reachable false, as resetVolatile leaves them
	s.Apps.OK, s.Apps.ArgsKnown, s.Apps.Watched, s.Apps.Apps = true, true, []string{"radarr"}, []collect.AppView{{Key: "radarr", Name: "Radarr", Watched: true}}
	e.Evaluate(ctx, s)
	if keys, _ := st.ActiveAlertKeys(ctx, b.ID); len(keys) != 0 {
		t.Fatalf("unpolled state raised %v", keys)
	}
	// An app unwatched in that window still resolves its restored alert.
	st.RaiseAlert(ctx, b.ID, "app:sonarr", "warn", "x")
	st.RaiseAlert(ctx, b.ID, "app:radarr", "warn", "x")
	e.Evaluate(ctx, s)
	if keys, _ := st.ActiveAlertKeys(ctx, b.ID); !slices.Equal(keys, []string{"app:radarr"}) {
		t.Fatalf("unwatched app alert must resolve while unpolled, watched one stays: %v", keys)
	}
	s.Unpolled = false
	e.Evaluate(ctx, s)
	keys, _ := st.ActiveAlertKeys(ctx, b.ID)
	slices.Sort(keys)
	if want := []string{"app:radarr", "docker", "qbt"}; !slices.Equal(keys, want) {
		t.Fatalf("polled state: %v, want %v", keys, want)
	}
	// A disabled box is still evaluated so its alerts resolve.
	s.Unpolled, s.Status = true, "disabled"
	e.Evaluate(ctx, s)
	if keys, _ := st.ActiveAlertKeys(ctx, b.ID); len(keys) != 0 {
		t.Fatalf("disabled unpolled state kept %v", keys)
	}
}

// Unwatching an app while its box is offline must resolve the app alert at
// once: the watch list is the dashboard's own, so no probe is needed.
func TestUnwatchResolvesWhileOffline(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	ctx := context.Background()
	b := &store.Box{Name: "x", Host: "h", User: "u", Port: 22, Enabled: true}
	if err := st.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"app:jellyfin", "app:radarr", "disk"} {
		if _, err := st.RaiseAlert(ctx, b.ID, k, "warn", k); err != nil {
			t.Fatal(err)
		}
	}
	e := New(st, store.DefaultSettings, slog.New(slog.NewTextHandler(io.Discard, nil)), nil)
	s := &collect.BoxState{ID: b.ID, Name: "x", Status: "offline", LastError: "down", Failures: 3}
	s.Apps.Watched = []string{"radarr"} // jellyfin was just unwatched
	e.Evaluate(ctx, s)
	keys, _ := st.ActiveAlertKeys(ctx, b.ID)
	slices.Sort(keys)
	if want := []string{"app:radarr", "disk", "unreachable"}; !slices.Equal(keys, want) {
		t.Fatalf("active after unwatch while offline: %v, want %v", keys, want)
	}
}

func TestWantRt(t *testing.T) {
	s := &collect.BoxState{Name: "b"}
	if wantRt(s) != nil {
		t.Fatal("disabled: nothing")
	}
	s.Rt.Enabled = true
	got := wantRt(s)
	if len(got) != 1 || got[0].key != "rtorrent" || !strings.Contains(got[0].title, "SCGI endpoint not responding") {
		t.Fatalf("%+v", got)
	}
	s.Rt.ProcessKnown, s.Rt.ProcessUp, s.Rt.Error = true, false, "rtorrent is not running"
	got = wantRt(s)
	if len(got) != 1 || !strings.HasSuffix(got[0].title, "rtorrent is not running") {
		t.Fatalf("detail must not repeat the headline: %+v", got)
	}
	s.Rt.Error = "rtorrent is not running (~/.rtorrentDisable is set)"
	if got = wantRt(s); !strings.HasSuffix(got[0].title, "rtorrent is not running") {
		t.Fatalf("a detail that only extends the headline is dropped too: %+v", got)
	}
	s.Rt.Error = "dial unix: no such file"
	if got = wantRt(s); !strings.Contains(got[0].title, "(dial unix: no such file)") {
		t.Fatalf("a different detail is kept: %+v", got)
	}
	s.Rt.Disabled = true
	if wantRt(s) != nil {
		t.Fatal("stopped on purpose is not an alert")
	}
	// A detached remove-with-data that failed after its request ended is
	// an alert of its own, whatever the connection state (Codex on PR 123).
	s.Rt.Disabled = false
	s.Rt.RemoveFailed = "Alpha (43519d14…): disk says no"
	got = wantRt(s)
	if len(got) != 2 || got[0].key != "rtorrent-remove" || !strings.Contains(got[0].title, "remove with files failed: Alpha") || got[1].key != "rtorrent" {
		t.Fatalf("%+v", got)
	}
	s.Rt.Reachable = true
	if got = wantRt(s); len(got) != 1 || got[0].key != "rtorrent-remove" {
		t.Fatalf("%+v", got)
	}
	s.Rt.RemoveFailed = ""
	s.Rt.Disabled, s.Rt.Reachable = false, true
	if wantRt(s) != nil {
		t.Fatal("reachable: nothing")
	}
}
