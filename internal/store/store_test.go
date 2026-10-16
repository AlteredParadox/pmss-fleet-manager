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

package store

import (
	"context"
	"path/filepath"
	"testing"
	"time"
)

func TestRoundTrip(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "d", "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	b := &Box{Name: "seed-01", Host: "odin.example", User: "ragnar", Password: "pw", QbtEnabled: true, QbtPort: 8080, Enabled: true, DuEnabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetBox(ctx, b.ID)
	if err != nil || got.Port != 22 || got.Password != "pw" || !got.HasPassword || got.Type != "seedbox" {
		t.Fatalf("get: %+v %v", got, err)
	}
	got.Password = "" // keep
	got.Plan = "Dragon-R"
	if err := s.UpdateBox(ctx, got); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetBox(ctx, b.ID)
	if got.Password != "pw" || got.Plan != "Dragon-R" {
		t.Fatalf("update kept secret? %+v", got)
	}

	st, _ := s.GetSettings(ctx)
	if st.PollIntervalSec != 30 {
		t.Fatalf("defaults: %+v", st)
	}
	st.PollIntervalSec = 60
	st.Theme = "light"
	if err := s.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	st, _ = s.GetSettings(ctx)
	if st.PollIntervalSec != 60 || st.Theme != "light" || st.DiskThresholdPct != 90 {
		t.Fatalf("settings: %+v", st)
	}

	now := time.Now()
	for i := 0; i < 3; i++ {
		if err := s.AddSample(ctx, b.ID, Sample{At: now.Add(time.Duration(i) * time.Minute), OK: true, DiskUsed: int64(i), Torrents: i}); err != nil {
			t.Fatal(err)
		}
	}
	smps, _ := s.Samples(ctx, b.ID, now.Add(-time.Hour))
	if len(smps) != 3 || smps[2].Torrents != 2 {
		t.Fatalf("samples: %+v", smps)
	}

	created, err := s.RaiseAlert(ctx, b.ID, "disk", "warn", "disk at 95%")
	if err != nil || !created {
		t.Fatalf("raise: %v %v", created, err)
	}
	created, _ = s.RaiseAlert(ctx, b.ID, "disk", "bad", "disk at 99%")
	if created {
		t.Fatal("dup alert created")
	}
	n, _ := s.CountUnacked(ctx)
	if n != 1 {
		t.Fatalf("unacked %d", n)
	}
	al, _ := s.ListAlerts(ctx, 10)
	if len(al) != 1 || al[0].Title != "disk at 99%" || al[0].Severity != "bad" {
		t.Fatalf("alerts: %+v", al)
	}
	_ = s.AckAlert(ctx, al[0].ID)
	_ = s.ResolveAlert(ctx, b.ID, "disk")
	al, _ = s.ListAlerts(ctx, 10)
	if al[0].Active || !al[0].Acked || al[0].ResolvedAt == nil {
		t.Fatalf("resolve: %+v", al[0])
	}

	if err := s.SetCache(ctx, b.ID, "du", []string{"a"}); err != nil {
		t.Fatal(err)
	}
	var out []string
	if _, err := s.GetCache(ctx, b.ID, "du", &out); err != nil || len(out) != 1 {
		t.Fatalf("cache: %v %v", out, err)
	}
	if err := s.DeleteBox(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetCache(ctx, b.ID, "du", &out); err != ErrNotFound {
		t.Fatalf("cascade: %v", err)
	}
}
