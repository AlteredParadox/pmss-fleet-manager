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

package pmapi

import (
	"bytes"
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
)

const sample = `{"client_id":54311,"generated_at":"2026-10-16T20:12:08Z","currency":"EUR","services":[
{"id":72645,"product":"Dragon-R Trophy 100 Years GiveAway","group":"The Eternal Väinämöinen","domain":"le4-0-103-172typhon.pulsedmedia.com","status":"Active","registered":"2026-09-04","next_due":"2027-05-03","billing_cycle":"Quarterly","recurring_amount":"44.97",
 "seedbox":{"username":"rc2","login_url":"https://le4-0-103-172typhon.pulsedmedia.com/user-rc2/","permanent_address":"http://f4b5738625319f86.mcx.fi/",
 "usage":{"tracked_since":"2026-09-08","sent_bytes":2905931361157,"received_bytes":1915497397,"cpu_hours":1,"cpu_since":"2026-10-06","disk_used_bytes":307260497920,"disk_quota_bytes":9910637035520,"torrents":7}}},
{"id":1,"product":"Something","group":"g","domain":"x.example","status":"Cancelled","registered":"2026-01-01","next_due":"","billing_cycle":"Free Account","recurring_amount":0}
]}`

func TestParse(t *testing.T) {
	f, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if f.Currency != "EUR" || len(f.Services) != 2 {
		t.Fatalf("bad feed: %+v", f)
	}
	s := f.Services[0]
	if s.Username() != "rc2" || s.Seedbox.Usage.Torrents != 7 || float64(s.RecurringAmount) != 44.97 {
		t.Fatalf("bad service: %+v", s)
	}
	if m := s.Monthly(); math.Abs(m-14.99) > 0.001 {
		t.Fatalf("monthly = %v", m)
	}
	if f.Services[1].Monthly() != 0 || f.Services[1].Username() != "" {
		t.Fatalf("free service should normalise to 0")
	}
	// Not a feed: an error envelope, a bare object, a wrong-typed member.
	for _, body := range []string{`{}`, `{"error":"rate limited"}`, `{"services":"nope"}`, `[]`, `{"client_id":1}`} {
		if _, err := Parse([]byte(body)); err == nil {
			t.Errorf("%s must not parse as an account", body)
		}
	}
	// The real "no services": an explicit empty list or null.
	for _, body := range []string{`{"client_id":1,"currency":"EUR","services":[]}`, `{"client_id":1,"services":null}`} {
		if f, err := Parse([]byte(body)); err != nil || len(f.Services) != 0 {
			t.Errorf("%s: %v %+v", body, err, f)
		}
	}
}

func TestFetchRefusesOversizeBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"services":[`))
		w.Write(bytes.Repeat([]byte(`{"id":1},`), (maxFeedBytes/9)+1))
		w.Write([]byte(`{"id":1}]}`))
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL}
	if _, _, err := c.Fetch(context.Background(), "k"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversize feed: %v", err)
	}
}

func TestFetch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "k" {
			w.WriteHeader(401)
			return
		}
		w.Write([]byte(sample))
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL}
	if _, _, err := c.Fetch(context.Background(), "wrong"); err == nil {
		t.Fatal("expected auth error")
	}
	f, raw, err := c.Fetch(context.Background(), "k")
	if err != nil || len(raw) == 0 || f.ClientID != 54311 {
		t.Fatalf("fetch: %v %+v", err, f)
	}
}

func TestParseAmount(t *testing.T) {
	cases := map[string]float64{"11.94": 11.94, "€11.94": 11.94, "1,234.56": 1234.56, "11,94": 11.94, "1,234": 1234, "": 0, "abc": 0, "-5.5": -5.5}
	for in, want := range cases {
		if got := parseAmount(in); math.Abs(got-want) > 1e-9 {
			t.Errorf("%q: got %v want %v", in, got, want)
		}
	}
	if _, err := Parse([]byte(`{"services":[{"id":1,"recurring_amount":true}]}`)); err != nil {
		t.Fatalf("odd amount must not reject the feed: %v", err)
	}
}

// The JSON token is decoded before it is normalised: an escaped string
// keeps its decimal point and a number its exponent (daybreak blue audit
// of e8fe98c).
func TestAmountDecodesJSON(t *testing.T) {
	for in, want := range map[string]float64{
		`"11.94"`: 11.94, `"1\u002e99"`: 1.99, `"\u20ac5,50"`: 5.5, `1e3`: 1000, `44.97`: 44.97,
		`null`: 0, `true`: 0, `{}`: 0, `1e300`: 0, `"x"`: 0,
	} {
		var a Amount
		if err := a.UnmarshalJSON([]byte(in)); err != nil || math.Abs(float64(a)-want) > 1e-9 {
			t.Errorf("%s: %v %v, want %v", in, float64(a), err, want)
		}
	}
}

// The feed sends cpu_hours with one decimal, and any usage counter may be
// null; neither may reject the feed and with it every service on the account.
func TestParseUsage(t *testing.T) {
	f, err := Parse([]byte(`{"services":[{"id":1,"seedbox":{"username":"a","usage":{"cpu_hours":3.4,"torrents":2}}}]}`))
	if err != nil {
		t.Fatalf("fractional cpu_hours must decode: %v", err)
	}
	if u := f.Services[0].Seedbox.Usage; u.CPUHours == nil || *u.CPUHours != 3.4 || u.Torrents != 2 {
		t.Fatalf("bad usage: %+v", u)
	}
	f, err = Parse([]byte(`{"services":[{"id":1,"seedbox":{"username":"a","usage":{"cpu_hours":null,"received_bytes":null,"disk_quota_bytes":null,"torrents":null}}}]}`))
	if err != nil {
		t.Fatalf("null usage counters must decode: %v", err)
	}
	if u := f.Services[0].Seedbox.Usage; u.CPUHours != nil || u.ReceivedBytes != 0 || u.DiskQuotaBytes != 0 || u.Torrents != 0 {
		t.Fatalf("bad usage: %+v", u)
	}
}

// The key is a custom header, which Go forwards across redirects to any
// host; the client must refuse to follow one instead.
func TestFetchRefusesRedirect(t *testing.T) {
	var leaked bool
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-API-Key") != "" {
			leaked = true
		}
		w.Write([]byte(sample))
	}))
	defer other.Close()
	feed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer feed.Close()
	c := &Client{URL: feed.URL}
	if _, _, err := c.Fetch(context.Background(), "k"); err == nil {
		t.Fatal("redirected fetch must fail")
	}
	if leaked {
		t.Fatal("API key was sent to the redirect target")
	}
}

// Parse refuses what Fetch would never hand it but a cache might: a body
// over the byte cap, and a services array longer than an account can
// have, counted before the services are decoded.
func TestParseRefusesOversizeAndTooManyServices(t *testing.T) {
	big := []byte(`{"services":[` + strings.Repeat(" ", maxFeedBytes) + `]}`)
	if _, err := Parse(big); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("oversize body: %v", err)
	}
	feed := func(n int) []byte {
		return []byte(`{"client_id":1,"currency":"EUR","services":[` + strings.TrimSuffix(strings.Repeat("{},", n), ",") + `]}`)
	}
	if f, err := Parse(feed(maxServices)); err != nil || len(f.Services) != maxServices {
		t.Fatalf("%d services: %v", maxServices, err)
	}
	if _, err := Parse(feed(maxServices + 1)); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("%d services: %v", maxServices+1, err)
	}
	if f, err := Parse([]byte(`{"services":null}`)); err != nil || len(f.Services) != 0 {
		t.Fatalf("null services: %v", err)
	}
	if _, err := Parse([]byte(`{"services":{"a":1}}`)); err == nil {
		t.Fatal("an object where the array goes must be an error")
	}
	if _, err := Parse([]byte(`{"services":[{},`)); err == nil {
		t.Fatal("a cut array must be an error")
	}
	// The decoder matches names without regard to case and keeps the last
	// duplicate: the count and the decode must see the same array.
	many := strings.TrimSuffix(strings.Repeat("{},", maxServices+1), ",")
	if _, err := Parse([]byte(`{"services":[],"Services":[` + many + `]}`)); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("mixed-case duplicate: %v", err)
	}
	if f, err := Parse([]byte(`{"Services":[{}]}`)); err != nil || len(f.Services) != 1 {
		t.Fatalf("a capitalised member alone is the member: %v", err)
	}
	// An oversized array followed by an empty duplicate: the empty one is
	// the member, and the big one costs its bytes once, not a Service each.
	dup := []byte(`{"services":[` + strings.TrimSuffix(strings.Repeat("{},", 200000), ",") + `],"services":[]}`)
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	f, err := Parse(dup)
	runtime.ReadMemStats(&after)
	if err != nil || len(f.Services) != 0 {
		t.Fatalf("duplicate member: %v, %d services", err, len(f.Services))
	}
	if alloc := after.TotalAlloc - before.TotalAlloc; alloc > 8*uint64(len(dup)) {
		t.Fatalf("the discarded array was decoded: %d bytes allocated for a %d-byte body", alloc, len(dup))
	}
}
