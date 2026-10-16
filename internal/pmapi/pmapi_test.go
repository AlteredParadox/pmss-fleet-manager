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
	"context"
	"math"
	"net/http"
	"net/http/httptest"
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
