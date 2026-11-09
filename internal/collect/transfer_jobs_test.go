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
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pmss-fleet-manager/internal/store"
)

func TestParseXferStatus(t *testing.T) {
	l := parseXferStatus("STARTED\nPIDFILE\nALIVE\nrc=24\npid=12\nskipped=3\nerrors.count=2\nprogress=  1,073,741,894  99%   48.31MB/s    0:00:21 (xfr#11, to-chk=0/14)\n--errors\nfile vanished: \"a\"\nfile vanished: \"b\"")
	if !l.started || !l.pidFile || !l.alive || l.rc != "24" || l.skipped != 3 || l.errCount != 2 || !strings.HasPrefix(l.errors, "file vanished: \"a\"\nfile") || l.stage != "" || l.move != "" {
		t.Fatalf("%+v", l)
	}
	if l := parseXferStatus("STARTED\nPIDFILE\nrc=0\nstage=remove\nmove=left 2\nremove_err=find: cannot delete 'x': Permission denied"); l.stage != "remove" || l.move != "left 2" || !strings.HasPrefix(l.removeErr, "find: cannot delete") {
		t.Fatalf("%+v", l)
	}
	p := parseProgress(l.progress)
	if p == nil || p.Bytes != 1073741894 || p.Pct != 99 || p.Files != 11 || p.Total != 14 || p.Time != "0:00:21" {
		t.Fatalf("%+v", p)
	}
	if parseProgress("") != nil || parseProgress("sending incremental file list") != nil {
		t.Error("non-progress text parsed")
	}
	if p := parseProgress("     93,487,104   8%   25.84MB/s    0:00:37"); p == nil || p.Pct != 8 || p.Files != 0 {
		t.Errorf("%+v", p)
	}
}

// Only an exit code finishes a job and only a runner proven gone without
// one interrupts it; a 99% (or 100%) progress line proves nothing, and a
// read that shows nothing new leaves the phase alone.
func TestXferDecide(t *testing.T) {
	full := "  1,073,741,894 100%   48.31MB/s    0:00:21 (xfr#11, to-chk=0/14)"
	for _, c := range []struct {
		name        string
		phase       string
		l           xferLive
		cancelAsked bool
		want        string
	}{
		{"rc 0", xferRunning, xferLive{started: true, pidFile: true, rc: "0"}, false, xferFinished},
		{"rc while alive", xferRunning, xferLive{started: true, pidFile: true, alive: true, rc: "0"}, false, xferFinished},
		{"100% but alive, no rc", xferRunning, xferLive{started: true, pidFile: true, alive: true, progress: full}, false, xferRunning},
		{"100% and gone, no rc", xferRunning, xferLive{started: true, pidFile: true, progress: full}, false, xferInterrupted},
		{"between marker and pid", xferRunning, xferLive{started: true}, false, xferRunning},
		{"unconfirmed, marker only", xferUnconfirmed, xferLive{started: true}, false, xferUnconfirmed},
		{"unconfirmed, marker only, cancel", xferUnconfirmed, xferLive{started: true}, true, xferUnconfirmed},
		{"unconfirmed, now started", xferUnconfirmed, xferLive{started: true, pidFile: true, alive: true}, false, xferRunning},
		{"unconfirmed, nothing yet", xferUnconfirmed, xferLive{}, false, xferUnconfirmed},
		{"unconfirmed, cancel, never started", xferUnconfirmed, xferLive{}, true, xferInterrupted},
		{"job dir gone", xferRunning, xferLive{noJob: true}, false, xferInterrupted},
		{"cancelled", xferRunning, xferLive{started: true, pidFile: true, rc: "cancelled"}, true, xferFinished},
	} {
		if got := xferDecide(c.phase, c.l, c.cancelAsked); got != c.want {
			t.Errorf("%s: %s, want %s", c.name, got, c.want)
		}
	}
}

func TestXferResult(t *testing.T) {
	prog := "  3,145,733  99%  600.00MB/s    0:00:00 (xfr#2, to-chk=0/5)"
	for _, c := range []struct {
		phase string
		l     xferLive
		want  string
	}{
		{xferFinished, xferLive{rc: "0", progress: prog, skipped: 1}, "copied 3.0 MiB in 2 files; 1 symlinks or special files skipped"},
		{xferFinished, xferLive{rc: "0", progress: prog, kept: 3, conflicts: 1}, "copied 3.0 MiB in 2 files; 3 existing files kept; 1 name conflicts (a file against a folder) skipped"},
		{xferFinished, xferLive{rc: "cancelled", kept: 3}, "cancelled"},
		{xferFinished, xferLive{rc: "24", progress: prog}, "completed with warnings: some source files vanished during the copy; 3.0 MiB in 2 files"},
		{xferFinished, xferLive{rc: "23", errors: "rsync: open \"x\" failed: Permission denied (13)"}, "partial: some files could not be transferred — rsync: open \"x\" failed: Permission denied (13)"},
		{xferFinished, xferLive{rc: "11", errors: "rsync: write failed on \"/h/d/big\": No space left on device (28)"}, "destination out of space or quota — rsync: write failed on \"/h/d/big\": No space left on device (28)"},
		{xferFinished, xferLive{rc: "12", errors: "rsync: write failed: Disk quota exceeded (122)"}, "destination out of space or quota — rsync: write failed: Disk quota exceeded (122)"},
		{xferFinished, xferLive{rc: "255", errors: "Host key verification failed."}, "rsync exited with code 255 — Host key verification failed."},
		{xferFinished, xferLive{rc: "cancelled", errors: "rsync error: received SIGINT, SIGTERM, or SIGHUP (code 20) at rsync.c(716) [sender=3.2.7]"}, "cancelled"},
		{xferInterrupted, xferLive{started: true, pidFile: true}, "interrupted: the runner ended without a result (box rebooted or the process was killed?)"},
		{xferInterrupted, xferLive{}, "not started: the runner never started on the source"},
	} {
		if got := xferResult(c.phase, c.l, false); got != c.want {
			t.Errorf("rc %q: %q\nwant %q", c.l.rc, got, c.want)
		}
	}
	// A move: only exit code 0 removed anything; every other outcome says
	// the source is still there, and an interrupted removal says what may
	// be half done.
	for _, c := range []struct {
		phase string
		l     xferLive
		want  string
	}{
		{xferFinished, xferLive{rc: "0", progress: prog, stage: "remove", move: "moved"}, "moved: copied and the source folder removed; 3.0 MiB in 2 files"},
		{xferFinished, xferLive{rc: "0", progress: prog, stage: "remove", move: "left 3", skipped: 1}, "moved: copied; 3 entries left in the source folder (symlinks and special files, which are never copied; files changed since the copy started; or entries that could not be removed); 3.0 MiB in 2 files; 1 symlinks or special files skipped"},
		{xferFinished, xferLive{rc: "0", stage: "remove", move: "left 0", removeErr: "find: cannot delete '/h/src': Permission denied"}, "moved: copied; the source folder is empty but could not be removed — find: cannot delete '/h/src': Permission denied;"},
		{xferFinished, xferLive{rc: "0", stage: "remove", move: "left 1", removeErr: "find: cannot delete '/h/src/a': Permission denied"}, "moved: copied; 1 entries left in the source folder (symlinks and special files, which are never copied; files changed since the copy started; or entries that could not be removed) — find: cannot delete '/h/src/a': Permission denied;"},
		{xferFinished, xferLive{rc: "0", progress: prog}, "copied, but not moved: the runner recorded no removal outcome; the source was left as it is; 3.0 MiB in 2 files"},
		{xferFinished, xferLive{rc: "24", progress: prog}, "completed with warnings: some source files vanished during the copy; 3.0 MiB in 2 files; the source was left in place"},
		{xferFinished, xferLive{rc: "cancelled"}, "cancelled; the source was left in place"},
		{xferFinished, xferLive{rc: "23", errors: "rsync: open \"x\" failed: Permission denied (13)"}, "partial: some files could not be transferred; the source was left in place — rsync: open \"x\" failed: Permission denied (13)"},
		{xferFinished, xferLive{rc: "11", errors: "rsync: write failed: No space left on device (28)"}, "destination out of space or quota; the source was left in place — rsync: write failed: No space left on device (28)"},
		{xferInterrupted, xferLive{started: true, pidFile: true}, "interrupted: the runner ended without a result (box rebooted or the process was killed?); the source was left in place"},
		{xferInterrupted, xferLive{started: true, pidFile: true, stage: "remove", progress: prog}, "interrupted while removing the source: the copy completed, the removal ended without a result (box rebooted or the process was killed?); check the source folder; 3.0 MiB in 2 files"},
	} {
		if got := xferResult(c.phase, c.l, true); got != c.want {
			t.Errorf("move rc %q: %q\nwant %q", c.l.rc, got, c.want)
		}
	}
}

func xferTestManager(t *testing.T, st *store.Store) *Manager {
	t.Helper()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	return m
}

func openTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// After a restart the dashboard reads jobs, never starts them: a job cut
// off mid-setup owes every cleanup it may have left; a running one stays
// running (its source is not reachable here, which changes nothing).
func TestTransfersRestart(t *testing.T) {
	defer func(d time.Duration) { xferInterval = d }(xferInterval)
	xferInterval = 50 * time.Millisecond
	ctx := context.Background()
	st := openTestStore(t)
	rows := []*store.Transfer{
		{Token: strings.Repeat("a", 32), SrcBoxID: 1, DstBoxID: 2, Phase: xferPreparing, Pending: store.TransferPendingSrcJob},
		{Token: strings.Repeat("b", 32), SrcBoxID: 1, DstBoxID: 2, Phase: xferPreparing, PubKey: testBlob, Pending: store.TransferPendingSrcJob | store.TransferPendingDstKey},
		{Token: strings.Repeat("c", 32), SrcBoxID: 1, DstBoxID: 2, Phase: xferRunning, PubKey: testBlob, Pending: store.TransferPendingSrcJob | store.TransferPendingDstKey},
		// Stopped between recording the launch and its answer: a runner
		// may exist, so the job is read, not taken for a failed setup.
		{Token: strings.Repeat("d", 32), SrcBoxID: 1, DstBoxID: 2, Phase: xferUnconfirmed, PubKey: testBlob, Pending: store.TransferPendingSrcJob | store.TransferPendingDstKey},
		// Into an existing directory: the same obligations, never the
		// directory (nothing was created).
		{Token: strings.Repeat("e", 32), SrcBoxID: 1, DstBoxID: 2, DstMode: xferModeKeep, Phase: xferPreparing, PubKey: testBlob, Pending: store.TransferPendingSrcJob | store.TransferPendingDstKey},
	}
	for _, r := range rows {
		if err := st.CreateTransfer(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	m := xferTestManager(t, st)
	time.Sleep(300 * time.Millisecond) // a few loop ticks
	got := map[string]TransferView{}
	for _, v := range m.Transfers() {
		got[v.Token] = v
	}
	if v := got[rows[0].Token]; v.Phase != xferSetupFailed || v.Pending != store.TransferPendingSrcJob {
		t.Errorf("preparing without a key: %s %d", v.Phase, v.Pending)
	}
	// The key may have been added; the directory is never assumed to be
	// this job's (it may have existed before), so it is not owed.
	if v := got[rows[1].Token]; v.Phase != xferSetupFailed || v.Pending != store.TransferPendingSrcJob|store.TransferPendingDstKey || len(v.Cleanup) != 2 {
		t.Errorf("preparing with a key: %s %d %v", v.Phase, v.Pending, v.Cleanup)
	}
	if v := got[rows[3].Token]; v.Phase != xferUnconfirmed || v.RC != "" {
		t.Errorf("unconfirmed: %s", v.Phase)
	}
	if v := got[rows[4].Token]; v.Phase != xferSetupFailed || v.DstMode != xferModeKeep || v.Pending != store.TransferPendingSrcJob|store.TransferPendingDstKey {
		t.Errorf("preparing into an existing directory: %s %s %d", v.Phase, v.DstMode, v.Pending)
	}
	v := got[rows[2].Token]
	if v.Phase != xferRunning || v.RC != "" || !strings.Contains(v.StatusErr, "was removed") || !strings.Contains(v.Blocked, "removed") || v.Scanning {
		t.Errorf("running: %+v", v)
	}
	// Stored, not just in memory.
	ts, _ := st.ListTransfers(ctx)
	if ts[0].Phase != xferSetupFailed || ts[2].Phase != xferRunning {
		t.Errorf("stored phases %s %s %s", ts[0].Phase, ts[1].Phase, ts[2].Phase)
	}
}

// A job acts only on a box whose record still names the account and
// machine it saved: an edited endpoint or a different pinned key refuses
// status, cancel and cleanup, and the owed cleanup stays recorded.
func TestTransferIdentityRefuses(t *testing.T) {
	ctx := context.Background()
	m, st, b := genTestManager(t)
	for _, c := range []struct{ name, endp, key string }{
		{"other endpoint", "elsewhere:22/u", ""},
		{"other host key", AuthEndpoint(b), "ecdsa-sha2-nistp256 AAAAother"},
	} {
		tr := &store.Transfer{Token: newTok(t), SrcBoxID: b.ID, SrcEndp: c.endp, SrcHostKey: c.key, DstBoxID: b.ID, DstEndp: c.endp, DstHostKey: c.key,
			Phase: xferRunning, PubKey: testBlob, Pending: store.TransferPendingSrcJob | store.TransferPendingDstKey}
		if err := st.CreateTransfer(ctx, tr); err != nil {
			t.Fatal(err)
		}
		m.xferMu.Lock()
		m.xfers[tr.ID] = &xferJob{t: *tr}
		m.xferMu.Unlock()
		if err := m.CancelTransfer(ctx, b.ID, tr.ID); !errors.Is(err, ErrTransferIdentity) {
			t.Errorf("%s: cancel: %v", c.name, err)
		}
		j, _ := m.xferJobFor(tr.ID, b.ID)
		m.xferRefreshLocked(ctx, j, false)
		if v := m.xferView(tr.ID); v.Phase != xferRunning || !strings.Contains(v.StatusErr, "another account or machine") || v.Blocked == "" {
			t.Errorf("%s: status read acted: %+v", c.name, v)
		}
		// Finished: cleanup is refused on both boxes and stays owed.
		jt := m.xferSnapshot(j)
		jt.Phase = xferFinished
		m.xferSave(j, jt)
		if err := m.RetryTransferCleanup(ctx, b.ID, tr.ID); err == nil || !strings.Contains(err.Error(), "another account or machine") {
			t.Errorf("%s: cleanup: %v", c.name, err)
		}
		if v := m.xferView(tr.ID); v.Pending != store.TransferPendingSrcJob|store.TransferPendingDstKey {
			t.Errorf("%s: pending %d", c.name, v.Pending)
		}
		// Dismissing it needs force and says what may be left.
		if left, err := m.DismissTransfer(ctx, tr.ID, false); !errors.Is(err, ErrTransferBusy) || len(left) != 2 {
			t.Errorf("%s: dismiss without force: %v %v", c.name, left, err)
		}
		if left, err := m.DismissTransfer(ctx, tr.ID, true); err != nil || len(left) != 2 {
			t.Errorf("%s: forced dismiss: %v %v", c.name, left, err)
		}
	}
}

// A setup that fails before anything is proven clean keeps its row, as
// setup_failed with the cleanup it owes, instead of forgetting it.
func TestTransferSetupFailureKeepsUnconfirmedCleanup(t *testing.T) {
	ctx := context.Background()
	m, st, b := genTestManager(t)
	p := m.poller(b.ID)
	tr := &store.Transfer{Token: newTok(t), SrcBoxID: b.ID, SrcEndp: AuthEndpoint(b), SrcHostKey: p.hostKey(),
		DstBoxID: b.ID, DstEndp: AuthEndpoint(b), DstHostKey: p.hostKey(), SrcPath: "data", DstPath: "copy",
		Phase: xferPreparing, Pending: store.TransferPendingSrcJob}
	if err := st.CreateTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	j := &xferJob{t: *tr}
	m.xferMu.Lock()
	m.xfers[tr.ID] = j
	m.xferMu.Unlock()
	j.op.Lock()
	err := m.xferSetupLocked(ctx, j, p, p) // 127.0.0.1:1 refuses: the source step fails, so does its undo
	j.op.Unlock()
	if err == nil || !strings.Contains(err.Error(), "cleanup not confirmed") {
		t.Fatalf("setup: %v", err)
	}
	ts, _ := st.ListTransfers(ctx)
	if len(ts) != 1 || ts[0].Phase != xferSetupFailed || ts[0].Pending != store.TransferPendingSrcJob {
		t.Fatalf("stored: %+v", ts)
	}
	if v := m.xferView(tr.ID); len(v.Cleanup) != 1 {
		t.Errorf("view cleanup %v", v.Cleanup)
	}
}

// One active copy per source box.
func TestTransferOnePerSource(t *testing.T) {
	ctx := context.Background()
	m, st, b := genTestManager(t)
	seedRemote(m, b.ID)
	tr := &store.Transfer{Token: newTok(t), SrcBoxID: b.ID, DstBoxID: 99, Phase: xferRunning}
	if err := st.CreateTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	m.xferMu.Lock()
	m.xfers[tr.ID] = &xferJob{t: *tr}
	m.xferMu.Unlock()
	other := &store.Box{Name: "b", Host: "127.0.0.2", Port: 1, User: "v", AuthMethod: "password", Password: "x", Enabled: true}
	if err := m.AddBox(ctx, other); err != nil {
		t.Fatal(err)
	}
	waitPoll(t, m, other.ID)
	seedRemote(m, other.ID)
	for _, id := range []int64{b.ID, other.ID} { // pins, as a first poll would have set them
		p := m.poller(id)
		p.mu.Lock()
		p.box.HostKey = "ecdsa-sha2-nistp256 AAAA"
		p.mu.Unlock()
	}
	_, err := m.StartTransfer(ctx, b.ID, TransferRequest{DstBox: other.ID, DstRev: m.poller(other.ID).box.Revision, SrcPath: "data", DstPath: "copy"})
	if !errors.Is(err, ErrTransferBusy) {
		t.Fatalf("second copy: %v", err)
	}
	_, err = m.StartTransfer(ctx, b.ID, TransferRequest{DstBox: b.ID, SrcPath: "data", DstPath: "copy"})
	if !errors.Is(err, ErrTransferInput) {
		t.Errorf("same box: %v", err)
	}
	_, err = m.StartTransfer(ctx, other.ID, TransferRequest{DstBox: b.ID, DstRev: m.poller(b.ID).box.Revision, SrcPath: "data", DstPath: "a'b"})
	if !errors.Is(err, ErrTransferInput) {
		t.Errorf("bad destination: %v", err)
	}
	_, err = m.StartTransfer(ctx, other.ID, TransferRequest{DstBox: b.ID, DstRev: m.poller(b.ID).box.Revision, SrcPath: "data", DstPath: "data", DstMode: "merge"})
	if !errors.Is(err, ErrTransferInput) || !strings.Contains(err.Error(), "unknown destination mode") {
		t.Errorf("bad mode: %v", err)
	}
	// A move may not keep the destination's files: a kept file may differ
	// from the source's, whose only copy the removal would then discard.
	_, err = m.StartTransfer(ctx, other.ID, TransferRequest{DstBox: b.ID, DstRev: m.poller(b.ID).box.Revision, SrcPath: "data", DstPath: "data", DstMode: xferModeKeep, Move: true})
	if !errors.Is(err, ErrTransferInput) || !strings.Contains(err.Error(), "a move cannot keep") {
		t.Errorf("move with keep: %v", err)
	}
	// One active copy per destination account too: an existing directory
	// is not claimed, so two copies into one place are refused here.
	recv := &store.Transfer{Token: newTok(t), SrcBoxID: 98, DstBoxID: b.ID, DstEndp: AuthEndpoint(m.poller(b.ID).box), DstMode: xferModeKeep, Phase: xferRunning}
	if err := st.CreateTransfer(ctx, recv); err != nil {
		t.Fatal(err)
	}
	m.xferMu.Lock()
	m.xfers[recv.ID] = &xferJob{t: *recv}
	m.xferMu.Unlock()
	_, err = m.StartTransfer(ctx, other.ID, TransferRequest{DstBox: b.ID, DstRev: m.poller(b.ID).box.Revision, SrcPath: "data", DstPath: "data", DstMode: xferModeReplace})
	if !errors.Is(err, ErrTransferBusy) || !strings.Contains(err.Error(), "already receiving a copy") {
		t.Errorf("second copy into one destination: %v", err)
	}
}

// A move reserves its source account, whichever side is admitted first:
// while a move from an account runs, nothing is copied into that account
// (its removal would delete what lands there without copying it on); a
// move does not start from an account a copy is still writing into. Plain
// copies from or into other accounts are unaffected (fred on PR 171).
func TestTransferMoveReservesSourceAccount(t *testing.T) {
	ctx := context.Background()
	m, st, b := genTestManager(t)
	seedRemote(m, b.ID)
	other := &store.Box{Name: "b", Host: "127.0.0.2", Port: 1, User: "v", AuthMethod: "password", Password: "x", Enabled: true}
	if err := m.AddBox(ctx, other); err != nil {
		t.Fatal(err)
	}
	waitPoll(t, m, other.ID)
	seedRemote(m, other.ID)
	for _, id := range []int64{b.ID, other.ID} {
		p := m.poller(id)
		p.mu.Lock()
		p.box.HostKey = "ecdsa-sha2-nistp256 AAAA"
		p.mu.Unlock()
	}
	add := func(tr *store.Transfer) int64 {
		if err := st.CreateTransfer(ctx, tr); err != nil {
			t.Fatal(err)
		}
		m.xferMu.Lock()
		m.xfers[tr.ID] = &xferJob{t: *tr}
		m.xferMu.Unlock()
		return tr.ID
	}
	drop := func(id int64) {
		m.xferMu.Lock()
		delete(m.xfers, id)
		m.xferMu.Unlock()
	}
	req := func(dst *store.Box, move bool) TransferRequest {
		return TransferRequest{DstBox: dst.ID, DstRev: m.poller(dst.ID).box.Revision, SrcPath: "data", DstPath: "copy", Move: move}
	}
	// Order 1: the move from b is active; a copy into b is refused.
	mv := add(&store.Transfer{Token: newTok(t), SrcBoxID: b.ID, SrcEndp: AuthEndpoint(m.poller(b.ID).box), DstBoxID: 99, DstEndp: "elsewhere:22/w", Move: true, Phase: xferRunning})
	if _, err := m.StartTransfer(ctx, other.ID, req(b, false)); !errors.Is(err, ErrTransferBusy) || !strings.Contains(err.Error(), "a move from a is in progress") {
		t.Errorf("copy into a moving source: %v", err)
	}
	drop(mv)
	// The same copy from b (not a move) reserves nothing of the kind.
	cp := add(&store.Transfer{Token: newTok(t), SrcBoxID: b.ID, SrcEndp: AuthEndpoint(m.poller(b.ID).box), DstBoxID: 99, DstEndp: "elsewhere:22/w", Phase: xferRunning})
	if _, err := m.StartTransfer(ctx, other.ID, req(b, false)); errors.Is(err, ErrTransferBusy) {
		t.Errorf("copy into a copying source refused: %v", err)
	}
	drop(cp)
	// Order 2: a copy into b is active; a move from b is refused, a copy
	// from b is not (the account check is the move's).
	rc := add(&store.Transfer{Token: newTok(t), SrcBoxID: 98, SrcEndp: "elsewhere:22/w", DstBoxID: b.ID, DstEndp: AuthEndpoint(m.poller(b.ID).box), Phase: xferRunning})
	if _, err := m.StartTransfer(ctx, b.ID, req(other, true)); !errors.Is(err, ErrTransferBusy) || !strings.Contains(err.Error(), "a is still receiving a copy; a move from it waits") {
		t.Errorf("move from a receiving account: %v", err)
	}
	if _, err := m.StartTransfer(ctx, b.ID, req(other, false)); errors.Is(err, ErrTransferBusy) {
		t.Errorf("copy from a receiving account refused: %v", err)
	}
	drop(rc)
}

// A move's removal outcome is stored with the final phase and read back
// after a restart: the card must not say "copied, not moved" of a source
// that is gone (fred on PR 171).
func TestTransferMoveStateStored(t *testing.T) {
	ctx := context.Background()
	st := openTestStore(t)
	done := &store.Transfer{Token: strings.Repeat("f", 32), SrcBoxID: 1, DstBoxID: 2, Move: true, Phase: xferFinished, RC: "0", MoveState: "left 2", Result: "moved: copied; 2 entries left"}
	if err := st.CreateTransfer(ctx, done); err != nil {
		t.Fatal(err)
	}
	m := xferTestManager(t, st)
	if v := m.xferView(done.ID); v.MoveState != "left 2" || !v.Move {
		t.Errorf("after restart: %+v", v.Transfer)
	}
	// Applying a final read stores the outcome the runner recorded.
	tr := &store.Transfer{Token: strings.Repeat("g", 32), SrcBoxID: 1, DstBoxID: 2, Move: true, Phase: xferRunning}
	if err := st.CreateTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	j := &xferJob{t: *tr}
	m.xferMu.Lock()
	m.xfers[tr.ID] = j
	m.xferMu.Unlock()
	j.op.Lock()
	m.xferApplyLocked(ctx, j, xferLive{started: true, pidFile: true, rc: "0", stage: "remove", move: "moved"}, false)
	j.op.Unlock()
	ts, err := st.ListTransfers(ctx)
	if err != nil || len(ts) != 2 || ts[1].MoveState != "moved" || ts[1].Phase != xferFinished || !strings.HasPrefix(ts[1].Result, "moved: copied and the source folder removed") {
		t.Errorf("stored after a final read: %+v %v", ts, err)
	}
}

func newTok(t *testing.T) string {
	tok, err := newXferToken()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// A running job whose source box now names another machine can never be
// read, stopped or cleaned up from here; force forgets it, touching no
// box, and says it may still be running. A job whose source is still
// reachable as saved is never forgotten while it runs (daybreak review B2).
func TestTransferForgetLostSource(t *testing.T) {
	ctx := context.Background()
	m, st, b := genTestManager(t)
	add := func(endp string) int64 {
		tr := &store.Transfer{Token: newTok(t), SrcBoxID: b.ID, SrcEndp: endp, SrcHostKey: m.poller(b.ID).hostKey(),
			DstBoxID: b.ID, DstEndp: endp, Phase: xferRunning, PubKey: testBlob,
			Pending: store.TransferPendingSrcJob | store.TransferPendingDstKey}
		if err := st.CreateTransfer(ctx, tr); err != nil {
			t.Fatal(err)
		}
		m.xferMu.Lock()
		m.xfers[tr.ID] = &xferJob{t: *tr}
		m.xferMu.Unlock()
		return tr.ID
	}
	kept := add(AuthEndpoint(b))
	if v := m.xferView(kept); v.SrcLost {
		t.Errorf("reachable source reads as lost")
	}
	if _, err := m.DismissTransfer(ctx, kept, true); !errors.Is(err, ErrTransferBusy) || !strings.Contains(err.Error(), "cancel it first") {
		t.Errorf("running job with its source in place forgotten: %v", err)
	}
	lost := add("elsewhere:22/u")
	if v := m.xferView(lost); !v.SrcLost {
		t.Errorf("lost source not flagged: %+v", v)
	}
	left, err := m.DismissTransfer(ctx, lost, false)
	if !errors.Is(err, ErrTransferBusy) || len(left) != 3 || !strings.Contains(left[0], "may still be running") {
		t.Errorf("without force: %v %v", left, err)
	}
	if _, err := m.DismissTransfer(ctx, lost, true); err != nil {
		t.Fatalf("forced: %v", err)
	}
	ts, _ := st.ListTransfers(ctx)
	if len(ts) != 1 || ts[0].ID != kept {
		t.Errorf("stored after forget: %+v", ts)
	}
}

// A final phase that cannot be stored is not adopted, and no cleanup
// runs: cleanup would delete the source's rc, the only evidence of how
// the copy ended (daybreak review S1).
func TestTransferFinalNotStoredKeepsEvidence(t *testing.T) {
	ctx := context.Background()
	m, st, b := genTestManager(t)
	tr := &store.Transfer{Token: newTok(t), SrcBoxID: b.ID, SrcEndp: AuthEndpoint(b), SrcHostKey: m.poller(b.ID).hostKey(),
		DstBoxID: b.ID, DstEndp: AuthEndpoint(b), Phase: xferRunning, PubKey: testBlob,
		Pending: store.TransferPendingSrcJob | store.TransferPendingDstKey}
	if err := st.CreateTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	j := &xferJob{t: *tr}
	m.xferMu.Lock()
	m.xfers[tr.ID] = j
	m.xferMu.Unlock()
	st.Close() // every save fails from here
	j.op.Lock()
	m.xferApplyLocked(ctx, j, xferLive{started: true, pidFile: true, rc: "0"}, false)
	j.op.Unlock()
	if v := m.xferView(tr.ID); v.Phase != xferRunning || v.Pending != tr.Pending || v.StatusErr == "" {
		t.Errorf("unsaved final phase adopted: phase %s pending %d err %q", v.Phase, v.Pending, v.StatusErr)
	}
}

// A pid write that stalls past the launch's wait: the job is unconfirmed,
// a read that sees the marker alone keeps it so, and once the runner gives
// up (pid and rc unwritable, marker taken back) cancel closes it. Promoted
// to running at the marker, it could never be closed (fred on PR 146).
func TestTransferMarkerOnlyStaysCancellable(t *testing.T) {
	ctx := context.Background()
	m, st, b := genTestManager(t)
	tr := &store.Transfer{Token: newTok(t), SrcBoxID: b.ID, SrcEndp: AuthEndpoint(b), SrcHostKey: m.poller(b.ID).hostKey(),
		DstBoxID: b.ID, DstEndp: AuthEndpoint(b), Phase: xferUnconfirmed, PubKey: testBlob,
		Pending: store.TransferPendingSrcJob | store.TransferPendingDstKey}
	if err := st.CreateTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	j := &xferJob{t: *tr}
	m.xferMu.Lock()
	m.xfers[tr.ID] = j
	m.xferMu.Unlock()
	j.op.Lock()
	defer j.op.Unlock()
	m.xferApplyLocked(ctx, j, xferLive{started: true}, false) // marker, pid write still stalled
	if p := m.xferSnapshot(j).Phase; p != xferUnconfirmed {
		t.Fatalf("marker only: %s", p)
	}
	m.xferApplyLocked(ctx, j, xferLive{started: true}, true) // cancel in that window: nothing to signal yet
	if p := m.xferSnapshot(j).Phase; p != xferUnconfirmed {
		t.Fatalf("cancel during the window ended it: %s", p)
	}
	m.xferApplyLocked(ctx, j, xferLive{}, true) // runner gave up and took the marker back
	if p := m.xferSnapshot(j).Phase; p != xferInterrupted {
		t.Fatalf("cancel after the runner gave up: %s", p)
	}
	ts, _ := st.ListTransfers(ctx)
	if len(ts) != 1 || ts[0].Phase != xferInterrupted {
		t.Errorf("stored: %+v", ts)
	}
}

// A running job whose source stops answering: for xferStale nothing but
// cancel is offered (the source may be back in a moment); after that the
// destination's key line can be revoked and the tracking stopped, each
// explicitly, and neither before. Here the destination is as unreachable
// as the source, so the revocation fails and changes nothing; the phase
// never moves, and admission keeps both accounts until the job is
// forgotten (2026-11 audits).
func TestTransferUnreadableSourceRecovery(t *testing.T) {
	ctx := context.Background()
	m, st, b := genTestManager(t)
	seedRemote(m, b.ID)
	other := &store.Box{Name: "b", Host: "127.0.0.2", Port: 1, User: "v", AuthMethod: "password", Password: "x", Enabled: true}
	if err := m.AddBox(ctx, other); err != nil {
		t.Fatal(err)
	}
	waitPoll(t, m, other.ID)
	seedRemote(m, other.ID)
	for _, id := range []int64{b.ID, other.ID} {
		p := m.poller(id)
		p.mu.Lock()
		p.box.HostKey = "ecdsa-sha2-nistp256 AAAA"
		p.mu.Unlock()
	}
	tr := &store.Transfer{Token: newTok(t), SrcBoxID: b.ID, SrcEndp: AuthEndpoint(m.poller(b.ID).box), SrcHostKey: m.poller(b.ID).hostKey(),
		DstBoxID: other.ID, DstEndp: AuthEndpoint(m.poller(other.ID).box), DstHostKey: m.poller(other.ID).hostKey(), Phase: xferRunning, PubKey: testBlob,
		Pending: store.TransferPendingSrcJob | store.TransferPendingDstKey}
	if err := st.CreateTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	j := &xferJob{t: *tr}
	m.xferMu.Lock()
	m.xfers[tr.ID] = j
	m.xferMu.Unlock()
	j.op.Lock()
	m.xferRefreshLocked(ctx, j, false) // connection refused: the read fails, the phase stays
	j.op.Unlock()
	v := m.xferView(tr.ID)
	if v.Phase != xferRunning || v.StatusErr == "" || v.SrcUnreadable || v.KeyRevocable || v.SrcLost {
		t.Fatalf("just after a failed read: %+v", v)
	}
	if err := m.RevokeTransferKey(ctx, other.ID, tr.ID); !errors.Is(err, ErrTransferBusy) || !strings.Contains(err.Error(), "cancel it, or wait") {
		t.Errorf("revoking while the source may be back: %v", err)
	}
	if err := m.RetryTransferCleanup(ctx, b.ID, tr.ID); !errors.Is(err, ErrTransferBusy) || !strings.Contains(err.Error(), "no cleanup is owed") {
		t.Errorf("cleanup of a job that is not over: %v", err)
	}
	if _, err := m.DismissTransfer(ctx, tr.ID, true); !errors.Is(err, ErrTransferBusy) || !strings.Contains(err.Error(), "cancel it first") {
		t.Errorf("dismiss while the source may be back: %v", err)
	}
	// Three minutes on (the job's creation stands in for the last read,
	// none has succeeded since the dashboard started).
	m.xferMu.Lock()
	j.t.CreatedAt = time.Now().Add(-xferStale - time.Minute)
	m.xferMu.Unlock()
	v = m.xferView(tr.ID)
	if !v.SrcUnreadable || !v.KeyRevocable || v.SrcLost || v.Phase != xferRunning {
		t.Fatalf("stale: %+v", v)
	}
	if err := m.RevokeTransferKey(ctx, b.ID, tr.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoking under the source's box id: %v", err)
	}
	if err := m.RevokeTransferKey(ctx, other.ID, tr.ID); err == nil || errors.Is(err, ErrTransferBusy) {
		t.Errorf("revoking against an unreachable destination: %v", err)
	}
	if v = m.xferView(tr.ID); v.Phase != xferRunning || v.Pending != tr.Pending || !v.KeyRevocable {
		t.Fatalf("a failed revocation changed the job: %+v", v)
	}
	req := TransferRequest{DstBox: other.ID, DstRev: m.poller(other.ID).box.Revision, SrcPath: "data", DstPath: "copy"}
	if _, err := m.StartTransfer(ctx, b.ID, req); !errors.Is(err, ErrTransferBusy) || !strings.Contains(err.Error(), "already has a copy in progress") {
		t.Errorf("the stale job must keep its source reserved: %v", err)
	}
	left, err := m.DismissTransfer(ctx, tr.ID, false)
	if !errors.Is(err, ErrTransferBusy) || !strings.Contains(err.Error(), "stop tracking") || len(left) != 3 || !strings.Contains(left[0], "may still be running") {
		t.Errorf("without force: %v %v", left, err)
	}
	if ts, _ := st.ListTransfers(ctx); len(ts) != 1 {
		t.Fatalf("refused dismiss forgot the job: %+v", ts)
	}
	if _, err := m.DismissTransfer(ctx, tr.ID, true); err != nil {
		t.Fatalf("forced: %v", err)
	}
	if ts, _ := st.ListTransfers(ctx); len(ts) != 0 {
		t.Errorf("stored after stop tracking: %+v", ts)
	}
	if err := m.RevokeTransferKey(ctx, other.ID, tr.ID); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("revoking a forgotten job: %v", err)
	}
	// Revocation is for jobs that are not over, and only while the key
	// line is still owed.
	addJob := func(phase string, pending int) *xferJob {
		x := &store.Transfer{Token: newTok(t), SrcBoxID: b.ID, SrcEndp: AuthEndpoint(m.poller(b.ID).box), SrcHostKey: m.poller(b.ID).hostKey(),
			DstBoxID: other.ID, DstEndp: AuthEndpoint(m.poller(other.ID).box), DstHostKey: m.poller(other.ID).hostKey(), Phase: phase, PubKey: testBlob, Pending: pending}
		if err := st.CreateTransfer(ctx, x); err != nil {
			t.Fatal(err)
		}
		x.CreatedAt = time.Now().Add(-xferStale - time.Minute)
		k := &xferJob{t: *x, err: "dial: connection refused"}
		m.xferMu.Lock()
		m.xfers[x.ID] = k
		m.xferMu.Unlock()
		return k
	}
	done := addJob(xferFinished, store.TransferPendingDstKey)
	if err := m.RevokeTransferKey(ctx, other.ID, done.t.ID); !errors.Is(err, ErrTransferBusy) || !strings.Contains(err.Error(), "its cleanup") {
		t.Errorf("revoking on a finished job: %v", err)
	}
	noKey := addJob(xferRunning, store.TransferPendingSrcJob)
	if v := m.xferView(noKey.t.ID); !v.SrcUnreadable || v.KeyRevocable {
		t.Errorf("no key owed: %+v", v)
	}
	if err := m.RevokeTransferKey(ctx, other.ID, noKey.t.ID); !errors.Is(err, ErrTransferBusy) || !strings.Contains(err.Error(), "no key line is owed") {
		t.Errorf("revoking with no key owed: %v", err)
	}
	// The rule itself: a recent successful read, an empty error, or a
	// final phase is never "unreadable".
	now := time.Now()
	old := store.Transfer{Phase: xferRunning, CreatedAt: now.Add(-time.Hour)}
	for i, c := range []struct {
		t    store.Transfer
		seen time.Time
		err  string
		want bool
	}{
		{old, time.Time{}, "dial", true},
		{old, now.Add(-time.Minute), "dial", false},
		{old, now.Add(-xferStale - time.Second), "dial", true},
		{old, time.Time{}, "", false},
		{store.Transfer{Phase: xferFinished, CreatedAt: now.Add(-time.Hour)}, time.Time{}, "dial", false},
		{store.Transfer{Phase: xferRunning, CreatedAt: now}, time.Time{}, "dial", false},
	} {
		if got := xferUnreadable(c.t, c.seen, c.err, now); got != c.want {
			t.Errorf("case %d: %v, want %v", i, got, c.want)
		}
	}
}

// Over the in-process sshd: a running job whose runner has recorded exit
// code 0 is read as finished and cleaned up end to end, the job directory
// removed on the source and the key line removed from the destination,
// through the manager's own paths (status read, decision, store, both
// cleanup legs). Until now these stopped at a refused dial.
func TestTransferFinishAndCleanupOverSSH(t *testing.T) {
	needTools(t, "rrsync", "flock", "perl")
	ctx := context.Background()
	m, st, _ := genTestManager(t)
	root := t.TempDir()
	port, hostKey := testSSHServer(t, root)
	src := sshTestBox(t, m, "src", "u", port, hostKey)
	dst := sshTestBox(t, m, "dst", "v", port, hostKey)
	tok := newTok(t)
	// The source's finished runner: its job directory with the exit code
	// and no pid (the runner is gone).
	srcHome, dstHome := filepath.Join(root, "u"), filepath.Join(root, "v")
	jdir := filepath.Join(srcHome, xferJobDir, tok)
	os.MkdirAll(filepath.Join(jdir, "started"), 0o700)
	os.WriteFile(filepath.Join(jdir, "rc"), []byte("0\n"), 0o600)
	os.WriteFile(filepath.Join(jdir, "started_at"), []byte("x"), 0o600)
	// The destination as the real claim leaves it: the folder and the key
	// line in a fresh authorized_keys.
	os.MkdirAll(dstHome, 0o700)
	out := runJobScript(t, dstHome, xferVars("TOKEN", tok, "DST", "incoming", "BLOB", testBlob, "MODE", "new"), xferDstClaimScript+xferKeyAddScript)
	if !strings.Contains(out, "KEYLINE") {
		t.Fatalf("claim: %s", out)
	}
	ak := filepath.Join(dstHome, ".ssh", "authorized_keys")
	if !strings.Contains(readFile(t, ak), "pfm-xfer-"+tok) {
		t.Fatalf("key line missing: %s", readFile(t, ak))
	}
	tr := &store.Transfer{Token: tok, SrcBoxID: src.ID, SrcEndp: AuthEndpoint(src), SrcHostKey: hostKey,
		DstBoxID: dst.ID, DstEndp: AuthEndpoint(dst), DstHostKey: hostKey, SrcPath: "data", DstPath: "incoming",
		Phase: xferRunning, PubKey: testBlob, Pending: store.TransferPendingSrcJob | store.TransferPendingDstKey}
	if err := st.CreateTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	j := &xferJob{t: *tr}
	m.xferMu.Lock()
	m.xfers[tr.ID] = j
	m.xferMu.Unlock()
	j.op.Lock()
	m.xferRefreshLocked(ctx, j, false)
	j.op.Unlock()
	v := m.xferView(tr.ID)
	if v.Phase != xferFinished || v.RC != "0" || v.Pending != 0 || len(v.Cleanup) != 0 || v.StatusErr != "" {
		t.Fatalf("after the read: %+v", v)
	}
	if _, err := os.Stat(jdir); !os.IsNotExist(err) {
		t.Errorf("job directory still on the source: %v", err)
	}
	if _, err := os.Stat(ak); !os.IsNotExist(err) {
		t.Errorf("authorized_keys (created by the job) still on the destination: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dstHome, "incoming")); err != nil {
		t.Errorf("the copied-into folder must stay: %v", err)
	}
	ts, _ := st.ListTransfers(ctx)
	if len(ts) != 1 || ts[0].Phase != xferFinished || ts[0].Pending != 0 {
		t.Errorf("stored: %+v", ts)
	}
}

// Over the in-process sshd: a running job whose source (a refusing port)
// has stopped answering gets its key line revoked on the destination,
// which is reachable; the job stays running and reserved, and the status
// note is not cleared by the success.
func TestTransferRevokeKeyOverSSH(t *testing.T) {
	needTools(t, "rrsync", "flock", "perl")
	ctx := context.Background()
	m, st, b := genTestManager(t) // b: 127.0.0.1:1, connection refused
	root := t.TempDir()
	port, hostKey := testSSHServer(t, root)
	dst := sshTestBox(t, m, "dst", "v", port, hostKey)
	p := m.poller(b.ID)
	p.mu.Lock()
	p.box.HostKey = "ecdsa-sha2-nistp256 AAAA"
	p.mu.Unlock()
	tok := newTok(t)
	dstHome := filepath.Join(root, "v")
	os.MkdirAll(filepath.Join(dstHome, ".ssh"), 0o700)
	ak := filepath.Join(dstHome, ".ssh", "authorized_keys")
	os.WriteFile(ak, []byte("ssh-ed25519 AAAAOPERATOR operator\n"), 0o600) // an existing line stays
	runJobScript(t, dstHome, xferVars("TOKEN", tok, "DST", "incoming", "BLOB", testBlob, "MODE", "new"), xferDstClaimScript+xferKeyAddScript)
	if got := readFile(t, ak); !strings.Contains(got, "pfm-xfer-"+tok) {
		t.Fatalf("key line missing: %s", got)
	}
	tr := &store.Transfer{Token: tok, SrcBoxID: b.ID, SrcEndp: AuthEndpoint(p.box), SrcHostKey: p.hostKey(),
		DstBoxID: dst.ID, DstEndp: AuthEndpoint(dst), DstHostKey: hostKey, SrcPath: "data", DstPath: "incoming",
		Phase: xferRunning, PubKey: testBlob, Pending: store.TransferPendingSrcJob | store.TransferPendingDstKey}
	if err := st.CreateTransfer(ctx, tr); err != nil {
		t.Fatal(err)
	}
	j := &xferJob{t: *tr}
	m.xferMu.Lock()
	j.t.CreatedAt = time.Now().Add(-xferStale - time.Minute)
	m.xfers[tr.ID] = j
	m.xferMu.Unlock()
	j.op.Lock()
	m.xferRefreshLocked(ctx, j, false) // the source refuses: the read fails
	j.op.Unlock()
	if v := m.xferView(tr.ID); !v.SrcUnreadable || !v.KeyRevocable {
		t.Fatalf("not offered: %+v", v)
	}
	if err := m.RevokeTransferKey(ctx, dst.ID, tr.ID); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if got := readFile(t, ak); got != "ssh-ed25519 AAAAOPERATOR operator\n" {
		t.Errorf("authorized_keys after the revocation: %q", got)
	}
	v := m.xferView(tr.ID)
	if v.Phase != xferRunning || v.Pending != store.TransferPendingSrcJob || v.KeyRevocable || !v.SrcUnreadable || v.StatusErr == "" {
		t.Fatalf("after the revocation: %+v", v)
	}
	if err := m.RevokeTransferKey(ctx, dst.ID, tr.ID); !errors.Is(err, ErrTransferBusy) || !strings.Contains(err.Error(), "no key line is owed") {
		t.Errorf("a second revocation: %v", err)
	}
	ts, _ := st.ListTransfers(ctx)
	if len(ts) != 1 || ts[0].Pending != store.TransferPendingSrcJob || ts[0].Phase != xferRunning {
		t.Errorf("stored: %+v", ts)
	}
}
