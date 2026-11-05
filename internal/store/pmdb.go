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

// PMDBSubmission is the receipt of a host's last accepted submission to
// the pmdb.fyi catalogue: the hash of the row this dashboard built for it,
// the identifier it was posted under and when. The collector resubmits a
// host when any of the three calls for it (the facts changed, the
// identifier changed, the refresh interval passed), and not otherwise, so
// a restart does not post the fleet again.
type PMDBSubmission struct {
	Hostname    string
	Username    string
	RowHash     string
	SubmittedAt time.Time
}

// PMDBSubmissions returns every receipt, keyed by hostname.
func (s *Store) PMDBSubmissions(ctx context.Context) (map[string]PMDBSubmission, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hostname, username, row_hash, submitted_at FROM pmdb_submissions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]PMDBSubmission{}
	for rows.Next() {
		var r PMDBSubmission
		var at string
		if err := rows.Scan(&r.Hostname, &r.Username, &r.RowHash, &at); err != nil {
			return nil, err
		}
		if r.SubmittedAt, err = time.Parse(time.RFC3339Nano, at); err != nil {
			continue // a receipt with no readable time is no receipt: the host is posted once more
		}
		out[r.Hostname] = r
	}
	return out, rows.Err()
}

// PutPMDBSubmission stores a receipt, replacing the host's previous one.
func (s *Store) PutPMDBSubmission(ctx context.Context, r PMDBSubmission) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO pmdb_submissions(hostname, username, row_hash, submitted_at) VALUES(?,?,?,?)
		ON CONFLICT(hostname) DO UPDATE SET username=excluded.username, row_hash=excluded.row_hash, submitted_at=excluded.submitted_at`,
		r.Hostname, r.Username, r.RowHash, r.SubmittedAt.UTC().Format(time.RFC3339Nano))
	return err
}
