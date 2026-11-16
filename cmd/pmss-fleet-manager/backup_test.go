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

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"pmss-fleet-manager/internal/secrets"
	"pmss-fleet-manager/internal/store"
)

func seedDataDir(t *testing.T, dir, name, secret string) {
	t.Helper()
	sl, err := secrets.New([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(filepath.Join(dir, dbName), store.WithSealer(sl))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.CreateBox(context.Background(), &store.Box{Name: name, Host: "h", User: "u", Password: "pw", Enabled: true}); err != nil {
		t.Fatal(err)
	}
}

func TestBackupRestoreCommands(t *testing.T) {
	const secret = "cccccccccccccccccccccccccccccccc"
	t.Setenv("PMSS_SECRET", "")
	t.Setenv("PMSS_SECRET_FILE", "")
	dir := t.TempDir()
	src, dst := filepath.Join(dir, "src"), filepath.Join(dir, "dst")
	seedDataDir(t, src, "from-backup", secret)
	seedDataDir(t, dst, "current", secret)
	secretFile := filepath.Join(dir, "secret")
	if err := os.WriteFile(secretFile, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}

	// Backup to stdout, as `docker exec` uses it; nothing is left behind.
	var out, errOut bytes.Buffer
	if rc := backupCmd([]string{"-data", src, "-out", "-"}, &out, &errOut); rc != 0 {
		t.Fatalf("backup rc %d: %s", rc, errOut.String())
	}
	if !bytes.HasPrefix(out.Bytes(), []byte("SQLite format 3\x00")) || !strings.Contains(errOut.String(), "1 boxes, credentials encrypted") {
		t.Fatalf("stdout backup: %d bytes, %q", out.Len(), errOut.String())
	}
	if left, _ := filepath.Glob(filepath.Join(src, ".backup-*")); len(left) != 0 {
		t.Fatalf("staging left: %v", left)
	}
	file := filepath.Join(dir, "backup.db")
	if err := os.WriteFile(file, out.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}

	errOut.Reset()
	if rc := restoreCmd([]string{"-data", dst, "-in", file}, &errOut); rc != 1 || !strings.Contains(errOut.String(), "give the secret it was made with") {
		t.Fatalf("restore without the secret: rc %d %q", rc, errOut.String())
	}

	if runtime.GOOS != "windows" {
		release, err := lockDataDir(dst)
		if err != nil {
			t.Fatal(err)
		}
		errOut.Reset()
		if rc := restoreCmd([]string{"-data", dst, "-secret-file", secretFile, "-in", file}, &errOut); rc != 1 || !strings.Contains(errOut.String(), "stop the dashboard first") {
			t.Fatalf("restore under a running dashboard: rc %d %q", rc, errOut.String())
		}
		release()
	}

	errOut.Reset()
	if rc := restoreCmd([]string{"-data", dst, "-secret-file", secretFile, "-in", file}, &errOut); rc != 0 {
		t.Fatalf("restore rc %d: %s", rc, errOut.String())
	}
	if !strings.Contains(errOut.String(), "saved as") || !strings.Contains(errOut.String(), "same secret") {
		t.Fatalf("restore output %q", errOut.String())
	}
	sl, _ := secrets.New([]byte(secret))
	s, err := store.Open(filepath.Join(dst, dbName), store.WithSealer(sl))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	boxes, err := s.ListBoxes(context.Background())
	if err != nil || len(boxes) != 1 || boxes[0].Name != "from-backup" || boxes[0].Password != "pw" {
		t.Fatalf("restored boxes %v %v", boxes, err)
	}
}

func TestDataDirLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no lock on this platform")
	}
	dir := t.TempDir()
	release, err := lockDataDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockDataDir(dir); err != errDataDirInUse {
		t.Fatalf("second lock: %v", err)
	}
	release()
	again, err := lockDataDir(dir)
	if err != nil {
		t.Fatalf("after release: %v", err)
	}
	again()
}

func TestBackupUsage(t *testing.T) {
	var out, errOut bytes.Buffer
	dir := t.TempDir()
	if rc := backupCmd([]string{"-data", dir, "-out", filepath.Join(dir, "b.db")}, &out, &errOut); rc != 1 {
		t.Fatalf("backup of a missing database: rc %d", rc)
	}
	if rc := backupCmd([]string{"-data", filepath.Join(dir, "none"), "-out", "-"}, &out, &errOut); rc != 1 {
		t.Fatalf("stdout backup without a data directory: rc %d", rc)
	}
	if rc := restoreCmd([]string{"-data", dir, "-in", filepath.Join(dir, "missing.db")}, &errOut); rc != 1 {
		t.Fatalf("restore of a missing file: rc %d", rc)
	}
	if rc := restoreCmd([]string{"-data", dir, "-secret-file", filepath.Join(dir, "nosecret"), "-in", "x"}, &errOut); rc != 2 {
		t.Fatalf("restore with a missing secret file: rc %d", rc)
	}
	if rc := backupCmd([]string{"-bogus"}, &out, &errOut); rc != 2 {
		t.Fatalf("bad flag: rc %d", rc)
	}
	if rc := backupCmd(nil, &out, &errOut); rc != 2 {
		t.Fatalf("no -out: rc %d", rc)
	}
	if rc := restoreCmd(nil, &errOut); rc != 2 {
		t.Fatalf("no -in: rc %d", rc)
	}
}

func TestOpenStoreHoldsTheLock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no lock on this platform")
	}
	dir := t.TempDir()
	st, release, err := openStore(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := openStore(dir, nil); err != errDataDirInUse {
		t.Fatalf("second server: %v", err)
	}
	var errOut bytes.Buffer
	if rc := restoreCmd([]string{"-data", dir, "-in", filepath.Join(dir, dbName)}, &errOut); rc != 1 || !strings.Contains(errOut.String(), "stop the dashboard first") {
		t.Fatalf("restore under the server: rc %d %q", rc, errOut.String())
	}
	st.Close()
	release()
	// A data directory that cannot be created, and a store that fails to
	// open, both come back as errors with the lock released.
	file := filepath.Join(dir, "file")
	os.WriteFile(file, nil, 0o600)
	if _, err := lockDataDir(filepath.Join(file, "sub")); err == nil {
		t.Fatal("lock under a file")
	}
	bad := t.TempDir()
	os.Mkdir(filepath.Join(bad, dbName), 0o700) // a directory where the database belongs
	if _, _, err := openStore(bad, nil); err == nil {
		t.Fatal("store open over a directory")
	}
	if _, release, err := openStore(bad, nil); err == nil || release != nil {
		t.Fatalf("lock not released after a failed open: %v", err)
	}
}
