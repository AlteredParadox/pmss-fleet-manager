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
	"errors"
	"path/filepath"
	"testing"
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
	if err != nil || got.Port != 22 || got.Password != "pw" || got.Type != "seedbox" {
		t.Fatalf("get: %+v %v", got, err)
	}
	got.Password = "" // keep
	got.Plan = "Dragon-R"
	if err := s.UpdateBox(ctx, got, Clears{}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetBox(ctx, b.ID)
	if got.Password != "pw" || got.Plan != "Dragon-R" {
		t.Fatalf("update kept secret? %+v", got)
	}
	got.QbtPass = "newqbt"
	if err := s.UpdateBox(ctx, got, Clears{Password: true}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetBox(ctx, b.ID)
	if got.Password != "" || got.QbtPass != "newqbt" {
		t.Fatalf("clear+replace in one update: %+v", got)
	}
	gone := *got
	gone.ID = 9999
	if err := s.UpdateBox(ctx, &gone, Clears{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("update of a missing row: %v", err)
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

func TestOneActiveAlertPerKey(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "d", "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	b := &Box{Name: "x", Host: "h", User: "u", Port: 22, Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	if created, err := s.RaiseAlert(ctx, b.ID, "k", "warn", "one"); err != nil || !created {
		t.Fatalf("first raise: %v %v", created, err)
	}
	// A concurrent evaluation that lost the race hits the partial unique
	// index on its INSERT; RaiseAlert must treat that as "already raised".
	now := "2026-01-01T00:00:00Z"
	if _, err := s.db.ExecContext(ctx, `INSERT INTO alerts (box_id, key, severity, title, active, acked, created_at, updated_at) VALUES (?,?,?,?,1,0,?,?)`, b.ID, "k", "warn", "dup", now, now); err == nil {
		t.Fatal("duplicate active alert must be rejected by the index")
	}
	if created, err := s.RaiseAlert(ctx, b.ID, "k", "bad", "two"); err != nil || created {
		t.Fatalf("second raise: %v %v", created, err)
	}
	if err := s.ResolveAlert(ctx, b.ID, "k"); err != nil {
		t.Fatal(err)
	}
	if created, err := s.RaiseAlert(ctx, b.ID, "k", "warn", "three"); err != nil || !created {
		t.Fatalf("raise after resolve: %v %v (resolved rows must not block a new one)", created, err)
	}
}

func TestSetHostKeyOnlyForUnchangedEndpoint(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "d", "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	b := &Box{Name: "x", Host: "Old.example", Port: 22, User: "u", Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	// A dial of a different host must not pin onto the record.
	s.SetHostKey(ctx, b.ID, "new.example", 22, "ssh-ed25519 WRONG")
	if g, _ := s.GetBox(ctx, b.ID); g.HostKey != "" {
		t.Fatal("key from another host must not be pinned")
	}
	s.SetHostKey(ctx, b.ID, "old.EXAMPLE", 22, "ssh-ed25519 RIGHT")
	if g, _ := s.GetBox(ctx, b.ID); g.HostKey != "ssh-ed25519 RIGHT" {
		t.Fatal("same endpoint (case-insensitive) must pin")
	}
	s.SetHostKey(ctx, b.ID, "old.example", 22, "ssh-ed25519 OTHER")
	if g, _ := s.GetBox(ctx, b.ID); g.HostKey != "ssh-ed25519 RIGHT" {
		t.Fatal("an existing pin must not be replaced")
	}
}
