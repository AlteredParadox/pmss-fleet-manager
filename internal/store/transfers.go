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
	"time"
)

// Transfer is one box-to-box copy: just enough to find the job on the
// boxes again after a restart, refuse to act on a box record that now
// names another machine, and remember the cleanup that is still owed. The
// progress itself is read from the source box, never stored. Nothing here
// is a secret (the key line is the public half).
type Transfer struct {
	ID         int64  `json:"id"`
	Token      string `json:"-"`
	SrcBoxID   int64  `json:"src_box"`
	SrcEndp    string `json:"src_endpoint"`
	SrcHostKey string `json:"-"`
	DstBoxID   int64  `json:"dst_box"`
	DstEndp    string `json:"dst_endpoint"`
	DstHostKey string `json:"-"`
	SrcPath    string `json:"src_path"`
	DstPath    string `json:"dst_path"`
	// PubKey is the job key's base64 blob, saved before the key is added
	// to the destination: removal matches this exact key.
	PubKey string `json:"-"`
	Phase  string `json:"phase"`
	RC     string `json:"rc,omitempty"`
	Result string `json:"result,omitempty"`
	// Pending is the cleanup still owed, as TransferPending* bits.
	Pending    int       `json:"-"`
	CreatedAt  time.Time `json:"created_at"`
	FinishedAt time.Time `json:"finished_at"`
}

// Cleanup a transfer may still owe.
const (
	TransferPendingSrcJob   = 1 // the job directory on the source (with the key files until the runner deletes them)
	TransferPendingDstKey   = 2 // the job's line in the destination's authorized_keys
	TransferPendingDstEmpty = 4 // the claimed destination directory, removed only while empty (failed setup)
)

const createTransfersSQL = `CREATE TABLE IF NOT EXISTS transfers (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	token TEXT NOT NULL UNIQUE,
	src_box_id INTEGER NOT NULL, src_endpoint TEXT NOT NULL, src_host_key TEXT NOT NULL,
	dst_box_id INTEGER NOT NULL, dst_endpoint TEXT NOT NULL, dst_host_key TEXT NOT NULL,
	src_path TEXT NOT NULL, dst_path TEXT NOT NULL, pubkey TEXT NOT NULL DEFAULT '',
	phase TEXT NOT NULL, rc TEXT NOT NULL DEFAULT '', result TEXT NOT NULL DEFAULT '',
	pending INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL, finished_at TEXT NOT NULL DEFAULT '')`

const transferCols = `id, token, src_box_id, src_endpoint, src_host_key, dst_box_id, dst_endpoint, dst_host_key,
	src_path, dst_path, pubkey, phase, rc, result, pending, created_at, finished_at`

// CreateTransfer inserts t and sets its ID and CreatedAt.
func (s *Store) CreateTransfer(ctx context.Context, t *Transfer) error {
	t.CreatedAt = time.Now().UTC().Truncate(time.Second)
	res, err := s.db.ExecContext(ctx, `INSERT INTO transfers (token, src_box_id, src_endpoint, src_host_key, dst_box_id, dst_endpoint, dst_host_key,
		src_path, dst_path, pubkey, phase, rc, result, pending, created_at, finished_at) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.Token, t.SrcBoxID, t.SrcEndp, t.SrcHostKey, t.DstBoxID, t.DstEndp, t.DstHostKey,
		t.SrcPath, t.DstPath, t.PubKey, t.Phase, t.RC, t.Result, t.Pending, t.CreatedAt.Format(time.RFC3339), fmtTime(t.FinishedAt))
	if err != nil {
		return err
	}
	t.ID, err = res.LastInsertId()
	return err
}

// SaveTransfer writes the fields a job changes over its life.
func (s *Store) SaveTransfer(ctx context.Context, t *Transfer) error {
	res, err := s.db.ExecContext(ctx, `UPDATE transfers SET pubkey=?, phase=?, rc=?, result=?, pending=?, finished_at=? WHERE id=?`,
		t.PubKey, t.Phase, t.RC, t.Result, t.Pending, fmtTime(t.FinishedAt), t.ID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

// ListTransfers returns every transfer, oldest first.
func (s *Store) ListTransfers(ctx context.Context) ([]*Transfer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+transferCols+` FROM transfers ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Transfer
	for rows.Next() {
		var t Transfer
		var created, finished string
		if err := rows.Scan(&t.ID, &t.Token, &t.SrcBoxID, &t.SrcEndp, &t.SrcHostKey, &t.DstBoxID, &t.DstEndp, &t.DstHostKey,
			&t.SrcPath, &t.DstPath, &t.PubKey, &t.Phase, &t.RC, &t.Result, &t.Pending, &created, &finished); err != nil {
			return nil, err
		}
		t.CreatedAt, _ = time.Parse(time.RFC3339, created)
		t.FinishedAt, _ = time.Parse(time.RFC3339, finished)
		out = append(out, &t)
	}
	return out, rows.Err()
}

// DeleteTransfer removes a transfer row.
func (s *Store) DeleteTransfer(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM transfers WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

func fmtTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}
