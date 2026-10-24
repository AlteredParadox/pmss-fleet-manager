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
	"fmt"

	"pmss-fleet-manager/internal/secrets"
)

// Two settings rows describe the encoding of the secret columns. They are
// what says whether a value is ciphertext: the "pmss1:" prefix alone is not,
// since a plaintext password may start with anything.
const (
	// sealedKey is "1" while every non-empty secret column is ciphertext
	// (a value without the prefix is plaintext written by an older build
	// and is sealed at the next open). Unset, every value is plaintext.
	sealedKey = "secrets_sealed"
	// cleanupKey is "1" from the commit that rewrote secrets until the
	// VACUUM and checkpoint that scrub the old bytes have both succeeded,
	// so a stop or a full disk in between is retried at the next open.
	cleanupKey = "secrets_cleanup"
)

// vacuumSQL is what the cleanup runs; a test swaps it for a failing
// statement to exercise the retry.
var vacuumSQL = "VACUUM"

// secretCell is one stored credential and how to rewrite it.
type secretCell struct {
	where string // for the error message
	label string
	value string // the stored form; after triage, the plaintext to store
	apply func(tx *sql.Tx, stored string) error
}

// resealSecrets runs at Open. It brings every stored credential into the
// configured encoding: with a sealer, plaintext ones (a database from
// before the secret was set, or written by a build without it) are sealed
// in place; a sealed one the current sealer does not open is tried with
// the previous sealer and re-encrypted (or stored plain, when there is no
// current sealer) if that works; failing that it is cleared when
// forgetting was asked for. Otherwise it fails the open, naming the row:
// starting half-working, with pollers presenting ciphertext as the
// password, is worse than not starting.
//
// After a rewrite, and the first time encryption is switched on (earlier,
// since-deleted plaintext may sit in free pages), the database is vacuumed
// and the WAL truncated. Revisions are not bumped: the record's content is
// unchanged, only its encoding.
func (s *Store) resealSecrets(ctx context.Context) error {
	sealedBefore, err := s.flag(ctx, sealedKey)
	if err != nil {
		return err
	}
	cleanupDue, err := s.flag(ctx, cleanupKey)
	if err != nil {
		return err
	}
	cells, err := s.secretCells(ctx)
	if err != nil {
		return err
	}

	var pending []secretCell
	forgotten := 0
	for _, c := range cells {
		if c.value == "" {
			continue
		}
		if !sealedBefore || !secrets.IsSealed(c.value) {
			// Plaintext (by the marker, or written by a build without the
			// secret): seal it if there is a sealer, leave it otherwise.
			if s.sealer.Enabled() {
				pending = append(pending, c)
			}
			continue
		}
		// Ciphertext: it has to open with the current secret, or with the
		// previous one on a change, or be forgotten on request.
		_, err := s.sealer.Open(c.label, c.value)
		if err == nil {
			continue
		}
		if s.prev != nil {
			if plain, perr := s.prev.Open(c.label, c.value); perr == nil {
				c.value = plain
				pending = append(pending, c)
				continue
			}
		}
		if s.forget {
			c.value = ""
			pending = append(pending, c)
			forgotten++
			continue
		}
		return fmt.Errorf("%s: %w", c.where, err)
	}

	sealedAfter := s.sealer.Enabled()
	cleanup := cleanupDue || len(pending) > 0 || (sealedAfter && !sealedBefore)
	if len(pending) > 0 || sealedAfter != sealedBefore || cleanup {
		if err := s.rewriteSecrets(ctx, pending, sealedAfter, cleanup); err != nil {
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
// encoding and records the encoding and, when due, the cleanup, in one
// transaction: the markers can never disagree with the rows.
func (s *Store) rewriteSecrets(ctx context.Context, pending []secretCell, sealed, cleanup bool) error {
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
	for _, m := range []struct {
		key string
		on  bool
	}{{sealedKey, sealed}, {cleanupKey, cleanup}} {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, m.key, boolKV(m.on)); err != nil {
			return err
		}
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

// flag reads a "1"/"0" settings row.
func (s *Store) flag(ctx context.Context, key string) (bool, error) {
	v, err := s.GetValue(ctx, key)
	return v == "1", err
}
