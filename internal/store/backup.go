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

	"pmss-fleet-manager/internal/secrets"
)

// Summary describes a database file for the backup and restore commands.
type Summary struct {
	Boxes     int   // rows in boxes
	Encrypted bool  // the secrets_sealed marker says the credentials are ciphertext
	Bytes     int64 // file size
	// Unbound counts box credentials encrypted before 1.24, when they were
	// not yet bound to their box (boxBinding): Restore refuses them unless
	// told to keep or drop them (WithOldCredentials).
	Unbound int
}

// OldCredentials is what Restore does with a backup's Unbound credentials.
type OldCredentials int

const (
	// RefuseOldCredentials (the default) fails the restore: anyone who
	// could edit the file without the secret could have pointed a box at
	// their own server, and the next poll would send it the password.
	RefuseOldCredentials OldCredentials = iota
	// KeepOldCredentials restores them; the first open binds them to the
	// box as the file names it. For a file nobody else could have changed.
	KeepOldCredentials
	// DropOldCredentials clears them before anything opens the copy; the
	// edit forms ask for them again.
	DropOldCredentials
)

// ErrOldCredentials is the refusal of a backup holding Unbound credentials.
var ErrOldCredentials = errors.New("the backup holds box credentials encrypted before 1.24, which are not bound to their box: anyone who could change the file without the secret could have pointed a box at their own server (a backup taken by 1.24 or later never holds them, so in one of those this means the file was altered)")

// WithOldCredentials sets what Restore does with credentials encrypted
// before they were bound to their box. Only Restore reads it.
func WithOldCredentials(m OldCredentials) Option { return func(s *Store) { s.oldCreds = m } }

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
	_, err = db.ExecContext(ctx, `VACUUM INTO ?`, dst)
	if err == nil {
		err = os.Chmod(dst, 0o600) // SQLite keeps the mode; this is belt and braces
	}
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("copy the database: %w", err)
	}
	return syncPath(dst)
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
	var tables int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM sqlite_master WHERE type='table' AND name IN ('boxes','settings')`).Scan(&tables); err != nil || tables != 2 {
		return sum, fmt.Errorf("not a pmss-fleet-manager database: it has no boxes and settings tables (%v)", err)
	}
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM boxes`).Scan(&sum.Boxes); err != nil {
		return sum, err
	}
	var sealed string
	if err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, sealedKey).Scan(&sealed); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return sum, err
	}
	sum.Encrypted = sealed == "1"
	if sum.Unbound, err = unboundCells(ctx, db, false); err != nil {
		return sum, err
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
	defer removeDB(staged) // a no-op after the rename
	sum, err := stage(ctx, in, staged, verify, opts)
	if err != nil {
		return res, err
	}
	res.Summary = sum
	res.Previous, err = replaceDB(ctx, dbPath, staged)
	return res, err
}

// stage copies in to staged, checks it, and with verify opens it as the
// dashboard would (steps 1 and 2 of Restore).
func stage(ctx context.Context, in, staged string, verify bool, opts []Option) (Summary, error) {
	if err := copyFile(in, staged); err != nil {
		return Summary{}, err
	}
	sum, err := Inspect(ctx, staged)
	if err != nil {
		return Summary{}, err
	}
	// Before anything opens the copy, so neither the check below nor the
	// first start can bind a tampered row's credentials to its new host.
	// It needs no secret, so -skip-secret-check does not skip it.
	var cfg Store
	for _, o := range opts {
		o(&cfg)
	}
	if sum.Unbound > 0 {
		switch cfg.oldCreds {
		case KeepOldCredentials:
		case DropOldCredentials:
			if err := dropUnbound(ctx, staged); err != nil {
				return Summary{}, err
			}
		default:
			return Summary{}, fmt.Errorf("%w (%d)", ErrOldCredentials, sum.Unbound)
		}
	}
	if verify {
		s, err := Open(staged, opts...)
		if err != nil {
			return Summary{}, fmt.Errorf("the dashboard would not start with this backup: %w", err)
		}
		if err := s.Close(); err != nil {
			return Summary{}, err
		}
		if err := removeSidecars(staged); err != nil {
			return Summary{}, err
		}
	}
	return Inspect(ctx, staged)
}

// unboundCells counts the box credentials in a well-formed version-1
// envelope, clearing them with drop. A malformed one never decrypts, in
// any reading of the metadata, so it cannot carry a password anywhere;
// and a plain-text password that merely starts with "pmss1:" is left to
// the marker, as everywhere else. It reads only the columns the file has: an old file is
// not migrated before it is checked.
func unboundCells(ctx context.Context, db *sql.DB, drop bool) (int, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('boxes') WHERE name IN ('password','qbt_pass','qbt_start_cmd')`)
	if err != nil {
		return 0, err
	}
	var cols []string
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			rows.Close()
			return 0, err
		}
		cols = append(cols, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	n := 0
	for _, col := range cols { // names from the fixed list above
		rows, err := db.QueryContext(ctx, `SELECT id, `+col+` FROM boxes`)
		if err != nil {
			return 0, err
		}
		var ids []int64
		for rows.Next() {
			var id int64
			var v string
			if err := rows.Scan(&id, &v); err != nil {
				rows.Close()
				return 0, err
			}
			if version, ok := secrets.Envelope(v); version == 1 && ok {
				ids = append(ids, id)
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return 0, err
		}
		n += len(ids)
		if !drop {
			continue
		}
		for _, id := range ids {
			if _, err := db.ExecContext(ctx, `UPDATE boxes SET `+col+`='' WHERE id=?`, id); err != nil {
				return 0, err
			}
		}
	}
	return n, nil
}

// dropUnbound clears the staged copy's unbound credentials.
func dropUnbound(ctx context.Context, path string) error {
	db, err := openExisting(path)
	if err != nil {
		return err
	}
	_, err = unboundCells(ctx, db, true)
	return errors.Join(err, db.Close())
}

// replaceDB saves the database at dbPath, if there is one, and puts
// staged in its place (steps 3 and 4 of Restore).
func replaceDB(ctx context.Context, dbPath, staged string) (prev string, err error) {
	if _, err := os.Stat(dbPath); err == nil {
		base := dbPath + ".before-restore-" + time.Now().UTC().Format("20060102T150405Z")
		prev = base
		_, err := Backup(ctx, dbPath, prev)
		// Two restores within one second: number the later saves.
		for n := 2; errors.Is(err, fs.ErrExist) && n < 100; n++ {
			prev = fmt.Sprintf("%s-%d", base, n)
			_, err = Backup(ctx, dbPath, prev)
		}
		if err != nil {
			return "", fmt.Errorf("save the current database before replacing it (nothing was changed; move %s aside by hand if it is damaged): %w", dbPath, err)
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return "", err
	}
	if err := removeSidecars(dbPath); err != nil {
		return prev, err
	}
	if err := os.Rename(staged, dbPath); err != nil {
		return prev, err
	}
	return prev, syncPath(filepath.Dir(dbPath))
}

// removeSidecars removes a database's write-ahead log and shared-memory
// file: next to a different main file, SQLite would replay the log into it.
func removeSidecars(path string) error {
	for _, side := range []string{"-wal", "-shm"} {
		if err := os.Remove(path + side); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
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
	if fi, err := in.Stat(); err != nil || !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file (%v)", src, err)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Sync(), out.Close())
}

// syncPath flushes a file, or a directory after a rename, to disk. Some
// filesystems cannot sync a directory; that is not worth failing a
// finished restore over, so only a file's sync error counts.
func syncPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.IsDir() {
		_ = f.Sync()
		return nil
	}
	return f.Sync()
}
