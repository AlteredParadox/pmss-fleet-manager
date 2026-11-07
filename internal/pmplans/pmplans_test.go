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

package pmplans

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func fixture(t *testing.T) []byte {
	t.Helper()
	b, err := os.ReadFile("testdata/eternal.html")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// The fixture is the campaign page's card section as served on 2026-11-07.
func TestParseFixture(t *testing.T) {
	cat, err := Parse(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Plans) != 30 {
		t.Fatalf("plans = %d, want 30", len(cat.Plans))
	}
	want := map[string]Plan{
		"SSD Seedbox 0.5TB 10G": {PID: 357, Cores: 4, CPUTier: "Performance", CPUWeight: 500, IOTier: "Standard", IOWeight: 100},
		"Storage 4TB R5 10G":    {PID: 353, Cores: 1, CPUTier: "Standard", CPUWeight: 100, IOTier: "Archive", IOWeight: 4},
		"Seedbox 8TB R5 10G":    {Cores: 4, CPUTier: "High", CPUWeight: 1000, IOTier: "Standard", IOWeight: 300}, // sold out: no link, no pid
		"Purnu Trophy":          {PID: 389, Cores: 2, CPUTier: "Archive", CPUWeight: 50, IOTier: "Archive", IOWeight: 12},
		"October 700 Special":   {Cores: 7.77, CPUTier: "Archive", CPUWeight: 77, IOTier: "Standard", IOWeight: 377},
	}
	for name, w := range want {
		w.Name = name
		var got *Plan
		for i := range cat.Plans {
			if cat.Plans[i].Name == name {
				got = &cat.Plans[i]
				break
			}
		}
		if got == nil {
			t.Fatalf("%q not parsed", name)
		}
		if *got != w {
			t.Fatalf("%q = %+v, want %+v", name, *got, w)
		}
	}
	if cat.Labels[389] != "Purnu Trophy" || cat.Labels[357] != "SSD Seedbox 0.5TB 10G" || len(cat.Labels) != 26 {
		t.Fatalf("labels = %v", cat.Labels)
	}
	for _, p := range cat.Plans {
		if strings.ContainsRune(p.Name, '🏆') || strings.TrimSpace(p.Name) != p.Name {
			t.Fatalf("name not cleaned: %q", p.Name)
		}
	}
}

func TestLookup(t *testing.T) {
	cat, err := Parse(fixture(t))
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name   string
		status string
		io     int
	}{
		{"Seedbox 1TB R5 10G", LookupMatched, 100},
		{"  seedbox 1tb r5 10g ", LookupMatched, 100}, // case and spacing folded
		{"🏆 Purnu Trophy", LookupMatched, 12},
		{"Storage 2TB R5 1G", LookupMatched, 2},
		{"SSD Seedbox 250GiB 10G", LookupAmbiguous, 0}, // two listings, different priorities
		{"Dragon-R Trophy", LookupMatched, 500},        // two listings that agree
		{"Dragon-R 2TB", LookupUnknown, 0},
		{"", LookupUnknown, 0},
		{"Eternal Väinämöinen", LookupUnknown, 0},
		// The customer API's spellings (a real feed, 2026-11-07): the
		// campaign prefix, marketing after the plan, the size sold.
		{"Eternal Väinämöinen - Seedbox 1TB R5 10G", LookupMatched, 100},
		{"Dragon-R Trophy 100 Years GiveAway", LookupMatched, 500},
		{"Eternal Väinämöinen - Storage 2TB R5 1G", LookupMatched, 2},
		{"Sampo Trophy 16TB R5 10G - 150 Years GivenAway", LookupMatched, 300},
		{"Eternal Väinämöinen - Dragon-R DDoS Recovery Special 1TB", LookupMatched, 100},
		{"Eternal Väinämöinen - M1000 DDoS Recovery Special 666GiB", LookupMatched, 100},
		{"Eternal Väinämöinen - Storage Box 1TB 10G", LookupMatched, 5},
		{"Eternal Väinämöinen - AI AppBox M1000 x30", LookupMatched, 150},
		{"Eternal Väinämöinen - AI AppBox M10G x30", LookupMatched, 150},
		{"Eternal Väinämöinen - Storage 4TB R5 10G", LookupMatched, 4},
		{"Arkku Trophy Storage Box 6TB R5 10G - GiveAway 8000€", LookupMatched, 6},
		// The longest listing wins: the Special, not the plain SSD seedbox
		// it starts with; a word that only begins the same is no match.
		{"Eternal Väinämöinen - SSD Seedbox 1TB 10G Special", LookupMatched, 400},
		{"Storage 2TB R5 10", LookupUnknown, 0},
	}
	for _, c := range cases {
		p, st := cat.Lookup(c.name)
		if st != c.status || (p != nil) != (st == LookupMatched) || (p != nil && p.IOWeight != c.io) {
			t.Fatalf("Lookup(%q) = %+v, %q; want %q io %d", c.name, p, st, c.status, c.io)
		}
	}
	var none *Catalogue
	if p, st := none.Lookup("x"); p != nil || st != LookupUnknown {
		t.Fatal("nil catalogue must answer unknown")
	}
}

// A name that only the page's pid map carries under the WHMCS spelling is
// matched through the card's order link.
func TestLookupThroughLabels(t *testing.T) {
	cat := &Catalogue{
		Plans:  []Plan{{Name: "Fancy Card Title", PID: 42, CPUWeight: 100, IOWeight: 100}},
		Labels: map[int]string{42: "WHMCS Product Name"},
	}
	if p, st := cat.Lookup("WHMCS Product Name"); st != LookupMatched || p.Name != "Fancy Card Title" {
		t.Fatalf("got %+v, %q", p, st)
	}
}

func TestWords(t *testing.T) {
	for in, want := range map[string]string{
		"🏆 Purnu   Trophy":                        "purnu trophy",
		"Eternal Väinämöinen - Purnu Trophy":      "purnu trophy",
		"The Eternal Väinämöinen":                 "",
		"Dragon-R DDoS Recovery Special 1TB":      "dragon r ddos recovery special 1tb",
		"Storage Box 6TB R5 10G - GiveAway 8000€": "storage box 6tb r5 10g giveaway 8000",
		"Seedbox Eternal":                         "seedbox eternal", // only a leading campaign word goes
	} {
		if got := Normalize(in); got != want {
			t.Fatalf("Normalize(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestParseRefusals(t *testing.T) {
	if _, err := Parse([]byte("<html><body>nothing</body></html>")); !errors.Is(err, ErrNoPlans) {
		t.Fatalf("err = %v", err)
	}
	// A card whose weight is off the scale or whose tier is not on the
	// ladder is skipped, never stored.
	bad := `<ul class="pricing-table"><li class="title">Odd</li>
<li><strong>I/O:</strong> Turbo (100)</li><li>4 <strong>CPU cores</strong> &middot; High (1000)</li></ul>
<ul class="pricing-table"><li class="title">Huge</li>
<li><strong>I/O:</strong> High (20000)</li><li>4 <strong>CPU cores</strong> &middot; High (1000)</li></ul>
<ul class="pricing-table"><li class="title">&#127942; Fine &lt;b&gt;</li>
<li><strong>I/O:</strong> Standard (150)</li><li>2.5 <strong>CPU cores</strong> &middot; Performance (999)</li></ul>`
	cat, err := Parse([]byte(bad))
	if err != nil {
		t.Fatal(err)
	}
	if len(cat.Plans) != 1 || cat.Plans[0].Name != "Fine b" || cat.Plans[0].Cores != 2.5 || cat.Plans[0].CPUTier != "Performance" {
		t.Fatalf("plans = %+v", cat.Plans)
	}
}

func TestTierLadder(t *testing.T) {
	for w, want := range map[int]string{0: "", 1: "Archive", 99: "Archive", 100: "Standard", 499: "Standard", 500: "Performance", 999: "Performance", 1000: "High", 10000: "High"} {
		if got := Tier(w); got != want {
			t.Fatalf("Tier(%d) = %q, want %q", w, got, want)
		}
	}
	// The page's label is checked but the ladder's word is kept, so an old
	// "Medium (500)" shows as Performance.
	if tier, w, ok := tierOf("Medium", "500"); !ok || tier != "Performance" || w != 500 {
		t.Fatalf("tierOf = %q %d %v", tier, w, ok)
	}
}

func TestCleanName(t *testing.T) {
	long := strings.Repeat("ä", 80)
	for in, want := range map[string]string{
		"&#127942; Purnu   Trophy ": "Purnu Trophy",
		"a\x00b\tc":                 "ab c",
		long:                        strings.Repeat("ä", 50),
	} {
		if got := cleanName(in); got != want {
			t.Fatalf("cleanName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFetch(t *testing.T) {
	page := fixture(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ok":
			_, _ = w.Write(page)
		case "/big":
			_, _ = w.Write(make([]byte, maxPageBytes+1))
		case "/gz":
			w.Header().Set("Content-Encoding", "gzip")
			_, _ = w.Write(page)
		case "/redir":
			http.Redirect(w, r, "/ok", http.StatusFound)
		default:
			w.WriteHeader(503)
		}
	}))
	defer srv.Close()
	ctx := context.Background()
	c := &Client{URL: srv.URL + "/ok"}
	cat, err := c.Fetch(ctx)
	if err != nil || len(cat.Plans) != 30 || cat.FetchedAt.IsZero() {
		t.Fatalf("fetch: %v, %+v", err, cat)
	}
	for path, want := range map[string]error{"/big": ErrTooLarge, "/gz": ErrEncoded, "/redir": ErrRedirect} {
		c := &Client{URL: srv.URL + path}
		if _, err := c.Fetch(ctx); !errors.Is(err, want) {
			t.Fatalf("%s: err = %v, want %v", path, err, want)
		}
	}
	c = &Client{URL: srv.URL + "/down"}
	var he *HTTPError
	if _, err := c.Fetch(ctx); !errors.As(err, &he) || he.Code != 503 {
		t.Fatalf("err = %v", err)
	}
}
