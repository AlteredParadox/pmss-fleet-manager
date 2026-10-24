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
	"database/sql"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pmss-fleet-manager/internal/secrets"
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
	st.PMDBEnabled, st.PMDBUploadKey = true, "words-that-make-a-passphrase"
	if err := s.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	st, _ = s.GetSettings(ctx)
	if st.PollIntervalSec != 60 || st.Theme != "light" || st.DiskThresholdPct != 90 || !st.PMDBEnabled || st.PMDBUploadKey != "words-that-make-a-passphrase" {
		t.Fatalf("settings: %+v", st)
	}
	if pub := st.Public(); pub.PMDBUploadKey != "" || !pub.HasPMDBUploadKey || pub.PMAPIKey != "" {
		t.Fatalf("public settings leak a key: %+v", pub)
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

func mustSealer(t *testing.T, secret string) *secrets.Sealer {
	t.Helper()
	sl, err := secrets.New([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	return sl
}

// rawSecrets reads the stored forms straight from the tables.
func rawSecrets(t *testing.T, s *Store, id int64) (pw, qp, apiKey string) {
	t.Helper()
	if err := s.db.QueryRow(`SELECT password, qbt_pass FROM boxes WHERE id=?`, id).Scan(&pw, &qp); err != nil {
		t.Fatal(err)
	}
	apiKey, err := s.GetValue(context.Background(), "pm_api_key")
	if err != nil {
		t.Fatal(err)
	}
	return pw, qp, apiKey
}

func TestSealedSecretsLifecycle(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "d", "test.db")
	const keyA, keyB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

	// 1. A database from before any secret: plain text.
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b := &Box{Name: "seed-01", Host: "odin.example", User: "ragnar", Password: "pw", QbtPass: "qbt", Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	st := DefaultSettings()
	st.PMAPIKey = "apikey"
	if err := s.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	if pw, _, _ := rawSecrets(t, s, b.ID); pw != "pw" {
		t.Fatalf("plain store wrote %q", pw)
	}
	s.Close()

	// 2. First start with a secret: everything is sealed in place, nothing
	// changes for the callers, the revision is untouched.
	s, err = Open(path, WithSealer(mustSealer(t, keyA)))
	if err != nil {
		t.Fatal(err)
	}
	if n := s.ResealedOnOpen(); n != 3 {
		t.Fatalf("resealed %d, want 3", n)
	}
	pw, qp, ak := rawSecrets(t, s, b.ID)
	for _, v := range []string{pw, qp, ak} {
		if !secrets.IsSealed(v) {
			t.Fatalf("not sealed: %q", v)
		}
	}
	got, err := s.GetBox(ctx, b.ID)
	if err != nil || got.Password != "pw" || got.QbtPass != "qbt" || !got.HasPassword || !got.HasQbtPass || got.Revision != 0 {
		t.Fatalf("after reseal: %+v %v", got, err)
	}
	if gs, err := s.GetSettings(ctx); err != nil || gs.PMAPIKey != "apikey" {
		t.Fatalf("settings after reseal: %+v %v", gs, err)
	}
	// A write seals; a blank keeps the sealed value; a clear removes it;
	// the Box handed in keeps its plaintext for the caller.
	got.Password, got.QbtPass = "", "qbt2"
	if err := s.UpdateBox(ctx, got, Clears{}); err != nil {
		t.Fatal(err)
	}
	if got.QbtPass != "qbt2" {
		t.Fatal("UpdateBox replaced the caller's plaintext")
	}
	pw2, qp2, _ := rawSecrets(t, s, b.ID)
	if pw2 != pw || qp2 == qp || !secrets.IsSealed(qp2) {
		t.Fatalf("update: pw %q->%q qp %q->%q", pw, pw2, qp, qp2)
	}
	got, _ = s.GetBox(ctx, b.ID)
	if got.Password != "pw" || got.QbtPass != "qbt2" {
		t.Fatalf("after update: %+v", got)
	}
	if err := s.UpdateBox(ctx, got, Clears{QbtPass: true}); err != nil {
		t.Fatal(err)
	}
	if _, qp3, _ := rawSecrets(t, s, b.ID); qp3 != "" {
		t.Fatalf("clear left %q", qp3)
	}
	if n := s.ResealedOnOpen(); n != 3 {
		t.Fatalf("resealed count changed to %d", n)
	}
	s.Close()

	// 3. Second start with the same secret: nothing to do.
	s, err = Open(path, WithSealer(mustSealer(t, keyA)))
	if err != nil {
		t.Fatal(err)
	}
	if s.ResealedOnOpen() != 0 || s.ForgottenOnOpen() != 0 {
		t.Fatalf("second open rewrote: %d %d", s.ResealedOnOpen(), s.ForgottenOnOpen())
	}
	s.Close()

	// 4. No secret, or another one: refused, naming the row.
	if _, err := Open(path); !errors.Is(err, secrets.ErrNoKey) || !strings.Contains(err.Error(), "stored credentials") {
		t.Fatalf("open without key: %v", err)
	}
	if _, err := Open(path, WithSealer(mustSealer(t, keyB))); !errors.Is(err, secrets.ErrWrongKey) {
		t.Fatalf("open with other key: %v", err)
	}

	// 5. Rotation: previous secret opens them, the new one stores them.
	s, err = Open(path, WithSealer(mustSealer(t, keyB)), WithPreviousSealer(mustSealer(t, keyA)))
	if err != nil {
		t.Fatal(err)
	}
	if n := s.ResealedOnOpen(); n != 2 { // password and API key; the WebUI password was cleared
		t.Fatalf("rotation resealed %d", n)
	}
	if got, err := s.GetBox(ctx, b.ID); err != nil || got.Password != "pw" {
		t.Fatalf("after rotation: %+v %v", got, err)
	}
	s.Close()
	if _, err := Open(path, WithSealer(mustSealer(t, keyA))); !errors.Is(err, secrets.ErrWrongKey) {
		t.Fatalf("old key still works after rotation: %v", err)
	}

	// 6. Switching encryption off: previous secret only, stored plain again.
	s, err = Open(path, WithPreviousSealer(mustSealer(t, keyB)))
	if err != nil {
		t.Fatal(err)
	}
	if pw, _, ak := rawSecrets(t, s, b.ID); pw != "pw" || ak != "apikey" {
		t.Fatalf("not plain after switching off: %q %q", pw, ak)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatalf("plain database refused: %v", err)
	}
	s.Close()

	// 7. Lost secret: forgetting clears only the unreadable ones.
	s, err = Open(path, WithSealer(mustSealer(t, keyA)))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path, WithForgetUnreadable())
	if err != nil {
		t.Fatal(err)
	}
	if s.ForgottenOnOpen() != 2 || s.ResealedOnOpen() != 0 {
		t.Fatalf("forget: %d forgotten, %d resealed", s.ForgottenOnOpen(), s.ResealedOnOpen())
	}
	got, _ = s.GetBox(ctx, b.ID)
	gs, _ := s.GetSettings(ctx)
	if got.Password != "" || got.HasPassword || gs.PMAPIKey != "" || got.Name != "seed-01" {
		t.Fatalf("after forget: %+v %+v", got, gs)
	}
	s.Close()
}

// The WAL is emptied after a reseal, so the plaintext is not left in it.
func TestResealLeavesNoPlaintextInFiles(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b := &Box{Name: "seed-01", Host: "odin.example", User: "ragnar", Password: "zebra-unique-password", Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path, WithSealer(mustSealer(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")))
	if err != nil {
		t.Fatal(err)
	}
	for _, side := range []string{"", "-wal"} {
		data, err := os.ReadFile(path + side)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "zebra-unique-password") {
			t.Fatalf("plaintext still in %s", path+side)
		}
	}
	s.Close()
}

// Without a secret, a password that happens to start with the ciphertext
// prefix is a password: the marker, not the prefix, says what a value is.
func TestPrefixedPlaintextStaysPlaintext(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	const odd = secrets.Prefix + "not-really-encrypted"
	b := &Box{Name: "seed-01", Host: "odin.example", User: "ragnar", Password: odd, Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetBox(ctx, b.ID); err != nil || got.Password != odd {
		t.Fatalf("read back: %+v %v", got, err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatalf("reopen refused a plaintext database: %v", err)
	}
	if got, _ := s.GetBox(ctx, b.ID); got.Password != odd {
		t.Fatalf("after reopen: %q", got.Password)
	}
	s.Close()
	// Switching encryption on seals it like any other password.
	s, err = Open(path, WithSealer(mustSealer(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")))
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetBox(ctx, b.ID); got.Password != odd {
		t.Fatalf("after sealing: %q", got.Password)
	}
	if pw, _, _ := rawSecrets(t, s, b.ID); pw == odd {
		t.Fatal("not sealed")
	}
	s.Close()
}

// A cleanup that fails after the rewrite committed is retried at the next
// open, and the marker that asks for it is cleared only once it succeeded.
func TestResealCleanupIsRetried(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b := &Box{Name: "seed-01", Host: "odin.example", User: "ragnar", Password: "zebra-unique-password", Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	s.Close()

	vacuumSQL = "VACUUM no_such_schema"
	defer func() { vacuumSQL = "VACUUM" }()
	sl := mustSealer(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	if _, err := Open(path, WithSealer(sl)); err == nil || !strings.Contains(err.Error(), "retried at the next start") {
		t.Fatalf("failed vacuum: %v", err)
	}
	// The rewrite is committed and the cleanup is owed.
	s, err = Open(path, WithSealer(sl))
	if err == nil {
		s.Close()
		t.Fatal("second open with a failing vacuum succeeded")
	}
	vacuumSQL = "VACUUM"
	s, err = Open(path, WithSealer(sl))
	if err != nil {
		t.Fatal(err)
	}
	if s.ResealedOnOpen() != 0 {
		t.Fatalf("rows rewritten again: %d", s.ResealedOnOpen())
	}
	if v, _ := s.GetValue(ctx, cleanupKey); v != "0" {
		t.Fatalf("cleanup marker %q after success", v)
	}
	if v, _ := s.GetValue(ctx, sealedKey); v != "1" {
		t.Fatalf("sealed marker %q", v)
	}
	if got, err := s.GetBox(ctx, b.ID); err != nil || got.Password != "zebra-unique-password" {
		t.Fatalf("after retry: %+v %v", got, err)
	}
	for _, side := range []string{"", "-wal"} {
		data, err := os.ReadFile(path + side)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "zebra-unique-password") {
			t.Fatalf("plaintext still in %s", path+side)
		}
	}
	s.Close()
}

// Switching encryption on with no live credentials still scrubs the file:
// a password cleared earlier may sit in free pages.
func TestEnablingWithNoCredentialsScrubs(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b := &Box{Name: "seed-01", Host: "odin.example", User: "ragnar", Password: "zebra-unique-password", Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateBox(ctx, b, Clears{Password: true}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = Open(path, WithSealer(mustSealer(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")))
	if err != nil {
		t.Fatal(err)
	}
	if s.ResealedOnOpen() != 0 {
		t.Fatalf("resealed %d", s.ResealedOnOpen())
	}
	if v, _ := s.GetValue(ctx, sealedKey); v != "1" {
		t.Fatalf("sealed marker %q", v)
	}
	if v, _ := s.GetValue(ctx, cleanupKey); v != "0" {
		t.Fatalf("cleanup marker %q: the scrub did not run or did not finish", v)
	}
	for _, side := range []string{"", "-wal"} {
		data, err := os.ReadFile(path + side)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "zebra-unique-password") {
			t.Fatalf("plaintext still in %s", path+side)
		}
	}
	s.Close()
}

// A value sealed under another secret that reaches the read path (written
// behind the store's back after Open) is an error, not a password.
func TestReadPathRefusesForeignCiphertext(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	a, b := mustSealer(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"), mustSealer(t, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
	s, err := Open(path, WithSealer(a))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	box := &Box{Name: "seed-01", Host: "odin.example", User: "ragnar", Password: "pw", QbtPass: "qp", Enabled: true}
	if err := s.CreateBox(ctx, box); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`UPDATE boxes SET password=? WHERE id=?`, b.Seal(labelPassword, "x"), box.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetBox(ctx, box.ID); !errors.Is(err, secrets.ErrWrongKey) || !strings.Contains(err.Error(), "SSH password") {
		t.Fatalf("foreign SSH password: %v", err)
	}
	if _, err := s.db.Exec(`UPDATE boxes SET password='', qbt_pass=? WHERE id=?`, b.Seal(labelQbtPass, "x"), box.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ListBoxes(ctx); !errors.Is(err, secrets.ErrWrongKey) || !strings.Contains(err.Error(), "WebUI password") {
		t.Fatalf("foreign WebUI password: %v", err)
	}
	for _, kv := range []struct{ key, what string }{{"pm_api_key", "API key"}, {"pmdb_upload_key", "pmdb upload key"}} {
		if err := s.SetValue(ctx, kv.key, b.Seal("settings."+kv.key, "x")); err != nil {
			t.Fatal(err)
		}
		if _, err := s.GetSettings(ctx); !errors.Is(err, secrets.ErrWrongKey) || !strings.Contains(err.Error(), kv.what) {
			t.Fatalf("foreign %s: %v", kv.key, err)
		}
		if err := s.SetValue(ctx, kv.key, ""); err != nil {
			t.Fatal(err)
		}
	}
}

// A checkpoint that cannot complete because another connection holds the
// log leaves the cleanup owed, and the open is refused with that reason.
func TestScrubRefusedWhileAnotherConnectionReads(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	b := &Box{Name: "seed-01", Host: "odin.example", User: "ragnar", Password: "pw", Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	s.Close()
	other, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(100)")
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	tx, err := other.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM boxes`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	sl := mustSealer(t, "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
	_, err = Open(path, WithSealer(sl))
	tx.Rollback()
	if err == nil || !strings.Contains(err.Error(), "retried at the next start") {
		t.Fatalf("open with a reader holding the log: %v", err)
	}
	// With the reader gone the retry succeeds and the marker clears.
	s, err = Open(path, WithSealer(sl))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.GetValue(ctx, cleanupKey); v != "0" {
		t.Fatalf("cleanup marker %q", v)
	}
	if got, err := s.GetBox(ctx, b.ID); err != nil || got.Password != "pw" {
		t.Fatalf("after retry: %+v %v", got, err)
	}
}

const keyA, keyB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func noPlaintextIn(t *testing.T, path, needle string) {
	t.Helper()
	for _, side := range []string{"", "-wal"} {
		data, err := os.ReadFile(path + side)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			t.Fatal(err)
		}
		if strings.Contains(string(data), needle) {
			t.Fatalf("plaintext still in %s", path+side)
		}
	}
}

// An encrypted store with no credential in it still authenticates the
// secret: the key check row does what a credential ciphertext would.
func TestEmptyEncryptedStoreAuthenticatesTheSecret(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path, WithSealer(mustSealer(t, keyA)))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetValue(ctx, checkKey); !secrets.IsSealed(v) {
		t.Fatalf("no key check written: %q", v)
	}
	s.Close()

	// Wrong secret and no secret are refused with nothing stored.
	if _, err := Open(path, WithSealer(mustSealer(t, keyB))); !errors.Is(err, secrets.ErrWrongKey) {
		t.Fatalf("wrong key on empty store: %v", err)
	}
	if _, err := Open(path); !errors.Is(err, secrets.ErrNoKey) {
		t.Fatalf("no key on empty store: %v", err)
	}
	// Rotation rewrites the check; the old secret is then refused.
	s, err = Open(path, WithSealer(mustSealer(t, keyB)), WithPreviousSealer(mustSealer(t, keyA)))
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	if _, err := Open(path, WithSealer(mustSealer(t, keyA))); !errors.Is(err, secrets.ErrWrongKey) {
		t.Fatalf("old key after rotation: %v", err)
	}
	if s, err = Open(path, WithSealer(mustSealer(t, keyB))); err != nil {
		t.Fatalf("new key after rotation: %v", err)
	}
	s.Close()
	// Switching off removes the check and marks the store plain.
	s, err = Open(path, WithPreviousSealer(mustSealer(t, keyB)))
	if err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetValue(ctx, checkKey); v != "" {
		t.Fatalf("check kept after switching off: %q", v)
	}
	if v, _ := s.GetValue(ctx, sealedKey); v != "0" {
		t.Fatalf("sealed marker %q after switching off", v)
	}
	s.Close()
	// Lost secret on an empty store: forgetting lets it start plain.
	if s, err = Open(path, WithSealer(mustSealer(t, keyA))); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path, WithForgetUnreadable()); err != nil {
		t.Fatalf("forget on empty store: %v", err)
	}
	if v, _ := s.GetValue(ctx, sealedKey); v != "0" {
		t.Fatalf("sealed marker %q after forget", v)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatalf("plain open after forget: %v", err)
	}
	s.Close()
}

// A plaintext database gets an explicit marker at its first open, so
// "never opened by this version" and "plain" can be told apart later.
func TestPlainStoreGetsExplicitMarker(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if v, _ := s.GetValue(ctx, sealedKey); v != "0" {
		t.Fatalf("marker %q", v)
	}
	if v, _ := s.GetValue(ctx, checkKey); v != "" {
		t.Fatalf("check %q in a plain store", v)
	}
}

// sealedFixture returns a sealed database under keyA holding one box with
// a password and the API key.
func sealedFixture(t *testing.T) (path string, boxID int64) {
	t.Helper()
	ctx := context.Background()
	path = filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path, WithSealer(mustSealer(t, keyA)))
	if err != nil {
		t.Fatal(err)
	}
	b := &Box{Name: "seed-01", Host: "odin.example", User: "ragnar", Password: "pw", Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	st := DefaultSettings()
	st.PMAPIKey = "apikey"
	if err := s.SaveSettings(ctx, st); err != nil {
		t.Fatal(err)
	}
	s.Close()
	return path, b.ID
}

// rawExec runs one statement against the file outside the store.
func rawExec(t *testing.T, path, query string, args ...any) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

// Metadata the open cannot trust fails closed, with the forget option as
// the documented way through.
func TestDamagedEncodingMetadataFailsClosed(t *testing.T) {
	ctx := context.Background()
	t.Run("sealed marker lost", func(t *testing.T) {
		path, id := sealedFixture(t)
		rawExec(t, path, `DELETE FROM settings WHERE key=?`, sealedKey)
		// Markers disagree: refused with or without the right key...
		if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "disagree") {
			t.Fatalf("no key: %v", err)
		}
		if _, err := Open(path, WithSealer(mustSealer(t, keyA))); err == nil || !strings.Contains(err.Error(), "disagree") {
			t.Fatalf("right key: %v", err)
		}
		// ...and forgetting with the right key keeps everything, since it all opens.
		s, err := Open(path, WithSealer(mustSealer(t, keyA)), WithForgetUnreadable())
		if err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetBox(ctx, id); err != nil || got.Password != "pw" {
			t.Fatalf("after repair: %+v %v", got, err)
		}
		if s.ForgottenOnOpen() != 0 {
			t.Fatalf("forgot %d with the right key", s.ForgottenOnOpen())
		}
		if v, _ := s.GetValue(ctx, sealedKey); v != "1" {
			t.Fatalf("marker not repaired: %q", v)
		}
		s.Close()
	})
	t.Run("both markers lost", func(t *testing.T) {
		path, id := sealedFixture(t)
		rawExec(t, path, `DELETE FROM settings WHERE key IN (?,?)`, sealedKey, checkKey)
		// The store reads as plain, but the cells are shaped like ciphertext:
		// never handed out as passwords.
		if _, err := Open(path); err == nil || !strings.Contains(err.Error(), "looks encrypted") {
			t.Fatalf("no key: %v", err)
		}
		if _, err := Open(path, WithSealer(mustSealer(t, keyB))); !errors.Is(err, secrets.ErrWrongKey) {
			t.Fatalf("wrong key: %v", err)
		}
		// The right key proves they are ours: repaired, not double-sealed.
		s, err := Open(path, WithSealer(mustSealer(t, keyA)))
		if err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetBox(ctx, id); err != nil || got.Password != "pw" {
			t.Fatalf("after repair: %+v %v", got, err)
		}
		if gs, err := s.GetSettings(ctx); err != nil || gs.PMAPIKey != "apikey" {
			t.Fatalf("settings after repair: %+v %v", gs, err)
		}
		if v, _ := s.GetValue(ctx, sealedKey); v != "1" {
			t.Fatalf("marker not repaired: %q", v)
		}
		if v, _ := s.GetValue(ctx, checkKey); !secrets.IsSealed(v) {
			t.Fatalf("check not rewritten: %q", v)
		}
		s.Close()
		// Without the key, forgetting drops them and the store is plain.
		rawExec(t, path, `DELETE FROM settings WHERE key IN (?,?)`, sealedKey, checkKey)
		s, err = Open(path, WithForgetUnreadable())
		if err != nil {
			t.Fatal(err)
		}
		if s.ForgottenOnOpen() != 2 {
			t.Fatalf("forgot %d", s.ForgottenOnOpen())
		}
		if got, _ := s.GetBox(ctx, id); got.Password != "" || got.Name != "seed-01" {
			t.Fatalf("after forget: %+v", got)
		}
		s.Close()
	})
	t.Run("key check lost", func(t *testing.T) {
		path, id := sealedFixture(t)
		rawExec(t, path, `DELETE FROM settings WHERE key=?`, checkKey)
		if _, err := Open(path, WithSealer(mustSealer(t, keyA))); err == nil || !strings.Contains(err.Error(), "disagree") {
			t.Fatalf("right key: %v", err)
		}
		s, err := Open(path, WithSealer(mustSealer(t, keyA)), WithForgetUnreadable())
		if err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetBox(ctx, id); err != nil || got.Password != "pw" {
			t.Fatalf("after repair: %+v %v", got, err)
		}
		if v, _ := s.GetValue(ctx, checkKey); !secrets.IsSealed(v) {
			t.Fatalf("check not rewritten: %q", v)
		}
		s.Close()
	})
	t.Run("key check replaced", func(t *testing.T) {
		path, _ := sealedFixture(t)
		rawExec(t, path, `UPDATE settings SET value=? WHERE key=?`, mustSealer(t, keyA).Seal(labelCheck, "something else"), checkKey)
		if _, err := Open(path, WithSealer(mustSealer(t, keyA))); !errors.Is(err, secrets.ErrWrongKey) {
			t.Fatalf("foreign check: %v", err)
		}
	})
	t.Run("malformed marker", func(t *testing.T) {
		path, id := sealedFixture(t)
		rawExec(t, path, `UPDATE settings SET value='yes' WHERE key=?`, sealedKey)
		if _, err := Open(path, WithSealer(mustSealer(t, keyA))); err == nil || !strings.Contains(err.Error(), "does not understand") {
			t.Fatalf("malformed: %v", err)
		}
		// The recovery flag the message recommends reads it as sealed:
		// with the right key everything is kept and the row rewritten...
		s, err := Open(path, WithSealer(mustSealer(t, keyA)), WithForgetUnreadable())
		if err != nil {
			t.Fatal(err)
		}
		if got, err := s.GetBox(ctx, id); err != nil || got.Password != "pw" || s.ForgottenOnOpen() != 0 {
			t.Fatalf("recovered with the right key: %+v %v, forgot %d", got, err, s.ForgottenOnOpen())
		}
		if v, _ := s.GetValue(ctx, sealedKey); v != "1" {
			t.Fatalf("marker %q after recovery", v)
		}
		s.Close()
		if s, err = Open(path, WithSealer(mustSealer(t, keyA))); err != nil {
			t.Fatalf("plain reopen after recovery: %v", err)
		}
		s.Close()
		// ...and without a key the unreadable credentials go and the store is plain.
		rawExec(t, path, `UPDATE settings SET value='yes' WHERE key=?`, sealedKey)
		if s, err = Open(path, WithForgetUnreadable()); err != nil {
			t.Fatal(err)
		}
		if s.ForgottenOnOpen() != 2 {
			t.Fatalf("forgot %d without a key", s.ForgottenOnOpen())
		}
		if v, _ := s.GetValue(ctx, sealedKey); v != "0" {
			t.Fatalf("marker %q after forgetting without a key", v)
		}
		if v, _ := s.GetValue(ctx, checkKey); v != "" {
			t.Fatalf("check %q kept in a plain store", v)
		}
		s.Close()
	})
	t.Run("damaged ciphertext", func(t *testing.T) {
		path, id := sealedFixture(t)
		rawExec(t, path, `UPDATE boxes SET password='pmss1:damaged' WHERE id=?`, id)
		if _, err := Open(path, WithSealer(mustSealer(t, keyA))); !errors.Is(err, secrets.ErrWrongKey) || !strings.Contains(err.Error(), "SSH password") {
			t.Fatalf("damaged: %v", err)
		}
		s, err := Open(path, WithSealer(mustSealer(t, keyA)), WithForgetUnreadable())
		if err != nil {
			t.Fatal(err)
		}
		if s.ForgottenOnOpen() != 1 {
			t.Fatalf("forgot %d", s.ForgottenOnOpen())
		}
		if gs, _ := s.GetSettings(ctx); gs.PMAPIKey != "apikey" {
			t.Fatal("the readable credential was dropped too")
		}
		s.Close()
	})
	t.Run("newer envelope", func(t *testing.T) {
		path, id := sealedFixture(t)
		rawExec(t, path, `UPDATE boxes SET password='pmss2:AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA' WHERE id=?`, id)
		if _, err := Open(path, WithSealer(mustSealer(t, keyA))); err == nil || !strings.Contains(err.Error(), "newer version") {
			t.Fatalf("newer envelope: %v", err)
		}
		s, err := Open(path, WithSealer(mustSealer(t, keyA)), WithForgetUnreadable())
		if err != nil {
			t.Fatal(err)
		}
		if s.ForgottenOnOpen() != 1 {
			t.Fatalf("forgot %d", s.ForgottenOnOpen())
		}
		s.Close()
	})
	t.Run("plaintext written by an older build into a sealed store", func(t *testing.T) {
		path, id := sealedFixture(t)
		rawExec(t, path, `UPDATE boxes SET qbt_pass='typed-by-old-build' WHERE id=?`, id)
		s, err := Open(path, WithSealer(mustSealer(t, keyA)))
		if err != nil {
			t.Fatal(err)
		}
		if s.ResealedOnOpen() != 1 {
			t.Fatalf("resealed %d", s.ResealedOnOpen())
		}
		if got, _ := s.GetBox(ctx, id); got.QbtPass != "typed-by-old-build" || got.Password != "pw" {
			t.Fatalf("after reseal: %+v", got)
		}
		if _, qp, _ := rawSecrets(t, s, id); !secrets.IsSealed(qp) {
			t.Fatal("old-build plaintext not sealed")
		}
		s.Close()
		noPlaintextIn(t, path, "typed-by-old-build")
	})
}

// In a store marked plain, the marker is authoritative: values shaped like
// an envelope, well-formed or of a newer version, are passwords.
func TestExplicitPlainMarkerIsAuthoritative(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	// A well-formed envelope from an unrelated secret, used as a password.
	shaped := mustSealer(t, keyB).Seal("elsewhere", "whatever")
	if v, ok := secrets.Envelope(shaped); v != 1 || !ok {
		t.Fatal("fixture is not a well-formed envelope")
	}
	b := &Box{Name: "seed-01", Host: "odin.example", User: "ragnar", Password: shaped, QbtPass: "pmss2:hunter2", Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	s.Close()
	if s, err = Open(path); err != nil {
		t.Fatalf("plain reopen: %v", err)
	}
	if got, _ := s.GetBox(ctx, b.ID); got.Password != shaped || got.QbtPass != "pmss2:hunter2" {
		t.Fatalf("plain reopen changed them: %+v", got)
	}
	s.Close()
	// Switching encryption on seals them like any other password.
	s, err = Open(path, WithSealer(mustSealer(t, keyA)))
	if err != nil {
		t.Fatalf("switch on: %v", err)
	}
	if s.ResealedOnOpen() != 2 {
		t.Fatalf("resealed %d", s.ResealedOnOpen())
	}
	if got, _ := s.GetBox(ctx, b.ID); got.Password != shaped || got.QbtPass != "pmss2:hunter2" {
		t.Fatalf("after sealing: %+v", got)
	}
	s.Close()
	if s, err = Open(path, WithSealer(mustSealer(t, keyA))); err != nil {
		t.Fatalf("sealed reopen: %v", err)
	}
	s.Close()
}

// A non-canonical spelling of the envelope is not an envelope: in a sealed
// store it is old-build plaintext and is sealed, never kept as is.
func TestNonCanonicalEnvelopeInSealedStoreIsSealed(t *testing.T) {
	ctx := context.Background()
	path, id := sealedFixture(t)
	odd := "pmss01:" + mustSealer(t, keyA).Seal(labelPassword, "x")[len(secrets.Prefix):]
	rawExec(t, path, `UPDATE boxes SET password=? WHERE id=?`, odd, id)
	s, err := Open(path, WithSealer(mustSealer(t, keyA)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.ResealedOnOpen() != 1 {
		t.Fatalf("resealed %d", s.ResealedOnOpen())
	}
	pw, _, _ := rawSecrets(t, s, id)
	if pw == odd || !secrets.IsSealed(pw) {
		t.Fatalf("left on disk as %q", pw)
	}
	if got, _ := s.GetBox(ctx, id); got.Password != odd {
		t.Fatalf("reads back %q", got.Password)
	}
}

// A cleanup row that is missing or unreadable in a store with encoding
// metadata is an owed scrub, not a cleared one.
func TestUnreadableCleanupMarkerScrubs(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct{ name, stmt string }{
		{"deleted", `DELETE FROM settings WHERE key=?`},
		{"malformed", `UPDATE settings SET value='yes' WHERE key=?`},
		{"unknown", `UPDATE settings SET value='2' WHERE key=?`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "test.db")
			s, err := Open(path)
			if err != nil {
				t.Fatal(err)
			}
			b := &Box{Name: "seed-01", Host: "odin.example", User: "ragnar", Password: "zebra-unique-password-" + strings.Repeat("x", 200), Enabled: true}
			if err := s.CreateBox(ctx, b); err != nil {
				t.Fatal(err)
			}
			if err := s.UpdateBox(ctx, b, Clears{Password: true}); err != nil {
				t.Fatal(err)
			}
			s.Close()
			// Enable encryption with the scrub failing: the rows commit, the scrub is owed.
			vacuumSQL = "VACUUM no_such_schema"
			_, err = Open(path, WithSealer(mustSealer(t, keyA)))
			vacuumSQL = "VACUUM"
			if err == nil {
				t.Fatal("open with a failing vacuum succeeded")
			}
			rawExec(t, path, tc.stmt, cleanupKey)
			s, err = Open(path, WithSealer(mustSealer(t, keyA)))
			if err != nil {
				t.Fatal(err)
			}
			if v, _ := s.GetValue(ctx, cleanupKey); v != "0" {
				t.Fatalf("cleanup marker %q: the scrub did not run", v)
			}
			s.Close()
			noPlaintextIn(t, path, "zebra-unique-password")
		})
	}
}
