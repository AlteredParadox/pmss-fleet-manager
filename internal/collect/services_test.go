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
	"testing"
	"time"

	"pmss-fleet-manager/internal/pmapi"
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
