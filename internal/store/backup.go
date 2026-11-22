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
	"slices"
	"strings"
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
	// Plaintext counts credentials stored in plain text. In a file with
	// any sign of encryption (Encrypted, a key check, a ciphertext), or
	// restored with a secret, Restore refuses them unless told to accept
	// them (WithPlainCredentials): someone without the secret could have
	// written their own start command or download link there.
	Plaintext int
	// encrypted is any sign of encryption: the marker, the key check row,
	// or a value in one of this build's envelopes.
	encrypted bool
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

// ErrForeignSchema is the refusal of a file whose schema holds objects
// this dashboard never creates (see Inspect).
var ErrForeignSchema = errors.New("not a database as this dashboard writes it")

// ErrPlainCredentials is the refusal of plain-text credentials in a
// backup that is, or is being restored as, encrypted.
var ErrPlainCredentials = errors.New("the backup holds credentials in plain text where this dashboard keeps them encrypted: a backup of an encrypted dashboard never does, so the file was altered (a start command or download link written by someone without the secret would run on your box), or it is a plain-text backup")

// WithPlainCredentials makes Restore accept plain-text credentials in an
// encrypted backup, or in any backup restored with a secret: for a
// plain-text backup the operator means to encrypt. Only Restore reads it.
func WithPlainCredentials() Option { return func(s *Store) { s.acceptPlain = true } }

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
	// The dashboard's schema has no triggers, views, virtual or shadow
	// tables. One in a file someone else could have edited would run, or
	// answer, in place of the rows the checks below and the restore's
	// writes (migrations, re-encryption, drop) mean, so the file is
	// refused before anything writes to it, and before quick_check walks
	// it (a virtual table's module would run). The table kinds are
	// SQLite's own (pragma_table_list), not read from the schema text.
	var foreign int
	if err := db.QueryRowContext(ctx, `SELECT (SELECT count(*) FROM pragma_table_list WHERE schema='main' AND type<>'table')
		+ (SELECT count(*) FROM sqlite_master WHERE type='trigger')`).Scan(&foreign); err != nil {
		return sum, fmt.Errorf("not a readable SQLite database: %w", err)
	}
	if foreign > 0 {
		return sum, fmt.Errorf("%w: it holds %d trigger(s), view(s), virtual or shadow table(s), which this dashboard never creates", ErrForeignSchema, foreign)
	}
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
	sc, err := scanCells(ctx, db, false)
	if err != nil {
		return sum, err
	}
	var checks int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM settings WHERE key=?`, checkKey).Scan(&checks); err != nil {
		return sum, err
	}
	sum.Unbound, sum.Plaintext = sc.unbound, sc.plain
	sum.encrypted = sum.Encrypted || checks > 0 || sc.sealed > 0
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
	raw := staged + "-in"
	removeDB(raw)
	defer removeDB(raw)
	if err := copyFile(in, raw); err != nil {
		return Summary{}, err
	}
	if _, err := Inspect(ctx, raw); err != nil {
		return Summary{}, err
	}
	if err := rebuild(ctx, raw, staged); err != nil {
		return Summary{}, fmt.Errorf("copy the backup's rows into a fresh database: %w", err)
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
	// Plain text where ciphertext belongs: an encrypted file has none (a
	// build without the secret writing into it is the one exception, and
	// rare enough to confirm). With a secret configured even a file with
	// no sign of encryption is checked, since its markers could have been
	// stripped along with the ciphertext.
	if sum.Plaintext > 0 && (sum.encrypted || cfg.sealer.Enabled()) && !cfg.acceptPlain {
		return Summary{}, fmt.Errorf("%w (%d)", ErrPlainCredentials, sum.Plaintext)
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
	// What is installed must pass the same rules, whatever the opening
	// wrote (Open seals plain text and binds old credentials).
	final, err := Inspect(ctx, staged)
	if err != nil {
		return Summary{}, err
	}
	if verify && cfg.sealer.Enabled() && (final.Plaintext > 0 || final.Unbound > 0) {
		return Summary{}, fmt.Errorf("the restored copy still holds %d plain-text and %d unbound credential(s) after opening; nothing was replaced", final.Plaintext, final.Unbound)
	}
	return final, nil
}

// rebuild writes to dst (which must not exist) a database with this
// build's schema holding src's rows: for every table of that schema, the
// columns src has too (a column src lacks, from an older version, takes
// this build's default; the migrations then run at Open as for any old
// database). Nothing of src's schema comes along: a foreign-key action,
// column default, constraint, index or table someone added to the file
// could otherwise write during the restore's own statements or the
// dashboard's later ones (Codex on #222: an ON DELETE SET DEFAULT that a
// migration's DELETE turned into a planted start command). src is only
// read, with foreign keys off, so its rows are copied as they are.
func rebuild(ctx context.Context, src, dst string) error {
	// Owner-only before SQLite opens it: credentials are copied in before
	// anything else would restrict it, and an unchecked restore installs
	// this file as it is (Codex and fred on #222). SQLite gives its journal
	// the database's mode.
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	f.Close()
	db, err := sql.Open("sqlite", "file:"+sqliteURIPath.Replace(dst)+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	db.SetMaxOpenConns(1) // the ATTACH belongs to the one connection
	defer db.Close()
	if err := (&Store{db: db}).migrate(); err != nil {
		return err
	}
	if _, err := db.ExecContext(ctx, `ATTACH DATABASE ? AS bk`, src); err != nil {
		return err
	}
	tables, err := columnsOf(ctx, db, `SELECT m.name, p.name FROM main.sqlite_master m, pragma_table_info(m.name, 'main') p
		WHERE m.type='table' AND m.name NOT LIKE 'sqlite_%' ORDER BY m.name, p.cid`)
	if err != nil {
		return err
	}
	have, err := columnsOf(ctx, db, `SELECT m.name, p.name FROM bk.sqlite_master m, pragma_table_info(m.name, 'bk') p
		WHERE m.type='table' AND m.name NOT LIKE 'sqlite_%'`)
	if err != nil {
		return err
	}
	for _, t := range tables {
		var cols []string
		for _, c := range t.cols {
			if have.has(t.name, c) {
				cols = append(cols, quoteIdent(c))
			}
		}
		if len(cols) == 0 {
			continue
		}
		list := strings.Join(cols, ", ")
		// Names from this build's own schema, quoted: no input reaches the
		// statement's text. An old backup may hold several active alerts
		// for one box and key, which the migration used to collapse onto
		// the oldest before creating the unique index; this schema has the
		// index already, so the oldest is kept by copying in rowid order
		// and ignoring the rest (Codex on #222).
		verb, order := `INSERT`, ``
		if t.name == "alerts" {
			verb, order = `INSERT OR IGNORE`, ` ORDER BY rowid`
		}
		q := verb + ` INTO main.` + quoteIdent(t.name) + ` (` + list + `) SELECT ` + list + ` FROM bk.` + quoteIdent(t.name) + order // NOSONAR: identifiers from this build's schema, not from the backup
		if _, err := db.ExecContext(ctx, q); err != nil {
			return fmt.Errorf("table %s: %w", t.name, err)
		}
	}
	// AUTOINCREMENT counters, so an ID a deleted box had is not handed out
	// again, for this build's tables only.
	var seq int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM bk.sqlite_master WHERE name='sqlite_sequence'`).Scan(&seq); err != nil {
		return err
	}
	if seq > 0 {
		if _, err := db.ExecContext(ctx, `UPDATE main.sqlite_sequence SET seq=max(seq, (SELECT max(b.seq) FROM bk.sqlite_sequence b WHERE b.name=main.sqlite_sequence.name AND typeof(b.seq)='integer'))
			WHERE EXISTS (SELECT 1 FROM bk.sqlite_sequence b WHERE b.name=main.sqlite_sequence.name AND typeof(b.seq)='integer')`); err != nil {
			return err
		}
		// A table copied empty has no counter yet, but the backup's may
		// be past zero (its rows were deleted): carry those too, for this
		// build's AUTOINCREMENT tables (fred on #222).
		if _, err := db.ExecContext(ctx, `INSERT INTO main.sqlite_sequence(name, seq)
			SELECT b.name, max(b.seq) FROM bk.sqlite_sequence b
			WHERE typeof(b.seq)='integer'
			  AND b.name IN (SELECT name FROM main.sqlite_master WHERE type='table' AND sql LIKE '%AUTOINCREMENT%')
			  AND b.name NOT IN (SELECT name FROM main.sqlite_sequence)
			GROUP BY b.name`); err != nil {
			return err
		}
	}
	if _, err := db.ExecContext(ctx, `DETACH DATABASE bk`); err != nil {
		return err
	}
	return db.Close()
}

// tableCols is one table's column names, in order.
type tableCols struct {
	name string
	cols []string
}

type schemaCols []tableCols

func (sc schemaCols) has(table, col string) bool {
	for _, t := range sc {
		if t.name == table {
			return slices.Contains(t.cols, col)
		}
	}
	return false
}

// columnsOf groups (table, column) rows by table, keeping their order.
func columnsOf(ctx context.Context, db *sql.DB, query string) (schemaCols, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out schemaCols
	for rows.Next() {
		var t, c string
		if err := rows.Scan(&t, &c); err != nil {
			return nil, err
		}
		if n := len(out); n > 0 && out[n-1].name == t {
			out[n-1].cols = append(out[n-1].cols, c)
		} else {
			out = append(out, tableCols{t, []string{c}})
		}
	}
	return out, rows.Err()
}

// quoteIdent quotes an SQL identifier.
func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// cellScan is what scanCells found in a file's secret cells.
type cellScan struct {
	unbound int // box credentials in a well-formed version-1 envelope
	// plain counts non-empty values that are not a well-formed envelope
	// the cell accepts: plain text, and text that merely starts like an
	// envelope ("pmss1:;…", "pmss3:…"), which a file with stripped
	// encryption markers would hand out as plain text.
	plain  int
	sealed int // values in a well-formed envelope the cell accepts
}

// scanCells reads every secret cell of the file (the columns and settings
// secretCells covers), clearing the unbound box credentials with drop.
// A malformed old envelope never decrypts, so it is not counted as
// unbound (drop leaves it): it counts as plain, which only matters where
// plain text is refused. It reads only the columns the file has: an old file is not migrated
// before it is checked.
func scanCells(ctx context.Context, db *sql.DB, drop bool) (cellScan, error) {
	var sc cellScan
	cols, err := boxSecretColumns(ctx, db)
	if err != nil {
		return sc, err
	}
	for _, col := range boxSecretCols {
		if !cols[col.name] {
			continue
		}
		ids, err := sc.scanColumn(ctx, db, col.list)
		if err != nil {
			return sc, err
		}
		if !drop {
			continue
		}
		for _, id := range ids {
			if _, err := db.ExecContext(ctx, col.clear, id); err != nil {
				return sc, err
			}
		}
	}
	for _, key := range []string{"pm_api_key", "qbt_download_url"} {
		var v string
		err := db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return sc, err
		}
		sc.count(v, false)
	}
	return sc, nil
}

// boxSecretCols are the box columns scanCells reads, with fixed queries.
var boxSecretCols = []struct{ name, list, clear string }{
	{"password", `SELECT id, password FROM boxes`, `UPDATE boxes SET password='' WHERE id=?`},
	{"qbt_pass", `SELECT id, qbt_pass FROM boxes`, `UPDATE boxes SET qbt_pass='' WHERE id=?`},
	{"qbt_start_cmd", `SELECT id, qbt_start_cmd FROM boxes`, `UPDATE boxes SET qbt_start_cmd='' WHERE id=?`},
}

// boxSecretColumns reports which of boxSecretCols the file's boxes table has.
func boxSecretColumns(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT name FROM pragma_table_info('boxes')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	cols := map[string]bool{}
	for rows.Next() {
		var c string
		if err := rows.Scan(&c); err != nil {
			return nil, err
		}
		cols[c] = true
	}
	return cols, rows.Err()
}

// scanColumn counts one box column's values (query lists id and value),
// returning the IDs of the unbound ones.
func (sc *cellScan) scanColumn(ctx context.Context, db *sql.DB, query string) ([]int64, error) {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int64
	for rows.Next() {
		var id int64
		var v string
		if err := rows.Scan(&id, &v); err != nil {
			return nil, err
		}
		if sc.count(v, true) {
			ids = append(ids, id)
		}
	}
	return ids, rows.Err()
}

// count classifies one value of a box column (bound) or a setting,
// reporting whether it is an unbound box credential.
func (sc *cellScan) count(v string, bound bool) (unbound bool) {
	if v == "" {
		return false
	}
	version, ok := secrets.Envelope(v)
	if !ok || !(version == 1 || bound && version == 2) {
		sc.plain++
		return false
	}
	sc.sealed++
	if bound && version == 1 {
		sc.unbound++
		return true
	}
	return false
}

// dropUnbound clears the staged copy's unbound credentials.
func dropUnbound(ctx context.Context, path string) error {
	db, err := openExisting(path)
	if err != nil {
		return err
	}
	_, err = scanCells(ctx, db, true)
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
