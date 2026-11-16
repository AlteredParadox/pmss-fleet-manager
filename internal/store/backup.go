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
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"time"
)

// Summary describes a database file for the backup and restore commands.
type Summary struct {
	Boxes     int   // rows in boxes
	Encrypted bool  // the secrets_sealed marker says the credentials are ciphertext
	Bytes     int64 // file size
}

// Backup writes a consistent copy of the database at src to dst, which
// must not exist yet. It is safe while the dashboard runs: VACUUM INTO
// reads one snapshot inside a read transaction, so the copy is the
// database and its write-ahead log at one moment, as a single file with
// no log beside it and no free pages (nothing deleted earlier, such as
// plain text from before the credentials were encrypted, comes along).
// It needs no secret: encrypted credentials are copied as they are stored.
func Backup(ctx context.Context, src, dst string) (Summary, error) {
	db, err := openExisting(src)
	if err != nil {
		return Summary{}, err
	}
	defer db.Close()
	if err := vacuumInto(ctx, db, dst); err != nil {
		return Summary{}, err
	}
	sum, err := Inspect(ctx, dst)
	if err != nil {
		os.Remove(dst)
		return Summary{}, fmt.Errorf("the copy does not check out (removed): %w", err)
	}
	return sum, nil
}

// vacuumInto writes db's current snapshot to the new file dst. The file
// is created owner-only first (VACUUM INTO accepts an empty file) so the
// copy, which holds the credentials, is never readable by anyone else.
func vacuumInto(ctx context.Context, db *sql.DB, dst string) error {
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, dst); err != nil {
		os.Remove(dst)
		return fmt.Errorf("copy the database: %w", err)
	}
	if err := os.Chmod(dst, 0o600); err != nil {
		os.Remove(dst)
		return err
	}
	return syncFile(dst)
}

// openExisting opens a database file that must already exist (mode=rw
// never creates one), without migrating it.
func openExisting(path string) (*sql.DB, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	db, err := sql.Open("sqlite", "file:"+sqliteURIPath.Replace(path)+"?mode=rw&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

// Inspect checks that path is an intact database of this dashboard and
// summarizes it. It does not migrate the file or need the secret.
func Inspect(ctx context.Context, path string) (Summary, error) {
	var sum Summary
	db, err := openExisting(path)
	if err != nil {
		return sum, err
	}
	defer db.Close()
	var check string
	if err := db.QueryRowContext(ctx, `PRAGMA quick_check`).Scan(&check); err != nil {
		return sum, fmt.Errorf("not a readable SQLite database: %w", err)
	}
	if check != "ok" {
		return sum, fmt.Errorf("the database is damaged: %s", check)
	}
	for _, t := range []string{"boxes", "settings"} {
		var n int
		if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name=?`, t).Scan(&n); err != nil {
			return sum, err
		}
		if n == 0 {
			return sum, fmt.Errorf("not a pmss-fleet-manager database: it has no %s table", t)
		}
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM boxes`).Scan(&sum.Boxes); err != nil {
		return sum, err
	}
	var sealed string
	switch err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, sealedKey).Scan(&sealed); {
	case errors.Is(err, sql.ErrNoRows):
	case err != nil:
		return sum, err
	default:
		sum.Encrypted = sealed == "1"
	}
	db.Close()
	fi, err := os.Stat(path)
	if err != nil {
		return sum, err
	}
	sum.Bytes = fi.Size()
	return sum, nil
}

// RestoreResult reports what Restore did.
type RestoreResult struct {
	Summary
	// Previous is where the database that was replaced was saved, "" when
	// there was none.
	Previous string
}

// Restore replaces the database at dbPath with the backup file in. The
// dashboard must not be running on it (the command holds the data
// directory's lock for that). In order:
//
//  1. in is copied next to dbPath (owner-only) and checked with Inspect,
//     so a damaged or foreign file never reaches the data directory's
//     database name. A file with a non-empty write-ahead log beside it is
//     refused: the file alone would be missing what the log holds.
//  2. With verify, the copy is opened as the dashboard opens it, with
//     opts (the configured secret): migrations, and the check that the
//     secret decrypts every stored credential. A copy the dashboard would
//     refuse to start with is refused here instead.
//  3. The database being replaced, if any, is saved beside it as
//     <name>.before-restore-<UTC time> with Backup, so nothing is lost.
//  4. The old write-ahead log and shared-memory files are removed (SQLite
//     would otherwise replay the old log into the restored file) and the
//     copy is renamed into place.
func Restore(ctx context.Context, dbPath, in string, verify bool, opts ...Option) (RestoreResult, error) {
	var res RestoreResult
	if fi, err := os.Stat(in + "-wal"); err == nil && fi.Size() > 0 {
		return res, fmt.Errorf("%s has a write-ahead log beside it (%s): it is a database still in use or not cleanly closed; make the file with the backup command instead", in, in+"-wal")
	}
	if err := os.MkdirAll(filepath.Dir(dbPath), 0o700); err != nil {
		return res, err
	}
	staged := dbPath + ".restore-tmp"
	removeDB(staged)
	if err := copyFile(in, staged); err != nil {
		removeDB(staged)
		return res, err
	}
	defer removeDB(staged) // a no-op after the rename
	if _, err := Inspect(ctx, staged); err != nil {
		return res, err
	}
	if verify {
		s, err := Open(staged, opts...)
		if err != nil {
			return res, fmt.Errorf("the dashboard would not start with this backup: %w", err)
		}
		if err := s.Close(); err != nil {
			return res, err
		}
		for _, side := range []string{"-wal", "-shm"} {
			if err := os.Remove(staged + side); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return res, err
			}
		}
	}
	sum, err := Inspect(ctx, staged)
	if err != nil {
		return res, err
	}
	res.Summary = sum
	if _, err := os.Stat(dbPath); err == nil {
		prev := dbPath + ".before-restore-" + time.Now().UTC().Format("20060102T150405Z")
		if _, err := Backup(ctx, dbPath, prev); err != nil {
			return res, fmt.Errorf("save the current database before replacing it (nothing was changed; move %s aside by hand if it is damaged): %w", dbPath, err)
		}
		res.Previous = prev
	} else if !errors.Is(err, fs.ErrNotExist) {
		return res, err
	}
	for _, side := range []string{"-wal", "-shm"} {
		if err := os.Remove(dbPath + side); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return res, err
		}
	}
	if err := os.Rename(staged, dbPath); err != nil {
		return res, err
	}
	return res, syncDir(filepath.Dir(dbPath))
}

// removeDB removes a database file and its sidecars, ignoring absence.
func removeDB(path string) {
	for _, side := range []string{"", "-wal", "-shm", "-journal"} {
		os.Remove(path + side)
	}
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	fi, err := in.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", src)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Sync(); err != nil {
		out.Close()
		return err
	}
	return out.Close()
}

func syncFile(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// syncDir makes a rename durable. Some filesystems cannot sync a
// directory; that is not worth failing a finished restore over.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	_ = d.Sync()
	return d.Close()
}
