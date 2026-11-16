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

package collect

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func estimateLocal(t *testing.T, home, p string) (TransferEstimate, error) {
	t.Helper()
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader("P=" + shellQuote(p) + "\n" + xferEstimateScript)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, _ := cmd.Output() // a refusal exits 3 after its ERR line
	return parseEstimate(out)
}

// Regular files by size and directories are counted, as rsync -rt would
// carry them; symlinks (to a file or a folder, even a large one outside)
// are not; names with newlines do not shift the count.
func TestXferEstimateCountsWhatACopyCarries(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(home, "dl", "sub", "deeper"), 0o700)
	os.WriteFile(filepath.Join(home, "dl", "a"), make([]byte, 1000), 0o600)
	os.WriteFile(filepath.Join(home, "dl", "sub", "b\nc"), make([]byte, 234), 0o600)
	os.WriteFile(filepath.Join(home, "dl", "sub", "deeper", "e"), nil, 0o600)
	os.WriteFile(filepath.Join(outside, "big"), make([]byte, 1<<20), 0o600)
	os.Symlink(outside, filepath.Join(home, "dl", "ln"))
	os.Symlink(filepath.Join(outside, "big"), filepath.Join(home, "dl", "lnf"))
	e, err := estimateLocal(t, home, "dl")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(filepath.Join(home, "dl"))
	if e.Path != resolved || e.Bytes != 1234 || e.Files != 3 || e.Dirs != 2 || !e.Complete || e.Incomplete != "" {
		t.Fatalf("estimate %+v", e)
	}
	empty := filepath.Join(home, "empty")
	os.Mkdir(empty, 0o700)
	if e, err := estimateLocal(t, home, "empty"); err != nil || e.Bytes != 0 || e.Files != 0 || e.Dirs != 0 || !e.Complete {
		t.Fatalf("empty %+v %v", e, err)
	}
}

func TestXferEstimateRefusals(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(home, "file"), nil, 0o600)
	os.Symlink(outside, filepath.Join(home, "out"))
	for _, p := range []string{"missing", "file", "out", outside, "../"} {
		_, err := estimateLocal(t, home, p)
		var refused *xferRefusal
		if !errors.As(err, &refused) {
			t.Errorf("%q: %v", p, err)
		}
	}
}

func TestXferEstimateUnreadableIsIncomplete(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	home := t.TempDir()
	locked := filepath.Join(home, "dl", "locked")
	os.MkdirAll(locked, 0o700)
	os.WriteFile(filepath.Join(home, "dl", "a"), make([]byte, 10), 0o600)
	os.Chmod(locked, 0)
	t.Cleanup(func() { os.Chmod(locked, 0o700) })
	e, err := estimateLocal(t, home, "dl")
	if err != nil || e.Complete || e.Incomplete != "some folders could not be read" || e.Bytes != 10 {
		t.Fatalf("estimate %+v %v", e, err)
	}
}

func TestParseEstimate(t *testing.T) {
	for _, tc := range []struct {
		out, incomplete string
		ok              bool
	}{
		{"DIR /h/u/dl\nSIZE 5 1 0 0\n", "", true},
		{"DIR /h/u/dl\nSIZE 5 1 0 124\n", "the scan stopped after 20 s", true},
		{"DIR /h/u/dl\nSIZE 5 1 0 2\n", "the scan ended early (exit 2)", true},
		{"DIR /h/u/dl\n", "", false},                             // cut off before the total
		{"DIR /h/u/dl\nSIZE 5 1 0 0\nSIZE 9 9 9 0\n", "", false}, // two totals: not this script's answer
		{"DIR /h/u/dl\nSIZE -5 1 0 0\n", "", false},
		{"DIR /h/u/dl\nSIZE 5 x 0 0\n", "", false},
		{"DIR rel\nSIZE 5 1 0 0\n", "", false},
		{"", "", false},
	} {
		e, err := parseEstimate([]byte(tc.out))
		if (err == nil) != tc.ok || (tc.ok && (e.Incomplete != tc.incomplete || e.Complete != (tc.incomplete == ""))) {
			t.Errorf("%q: %+v %v", tc.out, e, err)
		}
	}
}

func TestEstimateResultNeedsACompleteRun(t *testing.T) {
	if _, err := estimateResult([]byte("DIR /h/u\nSIZE 1 1 0 0\n"), nil, errors.New("connection lost")); err == nil {
		t.Fatal("a failed run passed")
	}
	if _, err := estimateResult([]byte("ERR no such directory\n"), nil, errors.New("exit 3")); !errors.Is(err, ErrTransferInput) {
		t.Fatalf("a refusal is an input error: %v", err)
	}
	if _, err := estimateResult([]byte(strings.Repeat("x", xferEstimateOutCap+1)), nil, nil); err == nil {
		t.Fatal("oversized answer passed")
	}
}

func TestEstimateWarnings(t *testing.T) {
	polled := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	const G = int64(1) << 30
	disk := DiskState{OK: true, QuotaBytes: 100 * G, UsedBytes: 60 * G, FilesSoft: 1000, FilesUsed: 400}
	has := func(e TransferEstimate, sub string) bool {
		for _, w := range e.Warnings {
			if strings.Contains(w, sub) {
				return true
			}
		}
		return false
	}
	e := TransferEstimate{Bytes: 10 * G, Files: 10, Complete: true}
	estimateWarnings(&e, disk, polled, xferModeNew)
	if len(e.Warnings) != 0 || !e.DstKnown || e.DstFreeBytes != 40*G || e.DstFreeFiles != 600 || !e.DstPolled.Equal(polled) {
		t.Fatalf("fits: %+v", e)
	}
	e = TransferEstimate{Bytes: 50 * G, Complete: true}
	estimateWarnings(&e, disk, polled, xferModeNew)
	if !has(e, "more than the 40.0 GiB free") || strings.Contains(e.Warnings[0], "at least") {
		t.Fatalf("too big: %v", e.Warnings)
	}
	e = TransferEstimate{Bytes: 39 * G, Complete: false}
	estimateWarnings(&e, disk, polled, xferModeNew)
	if !has(e, "at least 39.0 GiB of the 40.0 GiB free") {
		t.Fatalf("tight, partial: %v", e.Warnings)
	}
	e = TransferEstimate{Bytes: G, Files: 500, Dirs: 101, Complete: true}
	estimateWarnings(&e, disk, polled, xferModeReplace)
	if !has(e, "601 files and folders, more than the 600") || !has(e, "not taken into account") {
		t.Fatalf("inodes, replace: %v", e.Warnings)
	}
	// Over its quota already: no room, not a negative one.
	e = TransferEstimate{Bytes: 1, Complete: true}
	estimateWarnings(&e, DiskState{OK: true, QuotaBytes: G, UsedBytes: 2 * G}, polled, xferModeNew)
	if e.DstFreeBytes != 0 || e.DstFreeFiles != -1 || !has(e, "more than the 0 B free") {
		t.Fatalf("over quota: %+v", e)
	}
	// Unknown room is said, not taken for zero or plenty.
	e = TransferEstimate{Bytes: G, Complete: true}
	estimateWarnings(&e, DiskState{}, polled, xferModeNew)
	if e.DstKnown || !has(e, "unknown") || len(e.Warnings) != 1 {
		t.Fatalf("unknown: %+v", e)
	}
}

// Over the in-process sshd: the scan runs on the box, under the source
// revision, against the destination record named; one scan per box.
func TestEstimateOverSSH(t *testing.T) {
	ctx := context.Background()
	m, _, _ := genTestManager(t)
	root := t.TempDir()
	port, hostKey := testSSHServer(t, root)
	b := sshTestBox(t, m, "box", "u", port, hostKey)
	d := sshTestBox(t, m, "dst", "v", port, hostKey)
	seedRemote(m, b.ID)
	seedRemote(m, d.ID)
	home := filepath.Join(root, "u")
	os.MkdirAll(filepath.Join(home, "data", "sub"), 0o700)
	os.WriteFile(filepath.Join(home, "data", "sub", "f"), make([]byte, 4321), 0o600)
	p, dp := m.poller(b.ID), m.poller(d.ID)
	rctx := WithBoxRevision(ctx, p.box.Revision)
	e, err := m.EstimateTransfer(rctx, b.ID, "data", d.ID, dp.box.Revision, xferModeNew)
	if err != nil {
		t.Fatal(err)
	}
	if e.Bytes != 4321 || e.Files != 1 || e.Dirs != 1 || !e.Complete || e.BoxRev != p.box.Revision || e.Warnings == nil {
		t.Fatalf("estimate %+v", e)
	}
	if _, err := m.EstimateTransfer(rctx, b.ID, "missing", d.ID, dp.box.Revision, xferModeNew); !errors.Is(err, ErrTransferInput) {
		t.Errorf("missing folder: %v", err)
	}
	if _, err := m.EstimateTransfer(rctx, b.ID, "a/../b", d.ID, dp.box.Revision, xferModeNew); !errors.Is(err, ErrTransferInput) {
		t.Errorf("bad path: %v", err)
	}
	if _, err := m.EstimateTransfer(rctx, b.ID, "data", d.ID, dp.box.Revision+5, xferModeNew); !errors.Is(err, ErrStaleBox) {
		t.Errorf("stale destination: %v", err)
	}
	if _, err := m.EstimateTransfer(WithBoxRevision(ctx, p.box.Revision+5), b.ID, "data", d.ID, dp.box.Revision, xferModeNew); !errors.Is(err, ErrStaleBox) {
		t.Errorf("stale source: %v", err)
	}
	xferEstimating.Store(b.ID, true)
	_, err = m.EstimateTransfer(rctx, b.ID, "data", d.ID, dp.box.Revision, xferModeNew)
	xferEstimating.Delete(b.ID)
	if !errors.Is(err, ErrTransferBusy) {
		t.Errorf("a second scan of one box: %v", err)
	}
}
