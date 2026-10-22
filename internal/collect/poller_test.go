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
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"pmss-fleet-manager/internal/probe"
	"pmss-fleet-manager/internal/qbt"
	"pmss-fleet-manager/internal/store"
)

// fakeWebUI is a qBittorrent Web API that accepts admin/secret and reports
// the given connection status.
func fakeWebUI(t *testing.T, connection string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("username") == "admin" && r.FormValue("password") == "secret" {
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "x"})
			w.Write([]byte("Ok."))
			return
		}
		w.Write([]byte("Fails."))
	})
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("v5.3.0")) })
	mux.HandleFunc("/api/v2/app/preferences", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"dht":false,"pex":false,"lsd":false,"listen_port":51413}`))
	})
	mux.HandleFunc("/api/v2/sync/maindata", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"server_state":{"dl_info_speed":0,"up_info_speed":0,"global_ratio":"1.0","connection_status":"` + connection + `"},"torrents":{}}`))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

// pollQbtFixture is a poller whose WebUI dialer reaches srv instead of an
// SSH tunnel, with the latest-release lookup pre-seeded so no network
// request is made.
func pollQbtFixture(t *testing.T, srv *httptest.Server) (*poller, *probe.Snapshot) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.latest.version, m.latest.at = "v5.3.0", time.Now()
	const port = 8089
	p := newPoller(m, &store.Box{ID: 1, Name: "x", User: "u", QbtEnabled: true, QbtPort: port, QbtUser: "admin", QbtPass: "secret"})
	addr := srv.Listener.Addr().String()
	p.dialTCP = func(string) qbt.Dialer {
		return func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }
	}
	snap := &probe.Snapshot{UID: 1001, ListenersKnown: true, Listeners: map[int]int{port: 1001},
		Qbt: probe.QbtInfo{BinaryPresent: true, Running: true, ConfFound: true, ConfPort: port}}
	return p, snap
}

// The real pollQbt success path: a client restarted since the previous
// poll that still reports firewalled gets a fresh ten-minute clock; without
// a restart the old clock stands (fred review on PR 87). Reverting the
// production line to continue from st.Qbt.FirewalledSince fails this.
func TestPollQbtRestartResetsFirewalledClock(t *testing.T) {
	p, snap := pollQbtFixture(t, fakeWebUI(t, "firewalled"))
	old := time.Now().Add(-20 * time.Minute).UTC().Format(time.RFC3339)
	ctx := context.Background()

	p.qbtRestarted = true
	st := BoxState{Qbt: QbtState{FirewalledSince: old}}
	p.pollQbt(ctx, &st, snap)
	if !st.Qbt.Reachable || st.Qbt.Connection != "firewalled" || st.Qbt.ListenPort != 51413 {
		t.Fatalf("fixture: %+v", st.Qbt)
	}
	since, err := time.Parse(time.RFC3339, st.Qbt.FirewalledSince)
	if err != nil || time.Since(since) > time.Minute {
		t.Fatalf("restarted client must get a fresh clock, got %q", st.Qbt.FirewalledSince)
	}

	p.qbtRestarted = false
	st = BoxState{Qbt: QbtState{FirewalledSince: old}}
	p.pollQbt(ctx, &st, snap)
	if st.Qbt.FirewalledSince != old {
		t.Fatalf("no restart must keep the old clock, got %q", st.Qbt.FirewalledSince)
	}

	// Connected clears it either way.
	p2, snap2 := pollQbtFixture(t, fakeWebUI(t, "connected"))
	st = BoxState{Qbt: QbtState{FirewalledSince: old}}
	p2.pollQbt(ctx, &st, snap2)
	if st.Qbt.Connection != "connected" || st.Qbt.FirewalledSince != "" {
		t.Fatalf("connected: %+v", st.Qbt)
	}
}

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

// A restart seen across an unreadable poll is still noticed, and the
// successful API path keeps the reset (fred review / Codex on PR 87).
func TestTrackQbtProcessAndRestartReset(t *testing.T) {
	up := func(tick int64) *probe.Snapshot {
		return &probe.Snapshot{ProcsKnown: true, Procs: []probe.ProcInfo{{PID: 42, StartTick: tick, Comm: "qbittorrent-nox"}}}
	}
	restarted, key := trackQbtProcess("", up(100))
	if restarted || key != "42:100" {
		t.Fatalf("first: %v %q", restarted, key)
	}
	restarted, key = trackQbtProcess(key, &probe.Snapshot{}) // ps printed nothing this poll
	if restarted || key != "42:100" {
		t.Fatalf("unreadable list must keep the identity: %v %q", restarted, key)
	}
	// A client listed without its start tick (stat line missed this poll)
	// is no evidence of a replacement either way.
	restarted, key = trackQbtProcess(key, up(0))
	if restarted || key != "42:100" {
		t.Fatalf("missing start tick must keep the identity: %v %q", restarted, key)
	}
	restarted, key = trackQbtProcess(key, up(900))
	if !restarted || key != "42:900" {
		t.Fatalf("restart after an unreadable poll: %v %q", restarted, key)
	}
	// The pid section still saw the client but the listing did not (it
	// exited in between): inconsistent, so the baseline is kept.
	restarted, key = trackQbtProcess(key, &probe.Snapshot{ProcsKnown: true, Qbt: probe.QbtInfo{Running: true}})
	if restarted || key != "42:900" {
		t.Fatalf("inconsistent snapshot must keep the identity: %v %q", restarted, key)
	}
	restarted, key = trackQbtProcess(key, &probe.Snapshot{ProcsKnown: true})
	if restarted || key != "" {
		t.Fatalf("no client in a readable list forgets the identity: %v %q", restarted, key)
	}
	// pollQbt's success path: the carried value (reset by the restart) is
	// what the clock continues from, so the new client gets its grace.
	t0 := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	prev := QbtState{FirewalledSince: t0.Add(-20 * time.Minute).Format(time.RFC3339)}
	qs := QbtState{FirewalledSince: carriedFirewalledSince(prev, true, true)}
	qs.FirewalledSince = firewalledSince(qs.FirewalledSince, "firewalled", t0)
	if qs.FirewalledSince != "2026-10-21T12:00:00Z" {
		t.Fatalf("restart reset overwritten: %q", qs.FirewalledSince)
	}
}

// A successful stop/restart is remembered until a poll that started after
// it has seen a client; a poll that began before the action does not
// consume it (its snapshot predates the restart).
func TestTakeRestartBefore(t *testing.T) {
	p := &poller{}
	if p.takeRestartBefore(time.Now()) {
		t.Fatal("nothing recorded")
	}
	before := time.Now().Add(-time.Second)
	p.noteRestartDone()
	if p.takeRestartBefore(before) {
		t.Fatal("a poll started before the action must not consume it")
	}
	after := time.Now().Add(time.Second)
	if !p.takeRestartBefore(after) {
		t.Fatal("a poll started after the action must consume it")
	}
	if p.takeRestartBefore(after) {
		t.Fatal("consumed once")
	}
}

func TestQbtProcKey(t *testing.T) {
	procs := []probe.ProcInfo{{PID: 20, Comm: "sshd"}, {PID: 42, StartTick: 900, Comm: "qbittorrent-nox"}, {PID: 7, StartTick: 100, Comm: "qbittorrent-nox"}}
	if k, known := qbtProcKey(procs); k != "42:900,7:100" || !known {
		t.Fatalf("key: %q %v", k, known)
	}
	if k, known := qbtProcKey(nil); k != "" || !known {
		t.Fatalf("empty: %q %v", k, known)
	}
	if _, known := qbtProcKey([]probe.ProcInfo{{PID: 42, Comm: "qbittorrent-nox"}}); known {
		t.Fatal("no start tick must be unknown, not pid:0")
	}
	// A restart with the same pid is told apart by the start tick.
	a, _ := qbtProcKey([]probe.ProcInfo{{PID: 42, StartTick: 901, Comm: "qbittorrent-nox"}})
	b, _ := qbtProcKey([]probe.ProcInfo{{PID: 42, StartTick: 900, Comm: "qbittorrent-nox"}})
	if a == b {
		t.Fatal("start tick ignored")
	}
}

// The inventory sections run on the first poll, then every
// inventoryInterval, and at once when asked (refresh, install, autostart,
// a stale logs listing).
func TestInventoryDue(t *testing.T) {
	p := newPoller(nil, &store.Box{ID: 1})
	now := time.Now()
	if !p.inventoryDue(now) {
		t.Fatal("first poll must run the inventory")
	}
	p.inv, p.invAt = &probe.Snapshot{Inventory: true}, now
	if p.inventoryDue(now.Add(inventoryInterval - time.Second)) {
		t.Fatal("too soon")
	}
	if !p.inventoryDue(now.Add(inventoryInterval)) {
		t.Fatal("due again after the interval")
	}
	p.wakeInventory()
	if !p.inventoryDue(now) {
		t.Fatal("asked for")
	}
	select {
	case <-p.wakeCh:
	default:
		t.Fatal("wakeInventory must wake the poller")
	}
	// The request is consumed by the poll that takes it: a request posted
	// while that poll is in flight is for the next one, not wiped by it
	// (Codex on PR 96), and a poll that failed gives its request back.
	if p.inventoryDue(now) {
		t.Fatal("consumed")
	}
	p.wakeInventory()
	p.inventoryFailed(p.inventoryDue(now))
	if !p.inventoryDue(now) {
		t.Fatal("a failed poll must keep the request")
	}
	p.inventoryFailed(false)
	if p.inventoryDue(now) {
		t.Fatal("a failed poll without a request posts none")
	}
}

// A manual refresh asks for the inventory sections (logs, crontab, binary).
func TestRefreshRequestsInventory(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	p := newPoller(m, &store.Box{ID: 7, Enabled: true})
	m.mu.Lock()
	m.pollers[7] = p
	m.mu.Unlock()
	if err := m.Refresh(7); err != nil || !p.invWant.Load() {
		t.Fatalf("refresh: %v want=%v", err, p.invWant.Load())
	}
	if err := m.Refresh(8); err == nil {
		t.Fatal("unknown box")
	}
}
