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

	"pmss-fleet-manager/internal/secrets"
)

// Three settings rows describe the encoding of the secret columns. They,
// not the "pmss1:" prefix, say whether a value is ciphertext: a plaintext
// password may start with anything.
const (
	// sealedKey is "1" while every non-empty secret column is ciphertext
	// (envelope version 1), "0" while they are plaintext. Missing means a
	// database never opened by a build with encryption support, which is
	// plaintext too; the row is written at the first open so that missing
	// and "0" can be told apart afterwards.
	sealedKey = "secrets_sealed"
	// checkKey holds a fixed string sealed under the current secret while
	// sealedKey is "1". It is what authenticates the configured secret
	// when there is no credential to open, and it has to agree with
	// sealedKey: one without the other is a damaged or partially restored
	// settings table, and the open fails rather than guess.
	checkKey   = "secrets_check"
	labelCheck = "settings.secrets_check"
	checkPlain = "pmss-fleet-manager key check"
	// cleanupKey is "1" from the commit that rewrote secrets until the
	// VACUUM and checkpoint that scrub the old bytes have both succeeded,
	// so a stop or a full disk in between is retried at the next open.
	// It is an obligation, so only an explicit "0" means none: a row that
	// is missing or unreadable in a store with encoding metadata scrubs.
	cleanupKey = "secrets_cleanup"
	// inventoryKey names the set of columns secretCells covers. A store
	// encrypted before a column joined the set may still hold that
	// column's old plaintext in free pages or the WAL (one cleared before
	// the upgrade has no value to rewrite), so a sealed store owes one
	// scrub when the inventory advances. Bump it when a column is added.
	// 2: the qBittorrent start command and download URL (1.12).
	inventoryKey     = "secrets_inventory"
	inventoryVersion = "2"
)

// vacuumSQL is what the cleanup runs; a test swaps it for a failing
// statement to exercise the retry.
var vacuumSQL = "VACUUM"

// recoveryHint ends every error about metadata the open cannot trust.
const recoveryHint = "; restore the database from a consistent backup, or start once with -forget-encrypted-credentials to drop every credential that cannot be read"

// encoding is what the settings rows say about the secret columns.
type encoding struct {
	sealed bool // sealedKey is "1"
	// explicit: sealedKey is present ("0" or "1"), so the marker decides.
	// initialized: any of the three rows is present, so this database has
	// been opened by a build with encryption support and a missing row is
	// damage, not age. A database with none of them is from before.
	explicit, initialized bool
	check                 string // checkKey's value, "" when absent
	cleanup               bool   // a scrub is owed
	repair                bool   // the rows were read leniently (forget) and must be rewritten
}

// secretCell is one stored credential and how to rewrite it.
type secretCell struct {
	where string // for the error message
	label string
	value string // the stored form; once planned, the plaintext to store
	apply func(tx *sql.Tx, stored string) error
}

// resealSecrets runs at Open. It brings every stored credential into the
// configured encoding and verifies the configured secret against what
// the database holds, failing the open rather than starting with a
// credential it cannot read: pollers presenting ciphertext to a box as
// the password is worse than not starting.
//
// With a sealer, plaintext credentials (a database from before the secret
// was set, or written by a build without it) are sealed in place. A sealed
// credential or the key check that the current sealer does not open is
// tried with the previous sealer and rewritten (re-encrypted, or stored
// plain when there is no current sealer) if that works; failing that it
// is cleared when forgetting was asked for; otherwise the open fails,
// naming the row.
//
// After a rewrite, the first time encryption is switched on, and when the
// set of secret columns grew (inventoryKey) (earlier, since-deleted
// plaintext may sit in free pages), the database is vacuumed
// and the WAL truncated. Revisions are not bumped: the record's content is
// unchanged, only its encoding.
func (s *Store) resealSecrets(ctx context.Context) error {
	enc, err := s.readEncoding(ctx)
	if err != nil {
		return err
	}
	if err := s.verifyKeyCheck(enc); err != nil {
		return err
	}
	cells, err := s.secretCells(ctx)
	if err != nil {
		return err
	}
	pending, forgotten, err := s.planCells(enc, cells)
	if err != nil {
		return err
	}

	inv, _, err := s.lookup(ctx, inventoryKey)
	if err != nil {
		return err
	}
	newInventory := inv != inventoryVersion
	sealedAfter := s.sealer.Enabled()
	check, checkChanged := s.planCheck(enc, sealedAfter)
	cleanup := enc.cleanup || len(pending) > 0 || (sealedAfter && (!enc.sealed || newInventory))
	if len(pending) > 0 || checkChanged || cleanup || sealedAfter != enc.sealed || !enc.explicit || enc.repair || newInventory {
		if err := s.rewriteSecrets(ctx, pending, sealedAfter, cleanup, check); err != nil {
			return err
		}
	}
	if cleanup {
		if err := s.scrubFreePages(ctx); err != nil {
			return err
		}
	}
	s.resealed, s.forgotten = len(pending)-forgotten, forgotten
	return nil
}

// readEncoding reads the three rows and refuses states this build cannot
// interpret, unless forgetting was asked for: then a malformed marker, a
// missing one or an inconsistent set is read as sealed, the stricter
// reading, so nothing unreadable is kept, the rows are rewritten
// consistent and a scrub is owed (the lost rows may have been owing one).
func (s *Store) readEncoding(ctx context.Context) (encoding, error) {
	var enc encoding
	sv, sok, err := s.lookup(ctx, sealedKey)
	if err != nil {
		return enc, err
	}
	check, cok, err := s.lookup(ctx, checkKey)
	if err != nil {
		return enc, err
	}
	cv, clok, err := s.lookup(ctx, cleanupKey)
	if err != nil {
		return enc, err
	}
	enc.check, enc.initialized = check, sok || cok || clok
	switch {
	case !sok:
	case sv == "0":
		enc.explicit = true
	case sv == "1":
		enc.explicit, enc.sealed = true, true
	case s.forget:
		enc.explicit, enc.sealed, enc.repair = true, true, true
	default:
		return enc, fmt.Errorf("the settings row %s holds %q, which this version does not understand%s", sealedKey, sv, recoveryHint)
	}
	// A marker missing from an initialized database, or a marker and key
	// check that disagree, is damage: a partial restore or a repair.
	if (enc.initialized && !sok) || enc.sealed != cok {
		if !s.forget {
			return enc, fmt.Errorf("the database's encryption metadata is inconsistent (%s %s, key check %s, cleanup row %s): a partial restore or a repair?%s",
				sealedKey, rowState(sok, sv), presence(cok), presence(clok), recoveryHint)
		}
		enc.explicit, enc.sealed, enc.repair = true, true, true
	}
	// The cleanup row is an obligation: only an explicit "0" clears it.
	enc.cleanup = (clok && cv != "0") || (!clok && enc.initialized) || enc.repair
	return enc, nil
}

func presence(ok bool) string {
	if ok {
		return "present"
	}
	return "missing"
}

func rowState(ok bool, v string) string {
	if ok {
		return fmt.Sprintf("%q", v)
	}
	return "missing"
}

// verifyKeyCheck authenticates the configured secret against the key
// check, so a wrong or missing secret is refused even when no credential
// is stored. The previous sealer opening it means a rotation is under way;
// forgetting accepts anything (the check is rewritten, the unreadable
// credentials dropped).
func (s *Store) verifyKeyCheck(enc encoding) error {
	if !enc.sealed || s.forget || opensCheck(s.sealer, enc.check) || opensCheck(s.prev, enc.check) {
		return nil
	}
	_, err := s.sealer.Open(labelCheck, enc.check)
	if err == nil {
		err = secrets.ErrWrongKey // it opened but is not the check string: not ours
	}
	return fmt.Errorf("stored credentials: %w", err)
}

// opensCheck reports whether sl (possibly nil) opens v to the key-check
// string. Only a sealed value counts: "" and plaintext pass through Open
// and must not pass here.
func opensCheck(sl *secrets.Sealer, v string) bool {
	if sl == nil || !secrets.IsSealed(v) {
		return false
	}
	plain, err := sl.Open(labelCheck, v)
	return err == nil && plain == checkPlain
}

// planCells decides, for every stored credential, what the rewrite stores:
// nothing (keep), its plaintext (to seal, re-seal or store plain), or ""
// (forgotten). A value it can neither read nor legitimately leave alone
// fails the open.
//
// The marker is authoritative: in a store marked plain every value is
// plaintext whatever it looks like, and in a sealed store every value
// with the envelope is ciphertext. Only when the marker is missing (a
// database from before encryption support, or metadata lost) is a
// value's shape consulted, and then only to refuse to hand out as a
// password something that is exactly shaped like our ciphertext.
func (s *Store) planCells(enc encoding, cells []secretCell) (pending []secretCell, forgotten int, err error) {
	for _, c := range cells {
		if c.value == "" {
			continue
		}
		var plan secretCell
		var keep bool
		switch {
		case enc.sealed:
			plan, keep, err = s.planSealedCell(c)
		case enc.explicit:
			plan, keep = c, false // marked plain: plaintext, whatever it looks like
		default:
			plan, keep, err = s.planUnmarkedCell(c)
		}
		if err != nil {
			return nil, 0, err
		}
		if keep {
			continue
		}
		if plan.value == "" {
			forgotten++
		} else if !s.sealer.Enabled() && plan.value == c.value {
			continue // plaintext staying plaintext
		}
		pending = append(pending, plan)
	}
	return pending, forgotten, nil
}

// planSealedCell classifies a value in a sealed store. keep means the row
// is already right; otherwise plan.value is what to store (sealed if
// there is a sealer), "" for a forgotten credential.
func (s *Store) planSealedCell(c secretCell) (plan secretCell, keep bool, err error) {
	version, _ := secrets.Envelope(c.value)
	switch {
	case version == 0:
		// No envelope in a sealed store: plaintext written by a build
		// without the secret. Sealed if there is a sealer, left otherwise.
		return c, false, nil
	case version > 1:
		if s.forget {
			return c.cleared(), false, nil
		}
		return c, false, fmt.Errorf("%s was written by a newer version of pmss-fleet-manager (envelope v%d); upgrade%s", c.where, version, recoveryHint)
	}
	return s.planCiphertext(c, func(err error) error { return fmt.Errorf("%s: %w", c.where, err) })
}

// planUnmarkedCell classifies a value in a store with no encoding marker:
// plaintext, unless it is exactly shaped like our ciphertext, in which
// case it has to open (proving it ours) or be forgotten, never be handed
// out as a password.
func (s *Store) planUnmarkedCell(c secretCell) (plan secretCell, keep bool, err error) {
	if version, wellFormed := secrets.Envelope(c.value); version != 1 || !wellFormed {
		return c, false, nil
	}
	return s.planCiphertext(c, func(err error) error {
		return fmt.Errorf("%s looks encrypted but the database has no encryption marker (%w)%s", c.where, err, recoveryHint)
	})
}

// planCiphertext: a ciphertext has to open with the current secret (keep),
// or with the previous one on a change (rewrite), or be forgotten on
// request; otherwise the open fails with wrap around the current sealer's
// error.
func (s *Store) planCiphertext(c secretCell, wrap func(error) error) (plan secretCell, keep bool, err error) {
	_, cur := s.sealer.Open(c.label, c.value)
	if cur == nil {
		return c, true, nil
	}
	if plain, ok := s.opensWithPrevious(c.label, c.value); ok {
		return c.with(plain), false, nil
	}
	if s.forget {
		return c.cleared(), false, nil
	}
	return c, false, wrap(cur)
}

// opensWithPrevious opens a ciphertext with the previous sealer, if any.
func (s *Store) opensWithPrevious(label, stored string) (string, bool) {
	if s.prev == nil {
		return "", false
	}
	plain, err := s.prev.Open(label, stored)
	return plain, err == nil
}

// planCheck returns the key check row the rewrite should store ("" to
// remove it) and whether that differs from what is stored.
func (s *Store) planCheck(enc encoding, sealedAfter bool) (check string, changed bool) {
	if !sealedAfter {
		return "", enc.check != ""
	}
	if opensCheck(s.sealer, enc.check) {
		return enc.check, false
	}
	return s.sealer.Seal(labelCheck, checkPlain), true
}

func (c secretCell) with(plain string) secretCell { c.value = plain; return c }
func (c secretCell) cleared() secretCell          { c.value = ""; return c }

// secretCells lists every stored credential with its rewrite.
func (s *Store) secretCells(ctx context.Context) ([]secretCell, error) {
	var cells []secretCell
	rows, err := s.db.QueryContext(ctx, `SELECT id, password, qbt_pass, qbt_start_cmd FROM boxes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var pw, qp, sc string
		if err := rows.Scan(&id, &pw, &qp, &sc); err != nil {
			return nil, err
		}
		cells = append(cells,
			secretCell{fmt.Sprintf("box %d SSH password", id), labelPassword, pw, func(tx *sql.Tx, v string) error {
				_, err := tx.ExecContext(ctx, `UPDATE boxes SET password=? WHERE id=?`, v, id)
				return err
			}},
			secretCell{fmt.Sprintf("box %d WebUI password", id), labelQbtPass, qp, func(tx *sql.Tx, v string) error {
				_, err := tx.ExecContext(ctx, `UPDATE boxes SET qbt_pass=? WHERE id=?`, v, id)
				return err
			}},
			secretCell{fmt.Sprintf("box %d qBittorrent start command", id), labelQbtStartCmd, sc, func(tx *sql.Tx, v string) error {
				_, err := tx.ExecContext(ctx, `UPDATE boxes SET qbt_start_cmd=? WHERE id=?`, v, id)
				return err
			}})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() // one connection: the settings reads below need it
	for _, kv := range []struct{ key, where, label string }{
		{"pm_api_key", "Pulsed Media API key", labelPMAPIKey},
		{"pmdb_upload_key", "pmdb upload key", labelPMDBUploadKey},
		{"qbt_download_url", "qBittorrent download URL", labelQbtDownloadURL},
	} {
		v, err := s.GetValue(ctx, kv.key)
		if err != nil {
			return nil, err
		}
		key := kv.key
		cells = append(cells, secretCell{kv.where, kv.label, v, func(tx *sql.Tx, stored string) error {
			_, err := tx.ExecContext(ctx, `UPDATE settings SET value=? WHERE key=?`, stored, key)
			return err
		}})
	}
	return cells, nil
}

// rewriteSecrets stores the pending cells' plaintext in the current
// encoding and records the encoding, the key check, the inventory and,
// when due, the cleanup, in one transaction: the markers can never
// disagree with the rows.
func (s *Store) rewriteSecrets(ctx context.Context, pending []secretCell, sealed, cleanup bool, check string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, c := range pending {
		if err := c.apply(tx, s.sealer.Seal(c.label, c.value)); err != nil {
			return fmt.Errorf("rewrite %s: %w", c.where, err)
		}
	}
	const upsert = `INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`
	for _, m := range []struct{ key, value string }{{sealedKey, boolKV(sealed)}, {cleanupKey, boolKV(cleanup)}, {inventoryKey, inventoryVersion}} {
		if _, err := tx.ExecContext(ctx, upsert, m.key, m.value); err != nil {
			return err
		}
	}
	if check == "" {
		_, err = tx.ExecContext(ctx, `DELETE FROM settings WHERE key=?`, checkKey)
	} else {
		_, err = tx.ExecContext(ctx, upsert, checkKey, check)
	}
	if err != nil {
		return err
	}
	return tx.Commit()
}

// scrubFreePages removes what earlier secret values left behind: VACUUM
// (which cannot run inside a transaction) rewrites the file from the live
// rows, so vacated pages are gone, and a TRUNCATE checkpoint empties the
// WAL, which still holds the old pages. The cleanup marker is cleared only
// once both are known to have completed; a checkpoint that could not
// finish (another connection held the log) leaves it for the next open.
func (s *Store) scrubFreePages(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, vacuumSQL); err != nil {
		return fmt.Errorf("vacuum after rewriting credentials (retried at the next start): %w", err)
	}
	var busy, logFrames, checkpointed int
	if err := s.db.QueryRowContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`).Scan(&busy, &logFrames, &checkpointed); err != nil {
		return fmt.Errorf("checkpoint after rewriting credentials (retried at the next start): %w", err)
	}
	if busy != 0 {
		return fmt.Errorf("checkpoint after rewriting credentials could not complete: the database is in use by another process (retried at the next start)")
	}
	return s.SetValue(ctx, cleanupKey, "0")
}

// lookup reads one settings row, reporting whether it exists.
func (s *Store) lookup(ctx context.Context, key string) (string, bool, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return v, err == nil, err
}
