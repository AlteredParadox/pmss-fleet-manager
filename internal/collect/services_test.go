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
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"pmss-fleet-manager/internal/pmapi"
	"pmss-fleet-manager/internal/probe"
	"pmss-fleet-manager/internal/store"
)

func TestMatchService(t *testing.T) {
	sb := func(u string) *pmapi.Access { return &pmapi.Access{Username: u} }
	svcs := []pmapi.Service{
		{ID: 1, Domain: "typhon.pulsedmedia.com", Seedbox: sb("rc2")},
		{ID: 2, Domain: "caspian.pulsedmedia.com", Seedbox: sb("rc3")},
		{ID: 3, Domain: "reinhardt.pulsedmedia.com", Seedbox: sb("rc3")},
		{ID: 4, Domain: "shared.pulsedmedia.com", Seedbox: sb("a1")},
		{ID: 5, Domain: "shared.pulsedmedia.com", Seedbox: sb("a2")},
	}
	cases := []struct {
		host, user string
		want       int64
		how        string
	}{
		{"Typhon.pulsedmedia.com", "rc2", 1, "host+user"},
		{"typhon.pulsedmedia.com", "other", 1, "host"},     // host unique
		{"caspian.pulsedmedia.com", "rc3", 2, "host+user"}, // same user on two hosts
		{"10.0.0.1", "rc3", 0, ""},                         // ambiguous user
		{"10.0.0.1", "rc2", 1, "user"},
		{"shared.pulsedmedia.com", "a2", 5, "host+user"},
		{"shared.pulsedmedia.com", "zz", 0, ""}, // host ambiguous
	}
	for _, c := range cases {
		got, how := matchService(svcs, c.host, c.user)
		var id int64
		if got != nil {
			id = got.ID
		}
		if id != c.want || how != c.how {
			t.Errorf("%s/%s: got %d %q, want %d %q", c.host, c.user, id, how, c.want, c.how)
		}
	}
}

func TestDailySeriesUsesBoxZone(t *testing.T) {
	// 01:00 on the 10th in Helsinki is still the 9th in UTC; the box keys by
	// its own date, so the "today" bar must be the 10th.
	hel := time.FixedZone("hel", 3*3600)
	now := time.Date(2026, 10, 10, 1, 0, 0, 0, hel)
	out := map[string]float64{"2026/10/10": 5, "2026/10/09": 3}
	pts := dailySeries(out, nil, now)
	if last := pts[len(pts)-1]; last.Date != "2026-10-10" || last.OutMiB != 5 {
		t.Fatalf("last point = %+v", last)
	}
	if pts[len(pts)-2].OutMiB != 3 {
		t.Fatalf("yesterday = %+v", pts[len(pts)-2])
	}
}

func TestBusDropsSlowSubscriber(t *testing.T) {
	b := NewBus()
	ch, cancel := b.Subscribe()
	defer cancel()
	for i := 0; i < 70; i++ { // buffer is 64
		b.Publish(Event{Type: "state"})
	}
	n := 0
	for range ch {
		n++
	}
	if n != 64 {
		t.Fatalf("delivered %d, want the 64 buffered then a closed channel", n)
	}
	cancel() // must be safe after the bus already dropped the subscriber
}

func TestMibToBytesClamps(t *testing.T) {
	if mibToBytes(1) != 1048576 || mibToBytes(-1) != 0 || mibToBytes(1e300) != 1<<62 {
		t.Fatalf("%d %d %d", mibToBytes(1), mibToBytes(-1), mibToBytes(1e300))
	}
	if mibToBytes(math.NaN()) != 0 {
		t.Fatal("NaN must clamp to 0")
	}
}

func TestSanitizeFloats(t *testing.T) {
	nan := math.NaN()
	st := BoxState{}
	st.CPU.Pct = nan
	st.CPU.QuotaCPUs = math.Inf(1)
	st.Traffic.Daily = []DayPoint{{Date: "2026-10-16", OutMiB: math.Inf(-1), InMiB: 1}}
	st.Qbt.GlobalRatio = nan
	st.HostSys.Load1 = 0.5
	st.Sanitize()
	if st.CPU.Pct != 0 || st.CPU.QuotaCPUs != 0 || st.Traffic.Daily[0].OutMiB != 0 || st.Qbt.GlobalRatio != 0 {
		t.Fatalf("non-finite survived: %+v", st)
	}
	if st.HostSys.Load1 != 0.5 || st.Traffic.Daily[0].InMiB != 1 {
		t.Fatal("finite values must be untouched")
	}
	if _, err := json.Marshal(st); err != nil {
		t.Fatal(err)
	}
	if gibToBytes(1) != 1<<30 || gibToBytes(1e12) != 1<<62 || gibToBytes(-3) != 0 {
		t.Fatalf("gib: %d %d %d", gibToBytes(1), gibToBytes(1e12), gibToBytes(-3))
	}
}

func TestRetryInterval(t *testing.T) {
	base := 30 * time.Second
	for failures, want := range map[int]time.Duration{0: base, 1: base, 4: base, 5: 60 * time.Second, 6: 2 * time.Minute, 7: 4 * time.Minute, 8: 8 * time.Minute, 9: 10 * time.Minute, 50: 10 * time.Minute} {
		if got := retryInterval(base, failures); got != want {
			t.Errorf("failures=%d: got %s want %s", failures, got, want)
		}
	}
	if got := retryInterval(15*time.Minute, 9); got != 15*time.Minute {
		t.Fatalf("a base above the cap must not be shortened: %s", got)
	}
}

// clip bounds a remote string by runes, marks every truncation with an
// ellipsis, and never leaves a split rune in the kept part.
func TestClip(t *testing.T) {
	if got := clip("abc", 3); got != "abc" {
		t.Fatalf("no cut: %q", got)
	}
	if got := clip("abcd", 3); got != "abc…" {
		t.Fatalf("ascii cut: %q", got)
	}
	emoji := strings.Repeat("😀", 129) // 4-byte runes: the byte pre-cut lands on exactly max runes
	if got := clip(emoji, 128); got != strings.Repeat("😀", 128)+"…" {
		t.Fatalf("4-byte boundary lost the marker or cut wrong: len=%d", len([]rune(got)))
	}
	mixed := strings.Repeat("é", 300) // 2-byte runes
	got := clip(mixed, 100)
	if r := []rune(got); len(r) != 101 || r[100] != '…' || !utf8.ValidString(got) {
		t.Fatalf("2-byte cut: %d runes, valid=%v", len(r), utf8.ValidString(got))
	}
	exact := strings.Repeat("😀", 128)
	if got := clip(exact, 128); got != exact {
		t.Fatalf("exactly max runes must not be marked: %q", got[:8])
	}
}

// foldDu keeps the largest entries and sums the rest into one, so the state
// and cache row stay bounded whatever the box lists.
func TestFoldDu(t *testing.T) {
	var ds []probe.DirUsage
	for i := 0; i < maxDuEntries+3; i++ {
		ds = append(ds, probe.DirUsage{Name: "d", Bytes: 10})
	}
	got := foldDu(ds)
	if len(got) != maxDuEntries+1 {
		t.Fatalf("len %d, want %d", len(got), maxDuEntries+1)
	}
	if last := got[maxDuEntries]; last.Name != duRestName || last.Bytes != 30 {
		t.Fatalf("rest entry %+v", last)
	}
	if small := foldDu(ds[:3]); len(small) != 3 || small[2].Name != "d" {
		t.Fatalf("short list altered: %+v", small)
	}
	// A row an older build cached unbounded is capped the same way on restore.
	big := make([]DirEntry, maxDuEntries+10)
	for i := range big {
		big[i] = DirEntry{Name: "x", Bytes: 1}
	}
	if capped := capEntries(big); len(capped) != maxDuEntries+1 || capped[maxDuEntries].Bytes != 10 {
		t.Fatalf("restored row not capped: len %d", len(capped))
	}
}

// A hash from the WebUI is kept only when it is one: a hostile client's ".."
// would otherwise be POSTed as a path segment and path-cleaned by the mux.
func TestTorrentHash(t *testing.T) {
	v1 := "0123456789ABCDEF0123456789abcdef01234567"
	if got := torrentHash(v1); got != strings.ToLower(v1) {
		t.Errorf("v1 hash → %q", got)
	}
	if got := torrentHash(v1 + v1[:24]); got != strings.ToLower(v1+v1[:24]) {
		t.Errorf("v2 hash → %q", got)
	}
	for _, h := range []string{"..", "", "all", v1[:39], v1 + "0", v1[:39] + "g", strings.Repeat("a", 65)} {
		if torrentHash(h) != "" {
			t.Errorf("%q kept", h)
		}
	}
}

// A SetDiscovery write that lands while a poll is in flight must not be
// undone when that poll publishes the values it read before the write.
func TestOverlayDiscovery(t *testing.T) {
	p := &poller{}
	start := time.Now()
	d := Discovery{Known: true, DHT: true}
	p.overlayDiscovery(start, &d)
	if !d.DHT {
		t.Fatal("overlay without a write changed the value")
	}
	p.noteDiscovery(map[string]bool{"dht": false, "lsd": true})
	p.overlayDiscovery(start, &d)
	if d.DHT || !d.LSD || d.PeX {
		t.Errorf("overlay after write: %+v", d)
	}
	between := time.Now()
	time.Sleep(time.Millisecond)
	p.noteDiscovery(map[string]bool{"pex": true}) // second write
	d = Discovery{Known: true, DHT: true}
	p.overlayDiscovery(start, &d) // a poll straddling both writes sees both
	if d.DHT || !d.LSD || !d.PeX {
		t.Errorf("overlay after two writes: %+v", d)
	}
	mid := Discovery{Known: true, DHT: true}
	p.overlayDiscovery(between, &mid) // started after write 1: only write 2 applies, dht stays as read
	if !mid.DHT || !mid.PeX {
		t.Errorf("overlay for a poll between the writes: %+v", mid)
	}
	later := Discovery{Known: true, DHT: true}
	p.overlayDiscovery(time.Now(), &later) // a poll that started after the write read the truth
	if !later.DHT {
		t.Error("overlay applied to a poll that started after the write")
	}
	unknown := Discovery{}
	p.overlayDiscovery(start, &unknown)
	if unknown.Known || unknown.LSD {
		t.Error("overlay faked a known state")
	}
}

// The failure path keeps a du result that was merged into the stored state
// while the failed poll was in flight; the success path stores its own.
func TestStoreFixKeepsDirs(t *testing.T) {
	p := &poller{}
	start := time.Now()
	stored := &BoxState{Dirs: DirsState{Running: false, At: time.Now()}}
	snap := BoxState{Dirs: DirsState{Running: true}}
	p.storeFix(start, true)(&snap, stored)
	if snap.Dirs.Running {
		t.Error("failure path dropped the merged du result")
	}
	snap = BoxState{Dirs: DirsState{Running: true, At: stored.Dirs.At}}
	p.storeFix(start, false)(&snap, stored)
	if !snap.Dirs.Running {
		t.Error("success path must keep a du it just started (same At)")
	}
	snap = BoxState{Dirs: DirsState{Running: true, At: stored.Dirs.At.Add(-time.Hour)}}
	p.storeFix(start, false)(&snap, stored)
	if snap.Dirs.Running {
		t.Error("success path must take a newer merged du result")
	}
	p.storeFix(start, true)(&snap, nil) // no stored state: nothing to keep
}

// Replacing the API key drops the feed fetched with the old one at once, in
// memory and on disk, so a failed sync for the new key cannot leave the old
// account's services on the boxes, now or after a restart.
func TestKeyChangeForgetsFeed(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmapi.URL = bad.URL
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()

	raw := []byte(`{"client_id":1,"currency":"EUR","services":[{"id":5,"product":"P","status":"Active","recurring_amount":"10","billing_cycle":"Monthly"}]}`)
	feed, err := pmapi.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	a := m.Settings()
	a.PMAPIKey = "key-a"
	m.settings.Store(&a)
	m.svcMu.Lock()
	m.svc = servicesCache{feed: feed, syncedAt: time.Now()}
	m.svcMu.Unlock()
	if err := st.SetValue(ctx, svcCacheKey, string(raw)); err != nil {
		t.Fatal(err)
	}

	b := a
	b.PMAPIKey = "key-b"
	m.SetSettings(b)
	m.svcMu.RLock()
	f := m.svc.feed
	m.svcMu.RUnlock()
	if f != nil {
		t.Fatal("old key's feed still cached right after the key change")
	}
	if v, _ := st.GetValue(ctx, svcCacheKey); v != "" {
		t.Fatalf("old key's feed still persisted: %q", v)
	}
	// The sync for key-b fails (500); the old feed must not come back.
	deadline := time.Now().Add(5 * time.Second)
	for {
		m.svcMu.RLock()
		errStr, f := m.svc.err, m.svc.feed
		m.svcMu.RUnlock()
		if f != nil {
			t.Fatal("failed sync for the new key reattached the old feed")
		}
		if errStr != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sync for the new key never ran")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A sync whose fetch was in flight when the key was replaced must not commit
// its feed: the check runs under the cache lock after the fetch returns.
func TestInFlightSyncDiscardedOnKeyChange(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	release, arrived := make(chan struct{}), make(chan struct{}, 1)
	feedA := `{"client_id":1,"currency":"EUR","services":[{"id":5,"product":"A","status":"Active","recurring_amount":"10","billing_cycle":"Monthly"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") == "key-a" {
			arrived <- struct{}{}
			<-release // hold A's fetch open across the key change
			w.Write([]byte(feedA))
			return
		}
		w.WriteHeader(500)
	}))
	defer srv.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmapi.URL = srv.URL
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	a := m.Settings()
	a.PMAPIKey = "key-a"
	m.settings.Store(&a)
	done := make(chan error, 1)
	go func() { done <- m.SyncServices(ctx) }()
	<-arrived // A's fetch is now blocked in the handler
	b := a
	b.PMAPIKey = "key-b"
	m.settings.Store(&b) // the swap SetSettings does, without starting B's sync
	m.forgetServices(ctx)
	close(release)
	if err := <-done; err == nil {
		t.Fatal("superseded sync must report so")
	}
	m.svcMu.RLock()
	f := m.svc.feed
	m.svcMu.RUnlock()
	if f != nil {
		t.Fatal("in-flight sync for the old key committed its feed")
	}
	if v, _ := st.GetValue(ctx, svcCacheKey); v != "" {
		t.Fatalf("old key's feed persisted: %q", v)
	}
}

// The persisted cache names the key that produced it; a restart under a
// different key refuses it even when the clear never reached the disk.
func TestLoadServicesCacheChecksKey(t *testing.T) {
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	rec, _ := json.Marshal(svcCacheRecord{KeyFP: keyFingerprint("key-a"), SyncedAt: time.Now(),
		Feed: json.RawMessage(`{"client_id":1,"currency":"EUR","services":[{"id":5,"product":"A","status":"Active","recurring_amount":"10","billing_cycle":"Monthly"}]}`)})
	if err := st.SetValue(ctx, svcCacheKey, string(rec)); err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		key  string
		want bool
	}{{"key-a", true}, {"key-b", false}, {"", false}} {
		m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
		s := m.Settings()
		s.PMAPIKey = c.key
		m.settings.Store(&s)
		m.loadServicesCache(ctx)
		if got := m.svc.feed != nil; got != c.want {
			t.Errorf("key %q: feed restored = %v, want %v", c.key, got, c.want)
		}
	}
}

// An active service wins over a cancelled one on the same host; a lone
// cancelled match is still attached (history) but yields no renewal date.
func TestDecoratePrefersActiveService(t *testing.T) {
	sb := func(u string) *pmapi.Access { return &pmapi.Access{Username: u} }
	m := &Manager{}
	m.svc = servicesCache{feed: &pmapi.Feed{Currency: "EUR", Services: []pmapi.Service{
		{ID: 1, Domain: "typhon.pulsedmedia.com", Status: "Cancelled", NextDue: "2026-01-01", Seedbox: sb("rc2")},
		{ID: 2, Domain: "typhon.pulsedmedia.com", Status: "Active", NextDue: "2027-01-01", Seedbox: sb("rc2")},
		{ID: 3, Domain: "old.pulsedmedia.com", Status: "Cancelled", NextDue: "2025-01-01", Seedbox: sb("rc9")},
	}}}
	s := &BoxState{Host: "typhon.pulsedmedia.com", User: "rc2"}
	m.decorate(s)
	if s.Service == nil || s.Service.ID != 2 {
		t.Fatalf("want the active service 2, got %+v", s.Service)
	}
	if s.EffectiveRenewal() != "2027-01-01" {
		t.Fatalf("renewal = %q", s.EffectiveRenewal())
	}
	s = &BoxState{Host: "old.pulsedmedia.com", User: "rc9"}
	m.decorate(s)
	if s.Service == nil || s.Service.ID != 3 {
		t.Fatalf("a cancelled service should still be shown, got %+v", s.Service)
	}
	if s.EffectiveRenewal() != "" {
		t.Fatalf("a cancelled service's due date is not a renewal, got %q", s.EffectiveRenewal())
	}
	s.Renewal = "2026-12-01"
	if s.EffectiveRenewal() != "2026-12-01" {
		t.Fatal("manual renewal must still win")
	}
}

// A suspended service is usually an overdue one: it still counts towards
// costs and renewal alerts. Every other non-active status does not.
func TestServiceActiveStatuses(t *testing.T) {
	for status, want := range map[string]bool{
		"Active": true, "active": true, "Suspended": true,
		"Cancelled": false, "Terminated": false, "Pending": false, "Fraud": false, "Completed": false, "": false,
	} {
		if got := serviceActive(status); got != want {
			t.Errorf("%q: got %v want %v", status, got, want)
		}
	}
	m := &Manager{}
	m.svc = servicesCache{feed: &pmapi.Feed{Currency: "EUR", Services: []pmapi.Service{
		{ID: 1, Domain: "typhon.pulsedmedia.com", Status: "Suspended", NextDue: "2026-01-01", Seedbox: &pmapi.Access{Username: "rc2"}},
	}}}
	s := &BoxState{Host: "typhon.pulsedmedia.com", User: "rc2"}
	m.decorate(s)
	if s.EffectiveRenewal() != "2026-01-01" {
		t.Fatalf("a suspended service's due date is its renewal, got %q", s.EffectiveRenewal())
	}
}
