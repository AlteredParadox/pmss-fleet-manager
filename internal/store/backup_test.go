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

const backupKeyA, backupKeyB = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

// liveStore opens a store with one box, left open (WAL in use) as a
// running dashboard would hold it.
func liveStore(t *testing.T, path, name string, opts ...Option) *Store {
	t.Helper()
	s, err := Open(path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.CreateBox(context.Background(), &Box{Name: name, Host: "odin.example", User: "ragnar", Password: "pw-" + name, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	return s
}

func boxNames(t *testing.T, path string, opts ...Option) []string {
	t.Helper()
	s, err := Open(path, opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	boxes, err := s.ListBoxes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range boxes {
		names = append(names, b.Name+"/"+b.Password)
	}
	return names
}

func TestBackupWhileOpen(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	src := filepath.Join(dir, "live.db")
	liveStore(t, src, "a", WithSealer(mustSealer(t, backupKeyA)))
	if fi, err := os.Stat(src + "-wal"); err != nil || fi.Size() == 0 {
		t.Fatalf("test needs a live WAL: %v", err)
	}
	dst := filepath.Join(dir, "backup.db")
	sum, err := Backup(ctx, src, dst)
	if err != nil {
		t.Fatal(err)
	}
	if sum.Boxes != 1 || !sum.Encrypted || sum.Bytes == 0 {
		t.Fatalf("summary %+v", sum)
	}
	fi, err := os.Stat(dst)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("backup mode %v %v", fi.Mode(), err)
	}
	if _, err := os.Stat(dst + "-wal"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("backup left a log beside it: %v", err)
	}
	// The ciphertext is copied as stored: it opens with the secret only.
	if got := boxNames(t, dst, WithSealer(mustSealer(t, backupKeyA))); len(got) != 1 || got[0] != "a/pw-a" {
		t.Fatalf("restored boxes %v", got)
	}
	if _, err := Backup(ctx, src, dst); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("an existing target must be refused: %v", err)
	}
	if _, err := Backup(ctx, filepath.Join(dir, "missing.db"), filepath.Join(dir, "x.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a missing source: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "missing.db")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a backup of a missing database created one")
	}
}

func TestInspectRefusesForeignFiles(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	junk := filepath.Join(dir, "junk.db")
	if err := os.WriteFile(junk, []byte(strings.Repeat("not sqlite ", 100)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Inspect(ctx, junk); err == nil {
		t.Fatal("junk passed")
	}
	other := filepath.Join(dir, "other.db")
	db, err := sql.Open("sqlite", "file:"+other)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE notes (x TEXT)`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	if _, err := Inspect(ctx, other); err == nil || !strings.Contains(err.Error(), "not a pmss-fleet-manager database") {
		t.Fatalf("a foreign database: %v", err)
	}
}

func TestRestore(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	sealA := WithSealer(mustSealer(t, backupKeyA))

	// A backup of database "old" holding box a.
	old := filepath.Join(dir, "old.db")
	liveStore(t, old, "a", sealA)
	backup := filepath.Join(dir, "backup.db")
	if _, err := Backup(ctx, old, backup); err != nil {
		t.Fatal(err)
	}

	// The live database, holding box b, closed with a log still beside it.
	live := filepath.Join(dir, "data", "pmss-fleet-manager.db")
	s, err := Open(live, sealA)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.CreateBox(ctx, &Box{Name: "b", Host: "h", User: "u", Password: "pw-b", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`PRAGMA wal_autocheckpoint=0`); err != nil {
		t.Fatal(err)
	}

	// Wrong secret or none: refused, and the live database is untouched.
	for name, opt := range map[string]Option{"none": WithSealer(nil), "wrong": WithSealer(mustSealer(t, backupKeyB))} {
		_, err := Restore(ctx, live, backup, true, opt)
		if !errors.Is(err, secrets.ErrNoKey) && !errors.Is(err, secrets.ErrWrongKey) {
			t.Fatalf("%s secret: %v", name, err)
		}
	}
	if got := boxNames(t, live, sealA); len(got) != 1 || got[0] != "b/pw-b" {
		t.Fatalf("a refused restore changed the database: %v", got)
	}
	s.Close()

	res, err := Restore(ctx, live, backup, true, sealA)
	if err != nil {
		t.Fatal(err)
	}
	if res.Boxes != 1 || !res.Encrypted || res.Previous == "" {
		t.Fatalf("result %+v", res)
	}
	if got := boxNames(t, live, sealA); len(got) != 1 || got[0] != "a/pw-a" {
		t.Fatalf("restored %v", got)
	}
	if got := boxNames(t, res.Previous, sealA); len(got) != 1 || got[0] != "b/pw-b" {
		t.Fatalf("saved previous %v", got)
	}
	if fi, err := os.Stat(live); err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("restored mode %v %v", fi.Mode(), err)
	}
	if _, err := os.Stat(live + ".restore-tmp"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("staging file left: %v", err)
	}

	// The secret is lost: skipping the check restores, and the start that
	// forgets unreadable credentials then works.
	fresh := filepath.Join(dir, "fresh", "pmss-fleet-manager.db")
	if _, err := Restore(ctx, fresh, backup, false); err != nil {
		t.Fatal(err)
	}
	if got := boxNames(t, fresh, WithSealer(mustSealer(t, backupKeyB)), WithForgetUnreadable()); len(got) != 1 || got[0] != "a/" {
		t.Fatalf("after forgetting %v", got)
	}
}

func TestRestoreRefusesBadInput(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	live := filepath.Join(dir, "pmss-fleet-manager.db")
	liveStore(t, live, "keep").Close()

	junk := filepath.Join(dir, "junk.db")
	os.WriteFile(junk, []byte(strings.Repeat("x", 4096)), 0o600)
	if _, err := Restore(ctx, live, junk, true); err == nil {
		t.Fatal("junk restored")
	}

	// A database still in use: its file alone misses what its log holds.
	inUse := filepath.Join(dir, "inuse.db")
	liveStore(t, inUse, "x")
	if _, err := Restore(ctx, live, inUse, true); err == nil || !strings.Contains(err.Error(), "write-ahead log") {
		t.Fatalf("in-use database: %v", err)
	}
	if got := boxNames(t, live); len(got) != 1 || got[0] != "keep/pw-keep" {
		t.Fatalf("a refused restore changed the database: %v", got)
	}
	matches, _ := filepath.Glob(live + ".*")
	if len(matches) != 0 {
		t.Fatalf("a refused restore left files: %v", matches)
	}
}
