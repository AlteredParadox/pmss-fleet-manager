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

// The dashboard's side of a box-to-box copy (the box side is transfer.go):
// setup, the status loop, cancel, cleanup and dismiss. A job only ever
// acts on a box whose current record still has the endpoint and pinned
// host key saved when the job started. Completion comes from the runner's
// exit code alone, never from the progress percentage; losing contact
// changes nothing.

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"pmss-fleet-manager/internal/store"
)

// Transfer phases.
const (
	xferPreparing   = "preparing"    // setup running: key, claim, launch
	xferRunning     = "running"      // the runner started on the source
	xferUnconfirmed = "unconfirmed"  // the launch's outcome is unknown
	xferFinished    = "finished"     // the runner recorded an exit code (rc)
	xferInterrupted = "interrupted"  // the runner ended without recording one
	xferSetupFailed = "setup_failed" // setup failed and its cleanup is not confirmed
)

// xferInterval is how often an active job's status is read from its
// source (a variable for tests).
var xferInterval = 5 * time.Second

var (
	// ErrTransferInput is a request the dashboard refuses before touching a box.
	ErrTransferInput = errors.New("invalid transfer")
	// ErrTransferBusy refuses a second active copy from one box, or an
	// action on a job that is not in a state to take it.
	ErrTransferBusy = errors.New("transfer busy")
	// ErrTransferIdentity refuses acting on a box whose record no longer
	// names the account and machine the job started on.
	ErrTransferIdentity = errors.New("box record changed since the transfer started")
)

// xferJob is one transfer: its stored row and what the last status read saw.
// op serialises everything that acts on the job's boxes.
type xferJob struct {
	op   sync.Mutex
	t    store.Transfer // under Manager.xferMu
	live xferLive       // under Manager.xferMu
	seen time.Time      // last successful status read, under Manager.xferMu
	err  string         // last status or cleanup error, under Manager.xferMu
}

// TransferProgress is rsync's latest progress2 line, parsed. Pct is rsync's
// own estimate; Time is the remaining time while running and the elapsed
// time on the final line.
type TransferProgress struct {
	Bytes   int64  `json:"bytes"`
	Pct     int    `json:"pct"`
	Rate    string `json:"rate"`
	Time    string `json:"time"`
	Files   int    `json:"files,omitempty"`
	ToCheck int    `json:"to_check,omitempty"`
	Total   int    `json:"total,omitempty"`
}

// TransferView is a transfer for the UI.
type TransferView struct {
	store.Transfer
	SrcName   string            `json:"src_name"`
	DstName   string            `json:"dst_name"`
	Cleanup   []string          `json:"cleanup,omitempty"` // owed, in words
	Blocked   string            `json:"blocked,omitempty"` // why the job will not act on a box
	LastSeen  time.Time         `json:"last_seen"`
	StatusErr string            `json:"status_error,omitempty"`
	Scanning  bool              `json:"scanning"`
	Progress  *TransferProgress `json:"progress,omitempty"`
	Skipped   int               `json:"skipped"`
	ErrCount  int               `json:"error_count"`
	Errors    string            `json:"errors,omitempty"`
}

// TransferRequest starts a copy from the box the route names.
type TransferRequest struct {
	DstBox  int64  `json:"dst_box"`
	DstRev  int64  `json:"dst_rev"`
	SrcPath string `json:"src_path"`
	DstPath string `json:"dst_path"`
}

// xferLive is one status read of a job on its source.
type xferLive struct {
	noJob, started, pidFile, alive bool
	rc                             string
	skipped, errCount              int
	progress                       string
	errors                         string
}

func parseXferStatus(s string) xferLive {
	var l xferLive
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		switch {
		case ln == "NOJOB":
			l.noJob = true
		case ln == "STARTED":
			l.started = true
		case ln == "PIDFILE":
			l.pidFile = true
		case ln == "ALIVE":
			l.alive = true
		case strings.HasPrefix(ln, "rc="):
			l.rc = strings.TrimSpace(ln[3:])
		case strings.HasPrefix(ln, "skipped="):
			l.skipped, _ = strconv.Atoi(strings.TrimSpace(ln[8:]))
		case strings.HasPrefix(ln, "errors.count="):
			l.errCount, _ = strconv.Atoi(strings.TrimSpace(ln[13:]))
		case strings.HasPrefix(ln, "progress="):
			l.progress = strings.TrimSpace(ln[9:])
		case ln == "--errors":
			l.errors = strings.TrimSpace(strings.Join(lines[i+1:], "\n"))
			return l
		}
	}
	return l
}

var noSpaceRe = regexp.MustCompile(`(?i)no space left|quota exceeded`)

var progressRe = regexp.MustCompile(`^([\d,]+)\s+(\d+)%\s+(\S+/s)\s+(\d+:\d\d:\d\d)(?:\s+\(xfr#(\d+), (?:ir|to)-chk=(\d+)/(\d+)\))?`)

func parseProgress(s string) *TransferProgress {
	m := progressRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return nil
	}
	p := &TransferProgress{Rate: m[3], Time: m[4]}
	p.Bytes, _ = strconv.ParseInt(strings.ReplaceAll(m[1], ",", ""), 10, 64)
	p.Pct, _ = strconv.Atoi(m[2])
	p.Files, _ = strconv.Atoi(m[5])
	p.ToCheck, _ = strconv.Atoi(m[6])
	p.Total, _ = strconv.Atoi(m[7])
	return p
}

// xferDecide is what a status read proves about a job in phase: the next
// phase, and whether the job is over. Only a recorded exit code finishes
// a job; only a runner proven gone without one interrupts it. Anything
// else leaves the phase as it was.
func xferDecide(phase string, l xferLive, cancelAsked bool) string {
	switch {
	case l.rc != "":
		return xferFinished
	case l.noJob:
		return xferInterrupted // the job directory itself is gone
	case l.alive, l.started && !l.pidFile: // running, or between its marker and its pid file
		return xferRunning
	case l.started:
		return xferInterrupted // pid recorded, process gone, no exit code
	case cancelAsked && phase == xferUnconfirmed:
		return xferInterrupted // never started, and the operator gave up on it
	}
	return phase
}

// xferResult words a final outcome. rsync's codes: 23 some files not
// transferred, 24 source files vanished, 11/12 file or stream I/O (out of
// space shows up as 11 or 12 with ENOSPC/EDQUOT text).
func xferResult(phase string, l xferLive) string {
	first := ""
	if l.errors != "" {
		first = strings.SplitN(l.errors, "\n", 2)[0]
	}
	skipped := ""
	if l.skipped > 0 {
		skipped = fmt.Sprintf("; %d symlinks or special files skipped", l.skipped)
	}
	copied := ""
	if p := parseProgress(l.progress); p != nil {
		copied = fmt.Sprintf(" %s in %d files", fmtBytesShort(p.Bytes), p.Files)
	}
	var s string
	switch {
	case phase == xferInterrupted && l.noJob:
		s = "interrupted: the job directory on the source is gone"
	case phase == xferInterrupted && !l.started:
		s = "not started: the runner never started on the source"
	case phase == xferInterrupted:
		s = "interrupted: the runner ended without a result (box rebooted or the process was killed?)"
	case l.rc == "0":
		return "copied" + copied + skipped
	case l.rc == "24":
		return "completed with warnings: some source files vanished during the copy;" + copied + skipped
	case l.rc == "cancelled":
		return "cancelled" // rsync's own "received SIGTERM" line adds nothing
	case l.rc == "23":
		s = "partial: some files could not be transferred"
	case (l.rc == "11" || l.rc == "12") && noSpaceRe.MatchString(l.errors):
		s = "destination out of space or quota"
	default:
		s = "rsync exited with code " + l.rc
	}
	if first != "" {
		s += " — " + first
	}
	return clip(s, 4096)
}

func fmtBytesShort(n int64) string {
	units := []string{"B", "KiB", "MiB", "GiB", "TiB"}
	f, i := float64(n), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

func xferFinal(phase string) bool {
	return phase == xferFinished || phase == xferInterrupted || phase == xferSetupFailed
}

func xferActive(phase string) bool {
	return phase == xferPreparing || phase == xferRunning || phase == xferUnconfirmed
}

func cleanupWords(p int) []string {
	var out []string
	if p&store.TransferPendingSrcJob != 0 {
		out = append(out, "job directory (and key files) on the source")
	}
	if p&store.TransferPendingDstKey != 0 {
		out = append(out, "job key line in the destination's ~/.ssh/authorized_keys")
	}
	if p&store.TransferPendingDstEmpty != 0 {
		out = append(out, "the new destination directory, if still empty")
	}
	return out
}

// loadTransfers reads the jobs at start. A job still "preparing" was cut
// off by a stop or crash mid-setup: nothing proves what it left behind, so
// every cleanup it could owe is marked owed.
func (m *Manager) loadTransfers(ctx context.Context) error {
	ts, err := m.st.ListTransfers(ctx)
	if err != nil {
		return err
	}
	m.xferMu.Lock()
	defer m.xferMu.Unlock()
	for _, t := range ts {
		if t.Phase == xferPreparing {
			t.Phase = xferSetupFailed
			t.Result = "the dashboard stopped during setup; the cleanup below may be needed"
			t.Pending |= store.TransferPendingSrcJob
			if t.PubKey != "" {
				// The key may have been added. The directory may have been
				// claimed too, but nothing proves this job created it, so
				// it is never removed on that guess (Codex on PR 142).
				t.Pending |= store.TransferPendingDstKey
			}
			t.FinishedAt = time.Now().UTC()
			if err := m.st.SaveTransfer(ctx, t); err != nil {
				return err
			}
		}
		m.xfers[t.ID] = &xferJob{t: *t}
	}
	return nil
}

func (m *Manager) xferLoop() {
	defer m.wg.Done()
	tick := time.NewTicker(xferInterval)
	defer tick.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-tick.C:
		}
		m.xferMu.Lock()
		var jobs []*xferJob
		for _, j := range m.xfers {
			if j.t.Phase == xferRunning || j.t.Phase == xferUnconfirmed {
				jobs = append(jobs, j)
			}
		}
		m.xferMu.Unlock()
		for _, j := range jobs {
			if j.op.TryLock() { // a setup, cancel or cleanup is acting on it: next tick
				m.xferRefreshLocked(m.ctx, j, false)
				j.op.Unlock()
			}
		}
	}
}

// xferPoller is box id's poller, if its record still names the account
// and machine the job saved (endpoint and pinned host key).
func (m *Manager) xferPoller(id int64, endpoint, hostKey string) (*poller, error) {
	p := m.poller(id)
	if p == nil {
		return nil, fmt.Errorf("%w: box %d was removed", ErrTransferIdentity, id)
	}
	if AuthEndpoint(p.box) != endpoint || p.hostKey() != hostKey {
		return nil, fmt.Errorf("%w: %s now points at another account or machine; not touching it", ErrTransferIdentity, p.box.Name)
	}
	if !p.box.Enabled {
		return nil, fmt.Errorf("%s: %w", p.box.Name, ErrDisabled)
	}
	return p, nil
}

func (m *Manager) xferSnapshot(j *xferJob) store.Transfer {
	m.xferMu.Lock()
	defer m.xferMu.Unlock()
	return j.t
}

// xferSave stores the job's new row (under op) and publishes.
func (m *Manager) xferSave(j *xferJob, t store.Transfer) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err := m.st.SaveTransfer(ctx, &t)
	m.xferMu.Lock()
	j.t = t
	m.xferMu.Unlock()
	m.Publish(Event{Type: "transfers"})
	return err
}

func (m *Manager) xferNote(j *xferJob, live *xferLive, err error) {
	m.xferMu.Lock()
	if live != nil {
		j.live, j.seen = *live, time.Now().UTC()
	}
	j.err = ""
	if err != nil {
		j.err = clip(err.Error(), 300)
	}
	m.xferMu.Unlock()
	m.Publish(Event{Type: "transfers"})
}

// xferRefreshLocked reads the job's status from its source and applies
// what it proves (under op). A failed read changes nothing but the note.
func (m *Manager) xferRefreshLocked(ctx context.Context, j *xferJob, cancelAsked bool) {
	t := m.xferSnapshot(j)
	if !(t.Phase == xferRunning || t.Phase == xferUnconfirmed) {
		return
	}
	p, err := m.xferPoller(t.SrcBoxID, t.SrcEndp, t.SrcHostKey)
	if err != nil {
		m.xferNote(j, nil, err)
		return
	}
	cli, err := p.conn(ctx)
	if err != nil {
		m.xferNote(j, nil, err)
		return
	}
	raw, err := xferStatus(ctx, cli, t.Token)
	if err != nil {
		m.xferNote(j, nil, err)
		return
	}
	l := parseXferStatus(raw)
	m.xferNote(j, &l, nil)
	next := xferDecide(t.Phase, l, cancelAsked)
	if next == t.Phase {
		return
	}
	t.Phase = next
	if xferFinal(next) {
		t.RC, t.Result, t.FinishedAt = l.rc, xferResult(next, l), time.Now().UTC()
	}
	if err := m.xferSave(j, t); err != nil {
		m.log.Warn("save transfer", "id", t.ID, "err", err)
	}
	if xferFinal(next) {
		m.xferCleanupLocked(ctx, j)
	}
}

// xferCleanupLocked does the cleanup the job owes, each part only on a box
// whose record still names the job's machine, and clears what it
// confirmed (under op). A job whose setup failed and owes nothing more is
// deleted: nothing is left to show or to clean.
func (m *Manager) xferCleanupLocked(ctx context.Context, j *xferJob) error {
	t := m.xferSnapshot(j)
	var errs []string
	dst := func(bit int, run func(c context.Context, p *poller) (string, error)) {
		if t.Pending&bit == 0 {
			return
		}
		p, err := m.xferPoller(t.DstBoxID, t.DstEndp, t.DstHostKey)
		if err == nil {
			_, err = run(ctx, p)
		}
		if err != nil {
			errs = append(errs, err.Error())
			return
		}
		t.Pending &^= bit
	}
	// The source first: its cleanup refuses while the runner is alive, and
	// then nothing else is touched (a live copy is a job to read or cancel,
	// not to clean up after).
	running := false
	if t.Pending&store.TransferPendingSrcJob != 0 {
		var err error
		if running, err = m.xferCleanSource(ctx, t); err != nil {
			errs = append(errs, err.Error())
		} else {
			t.Pending &^= store.TransferPendingSrcJob
		}
	}
	if !running {
		dst(store.TransferPendingDstKey, func(c context.Context, p *poller) (string, error) {
			cli, err := p.conn(c)
			if err != nil {
				return "", err
			}
			return xferDstRemoveKey(c, cli, t.Token, t.PubKey)
		})
		dst(store.TransferPendingDstEmpty, func(c context.Context, p *poller) (string, error) {
			cli, err := p.conn(c)
			if err != nil {
				return "", err
			}
			return xferDstRmdirEmpty(c, cli, t.Token, t.DstPath)
		})
	}
	var err error
	if len(errs) > 0 {
		err = errors.New("cleanup not confirmed: " + strings.Join(errs, "; "))
	}
	if t.Phase == xferSetupFailed && t.Pending == 0 {
		// Not cut by an expired setup deadline: the row must go now.
		dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		defer cancel()
		if m.st.DeleteTransfer(dctx, t.ID) == nil {
			m.xferMu.Lock()
			delete(m.xfers, t.ID)
			m.xferMu.Unlock()
			m.Publish(Event{Type: "transfers"})
			return nil
		}
	}
	if serr := m.xferSave(j, t); serr != nil && err == nil {
		err = serr
	}
	m.xferNote(j, nil, err)
	return err
}

// xferCleanSource removes the job directory on the source; running
// reports that it refused because the runner is still alive.
func (m *Manager) xferCleanSource(ctx context.Context, t store.Transfer) (running bool, err error) {
	p, err := m.xferPoller(t.SrcBoxID, t.SrcEndp, t.SrcHostKey)
	if err != nil {
		return false, err
	}
	cli, err := p.conn(ctx)
	if err != nil {
		return false, err
	}
	_, err = xferSrcCleanup(ctx, cli, t.Token)
	var refused *xferRefusal
	if errors.As(err, &refused) && strings.Contains(refused.msg, "still running") {
		return true, errors.New("the copy is still running on the source; nothing was cleaned up")
	}
	return false, err
}

// StartTransfer sets up and launches a copy from box srcID (its revision
// on ctx, see WithBoxRevision) and returns the job. Setup runs on the
// manager's own context: a browser that goes away does not cut it short.
func (m *Manager) StartTransfer(ctx context.Context, srcID int64, req TransferRequest) (TransferView, error) {
	sp, err := m.actionPoller(ctx, srcID)
	if err != nil {
		return TransferView{}, err
	}
	if req.DstBox == srcID {
		return TransferView{}, fmt.Errorf("%w: choose another box as the destination", ErrTransferInput)
	}
	if err := m.CheckBoxRevision(req.DstBox, req.DstRev); err != nil {
		return TransferView{}, err
	}
	dp, err := m.actionPoller(WithBoxRevision(ctx, req.DstRev), req.DstBox)
	if err != nil {
		return TransferView{}, err
	}
	for _, p := range []*poller{sp, dp} {
		if _, err := p.readyState(); err != nil {
			return TransferView{}, fmt.Errorf("%s: %w", p.box.Name, err)
		}
	}
	if AuthEndpoint(sp.box) == AuthEndpoint(dp.box) {
		return TransferView{}, fmt.Errorf("%w: both boxes are the same account", ErrTransferInput)
	}
	src, dstPath := strings.TrimPrefix(req.SrcPath, "~/"), strings.TrimPrefix(req.DstPath, "~/")
	if err := validXferPath(src, false); err != nil {
		return TransferView{}, fmt.Errorf("%w: source: %v", ErrTransferInput, err)
	}
	if err := validXferPath(dstPath, true); err != nil {
		return TransferView{}, fmt.Errorf("%w: destination: %v", ErrTransferInput, err)
	}
	sKey, dKey := sp.hostKey(), dp.hostKey()
	if sKey == "" || dKey == "" {
		return TransferView{}, fmt.Errorf("%w: both boxes need a pinned host key (wait for their first poll)", ErrTransferInput)
	}
	token, err := newXferToken()
	if err != nil {
		return TransferView{}, err
	}
	t := store.Transfer{Token: token, SrcBoxID: srcID, SrcEndp: AuthEndpoint(sp.box), SrcHostKey: sKey,
		DstBoxID: req.DstBox, DstEndp: AuthEndpoint(dp.box), DstHostKey: dKey,
		SrcPath: src, DstPath: dstPath, Phase: xferPreparing, Pending: store.TransferPendingSrcJob}
	// One active copy per source box, checked and recorded under one lock.
	// The row exists before anything changes on a box.
	m.xferMu.Lock()
	for _, o := range m.xfers {
		if o.t.SrcBoxID == srcID && xferActive(o.t.Phase) {
			m.xferMu.Unlock()
			return TransferView{}, fmt.Errorf("%w: %s already has a copy in progress", ErrTransferBusy, sp.box.Name)
		}
	}
	if err := m.st.CreateTransfer(ctx, &t); err != nil {
		m.xferMu.Unlock()
		return TransferView{}, err
	}
	j := &xferJob{t: t}
	j.op.Lock() // held by the setup until it is done
	m.xfers[t.ID] = j
	m.xferMu.Unlock()
	m.Publish(Event{Type: "transfers"})
	res, admitted := m.detached(10*time.Minute, func(c context.Context) error {
		defer j.op.Unlock()
		return m.xferSetupLocked(c, j, sp, dp)
	})
	if !admitted { // shutting down: nothing touched a box
		_ = m.st.DeleteTransfer(context.WithoutCancel(ctx), t.ID)
		m.xferMu.Lock()
		delete(m.xfers, t.ID)
		m.xferMu.Unlock()
		j.op.Unlock()
		return TransferView{}, <-res
	}
	select {
	case err = <-res:
	case <-ctx.Done():
		err = nil // the setup goes on; the list shows how it ends
	}
	return m.xferView(t.ID), err
}

// xferSetupLocked makes the key on the source, claims the destination and
// adds the key there, then launches the runner (under op). A failure that
// proves nothing ran undoes what setup did; the row goes only once every
// undo is confirmed.
func (m *Manager) xferSetupLocked(ctx context.Context, j *xferJob, sp, dp *poller) error {
	t := m.xferSnapshot(j)
	fail := func(err error) error {
		t.Phase, t.Result, t.FinishedAt = xferSetupFailed, clip(err.Error(), 4096), time.Now().UTC()
		_ = m.xferSave(j, t)
		if cerr := m.xferCleanupLocked(ctx, j); cerr != nil {
			return fmt.Errorf("%v (%v)", err, cerr)
		}
		return err
	}
	cli, err := sp.conn(ctx)
	if err != nil {
		return fail(err)
	}
	resolved, blob, err := xferSrcPrepare(ctx, cli, t.Token, t.SrcPath)
	if err != nil {
		return fail(err)
	}
	// Saved before the destination is touched: removal matches this key.
	t.PubKey = blob
	t.Pending |= store.TransferPendingDstKey
	if err := m.xferSave(j, t); err != nil {
		return fail(err)
	}
	dcli, err := dp.conn(ctx)
	if err != nil {
		return fail(err)
	}
	dir, _, _, err := xferDstPrepare(ctx, dcli, t.Token, t.DstPath, blob)
	if dir != "" {
		// The claim answered: this job created the directory. Only then
		// may a failed setup remove it (if still empty); a refused or
		// unanswered claim proves nothing, and a directory that already
		// existed is never touched (Codex on PR 142).
		t.Pending |= store.TransferPendingDstEmpty
	}
	if err != nil {
		return fail(err)
	}
	if cli, err = sp.conn(ctx); err != nil {
		return fail(err)
	}
	// Recorded before the launch: from here a runner may exist, and a
	// dashboard that stops before the answer must find the job again and
	// go on reading it, not take it for a failed setup (Codex on PR 142).
	// The destination directory may hold data from now on: never removed.
	t.Phase = xferUnconfirmed
	t.Pending &^= store.TransferPendingDstEmpty
	if err := m.xferSave(j, t); err != nil {
		return fail(err)
	}
	confirmed, err := xferLaunch(ctx, cli, t.Token, resolved, xferDest{dp.box.User, dp.box.Host, dp.box.Port, t.DstHostKey})
	var refused *xferRefusal
	if errors.As(err, &refused) {
		return fail(err) // the launch script refused before starting anything
	}
	if err == nil && confirmed {
		t.Phase = xferRunning
	}
	if serr := m.xferSave(j, t); serr != nil {
		m.log.Warn("save transfer", "id", t.ID, "err", serr)
	}
	m.xferNote(j, nil, err)
	return nil
}

// xferJobFor finds job id and checks it belongs to source box srcID.
func (m *Manager) xferJobFor(id, srcID int64) (*xferJob, error) {
	m.xferMu.Lock()
	defer m.xferMu.Unlock()
	j, ok := m.xfers[id]
	if !ok || (srcID != 0 && j.t.SrcBoxID != srcID) {
		return nil, store.ErrNotFound
	}
	return j, nil
}

// CancelTransfer stops a running copy. The runner records "cancelled" and
// the usual cleanup follows; a launch that never started is closed too.
func (m *Manager) CancelTransfer(ctx context.Context, srcID, id int64) error {
	j, err := m.xferJobFor(id, srcID)
	if err != nil {
		return err
	}
	j.op.Lock()
	defer j.op.Unlock()
	t := m.xferSnapshot(j)
	if t.Phase != xferRunning && t.Phase != xferUnconfirmed {
		return fmt.Errorf("%w: the transfer is %s", ErrTransferBusy, t.Phase)
	}
	p, err := m.xferPoller(t.SrcBoxID, t.SrcEndp, t.SrcHostKey)
	if err != nil {
		return err
	}
	cli, err := p.conn(ctx)
	if err != nil {
		return err
	}
	out, err := xferCancel(ctx, cli, t.Token)
	if err != nil {
		return err
	}
	if out == "SIGNALLED" {
		time.Sleep(time.Second) // let the trap record it
	}
	m.xferRefreshLocked(ctx, j, true)
	return nil
}

// RetryTransferCleanup tries the owed cleanup of a finished job again.
func (m *Manager) RetryTransferCleanup(ctx context.Context, srcID, id int64) error {
	j, err := m.xferJobFor(id, srcID)
	if err != nil {
		return err
	}
	j.op.Lock()
	defer j.op.Unlock()
	t := m.xferSnapshot(j)
	if !xferFinal(t.Phase) || t.Pending == 0 {
		return fmt.Errorf("%w: no cleanup is owed", ErrTransferBusy)
	}
	return m.xferCleanupLocked(ctx, j)
}

// DismissTransfer forgets a finished job. While cleanup is still owed it
// takes force, and the answer lists what may be left on the boxes.
func (m *Manager) DismissTransfer(ctx context.Context, id int64, force bool) ([]string, error) {
	j, err := m.xferJobFor(id, 0)
	if err != nil {
		return nil, err
	}
	if !j.op.TryLock() {
		return nil, fmt.Errorf("%w: the transfer is being worked on", ErrTransferBusy)
	}
	defer j.op.Unlock()
	t := m.xferSnapshot(j)
	if !xferFinal(t.Phase) {
		return nil, fmt.Errorf("%w: the transfer is %s; cancel it first", ErrTransferBusy, t.Phase)
	}
	left := cleanupWords(t.Pending)
	if len(left) > 0 && !force {
		return left, fmt.Errorf("%w: cleanup is still owed (%s); retry it, or dismiss anyway", ErrTransferBusy, strings.Join(left, "; "))
	}
	if err := m.st.DeleteTransfer(ctx, id); err != nil {
		return left, err
	}
	m.xferMu.Lock()
	delete(m.xfers, id)
	m.xferMu.Unlock()
	m.Publish(Event{Type: "transfers"})
	return left, nil
}

// Transfers lists every job, newest first.
func (m *Manager) Transfers() []TransferView {
	m.xferMu.Lock()
	ids := make([]int64, 0, len(m.xfers))
	for id := range m.xfers {
		ids = append(ids, id)
	}
	m.xferMu.Unlock()
	sort.Slice(ids, func(a, b int) bool { return ids[a] > ids[b] })
	out := make([]TransferView, 0, len(ids))
	for _, id := range ids {
		if v := m.xferView(id); v.ID != 0 {
			out = append(out, v)
		}
	}
	return out
}

func (m *Manager) xferView(id int64) TransferView {
	m.xferMu.Lock()
	j, ok := m.xfers[id]
	if !ok {
		m.xferMu.Unlock()
		return TransferView{}
	}
	t, l, seen, serr := j.t, j.live, j.seen, j.err
	m.xferMu.Unlock()
	v := TransferView{Transfer: t, Cleanup: cleanupWords(t.Pending), LastSeen: seen, StatusErr: serr,
		Skipped: l.skipped, ErrCount: l.errCount, Errors: l.errors, Progress: parseProgress(l.progress)}
	// Scanning: the runner was seen running and rsync has not printed
	// progress yet (it is building its file list).
	v.Scanning = t.Phase == xferRunning && v.Progress == nil && !seen.IsZero() && l.started && l.rc == ""
	if s, ok := m.State(t.SrcBoxID); ok {
		v.SrcName = s.Name
	}
	if s, ok := m.State(t.DstBoxID); ok {
		v.DstName = s.Name
	}
	if xferActive(t.Phase) || t.Pending != 0 {
		var why []string
		if _, err := m.xferPoller(t.SrcBoxID, t.SrcEndp, t.SrcHostKey); errors.Is(err, ErrTransferIdentity) {
			why = append(why, "source: "+err.Error())
		}
		if t.Pending&(store.TransferPendingDstKey|store.TransferPendingDstEmpty) != 0 {
			if _, err := m.xferPoller(t.DstBoxID, t.DstEndp, t.DstHostKey); errors.Is(err, ErrTransferIdentity) {
				why = append(why, "destination: "+err.Error())
			}
		}
		v.Blocked = strings.Join(why, "; ")
	}
	return v
}
