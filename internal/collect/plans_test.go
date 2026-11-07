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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"pmss-fleet-manager/internal/pmapi"
	"pmss-fleet-manager/internal/pmplans"
	"pmss-fleet-manager/internal/store"
)

func testCatalogue() *pmplans.Catalogue {
	return &pmplans.Catalogue{Plans: []pmplans.Plan{
		{Name: "Seedbox 1TB R5 10G", PID: 354, Cores: 2, CPUTier: "Performance", CPUWeight: 500, IOTier: "Standard", IOWeight: 100},
		{Name: "Storage 4TB R5 10G", Cores: 1, CPUTier: "Standard", CPUWeight: 100, IOTier: "Archive", IOWeight: 4},
		{Name: "SSD Seedbox 250GiB 10G", CPUWeight: 250, IOWeight: 100},
		{Name: "SSD Seedbox 250GiB 10G", CPUWeight: 350, IOWeight: 500},
	}}
}

// Typed-in weights win, the catalogue fills the rest, the typed plan name
// is looked up before the matched service's product, and the lookup
// status tells the page why a value is missing.
func TestTiersFor(t *testing.T) {
	m := &Manager{}
	svc := func(product string) *ServiceInfo { return &ServiceInfo{Product: product} }
	cases := []struct {
		name string
		s    BoxState
		cat  *pmplans.Catalogue
		want TierInfo
	}{
		{"nothing to look up", BoxState{}, testCatalogue(), TierInfo{Lookup: "no-name"}},
		{"no catalogue yet", BoxState{Service: svc("Seedbox 1TB R5 10G")}, nil, TierInfo{Lookup: "no-catalogue"}},
		{"from the service", BoxState{Service: svc("seedbox 1tb r5 10g")}, testCatalogue(),
			TierInfo{IOWeight: 100, IOTier: "Standard", IOSource: "catalogue", CPUWeight: 500, CPUTier: "Performance", CPUSource: "catalogue", Cores: 2, Product: "Seedbox 1TB R5 10G", Lookup: "matched"}},
		{"typed plan first", BoxState{Plan: "Storage 4TB R5 10G", Service: svc("Seedbox 1TB R5 10G")}, testCatalogue(),
			TierInfo{IOWeight: 4, IOTier: "Archive", IOSource: "catalogue", CPUWeight: 100, CPUTier: "Standard", CPUSource: "catalogue", Cores: 1, Product: "Storage 4TB R5 10G", Lookup: "matched"}},
		{"typed plan unknown, service matches", BoxState{Plan: "Dragon-R 2TB", Service: svc("Seedbox 1TB R5 10G")}, testCatalogue(),
			TierInfo{IOWeight: 100, IOTier: "Standard", IOSource: "catalogue", CPUWeight: 500, CPUTier: "Performance", CPUSource: "catalogue", Cores: 2, Product: "Seedbox 1TB R5 10G", Lookup: "matched"}},
		{"unknown everywhere", BoxState{Plan: "Dragon-R 2TB"}, testCatalogue(), TierInfo{Lookup: "unknown"}},
		{"ambiguous", BoxState{Service: svc("SSD Seedbox 250GiB 10G")}, testCatalogue(), TierInfo{Lookup: "ambiguous"}},
		{"manual io, catalogue cpu", BoxState{IOWeight: 42, Service: svc("Seedbox 1TB R5 10G")}, testCatalogue(),
			TierInfo{IOWeight: 42, IOTier: "Archive", IOSource: "manual", CPUWeight: 500, CPUTier: "Performance", CPUSource: "catalogue", Cores: 2, Product: "Seedbox 1TB R5 10G", Lookup: "matched"}},
		{"manual both, no name", BoxState{IOWeight: 1000, CPUWeight: 7}, nil,
			TierInfo{IOWeight: 1000, IOTier: "High", IOSource: "manual", CPUWeight: 7, CPUTier: "Archive", CPUSource: "manual", Lookup: "no-name"}},
	}
	for _, c := range cases {
		m.plans.cat = c.cat
		got := m.tiersFor(&c.s)
		if got == nil || *got != c.want {
			t.Fatalf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// decorate attaches the tiers alongside the service.
func TestDecorateAttachesTiers(t *testing.T) {
	m := &Manager{}
	m.plans.cat = testCatalogue()
	m.svc = servicesCache{feed: &pmapi.Feed{Services: []pmapi.Service{
		{ID: 1, Product: "Seedbox 1TB R5 10G", Domain: "h.pulsedmedia.com", Status: "Active", Seedbox: &pmapi.Access{Username: "u"}},
	}}}
	s := &BoxState{Host: "h.pulsedmedia.com", User: "u"}
	m.decorate(s)
	if s.Service == nil || s.Tiers == nil || s.Tiers.Lookup != "matched" || s.Tiers.CPUWeight != 500 {
		t.Fatalf("service %+v tiers %+v", s.Service, s.Tiers)
	}
}

// SyncPlans reads the page, keeps the catalogue for the next start and
// resolves the boxes; a failed read keeps the last catalogue. The
// automatic read after a services sync happens once per unknown product
// (not after every sync), at least an hour apart, and a failed one is
// retried at the next occasion.
func TestSyncPlansAndAutoFetch(t *testing.T) {
	page, err := os.ReadFile(filepath.Join("..", "pmplans", "testdata", "eternal.html"))
	if err != nil {
		t.Fatal(err)
	}
	var pageHits atomic.Int32
	var pageDown atomic.Bool
	var product atomic.Value
	product.Store("Dragon-R 2TB")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/plans":
			pageHits.Add(1)
			if pageDown.Load() {
				w.WriteHeader(503)
				return
			}
			_, _ = w.Write(page)
		case "/feed":
			_, _ = io.WriteString(w, `{"client_id":1,"currency":"EUR","services":[{"id":5,"product":"`+product.Load().(string)+`","domain":"h.pulsedmedia.com","status":"Active","recurring_amount":"10","billing_cycle":"Monthly","seedbox":{"username":"u"}}]}`)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmapi.URL, m.pmplans.URL = srv.URL+"/feed", srv.URL+"/plans"
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	b := &store.Box{Name: "b", Host: "h.pulsedmedia.com", User: "u", Enabled: false}
	if err := m.AddBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	a := m.Settings()
	a.PMAPIKey = "k"
	m.settings.Store(&a)
	// sync runs a services sync; when an automatic read is expected it
	// waits for that read to end (the hook), otherwise it gives an
	// unexpected one a moment to show up.
	done := make(chan struct{}, 8)
	plansAutoDoneHook = func() { done <- struct{}{} }
	t.Cleanup(func() { plansAutoDoneHook = nil })
	sync := func(wantHits int32) {
		t.Helper()
		before := pageHits.Load()
		if err := m.SyncServices(ctx); err != nil {
			t.Fatal(err)
		}
		if wantHits > before {
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the automatic read did not end in time")
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	ageAuto := func() { m.plansMu.Lock(); m.plans.autoAt = time.Now().Add(-2 * plansAutoInterval); m.plansMu.Unlock() }
	hits := func(want int32, what string) {
		t.Helper()
		if n := pageHits.Load(); n != want {
			t.Fatalf("%s: page read %d times, want %d", what, n, want)
		}
	}

	// Without a key nothing was fetched on its own; with one, the sync's
	// unknown product triggers one read of the page.
	hits(0, "before any key")
	sync(1)
	hits(1, "first unknown product")
	if m.Plans().FetchedAt == "" {
		t.Fatal("catalogue not kept")
	}
	if v, _ := st.GetValue(ctx, plansCacheKey); v == "" {
		t.Fatal("catalogue not persisted")
	}
	states := m.States()
	if len(states) != 1 || states[0].Tiers == nil || states[0].Tiers.Lookup != "unknown" {
		t.Fatalf("states %+v", states)
	}
	// The same unknown product again, even after the interval: no read.
	sync(1)
	hits(1, "repeat sync")
	ageAuto()
	sync(1)
	hits(1, "repeat sync after the interval")
	// A new unknown product within the hour waits for the interval.
	product.Store("M1000 2TB")
	m.plansMu.Lock()
	m.plans.autoAt = time.Now()
	m.plansMu.Unlock()
	sync(1)
	hits(1, "new product within the hour")
	ageAuto()
	sync(2)
	hits(2, "new product after the interval")
	sync(2)
	hits(2, "the new product is tried once too")
	// A failed automatic read is tried again at the next occasion.
	pageDown.Store(true)
	product.Store("Value250")
	ageAuto()
	sync(3)
	hits(3, "failed read")
	ageAuto()
	sync(4)
	hits(4, "failed read retried")
	// A manual read that fails keeps the catalogue and reports the error.
	if err := m.SyncPlans(ctx); err == nil {
		t.Fatal("expected an error from the 503")
	}
	v := m.Plans()
	if v.Error == "" || len(v.Plans) != 30 || !v.Configured {
		t.Fatalf("view after a failed read: %+v", v)
	}
	// A restart restores the catalogue.
	m2 := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m2.loadPlansCache(ctx)
	if len(m2.Plans().Plans) != 30 {
		t.Fatal("catalogue not restored from the store")
	}
}
