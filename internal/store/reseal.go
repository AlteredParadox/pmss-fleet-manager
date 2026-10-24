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
	cleanupKey = "secrets_cleanup"
)

// vacuumSQL is what the cleanup runs; a test swaps it for a failing
// statement to exercise the retry.
var vacuumSQL = "VACUUM"

// recoveryHint ends every error about metadata the open cannot trust.
const recoveryHint = "; restore the database from a consistent backup, or start once with -forget-encrypted-credentials to drop every credential that cannot be read"

// encoding is what the settings rows say about the secret columns.
type encoding struct {
	sealed   bool   // sealedKey is "1"
	explicit bool   // sealedKey is present ("0" or "1")
	check    string // checkKey's value, "" when absent
	cleanup  bool   // cleanupKey is "1"
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
// After a rewrite, and the first time encryption is switched on (earlier,
// since-deleted plaintext may sit in free pages), the database is vacuumed
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

	sealedAfter := s.sealer.Enabled()
	check, checkChanged := s.planCheck(enc, sealedAfter)
	cleanup := enc.cleanup || len(pending) > 0 || (sealedAfter && !enc.sealed)
	if len(pending) > 0 || checkChanged || cleanup || sealedAfter != enc.sealed || !enc.explicit {
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
// interpret, unless forgetting was asked for: then an inconsistent pair
// is read as sealed, the stricter reading, so nothing unreadable is kept.
func (s *Store) readEncoding(ctx context.Context) (encoding, error) {
	var enc encoding
	v, ok, err := s.lookup(ctx, sealedKey)
	if err != nil {
		return enc, err
	}
	switch {
	case !ok:
	case v == "0":
		enc.explicit = true
	case v == "1":
		enc.explicit, enc.sealed = true, true
	default:
		return enc, fmt.Errorf("the settings row %s holds %q, which this version does not understand%s", sealedKey, v, recoveryHint)
	}
	if enc.check, _, err = s.lookup(ctx, checkKey); err != nil {
		return enc, err
	}
	if enc.sealed != (enc.check != "") {
		if !s.forget {
			state := "missing"
			if enc.check != "" {
				state = "present"
			}
			return enc, fmt.Errorf("the database's encryption markers disagree (%s=%q, key check %s): a partial restore or a repair?%s", sealedKey, v, state, recoveryHint)
		}
		enc.sealed = true
	}
	cv, _, err := s.lookup(ctx, cleanupKey)
	enc.cleanup = cv == "1"
	return enc, err
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
func (s *Store) planCells(enc encoding, cells []secretCell) (pending []secretCell, forgotten int, err error) {
	for _, c := range cells {
		if c.value == "" {
			continue
		}
		version, wellFormed := secrets.Envelope(c.value)
		switch {
		case version > 1:
			if !s.forget {
				return nil, 0, fmt.Errorf("%s was written by a newer version of pmss-fleet-manager (envelope v%d); upgrade%s", c.where, version, recoveryHint)
			}
			pending = append(pending, c.cleared())
			forgotten++
		case enc.sealed && version == 0, !enc.sealed && !wellFormed:
			// Plaintext: by the marker, or written by a build without the
			// secret (in a sealed store a value without an envelope can
			// only be that). Sealed if there is a sealer, left otherwise.
			if s.sealer.Enabled() {
				pending = append(pending, c)
			}
		default:
			// Ciphertext, or in a plaintext store something that is
			// exactly shaped like it: it has to open with the current
			// secret, or with the previous one on a change, or be
			// forgotten on request. A plaintext store never passes such
			// a value through as a password.
			_, cur := s.sealer.Open(c.label, c.value)
			if cur == nil {
				continue // already under the current secret (a lost marker is repaired below)
			}
			if plain, ok := s.opensWithPrevious(c.label, c.value); ok {
				pending = append(pending, c.with(plain))
				continue
			}
			if s.forget {
				pending = append(pending, c.cleared())
				forgotten++
				continue
			}
			if !enc.sealed {
				return nil, 0, fmt.Errorf("%s looks encrypted but the database is marked plain text (%w)%s", c.where, cur, recoveryHint)
			}
			return nil, 0, fmt.Errorf("%s: %w", c.where, cur)
		}
	}
	return pending, forgotten, nil
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
	rows, err := s.db.QueryContext(ctx, `SELECT id, password, qbt_pass FROM boxes`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var pw, qp string
		if err := rows.Scan(&id, &pw, &qp); err != nil {
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
			}})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close() // one connection: the settings reads below need it
	for _, kv := range []struct{ key, where, label string }{
		{"pm_api_key", "Pulsed Media API key", labelPMAPIKey},
		{"pmdb_upload_key", "pmdb upload key", labelPMDBUploadKey},
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
// encoding and records the encoding, the key check and, when due, the
// cleanup, in one transaction: the markers can never disagree with the
// rows.
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
	for _, m := range []struct{ key, value string }{{sealedKey, boolKV(sealed)}, {cleanupKey, boolKV(cleanup)}} {
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
