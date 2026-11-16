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
	"path/filepath"
	"testing"
	"time"
)

func TestHistoryMergeAndPrune(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "h.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	b := &Box{Name: "a", Host: "h", User: "u", Enabled: true}
	if err := s.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Hour)
	add := func(hour time.Time, gen string, used int64, io float64) {
		t.Helper()
		if err := s.AddHistory(ctx, b.ID, HistoryDelta{Hour: hour, Gen: gen, DiskUsed: used, DiskQuota: 100, FilesUsed: -1, IOSum: io, IOMax: io, BusySum: 1, CPUN: 1, Polls: 1}); err != nil {
			t.Fatal(err)
		}
	}
	add(now, "g", 10, 4)
	add(now, "g", -1, 8) // unknown disk keeps the hour's last known figure
	add(now, "other", 99, 1)
	add(now.Add(-40*24*time.Hour), "g", 5, 1)
	rows, err := s.History(ctx, b.ID, now.Add(-24*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows %+v", rows)
	}
	var g HistoryRow
	for _, r := range rows {
		if r.Gen == "g" {
			g = r
		}
	}
	if !g.Hour.Equal(now) || g.DiskUsed != 10 || g.DiskQuota != 100 || g.FilesUsed != -1 || g.IOSum != 12 || g.IOMax != 8 || g.CPUN != 2 || g.Polls != 2 {
		t.Fatalf("merged %+v", g)
	}
	if err := s.PruneHistory(ctx, HistoryKeep); err != nil {
		t.Fatal(err)
	}
	all, _ := s.History(ctx, b.ID, now.Add(-60*24*time.Hour))
	if len(all) != 2 {
		t.Fatalf("after prune %+v", all)
	}
	if err := s.DeleteBox(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	if left, _ := s.History(ctx, b.ID, now.Add(-60*24*time.Hour)); len(left) != 0 {
		t.Fatalf("a removed box's history stays: %+v", left)
	}
}
