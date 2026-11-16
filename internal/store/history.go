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

// The longer-term box history: one row per box per UTC hour, kept for
// HistoryKeep. The poller adds what it measured (sums and counts, so a
// row written in several flushes, or by two pollers across an edit,
// still averages right); disk figures are the hour's latest known ones.
// Rows carry the generation (collect's host:port/user@key) they were
// measured under, and a reader keeps only the current one's.

// HistoryKeep is how long hourly rows are kept.
const HistoryKeep = 31 * 24 * time.Hour

const createHistorySQL = `CREATE TABLE IF NOT EXISTS box_history (
	box_id INTEGER NOT NULL REFERENCES boxes(id) ON DELETE CASCADE,
	hour TEXT NOT NULL, gen TEXT NOT NULL,
	disk_used INTEGER NOT NULL DEFAULT -1, disk_quota INTEGER NOT NULL DEFAULT -1, files_used INTEGER NOT NULL DEFAULT -1,
	io_sum REAL NOT NULL DEFAULT 0, io_max REAL NOT NULL DEFAULT 0, busy_sum REAL NOT NULL DEFAULT 0, cpu_n INTEGER NOT NULL DEFAULT 0,
	polls INTEGER NOT NULL DEFAULT 0,
	PRIMARY KEY (box_id, hour, gen))`

// historyHour is the row key format: the UTC hour.
const historyHour = "2006-01-02T15"

// HistoryDelta is what one flush adds to an hour's row. Disk figures
// below 0 are unknown and leave the row's as they are.
type HistoryDelta struct {
	Hour                           time.Time
	Gen                            string
	DiskUsed, DiskQuota, FilesUsed int64
	IOSum, IOMax, BusySum          float64
	CPUN                           int // polls with a host CPU reading (the sums' count)
	Polls                          int
}

// AddHistory merges d into its hour's row.
func (s *Store) AddHistory(ctx context.Context, boxID int64, d HistoryDelta) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO box_history(box_id, hour, gen, disk_used, disk_quota, files_used, io_sum, io_max, busy_sum, cpu_n, polls)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(box_id, hour, gen) DO UPDATE SET
			disk_used = CASE WHEN excluded.disk_used >= 0 THEN excluded.disk_used ELSE disk_used END,
			disk_quota = CASE WHEN excluded.disk_quota >= 0 THEN excluded.disk_quota ELSE disk_quota END,
			files_used = CASE WHEN excluded.files_used >= 0 THEN excluded.files_used ELSE files_used END,
			io_sum = io_sum + excluded.io_sum, io_max = max(io_max, excluded.io_max),
			busy_sum = busy_sum + excluded.busy_sum, cpu_n = cpu_n + excluded.cpu_n, polls = polls + excluded.polls`,
		boxID, d.Hour.UTC().Format(historyHour), d.Gen, d.DiskUsed, d.DiskQuota, d.FilesUsed, d.IOSum, d.IOMax, d.BusySum, d.CPUN, d.Polls)
	return err
}

// HistoryRow is one stored hour.
type HistoryRow struct {
	Hour                           time.Time
	Gen                            string
	DiskUsed, DiskQuota, FilesUsed int64 // -1: unknown
	IOSum, IOMax, BusySum          float64
	CPUN, Polls                    int
}

// History returns box boxID's rows from since on, oldest first, of every
// generation (the caller keeps its own).
func (s *Store) History(ctx context.Context, boxID int64, since time.Time) ([]HistoryRow, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT hour, gen, disk_used, disk_quota, files_used, io_sum, io_max, busy_sum, cpu_n, polls
		FROM box_history WHERE box_id=? AND hour >= ? ORDER BY hour`, boxID, since.UTC().Format(historyHour))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []HistoryRow
	for rows.Next() {
		var r HistoryRow
		var hour string
		if err := rows.Scan(&hour, &r.Gen, &r.DiskUsed, &r.DiskQuota, &r.FilesUsed, &r.IOSum, &r.IOMax, &r.BusySum, &r.CPUN, &r.Polls); err != nil {
			return nil, err
		}
		t, err := time.Parse(historyHour, hour)
		if err != nil {
			continue // not a row this code wrote
		}
		r.Hour = t
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneHistory removes the rows older than keep.
func (s *Store) PruneHistory(ctx context.Context, keep time.Duration) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM box_history WHERE hour < ?`, time.Now().Add(-keep).UTC().Format(historyHour))
	return err
}
