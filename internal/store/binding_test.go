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
	"path/filepath"
	"testing"

	"pmss-fleet-manager/internal/secrets"
)

// rawCell reads one stored column of a box from the file, outside the store.
func rawCell(t *testing.T, path, col string, id int64) string {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var v string
	if err := db.QueryRow(`SELECT `+col+` FROM boxes WHERE id=?`, id).Scan(&v); err != nil {
		t.Fatal(err)
	}
	return v
}

// boundBackup is an encrypted backup of a store holding two password
// boxes, with their IDs.
func boundBackup(t *testing.T, dir string) (backup string, a, b int64) {
	t.Helper()
	ctx := context.Background()
	src := filepath.Join(dir, "src.db")
	s, err := Open(src, WithSealer(mustSealer(t, backupKeyA)))
	if err != nil {
		t.Fatal(err)
	}
	ba := &Box{Name: "a", Host: "odin.example", User: "ragnar", Password: "pw-a", QbtPass: "qp-a", QbtStartCmd: "start-a", Enabled: true}
	bb := &Box{Name: "b", Host: "thor.example", User: "bjorn", Password: "pw-b", Enabled: true}
	for _, x := range []*Box{ba, bb} {
		if err := s.CreateBox(ctx, x); err != nil {
			t.Fatal(err)
		}
	}
	s.Close()
	backup = filepath.Join(dir, "backup.db")
	if _, err := Backup(ctx, src, backup); err != nil {
		t.Fatal(err)
	}
	return backup, ba.ID, bb.ID
}

// The Codex audit of 1.23.0 (restore-sealed-credential-endpoint-
// rebinding): someone who can edit a backup but not decrypt it points a
// box at their own server, or moves a box's ciphertext onto a box they
// pointed there. The credential no longer opens, so the restore is
// refused before the live database is replaced, and a copy restored
// without the check does not start (or, forgetting, drops it).
func TestRestoreRefusesReboundCredentials(t *testing.T) {
	ctx := context.Background()
	sealA := WithSealer(mustSealer(t, backupKeyA))
	for name, tamper := range map[string]func(t *testing.T, path string, a, b int64){
		"host": func(t *testing.T, p string, a, _ int64) {
			rawExec(t, p, `UPDATE boxes SET host='127.0.0.1', host_key='' WHERE id=?`, a)
		},
		"host case": func(t *testing.T, p string, a, _ int64) {
			rawExec(t, p, `UPDATE boxes SET host='ODIN.example' WHERE id=?`, a)
		},
		"port": func(t *testing.T, p string, a, _ int64) { rawExec(t, p, `UPDATE boxes SET port=2222 WHERE id=?`, a) },
		"user": func(t *testing.T, p string, a, _ int64) {
			rawExec(t, p, `UPDATE boxes SET user='mallory' WHERE id=?`, a)
		},
		"WebUI password": func(t *testing.T, p string, a, b int64) {
			rawExec(t, p, `UPDATE boxes SET qbt_pass=(SELECT qbt_pass FROM boxes WHERE id=?) WHERE id=?`, a, b)
		},
		"start command": func(t *testing.T, p string, a, b int64) {
			rawExec(t, p, `UPDATE boxes SET qbt_start_cmd=(SELECT qbt_start_cmd FROM boxes WHERE id=?) WHERE id=?`, a, b)
		},
		"row swap": func(t *testing.T, p string, a, b int64) {
			rawExec(t, p, `UPDATE boxes SET password=(SELECT password FROM boxes WHERE id=?) WHERE id=?`, a, b)
		},
		"id swap": func(t *testing.T, p string, a, b int64) {
			rawExec(t, p, `UPDATE boxes SET id=id+100`)
			rawExec(t, p, `UPDATE boxes SET id=? WHERE id=?`, b, a+100)
			rawExec(t, p, `UPDATE boxes SET id=? WHERE id=?`, a, b+100)
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			backup, a, b := boundBackup(t, dir)
			before := rawCell(t, backup, "password", a)
			tamper(t, backup, a, b)
			if name != "row swap" && name != "id swap" && rawCell(t, backup, "password", a) != before {
				t.Fatal("the tamper changed the ciphertext")
			}
			live := filepath.Join(dir, "data", "pmss-fleet-manager.db")
			liveStore(t, live, "current", sealA).Close()
			if _, err := Restore(ctx, live, backup, true, sealA); !errors.Is(err, secrets.ErrWrongKey) {
				t.Fatalf("tampered restore: %v", err)
			}
			if got := boxNames(t, live, sealA); len(got) != 1 || got[0] != "current/pw-current" {
				t.Fatalf("a refused restore changed the database: %v", got)
			}
			// Without the check, the dashboard refuses to start on it; the
			// start that forgets keeps nothing the tamper touched.
			fresh := filepath.Join(dir, "fresh", "pmss-fleet-manager.db")
			if _, err := Restore(ctx, fresh, backup, false); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(fresh, sealA); !errors.Is(err, secrets.ErrWrongKey) {
				t.Fatalf("start on the tampered copy: %v", err)
			}
			s, err := Open(fresh, sealA, WithForgetUnreadable())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if s.ForgottenOnOpen() == 0 {
				t.Fatal("nothing forgotten")
			}
		})
	}
}

// The untouched backup restores, and each credential still opens.
func TestRestoreBoundCredentials(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sealA := WithSealer(mustSealer(t, backupKeyA))
	backup, a, _ := boundBackup(t, dir)
	if v, _ := secrets.Envelope(rawCell(t, backup, "password", a)); v != 2 {
		t.Fatalf("stored as envelope v%d", v)
	}
	live := filepath.Join(dir, "data", "pmss-fleet-manager.db")
	if _, err := Restore(ctx, live, backup, true, sealA); err != nil {
		t.Fatal(err)
	}
	s, err := Open(live, sealA)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetBox(ctx, a)
	if err != nil || got.Password != "pw-a" || got.QbtPass != "qp-a" || got.QbtStartCmd != "start-a" {
		t.Fatalf("%+v %v", got, err)
	}
}

// unboundFixture is a backup as 1.23 wrote it: the box's credentials in
// envelope version 1, sealed to their column only.
func unboundFixture(t *testing.T, dir string) (backup string, id int64) {
	t.Helper()
	backup, id, _ = boundBackup(t, dir)
	sl := mustSealer(t, backupKeyA)
	rawExec(t, backup, `UPDATE boxes SET password=?, qbt_pass=?, qbt_start_cmd=? WHERE id=?`,
		sl.Seal(labelPassword, "pw-a"), sl.Seal(labelQbtPass, "qp-a"), sl.Seal(labelQbtStartCmd, "start-a"), id)
	return backup, id
}

// A backup from before binding is refused unless the operator says what
// to do with its credentials, with or without the secret check (the scan
// needs no secret); keep binds them as the file names the box, drop
// clears them before anything opens the copy.
func TestRestoreUnboundCredentials(t *testing.T) {
	ctx := context.Background()
	sealA := WithSealer(mustSealer(t, backupKeyA))
	dir := t.TempDir()
	backup, id := unboundFixture(t, dir)
	if sum, err := Inspect(ctx, backup); err != nil || sum.Unbound != 3 {
		t.Fatalf("inspect: %+v %v", sum, err)
	}
	live := filepath.Join(dir, "data", "pmss-fleet-manager.db")
	for _, verify := range []bool{true, false} {
		if _, err := Restore(ctx, live, backup, verify, sealA); !errors.Is(err, ErrOldCredentials) {
			t.Fatalf("verify=%v: %v", verify, err)
		}
		if _, err := Restore(ctx, live, backup, verify, sealA, WithOldCredentials(RefuseOldCredentials)); !errors.Is(err, ErrOldCredentials) {
			t.Fatalf("verify=%v explicit refuse: %v", verify, err)
		}
	}

	res, err := Restore(ctx, live, backup, true, sealA, WithOldCredentials(KeepOldCredentials))
	if err != nil || res.Unbound != 0 {
		t.Fatalf("keep: %+v %v", res, err)
	}
	for _, col := range []string{"password", "qbt_pass", "qbt_start_cmd"} {
		if v, _ := secrets.Envelope(rawCell(t, live, col, id)); v != 2 {
			t.Fatalf("kept %s left in envelope v%d", col, v)
		}
	}
	if got := boxNames(t, live, sealA); len(got) != 2 || got[0] != "a/pw-a" {
		t.Fatalf("kept %v", got)
	}

	fresh := filepath.Join(dir, "fresh", "pmss-fleet-manager.db")
	if _, err := Restore(ctx, fresh, backup, false, WithOldCredentials(DropOldCredentials)); err != nil {
		t.Fatal(err)
	}
	for _, col := range []string{"password", "qbt_pass", "qbt_start_cmd"} {
		if v := rawCell(t, fresh, col, id); v != "" {
			t.Fatalf("dropped %s holds %q", col, v)
		}
	}
	if got := boxNames(t, fresh, sealA); len(got) != 2 || got[0] != "a/" || got[1] != "b/pw-b" {
		t.Fatalf("after drop %v", got)
	}
}

// A live database from 1.23 opens as before: its unbound credentials
// are bound in place (and the old bytes scrubbed), and a tamper of the
// live file afterwards no longer opens.
func TestOpenBindsUnboundCredentials(t *testing.T) {
	ctx := context.Background()
	sealA := WithSealer(mustSealer(t, backupKeyA))
	dir := t.TempDir()
	path, id := unboundFixture(t, dir)
	s, err := Open(path, sealA)
	if err != nil {
		t.Fatal(err)
	}
	if s.ResealedOnOpen() != 3 {
		t.Fatalf("resealed %d", s.ResealedOnOpen())
	}
	if v, _ := s.GetValue(ctx, cleanupKey); v != "0" {
		t.Fatalf("cleanup %q", v)
	}
	got, err := s.GetBox(ctx, id)
	if err != nil || got.Password != "pw-a" || got.QbtPass != "qp-a" || got.QbtStartCmd != "start-a" {
		t.Fatalf("%+v %v", got, err)
	}
	s.Close()
	rawExec(t, path, `UPDATE boxes SET host='127.0.0.1' WHERE id=?`, id)
	if _, err := Open(path, sealA); !errors.Is(err, secrets.ErrWrongKey) {
		t.Fatalf("tampered live file: %v", err)
	}
}

// An edit that moves a box keeps its saved credentials, sealed again for
// the new endpoint; one that does not move it leaves their bytes alone.
func TestUpdateBoxRebindsKeptCredentials(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	s, err := Open(path, WithSealer(mustSealer(t, backupKeyA)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := &Box{Name: "a", Host: "odin.example", User: "ragnar", Password: "pw", QbtPass: "qp", Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	pw0, qp0, _ := rawSecrets(t, s, b.ID)

	b.Password, b.QbtPass, b.Name = "", "", "renamed"
	if err := s.UpdateBox(ctx, b, Clears{}); err != nil {
		t.Fatal(err)
	}
	if pw, qp, _ := rawSecrets(t, s, b.ID); pw != pw0 || qp != qp0 {
		t.Fatal("an edit that kept the endpoint rewrote the credentials")
	}

	for i, move := range []func(*Box){
		func(x *Box) { x.Host = "thor.example" },
		func(x *Box) { x.Port = 2222 },
		func(x *Box) { x.User = "bjorn" },
	} {
		move(b)
		b.Password, b.QbtPass = "", ""
		if err := s.UpdateBox(ctx, b, Clears{ReplaceHostKey: true}); err != nil {
			t.Fatalf("move %d: %v", i, err)
		}
		pw, qp, _ := rawSecrets(t, s, b.ID)
		if pw == pw0 || qp == qp0 {
			t.Fatalf("move %d kept the old binding", i)
		}
		pw0, qp0 = pw, qp
		got, err := s.GetBox(ctx, b.ID)
		if err != nil || got.Password != "pw" || got.QbtPass != "qp" {
			t.Fatalf("move %d: %+v %v", i, got, err)
		}
		b.Revision = got.Revision
	}

	// A stale edit is refused before anything is decrypted or written.
	b.Revision--
	b.Host = "loki.example"
	if err := s.UpdateBox(ctx, b, Clears{}); !errors.Is(err, ErrStale) {
		t.Fatalf("stale: %v", err)
	}
	if err := s.UpdateBox(ctx, &Box{ID: 999, Host: "x", User: "y"}, Clears{}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing: %v", err)
	}
}

// Without a secret nothing changes: credentials stay plain text through
// an edit that moves the box.
func TestUpdateBoxPlainStoreMove(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := &Box{Name: "a", Host: "odin.example", User: "ragnar", Password: "pw", Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	b.Password, b.Host = "", "thor.example"
	if err := s.UpdateBox(ctx, b, Clears{}); err != nil {
		t.Fatal(err)
	}
	if pw, _, _ := rawSecrets(t, s, b.ID); pw != "pw" {
		t.Fatalf("stored %q", pw)
	}
}
