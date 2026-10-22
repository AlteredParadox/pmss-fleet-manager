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
