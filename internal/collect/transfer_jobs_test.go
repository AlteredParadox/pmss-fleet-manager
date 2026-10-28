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
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pmss-fleet-manager/internal/store"
)

func TestParseXferStatus(t *testing.T) {
	l := parseXferStatus("STARTED\nPIDFILE\nALIVE\nrc=24\npid=12\nskipped=3\nerrors.count=2\nprogress=  1,073,741,894  99%   48.31MB/s    0:00:21 (xfr#11, to-chk=0/14)\n--errors\nfile vanished: \"a\"\nfile vanished: \"b\"")
	if !l.started || !l.pidFile || !l.alive || l.rc != "24" || l.skipped != 3 || l.errCount != 2 || !strings.HasPrefix(l.errors, "file vanished: \"a\"\nfile") {
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
		{xferFinished, xferLive{rc: "24", progress: prog}, "completed with warnings: some source files vanished during the copy; 3.0 MiB in 2 files"},
		{xferFinished, xferLive{rc: "23", errors: "rsync: open \"x\" failed: Permission denied (13)"}, "partial: some files could not be transferred — rsync: open \"x\" failed: Permission denied (13)"},
		{xferFinished, xferLive{rc: "11", errors: "rsync: write failed on \"/h/d/big\": No space left on device (28)"}, "destination out of space or quota — rsync: write failed on \"/h/d/big\": No space left on device (28)"},
		{xferFinished, xferLive{rc: "12", errors: "rsync: write failed: Disk quota exceeded (122)"}, "destination out of space or quota — rsync: write failed: Disk quota exceeded (122)"},
		{xferFinished, xferLive{rc: "255", errors: "Host key verification failed."}, "rsync exited with code 255 — Host key verification failed."},
		{xferFinished, xferLive{rc: "cancelled"}, "cancelled"},
		{xferInterrupted, xferLive{started: true, pidFile: true}, "interrupted: the runner ended without a result (box rebooted or the process was killed?)"},
		{xferInterrupted, xferLive{}, "not started: the runner never started on the source"},
	} {
		if got := xferResult(c.phase, c.l); got != c.want {
			t.Errorf("rc %q: %q\nwant %q", c.l.rc, got, c.want)
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
}

func newTok(t *testing.T) string {
	tok, err := newXferToken()
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
