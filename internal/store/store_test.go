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
	"io/fs"
	"os"
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
	// Escalation is a material change (announced); the same raise again is not.
	if changed, _ := s.RaiseAlert(ctx, b.ID, "disk", "bad", "disk at 99%"); !changed {
		t.Fatal("escalation must report a change")
	}
	if changed, _ := s.RaiseAlert(ctx, b.ID, "disk", "bad", "disk at 99%"); changed {
		t.Fatal("unchanged alert must be a no-op")
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

// A data directory with characters that mean something in a SQLite file
// URI still opens the file that was created and chmodded (GPT audit
// 2026-10-22 finding 10).
func TestOpenPathWithURICharacters(t *testing.T) {
	ctx := context.Background()
	dir := filepath.Join(t.TempDir(), "odd ?dir#%41 x")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "fleet.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBox(ctx, &Box{Name: "a", Host: "h", User: "u", Port: 22}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
		t.Fatalf("the database is not at the configured path: %v", err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) > 3 {
		t.Fatalf("unexpected files in the data dir: %v", entries)
	}
	s, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if boxes, err := s.ListBoxes(ctx); err != nil || len(boxes) != 1 {
		t.Fatalf("reopen: %v %d boxes", err, len(boxes))
	}
}

// ResolveBoxAlerts resolves a box's active alerts except the named keys
// (the renewal alert belongs to the record, not the machine), and
// DeleteCache drops only the named kinds.
func TestResolveBoxAlertsAndDeleteCache(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := &Box{Name: "a", Host: "h", User: "u", Port: 22}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"disk", "unreachable", "renewal"} {
		if _, err := s.RaiseAlert(ctx, b.ID, k, "warn", k); err != nil {
			t.Fatal(err)
		}
	}
	n, err := s.ResolveBoxAlerts(ctx, b.ID, "renewal")
	if err != nil || n != 2 {
		t.Fatalf("resolved %d, %v", n, err)
	}
	keys, _ := s.ActiveAlertKeys(ctx, b.ID)
	if len(keys) != 1 || keys[0] != "renewal" {
		t.Fatalf("active after resolve: %v", keys)
	}
	for _, k := range []string{"state", "du", "qbt_auth"} {
		if err := s.SetCache(ctx, b.ID, k, k); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.DeleteCache(ctx, b.ID, "state", "du"); err != nil {
		t.Fatal(err)
	}
	var v string
	for k, want := range map[string]error{"state": ErrNotFound, "du": ErrNotFound, "qbt_auth": nil} {
		if _, err := s.GetCache(ctx, b.ID, k, &v); !errors.Is(err, want) {
			t.Errorf("%s after delete: %v, want %v", k, err, want)
		}
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
	if changed, err := s.RaiseAlert(ctx, b.ID, "k", "warn", "one"); err != nil || changed {
		t.Fatalf("same raise again: %v %v", changed, err)
	}
	if changed, err := s.RaiseAlert(ctx, b.ID, "k", "bad", "two"); err != nil || !changed {
		t.Fatalf("escalated raise: %v %v", changed, err)
	}
	if err := s.ResolveAlert(ctx, b.ID, "k"); err != nil {
		t.Fatal(err)
	}
	if created, err := s.RaiseAlert(ctx, b.ID, "k", "warn", "three"); err != nil || !created {
		t.Fatalf("raise after resolve: %v %v (resolved rows must not block a new one)", created, err)
	}
}

func TestEscalatedAlertIsUnacked(t *testing.T) {
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
	unacked := func() int {
		t.Helper()
		n, err := s.CountUnacked(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	if _, err := s.RaiseAlert(ctx, b.ID, "disk", "warn", "disk at 92%"); err != nil {
		t.Fatal(err)
	}
	if err := s.AckAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RaiseAlert(ctx, b.ID, "disk", "warn", "disk at 94%"); err != nil {
		t.Fatal(err)
	}
	if n := unacked(); n != 0 {
		t.Fatalf("same severity must keep the ack, unacked = %d", n)
	}
	if _, err := s.RaiseAlert(ctx, b.ID, "disk", "bad", "disk over quota"); err != nil {
		t.Fatal(err)
	}
	if n := unacked(); n != 1 {
		t.Fatalf("warn escalated to bad must be unacked, unacked = %d", n)
	}
	if err := s.AckAll(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RaiseAlert(ctx, b.ID, "disk", "warn", "disk at 96%"); err != nil {
		t.Fatal(err)
	}
	if n := unacked(); n != 0 {
		t.Fatalf("a lower severity must keep the ack, unacked = %d", n)
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

// A data directory the process cannot write must surface as fs.ErrPermission
// so main can print the chown / --user hint instead of a bare error.
func TestOpenUnwritableDirIsPermissionError(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("root ignores directory modes")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	_, err := Open(filepath.Join(dir, "test.db"))
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Open in a read-only dir: %v, want fs.ErrPermission", err)
	}
	_, err = Open(filepath.Join(dir, "sub", "test.db"))
	if !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("Open with an uncreatable subdir: %v, want fs.ErrPermission", err)
	}
}

func TestAppsWatchColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "w.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	b := &Box{Name: "x", Host: "h", User: "u", Port: 22, Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, _ := s.GetBox(ctx, b.ID)
	if got.AppsWatch == nil || len(got.AppsWatch) != 0 {
		t.Fatalf("default watch list must be empty, not nil: %#v", got.AppsWatch)
	}
	if err := s.SetAppsWatch(ctx, b.ID, []string{"radarr", "other:bot.py"}); err != nil {
		t.Fatal(err)
	}
	// A box edit must not touch the list.
	got, _ = s.GetBox(ctx, b.ID)
	got.Plan = "p"
	got.AppsWatch = nil
	if err := s.UpdateBox(ctx, got, Clears{}); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetBox(ctx, b.ID)
	if len(got.AppsWatch) != 2 || got.AppsWatch[0] != "radarr" {
		t.Fatalf("watch list: %v", got.AppsWatch)
	}
	if err := s.SetAppsWatch(ctx, 999, nil); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing box: %v", err)
	}
	if err := s.SetAppsWatch(ctx, b.ID, nil); err != nil {
		t.Fatal(err)
	}
	got, _ = s.GetBox(ctx, b.ID)
	if len(got.AppsWatch) != 0 {
		t.Fatalf("cleared: %v", got.AppsWatch)
	}
	// Reopening runs the migration again: the column must not be re-added.
	s.Close()
	s, err = Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer s.Close()
	if _, err := s.GetBox(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
}

// SetHostKey pins only an unpinned row at the same endpoint and says so,
// which is what the poller relies on to refuse an unpinned connection.
func TestSetHostKeyReportsPin(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "p.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	b := &Box{Name: "x", Host: "H.example", User: "u", Port: 22, Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	if pinned, err := s.SetHostKey(ctx, b.ID, "h.example", 22, "ssh-ed25519 AAA"); err != nil || !pinned {
		t.Fatalf("first pin (host compared case-insensitively): %v %v", pinned, err)
	}
	if pinned, err := s.SetHostKey(ctx, b.ID, "h.example", 22, "ssh-ed25519 BBB"); err != nil || pinned {
		t.Fatalf("a second key must not replace the pin: %v %v", pinned, err)
	}
	if pinned, _ := s.SetHostKey(ctx, b.ID, "other.example", 22, "ssh-ed25519 CCC"); pinned {
		t.Fatal("another endpoint must not be pinned")
	}
	got, _ := s.GetBox(ctx, b.ID)
	if got.HostKey != "ssh-ed25519 AAA" {
		t.Fatalf("pin: %q", got.HostKey)
	}
}

// UpdateBox is a compare-and-set on the revision, and the host-key pin is
// decided inside the statement: kept unless forgotten or the endpoint
// changed, filled when empty.
func TestUpdateBoxRevisionAndPin(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "r.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	b := &Box{Name: "x", Host: "h", User: "u", Port: 22, Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	a, _ := s.GetBox(ctx, b.ID) // two tabs load revision 0
	c, _ := s.GetBox(ctx, b.ID)
	a.Name = "from A"
	if err := s.UpdateBox(ctx, a, Clears{}); err != nil || a.Revision != 1 {
		t.Fatalf("first edit: %v rev %d", err, a.Revision)
	}
	c.Name = "from C"
	if err := s.UpdateBox(ctx, c, Clears{}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale edit must be refused, got %v", err)
	}
	got, _ := s.GetBox(ctx, b.ID)
	if got.Name != "from A" || got.Revision != 1 {
		t.Fatalf("after stale edit: %+v", got)
	}
	// Pin rules. A poll pins between the edit's read and its write: kept.
	e, _ := s.GetBox(ctx, b.ID)
	if pinned, _ := s.SetHostKey(ctx, b.ID, "h", 22, "ssh-ed25519 POLL"); !pinned {
		t.Fatal("pin")
	}
	e.HostKey = "" // the edit carries no key
	if err := s.UpdateBox(ctx, e, Clears{}); err != nil {
		t.Fatal(err)
	}
	if got, _ = s.GetBox(ctx, b.ID); got.HostKey != "ssh-ed25519 POLL" {
		t.Fatalf("a pin stored meanwhile must survive an unrelated edit: %q", got.HostKey)
	}
	// A supplied key does not replace an existing pin, forget clears it,
	// an empty pin is filled, a changed endpoint takes the supplied key.
	e, _ = s.GetBox(ctx, b.ID)
	e.HostKey = "ssh-ed25519 OTHER"
	s.UpdateBox(ctx, e, Clears{})
	if got, _ = s.GetBox(ctx, b.ID); got.HostKey != "ssh-ed25519 POLL" {
		t.Fatalf("existing pin replaced: %q", got.HostKey)
	}
	e, _ = s.GetBox(ctx, b.ID)
	s.UpdateBox(ctx, e, Clears{ForgetHostKey: true})
	if got, _ = s.GetBox(ctx, b.ID); got.HostKey != "" {
		t.Fatalf("forget: %q", got.HostKey)
	}
	e, _ = s.GetBox(ctx, b.ID)
	e.HostKey = "ssh-ed25519 FILL"
	s.UpdateBox(ctx, e, Clears{})
	if got, _ = s.GetBox(ctx, b.ID); got.HostKey != "ssh-ed25519 FILL" {
		t.Fatalf("fill: %q", got.HostKey)
	}
	e, _ = s.GetBox(ctx, b.ID)
	e.Host, e.HostKey = "h2", ""
	s.UpdateBox(ctx, e, Clears{ReplaceHostKey: true})
	if got, _ = s.GetBox(ctx, b.ID); got.HostKey != "" || got.Host != "h2" {
		t.Fatalf("replace on move: %+v", got)
	}
	gone := *got
	gone.ID = 9999
	if err := s.UpdateBox(ctx, &gone, Clears{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing row: %v", err)
	}
}
