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

// resealSecrets runs at Open. It checks every stored credential against the
// configured sealer and, when there is one, encrypts the ones still in
// plain text (a database from before the secret was set, or written by a
// build without it). A sealed credential the current sealer does not open
// is tried with the previous one and re-encrypted (or stored plain, when
// there is no current sealer) if that works; failing that it is cleared
// when forgetting was asked for. Otherwise it fails the open, naming the
// row: starting half-working, with pollers presenting ciphertext as the
// password, is worse than not starting.
//
// After a rewrite the database is vacuumed and the WAL truncated, so the
// plaintext does not survive in free pages or in the log. Revisions are
// not bumped: the record's content is unchanged, only its encoding.
func (s *Store) resealSecrets(ctx context.Context) error {
	type cell struct {
		where string // for the error message
		label string
		value string
		apply func(tx *sql.Tx, sealed string) error
	}
	var cells []cell

	rows, err := s.db.QueryContext(ctx, `SELECT id, password, qbt_pass FROM boxes`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var id int64
		var pw, qp string
		if err := rows.Scan(&id, &pw, &qp); err != nil {
			rows.Close()
			return err
		}
		cells = append(cells,
			cell{fmt.Sprintf("box %d SSH password", id), labelPassword, pw, func(tx *sql.Tx, v string) error {
				_, err := tx.ExecContext(ctx, `UPDATE boxes SET password=? WHERE id=?`, v, id)
				return err
			}},
			cell{fmt.Sprintf("box %d WebUI password", id), labelQbtPass, qp, func(tx *sql.Tx, v string) error {
				_, err := tx.ExecContext(ctx, `UPDATE boxes SET qbt_pass=? WHERE id=?`, v, id)
				return err
			}})
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()

	for _, kv := range []struct{ key, where, label string }{
		{"pm_api_key", "Pulsed Media API key", labelPMAPIKey},
		{"pmdb_upload_key", "pmdb upload key", labelPMDBUploadKey},
	} {
		v, err := s.GetValue(ctx, kv.key)
		if err != nil {
			return err
		}
		key := kv.key
		cells = append(cells, cell{kv.where, kv.label, v, func(tx *sql.Tx, sealed string) error {
			_, err := tx.ExecContext(ctx, `UPDATE settings SET value=? WHERE key=?`, sealed, key)
			return err
		}})
	}

	var pending []cell // c.value is the plaintext to store (sealed if there is a sealer)
	forgotten := 0
	for _, c := range cells {
		if c.value == "" {
			continue
		}
		if !secrets.IsSealed(c.value) {
			if s.sealer.Enabled() {
				pending = append(pending, c)
			}
			continue
		}
		// Already encrypted: it has to open with the current secret, or
		// with the previous one on a change, or be forgotten on request.
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
	if len(pending) == 0 {
		return nil
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for _, c := range pending {
		sealed, err := s.sealer.Seal(c.label, c.value)
		if err == nil {
			err = c.apply(tx, sealed)
		}
		if err != nil {
			tx.Rollback()
			return fmt.Errorf("encrypt %s: %w", c.where, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// VACUUM cannot run inside a transaction. It rewrites the file from
	// the live rows, so the vacated plaintext pages are gone; the
	// checkpoint then empties the WAL, which still holds the old pages.
	if _, err := s.db.ExecContext(ctx, `VACUUM`); err != nil {
		return fmt.Errorf("vacuum after encrypting credentials: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return fmt.Errorf("checkpoint after encrypting credentials: %w", err)
	}
	s.resealed, s.forgotten = len(pending)-forgotten, forgotten
	return nil
}
