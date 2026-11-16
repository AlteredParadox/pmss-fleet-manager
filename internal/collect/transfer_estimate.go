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

// The transfer preflight: on request, an estimate of the source folder's
// size, compared with the room the destination's last poll showed. It is
// advice, never a gate: the start does not depend on it, and files and
// free space can both change between the estimate and the copy.

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// xferEstimateSecs bounds the scan on the box; a larger tree is reported
// as at least what was counted by then.
const xferEstimateSecs = 20

// xferEstimateOutCap bounds the answer: one DIR line (a path up to
// PATH_MAX) and one SIZE line.
const xferEstimateOutCap = 4096 + 128

// xferEstimateScript sums what a copy would carry: regular files (their
// sizes) and directories. Symlinks and special files are not copied, so
// they are not counted. The scan runs at idle I/O and lowest CPU priority
// and stops after xferEstimateSecs; only sizes and types are printed,
// never names, so a name with a newline cannot shift the count. RC is
// find's exit status: 124 timed out, 1 some folders could not be read.
var xferEstimateScript = xferResolveDirScript + `printf 'DIR %s\n' "$d"
run(){ if command -v ionice >/dev/null 2>&1; then ionice -c3 nice -n19 "$@"; else nice -n19 "$@"; fi; }
{ run timeout ` + strconv.Itoa(xferEstimateSecs) + ` find "$d" -mindepth 1 \( -type f -printf 'f %s\n' \) -o \( -type d -printf 'd\n' \) 2>/dev/null; echo "RC $?"; } |
  awk '$1=="f"{f++; b+=$2; next} $1=="d"{n++; next} $1=="RC"{rc=$2} END{printf "SIZE %.0f %d %d %s\n", b, f, n, rc}'
exit 0
`

// TransferEstimate is the answer of one preflight.
type TransferEstimate struct {
	BoxRev   int64  `json:"box_rev"` // the source record revision the scan ran under
	Path     string `json:"path"`    // the source folder, resolved
	Bytes    int64  `json:"bytes"`   // sum of the regular files' sizes
	Files    int64  `json:"files"`
	Dirs     int64  `json:"dirs"`
	Complete bool   `json:"complete"`
	// Incomplete says why the figures are a lower bound.
	Incomplete string  `json:"incomplete,omitempty"`
	Seconds    float64 `json:"seconds"`
	// The destination's room at its last poll, as the warnings used it.
	DstKnown     bool      `json:"dst_known"`
	DstFreeBytes int64     `json:"dst_free_bytes"`
	DstFreeFiles int64     `json:"dst_free_files"` // -1: no inode limit known
	DstPolled    time.Time `json:"dst_polled,omitzero"`
	Warnings     []string  `json:"warnings"`
}

// parseEstimate reads the script's answer.
func parseEstimate(out []byte) (TransferEstimate, error) {
	var e TransferEstimate
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for _, l := range lines {
		if msg, ok := strings.CutPrefix(l, "ERR "); ok {
			return e, &xferRefusal{msg}
		}
	}
	path, ok := xferOnly(lines, "DIR")
	if !ok || !strings.HasPrefix(path, "/") || !utf8.ValidString(path) {
		return e, errors.New("unexpected estimate output")
	}
	size, ok := xferOnly(lines, "SIZE")
	f := strings.Fields(size)
	if !ok || len(f) != 4 {
		return e, errors.New("the estimate did not finish")
	}
	nums := make([]int64, 3)
	for i := range nums {
		n, err := strconv.ParseInt(f[i], 10, 64)
		if err != nil || n < 0 {
			return e, errors.New("unexpected estimate output")
		}
		nums[i] = n
	}
	e.Path, e.Bytes, e.Files, e.Dirs = path, nums[0], nums[1], nums[2]
	switch f[3] {
	case "0":
		e.Complete = true
	case "124":
		e.Incomplete = fmt.Sprintf("the scan stopped after %d s", xferEstimateSecs)
	case "1":
		e.Incomplete = "some folders could not be read"
	default:
		e.Incomplete = "the scan ended early (exit " + clip(f[3], 8) + ")"
	}
	return e, nil
}

// estimateWarnings compares the estimate with the destination's room at
// its last poll (disk: its DiskState). mode is the transfer's destination
// mode: into an existing folder some files may already be there, which
// the estimate does not know about, so the figures are an upper bound.
func estimateWarnings(e *TransferEstimate, disk DiskState, polled time.Time, mode string) {
	e.Warnings = []string{}
	e.DstFreeFiles = -1
	if !disk.OK || disk.QuotaBytes <= 0 {
		e.Warnings = append(e.Warnings, "the destination's free space is unknown (no quota reading yet)")
		return
	}
	e.DstKnown, e.DstPolled = true, polled
	e.DstFreeBytes = max(0, disk.QuotaBytes-disk.UsedBytes)
	if disk.FilesSoft > 0 {
		e.DstFreeFiles = max(0, disk.FilesSoft-disk.FilesUsed)
	}
	atLeast := ""
	if !e.Complete {
		atLeast = "at least "
	}
	switch {
	case e.Bytes > e.DstFreeBytes:
		e.Warnings = append(e.Warnings, fmt.Sprintf("it needs %s%s, more than the %s free in the destination's quota: the copy would likely fail with “out of space”", atLeast, fmtBytesShort(e.Bytes), fmtBytesShort(e.DstFreeBytes)))
	case e.Bytes > e.DstFreeBytes*9/10:
		e.Warnings = append(e.Warnings, fmt.Sprintf("it needs %s%s of the %s free in the destination's quota: little room is left for anything else", atLeast, fmtBytesShort(e.Bytes), fmtBytesShort(e.DstFreeBytes)))
	}
	if n := e.Files + e.Dirs; e.DstFreeFiles >= 0 && n > e.DstFreeFiles {
		e.Warnings = append(e.Warnings, fmt.Sprintf("it holds %s%d files and folders, more than the %d the destination's file limit has left", atLeast, n, e.DstFreeFiles))
	}
	if mode == xferModeKeep || mode == xferModeReplace {
		e.Warnings = append(e.Warnings, "files already in the destination folder are not taken into account: the copy may need less")
	}
}

// xferEstimating holds the boxes with a scan in flight: one at a time per
// box, so repeated clicks cannot pile scans up on one disk.
var xferEstimating sync.Map

// EstimateTransfer scans folder path on box srcID (under the revision ctx
// names) and compares it with box dstID's room at the revision dstRev.
func (m *Manager) EstimateTransfer(ctx context.Context, srcID int64, path string, dstID, dstRev int64, mode string) (TransferEstimate, error) {
	sp, err := m.actionPoller(ctx, srcID)
	if err != nil {
		return TransferEstimate{}, err
	}
	if _, err := sp.readyState(); err != nil {
		return TransferEstimate{}, err
	}
	if err := m.CheckBoxRevision(dstID, dstRev); err != nil {
		return TransferEstimate{}, err
	}
	dst, ok := m.State(dstID)
	if !ok {
		return TransferEstimate{}, fmt.Errorf("%w: unknown destination box", ErrTransferInput)
	}
	path = strings.TrimPrefix(path, "~/")
	if err := validXferPath(path, false); err != nil {
		return TransferEstimate{}, fmt.Errorf("%w: %v", ErrTransferInput, err)
	}
	if _, busy := xferEstimating.LoadOrStore(srcID, true); busy {
		return TransferEstimate{}, fmt.Errorf("%w: a size estimate of this box is already running", ErrTransferBusy)
	}
	defer xferEstimating.Delete(srcID)
	cli, err := sp.conn(ctx)
	if err != nil {
		return TransferEstimate{}, err
	}
	rctx, cancel := context.WithTimeout(ctx, (xferEstimateSecs+25)*time.Second)
	defer cancel()
	began := time.Now()
	out, stderr, err := cli.RunLimited(rctx, "/bin/sh", strings.NewReader("P="+shellQuote(path)+"\n"+xferEstimateScript), xferEstimateOutCap+1)
	e, err := estimateResult(out, stderr, err)
	if err != nil {
		return TransferEstimate{}, err
	}
	e.BoxRev, e.Seconds = sp.box.Revision, time.Since(began).Round(100*time.Millisecond).Seconds()
	estimateWarnings(&e, dst.Disk, dst.LastPoll, mode)
	return e, nil
}

// estimateResult turns one scan run into its answer, as xferDirsResult
// does for a listing: the box's refusal is an input error, a run that did
// not complete is an error whatever it printed.
func estimateResult(out, stderr []byte, runErr error) (TransferEstimate, error) {
	if len(out) > xferEstimateOutCap {
		return TransferEstimate{}, fmt.Errorf("the box sent %d bytes, more than an estimate holds", len(out))
	}
	e, perr := parseEstimate(out)
	var refused *xferRefusal
	if errors.As(perr, &refused) {
		return TransferEstimate{}, xferInputError(refused.msg)
	}
	if runErr != nil {
		return TransferEstimate{}, fmt.Errorf("estimate not completed: %v %s", runErr, clip(strings.TrimSpace(string(stderr)), 300))
	}
	return e, perr
}
