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
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pmss-fleet-manager/internal/probe"
	"pmss-fleet-manager/internal/qbt"
	"pmss-fleet-manager/internal/rtorrent"
	"pmss-fleet-manager/internal/sshx"
	"pmss-fleet-manager/internal/store"
)

// rTorrent on a PMSS box is the panel's own client: root's watchdog starts
// it in the tenant's slice, the tenant stops it with ~/.rtorrentDisable, and
// it answers XML-RPC on an SCGI unix socket inside the home (mode 770 in a
// 0770 home, so only the tenant reaches it). The dashboard dials that socket
// through the SSH channel, reads the client-wide numbers and the main view
// every poll, and runs torrent actions on request. Nothing here installs,
// starts or stops the daemon.

// rtEndpoint is the SCGI endpoint to use: the box setting, else what the
// config named, else PMSS's default socket under the home. A config that
// names an endpoint the probe refused yields nothing: the default socket
// could be another daemon's, and the misconfiguration is to be reported,
// not papered over (daybreak blue review of PR 123).
func rtEndpoint(b rtBox, snap *probe.Snapshot) string {
	if b.RtSCGI != "" {
		return b.RtSCGI
	}
	if snap.Rt.SCGI != "" {
		return snap.Rt.SCGI
	}
	if snap.Rt.ConfFound {
		return ""
	}
	return probe.DefaultSCGI(snap.Home)
}

// rtDataDir is the download root remove-with-data may delete under: the
// config's directory.default (read over SSH from the box's own files), else
// PMSS's ~/data. Never anything the SCGI peer said.
func rtDataDir(snap *probe.Snapshot) string {
	if snap.Rt.DataDir != "" {
		return snap.Rt.DataDir
	}
	if snap.Home == "" {
		return ""
	}
	return snap.Home + "/data"
}

// rtBox is what rtEndpoint needs from the box record.
type rtBox struct{ RtSCGI string }

// ValidRtEndpoint accepts "unix:/absolute/path" or "tcp:127.0.0.1:port"
// (the forms the probe normalises to); it is the box-edit validator.
func ValidRtEndpoint(ep string) bool {
	switch {
	case ep == "":
		return true
	case strings.HasPrefix(ep, "unix:"):
		p := ep[len("unix:"):]
		return strings.HasPrefix(p, "/") && len(p) <= 512 && !strings.ContainsAny(p, "\x00\n\r") && !strings.Contains(p, "/../") && !strings.HasSuffix(p, "/..")
	case strings.HasPrefix(ep, "tcp:127.0.0.1:"):
		n, err := strconv.Atoi(ep[len("tcp:127.0.0.1:"):])
		return err == nil && n > 0 && n <= 65535
	}
	return false
}

// rtClient returns the cached client for ep over cli, building one when the
// endpoint or the SSH transport changed: the dialer is bound to one
// transport, and conn() replaces a dead one (the cache is also dropped
// wherever the transport is closed). A unix socket is dialed as-is: the
// kernel enforces who may connect inside the home. A loopback port goes
// through verifiedDial like the WebUI, since on a shared host a co-tenant
// can bind a free port. A client being replaced is retired first, the same
// way a teardown retires it (dropRtLocked): the labels it owes move to the
// successor only once its operation in flight is complete, and a caller
// still holding it gets rtorrent.ErrRetired rather than a client the box
// no longer listens to (daybreak blue re-review of PR 123).
func (p *poller) rtClient(ctx context.Context, cli *sshx.Client, ep string) (*rtorrent.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rt != nil && p.rtEP == ep && p.rtSSH == cli {
		return p.rt, nil
	}
	dial, err := p.rtDialer(cli, ep)
	if err != nil {
		return nil, err
	}
	if p.rt != nil {
		p.dropRtLocked(ctx)
	}
	nc := rtorrent.New(dial)
	// Labels still owed to magnets (see rtorrent.Client.Pending) belong to
	// the box, not to one client object: the successor takes the saved
	// copy, from the retired client or from the cache on the first client
	// after a start, and is the authority from here (the copy is cleared).
	nc.Restore(p.rtLabels)
	p.rtLabels = nil
	p.rt, p.rtEP, p.rtSSH = nc, ep, cli
	return p.rt, nil
}

// onRetired runs fn against cli and, when that found the client retired by
// a handoff since the caller resolved it, once more against the box's
// current client (daybreak blue re-review of PR 123: a holder of an old
// pointer must resolve the current client, not fail or act on the old).
func (p *poller) onRetired(ctx context.Context, cli *rtorrent.Client, fn func(*rtorrent.Client) error) error {
	err := fn(cli)
	if !errors.Is(err, rtorrent.ErrRetired) {
		return err
	}
	cur, _, _, rerr := p.rtReady(ctx)
	if rerr != nil {
		// Nothing was sent on either attempt: the caller's own error (its
		// code for the API) still carries the unsent mark, so an erase
		// intent recorded ahead of the call is withdrawn (Codex on PR 123).
		return fmt.Errorf("%w (%w)", rerr, rtorrent.ErrNotSent)
	}
	return fn(cur)
}

// rtDialer builds the SCGI dialer for ep over the transport cli (or the
// test hook).
func (p *poller) rtDialer(cli *sshx.Client, ep string) (rtorrent.Dialer, error) {
	switch {
	case p.dialRt != nil:
		return p.dialRt(ep), nil
	case cli == nil:
		return nil, errors.New("rtorrent: no SSH connection")
	case strings.HasPrefix(ep, "unix:"):
		path := ep[len("unix:"):]
		return func(ctx context.Context) (netConn, error) { return cli.DialUnix(ctx, path) }, nil
	case strings.HasPrefix(ep, "tcp:"):
		hp := ep[len("tcp:"):]
		_, portStr, _ := strings.Cut(hp, ":")
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, errors.New("rtorrent: bad endpoint port")
		}
		return rtorrent.Dialer(verifiedDial(cli, hp, port)), nil
	}
	return nil, errors.New("rtorrent: no SCGI endpoint known (set one under edit, or check ~/.rtorrent.rc)")
}

// detachedTransport opens the SSH connection the destructive phase of
// remove-with-data runs on: its own, not the poller's, which an edit
// (stopAndWait), a reconnect or a probe failure may close at any moment
// (fred on PR 123: a transport closed between the erase and the rm leaves
// the files behind). Returns the script runner, the SCGI dialer over it,
// and the close function. Tests replace it through p.dialDetached.
func (p *poller) detachedTransport(ctx context.Context, ep string) (scriptRunner, rtorrent.Dialer, func(), error) {
	if p.dialDetached != nil {
		return p.dialDetached(ctx, ep)
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cli, err := sshx.Dial(cctx, sshConfig(p.box))
	if err != nil {
		return nil, nil, nil, err
	}
	dial, err := p.rtDialer(cli, ep)
	if err != nil {
		cli.Close()
		return nil, nil, nil, err
	}
	return cli, dial, func() { cli.Close() }, nil
}

// dropRtLocked forgets the rTorrent client (its dialer is bound to a
// transport being closed) but keeps the labels it still owes, so the next
// client inherits them; p.mu must be held. Without this a reconnect would
// lose the only in-memory copy and the next poll would persist an empty
// list over the cached one (Codex on PR 123).
func (p *poller) dropRtLocked(ctx context.Context) {
	cli := p.rt
	p.rt, p.rtSSH = nil, nil
	if cli == nil {
		return
	}
	// Retire waits for the operation in flight on the client to complete
	// its remote and local steps before snapshotting, so an Add's label set
	// on the box but not yet remembered, or a remove's erase not yet
	// forgotten, cannot be lost or kept (daybreak blue re-review of PR 123).
	// Bounded by ctx; a client whose transport just died fails fast.
	if ctx == nil {
		ctx = context.Background()
	}
	p.rtEpoch++
	retiredAt := time.Now() // from here the client takes no new operation
	rctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	owed, err := cli.Retire(rctx)
	if err != nil {
		// The operation in flight outlived the wait. The client is retired
		// all the same (nothing new starts on it), so its owed set is final
		// once that operation ends: it is collected then and adopted by
		// whoever owns the box's labels by that time. Nothing is read from
		// the client meanwhile: a snapshot now could miss a label the
		// operation is about to remember, or keep one it is about to forget.
		p.m.lateWG.Add(1)
		go p.adoptLate(cli, retiredAt)
		return
	}
	// A new, non-nil slice even when nothing is owed: an empty snapshot is
	// a fact to persist (the labels settled), not an absence of knowledge.
	p.rtLabels = append([]rtorrent.PendingLabel{}, owed...)
}

// adoptLate finishes the retirement of cli after dropRtLocked could not
// wait for it, and hands what it still owed to the box's current owner:
// the current poller of the same generation (an edit may have replaced
// this one meanwhile; Codex on PR 123), its client or its saved copy. The
// snapshot predates retiredAt, so any hash erased on the box since then is
// refused, whether its tombstone is still open or already settled (fred on
// PR 123: the barrier must outlive the settlement while an older handoff
// can still arrive); a label set since is never overwritten (Adopt). The
// transfer runs under the box's mutation lease, so it cannot interleave
// with an erase or a settle, and under the edit lock, so the current
// poller is the published one.
func (p *poller) adoptLate(cli *rtorrent.Client, retiredAt time.Time) {
	defer p.m.lateWG.Done()
	ctx, cancel := context.WithTimeout(context.Background(), 2*apiTimeout)
	defer cancel()
	owed, err := cli.Retire(ctx)
	if err != nil {
		p.m.log.Warn("rtorrent handoff: the retired client's operation did not end; its owed labels are lost", "box", p.box.Name, "err", err)
		return
	}
	if len(owed) == 0 {
		return
	}
	// The lease may be held by a remove-with-data for its whole life.
	lctx, lcancel := context.WithTimeout(context.Background(), rtRemoveSetupTimeout+rtRemoveTimeout+apiTimeout)
	defer lcancel()
	release, err := p.m.rtLease(p.box.ID).Acquire(lctx)
	if err != nil {
		p.m.log.Warn("rtorrent handoff: the box stayed busy; late owed labels are lost", "box", p.box.Name, "err", err)
		return
	}
	defer release()
	unlock, ok := lockWithin(lctx, &p.m.boxMu)
	if !ok {
		p.m.log.Warn("rtorrent handoff: the edit lock stayed held; late owed labels are lost", "box", p.box.Name)
		return
	}
	defer unlock()
	gen := p.generation()
	cur := p.m.poller(p.box.ID)
	if cur == nil || cur.generation() != gen {
		return // the box was removed or moved: the labels belong to nobody now
	}
	kept := make([]rtorrent.PendingLabel, 0, len(owed))
	for _, l := range owed {
		if !p.m.rtErasedSince(p.box.ID, gen, l.Hash, retiredAt) {
			kept = append(kept, l)
		}
	}
	cur.mu.Lock()
	switch {
	case cur.rt != nil:
		cur.rt.Adopt(kept)
	default:
		have := map[string]bool{}
		for _, l := range cur.rtLabels {
			have[strings.ToUpper(l.Hash)] = true
		}
		merged := append([]rtorrent.PendingLabel{}, cur.rtLabels...)
		for _, l := range kept {
			if !have[strings.ToUpper(l.Hash)] {
				merged = append(merged, l)
			}
		}
		cur.rtLabels = merged
	}
	cur.mu.Unlock()
	cur.recordRtLabels(lctx)
}

// forgetRtLabel drops any label owed to hash from this poller's client and
// its saved copy, and persists the owed set when asked (only the box's
// current poller writes the cache, so a retired one cannot overwrite
// additions made since). It records an erase tombstone for the hash first
// and reports whether the forget is known complete here; the caller lifts
// the tombstone when it is complete on the box's current owner, else a
// poll settles it (settleRtTombs).
func (p *poller) forgetRtLabel(ctx context.Context, hash string, persist bool) (settled bool) {
	hash = strings.ToUpper(hash)
	if err := p.m.rtTombstone(p.box.ID, p.generation(), hash); err != nil {
		p.m.log.Warn("cache rtorrent tombstones", "box", p.box.Name, "err", err)
	}
	// The saved copy and the client are two homes for the same label, and a
	// reconnect (dropRtLocked) moves it from the client to the copy at any
	// moment, including while Forget below waits for the client's operation
	// token: strip the copy, forget on the client, then strip the copy
	// again and forget on whatever client is current now, until nothing
	// moved (Codex on PR 123).
	// Until stable: a forget on the current client, then a locked look that
	// finds the same client epoch (no handoff happened meanwhile) and the
	// saved copy clear. The context bounds a reconnect storm.
	for {
		p.mu.Lock()
		if len(p.rtLabels) > 0 {
			kept := make([]rtorrent.PendingLabel, 0, len(p.rtLabels)) // a new slice: a snapshot in flight keeps its own
			for _, l := range p.rtLabels {
				if strings.ToUpper(l.Hash) != hash {
					kept = append(kept, l)
				}
			}
			p.rtLabels = kept
		}
		cli, epoch := p.rt, p.rtEpoch
		p.mu.Unlock()
		if cli == nil {
			settled = true // the saved copy was the only home
			break
		}
		// Outside p.mu: Forget waits for the client's operation token.
		if err := cli.Forget(ctx, hash); err != nil {
			if errors.Is(err, rtorrent.ErrRetired) && ctx.Err() == nil {
				continue // a handoff retired it under us: the successor is current now
			}
			p.m.log.Warn("forget rtorrent label", "box", p.box.Name, "err", err)
			break
		}
		p.mu.Lock()
		stable := p.rt == cli && p.rtEpoch == epoch
		p.mu.Unlock()
		if stable || ctx.Err() != nil {
			if stable {
				settled = true
				// One last strip: a handoff cannot have happened, but the
				// copy is cheap to clear again.
				p.mu.Lock()
				if len(p.rtLabels) > 0 {
					kept := make([]rtorrent.PendingLabel, 0, len(p.rtLabels))
					for _, l := range p.rtLabels {
						if strings.ToUpper(l.Hash) != hash {
							kept = append(kept, l)
						}
					}
					p.rtLabels = kept
				}
				p.mu.Unlock()
			}
			break
		}
	}
	if persist {
		p.recordRtLabels(ctx)
	}
	return settled
}

// settleRtTombs forgets the labels still owed to tombstoned hashes (a
// removal whose bookkeeping was skipped or cut) and lifts the tombstones
// it settled, then persists. Serialised with adds and removals through the
// box's mutation lease: a caller that holds it says so, a poll takes it
// for a moment and leaves the work to the next poll if a mutation holds it
// (the one that left the tombstone, perhaps). A poll's settle is also
// serialised with edits (stopAndWait waits for it), so a label it drops
// cannot be restored by an edit's replacement poller.
func (p *poller) settleRtTombs(ctx context.Context, leased bool) {
	gen := p.generation()
	if len(p.m.rtTombList(p.box.ID, gen)) == 0 {
		return
	}
	if !leased {
		lctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		release, err := p.m.rtLease(p.box.ID).Acquire(lctx)
		cancel()
		if err != nil {
			return
		}
		defer release()
	}
	// Re-read under the lease: an add may have settled and lifted some.
	// The cleaned label row goes to the store first; only once it is durable
	// are the tombstones settled durably, or a restart between the two
	// writes would restore the old row with nothing left to settle it
	// (fred on PR 123). Settled in memory and on disk only after that
	// write, so a failed write leaves them for the next poll.
	var done []string
	for _, h := range p.m.rtTombList(p.box.ID, gen) {
		if p.forgetRtLabel(ctx, h, false) {
			done = append(done, h)
		}
	}
	if p.recordRtLabels(ctx) {
		for _, h := range done {
			p.m.rtSettleTomb(p.box.ID, h)
		}
	}
}

// rtLabelsCacheKind is the box_cache kind holding the labels owed to
// magnets (rtLabelsCache) across restarts.
const rtLabelsCacheKind = "rt_labels"

// rtLabelsCache is the persisted form: the owed labels and the generation
// of the box they were recorded for, so a record moved to another machine
// does not label a stranger's torrents.
type rtLabelsCache struct {
	Generation string                  `json:"generation"`
	Labels     []rtorrent.PendingLabel `json:"labels"`
}

// recordRtLabels writes the owed labels to the cache and reports whether
// the write succeeded (a tombstone is settled durably only after it has;
// fred on PR 123): the client's while
// one is up (an empty list when there is none, so a settled label does not
// come back), else the ones saved from the last client by dropRtLocked, so
// a box that stays offline across a dashboard restart does not lose them
// (Codex on PR 123). With neither, the cache is left as it is.
func (p *poller) recordRtLabels(ctx context.Context) (written bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	// Snapshot and write under the box's label guard, so two writers'
	// snapshots reach the store in the order they were taken and an older
	// one cannot resurrect a retired label; the saved slice is copied under
	// p.mu, since forgetRtLabel rewrites it (daybreak blue re-review of PR
	// 123, race detector).
	g := p.m.labelGuard(p.box.ID)
	g.Lock()
	defer g.Unlock()
	// Only the box's current poller writes: a retired one (an edit replaced
	// it while one of its requests was between its remote work and this
	// write) would put its older snapshot over the successor's newer one
	// (daybreak blue re-review 5 of PR 123). UpdateBox's own flush of the
	// retiring poller runs before the successor is published, so it passes.
	if p.m.poller(p.box.ID) != p {
		return false
	}
	p.mu.Lock()
	cli := p.rt
	var saved []rtorrent.PendingLabel
	if p.rtLabels != nil {
		saved = append([]rtorrent.PendingLabel{}, p.rtLabels...)
	}
	p.mu.Unlock()
	var labels []rtorrent.PendingLabel
	switch {
	case cli != nil:
		labels = cli.Pending()
	case saved != nil:
		labels = saved // an empty saved copy is known-empty and is written as such
	default:
		return false
	}
	// Never persist a label whose torrent was erased: a tombstone outlives
	// a skipped cleanup, the written copy must not.
	gen := p.generation()
	kept := make([]rtorrent.PendingLabel, 0, len(labels))
	for _, l := range labels {
		if !p.m.rtTombed(p.box.ID, gen, l.Hash) {
			kept = append(kept, l)
		}
	}
	c := rtLabelsCache{Generation: gen, Labels: kept}
	if p.m.cacheFault != nil {
		if err := p.m.cacheFault(rtLabelsCacheKind); err != nil {
			p.m.log.Warn("cache rtorrent labels", "box", p.box.Name, "err", err)
			return false
		}
	}
	if err := p.m.st.SetCache(ctx, p.box.ID, rtLabelsCacheKind, c); err != nil {
		p.m.log.Warn("cache rtorrent labels", "box", p.box.Name, "err", err)
		return false
	}
	return true
}

// restoredRtLabels loads the owed labels recorded for this box's current
// generation; anything else is ignored.
func (m *Manager) restoredRtLabels(b *store.Box) []rtorrent.PendingLabel {
	ctx := m.ctx
	if ctx == nil { // before Start (tests); a nil context must never reach the driver
		ctx = context.Background()
	}
	var c rtLabelsCache
	if _, err := m.st.GetCache(ctx, b.ID, rtLabelsCacheKind, &c); err != nil || c.Generation != boxGeneration(b) {
		return nil
	}
	if len(c.Labels) > 1000 {
		c.Labels = c.Labels[:1000]
	}
	return c.Labels
}

// detectRtDial is the test hook for detectRt's socket dial.
var detectRtDial func(ep string) rtorrent.Dialer

// pollRt fills st.Rt from the snapshot and the client.
func (p *poller) pollRt(ctx context.Context, st *BoxState, snap *probe.Snapshot) {
	running, known := snap.RtProcess()
	rs := RtState{Enabled: true, ProcessUp: running, ProcessKnown: known, ConfFound: snap.Rt.ConfFound, Disabled: snap.Rt.Disabled,
		Version: st.Rt.Version, Library: st.Rt.Library, RemoveFailed: st.Rt.RemoveFailed, RemoveFailedAt: st.Rt.RemoveFailedAt}
	rs.Endpoint = clip(rtEndpoint(rtBox{p.box.RtSCGI}, snap), 600)
	defer func() { st.Rt = rs }()
	if known && !running {
		rs.Error = "rtorrent is not running"
		if rs.Disabled {
			rs.Error += " (~/.rtorrentDisable is set)"
		}
		return
	}
	rs.DataDir = clip(rtDataDir(snap), 600)
	if rs.Endpoint == "" {
		rs.Error = "no SCGI endpoint: the probe could not read the home path or ~/.rtorrent.rc"
		if snap.Rt.ConfError != "" {
			rs.Error = clip(snap.Rt.ConfError, 300) + "; set the endpoint under edit to override"
		}
		return
	}
	if strings.HasPrefix(rs.Endpoint, "tcp:") {
		_, portStr, _ := strings.Cut(rs.Endpoint[len("tcp:"):], ":")
		port, _ := strconv.Atoi(portStr)
		if ok, reason := snap.PortOwner(port); !ok {
			rs.Error = reason
			return
		}
		rs.PortOK = true
	}
	var sshc *sshx.Client
	if p.dialRt == nil { // the test hook replaces the SSH channel
		var err error
		if sshc, err = p.conn(ctx); err != nil {
			rs.Error = describeErr(err)
			return
		}
	}
	cli, err := p.rtClient(ctx, sshc, rs.Endpoint)
	if err != nil {
		rs.Error = describeErr(err)
		return
	}
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	// Labels left owed by a removal whose bookkeeping was skipped go before
	// the listing applies any label.
	p.settleRtTombs(actx, false)
	var stats *rtorrent.Stats
	var list []rtorrent.Torrent
	err = p.onRetiredPoll(actx, &cli, sshc, rs.Endpoint, func(c *rtorrent.Client) error {
		var err error
		if stats, err = c.Stats(actx); err != nil {
			return err
		}
		list, err = c.Torrents(actx)
		return err
	})
	if err != nil {
		rs.Error = describeErr(err)
		return
	}
	rs.Version, rs.Library = clip(stats.Version, 32), clip(stats.Library, 32)
	rs.ListenPort, rs.DHT = stats.ListenPort, stats.DHT
	rs.Reachable = true
	// A socket that answers is a running client, whatever ps said.
	rs.ProcessUp = true
	// Rates: beside qBittorrent's when both answered this poll, else alone
	// (over the PMSS 15-minute estimate, like qBittorrent's).
	if st.Rates.Source == "qbt" {
		st.Rates = RateState{UpBps: st.Rates.UpBps + float64(stats.UpBPS), DownBps: st.Rates.DownBps + float64(stats.DownBPS), Source: "clients"}
	} else {
		st.Rates = RateState{UpBps: float64(stats.UpBPS), DownBps: float64(stats.DownBPS), Source: "rtorrent"}
	}
	rs.Torrents = make([]TorrentView, 0, len(list))
	for _, t := range list {
		kind := t.Kind()
		switch kind {
		case "seeding":
			rs.Seeding++
		case "leeching":
			rs.Leeching++
		case "paused":
			rs.Paused++
		case "error":
			rs.Errored++
		}
		var progress float64
		if t.Size > 0 {
			progress = unit(float64(t.Completed) / float64(t.Size))
		}
		state := t.State()
		if kind == "error" {
			state = clip(t.Message, 32)
		}
		rs.Torrents = append(rs.Torrents, TorrentView{
			Hash: strings.ToLower(t.Hash), Client: "rtorrent", Name: clip(t.Name, 400), Size: t.Size, Progress: progress,
			UpBps: t.UpRate, DownBps: t.DownRate, Ratio: float64(t.Ratio) / 1000, State: state, Kind: kind,
			AddedOn: t.Finished, Category: clip(t.Label, 100),
			Uploaded: t.UpTotal, Downloaded: t.DownTotal, Seeds: int(min(t.Seeds, 1<<30)), Peers: int(min(t.Peers, 1<<30)),
		})
	}
	rs.Total = len(rs.Torrents)
	sort.Slice(rs.Torrents, func(i, j int) bool {
		a, b := rs.Torrents[i], rs.Torrents[j]
		if (a.UpBps+a.DownBps > 0) != (b.UpBps+b.DownBps > 0) {
			return a.UpBps+a.DownBps > 0
		}
		if a.UpBps+a.DownBps != b.UpBps+b.DownBps {
			return a.UpBps+a.DownBps > b.UpBps+b.DownBps
		}
		if a.AddedOn != b.AddedOn {
			return a.AddedOn > b.AddedOn
		}
		return a.Name < b.Name
	})
}

// rtRemoveTimeout bounds the rm of a torrent's data on the box, as a fresh
// deadline started after the erase and its bookkeeping: a large tree on a
// slow disk takes a while. rtRemoveSetupTimeout bounds what comes before
// it in the detached job: the connection, the erase and the label
// bookkeeping (including the wait for the edit lock). The two run one
// after the other, so a remove-with-data can take up to their sum, about
// 225 seconds, before the dashboard gives up on it; the request that
// started it is answered as soon as its own deadline passes.
const (
	rtRemoveTimeout      = 75 * time.Second
	rtRemoveSetupTimeout = 150 * time.Second
)

// RtActionRemoveData is the one torrent action rTorrent has no command
// for: remove the torrent, then delete its data. See rtRemoveData.
const RtActionRemoveData = "remove-data"

// ValidRtAction reports whether a is an action the API accepts for
// rTorrent: the client's own plus remove-data. A closed switch, so nothing
// can add a key that validates but runs no case.
func ValidRtAction(a string) bool {
	return rtorrent.ValidAction(a) || a == RtActionRemoveData
}

// rtReady returns the client after the checks that precede any rTorrent
// action: monitoring on, the box probed by this poller, the SSH connection
// up and, for a loopback endpoint, the port verified as this account's
// right now.
func (p *poller) rtReady(ctx context.Context) (*rtorrent.Client, *sshx.Client, BoxState, error) {
	if p.readyHook != nil {
		if err := p.readyHook(); err != nil {
			return nil, nil, BoxState{}, err
		}
	}
	if !p.box.RtEnabled {
		return nil, nil, BoxState{}, errors.New("rTorrent is not enabled for this box")
	}
	st, err := p.readyState()
	if err != nil {
		return nil, nil, st, err
	}
	if st.Rt.Endpoint == "" {
		return nil, nil, st, errors.New("no SCGI endpoint known for rTorrent yet")
	}
	var sshc *sshx.Client
	if p.dialRt == nil {
		if sshc, err = p.conn(ctx); err != nil {
			return nil, nil, st, err
		}
	}
	if strings.HasPrefix(st.Rt.Endpoint, "tcp:") {
		if !st.Rt.PortOK {
			return nil, nil, st, errors.New("the SCGI port has not been verified as owned by this account yet")
		}
		_, portStr, _ := strings.Cut(st.Rt.Endpoint[len("tcp:"):], ":")
		port, _ := strconv.Atoi(portStr)
		if sshc != nil { // nil only under the test hook, which has no box to ask
			if err := verifyPortNow(ctx, sshc, port); err != nil {
				return nil, nil, st, err
			}
		}
	}
	cli, err := p.rtClient(ctx, sshc, st.Rt.Endpoint)
	if err != nil {
		return nil, nil, st, err
	}
	return cli, sshc, st, nil
}

// onRetiredPoll is onRetired for the poll, which resolves clients through
// rtClient rather than the Ready gate (the state is being built).
func (p *poller) onRetiredPoll(ctx context.Context, cli **rtorrent.Client, sshc *sshx.Client, ep string, fn func(*rtorrent.Client) error) error {
	err := fn(*cli)
	if !errors.Is(err, rtorrent.ErrRetired) {
		return err
	}
	cur, rerr := p.rtClient(ctx, sshc, ep)
	if rerr != nil {
		return rerr
	}
	*cli = cur
	return fn(cur)
}

// sshRunner adapts a possibly nil SSH client to scriptRunner (a nil
// interface, not an interface holding a nil pointer).
func sshRunner(c *sshx.Client) scriptRunner {
	if c == nil {
		return nil
	}
	return c
}

// rtTorrentAction runs one RtTorrentActions entry on hash.
func (p *poller) rtTorrentAction(ctx context.Context, hash, action string) error {
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	cli, sshc, st, err := p.rtReady(actx)
	if err != nil {
		return err
	}
	// One rTorrent mutation at a time per box: a remove-with-data holds the
	// lease for its whole life, including the detached phase, so an add of
	// the same hash cannot land between its erase and its deletion.
	release, err := p.m.rtLease(p.box.ID).Acquire(actx)
	if err != nil {
		return err
	}
	if action == RtActionRemoveData {
		// The caller's context: the deletion inside runs under the
		// manager's own deadline, not the request's; release travels with it.
		return p.rtRemoveData(ctx, cli, sshRunner(sshc), st.Rt.Endpoint, st.Rt.DataDir, hash, release)
	}
	defer release()
	var prevTomb rtTomb
	var hadTomb bool
	if action == rtorrent.ActionRemove {
		// The erase intent is on record, durably, before the irreversible
		// command goes out: should the answer be lost or the process die
		// after rTorrent acted, the restart still knows the hash was
		// erased and settles its label before anything can apply it
		// (daybreak blue re-review 5 of PR 123). Not recordable: not sent.
		prevTomb, hadTomb = p.m.rtTombSnapshot(p.box.ID, hash)
		if err := p.m.rtTombstone(p.box.ID, p.generation(), hash); err != nil {
			return fmt.Errorf("could not record the removal before sending it; nothing was changed: %w", err)
		}
	}
	if err := p.onRetired(actx, cli, func(c *rtorrent.Client) error { return c.Action(actx, action, hash) }); err != nil {
		if action == rtorrent.ActionRemove && rtorrent.NotSent(err) {
			// The command provably never left: the intent is withdrawn, or
			// the next poll would revoke a live torrent's label (daybreak
			// blue re-review 6 of PR 123). A failure after the write keeps it.
			p.m.rtRestoreTomb(p.box.ID, hash, prevTomb, hadTomb)
		}
		return err
	}
	if action == rtorrent.ActionRemove {
		// A removed torrent is owed nothing: forget the label now and
		// persist, so a stop or edit before the next poll does not carry
		// it into a restart (daybreak blue re-review of PR 123); the
		// tombstone is lifted by the next poll, which runs before an edit
		// can restore anything.
		p.forgetRtLabel(actx, hash, true)
	}
	return nil
}

// scriptRunner runs a shell script on the box: sshx.Client, or a fake in
// tests.
type scriptRunner interface {
	Run(ctx context.Context, cmd string, stdin io.Reader) ([]byte, []byte, error)
}

// rtRemoveData is "remove torrent and data" for a client that has no such
// command (ruTorrent does the deletion itself in PHP).
//
// The boundary is root, the download directory read from the box's own
// rTorrent config over SSH (RtState.DataDir; ~/data on PMSS), never a
// path the SCGI peer named: the peer only says what to delete, the box
// decides whether that lies inside the boundary (daybreak blue review of
// PR 123). The box check runs first, with nothing changed on refusal; only
// then is the torrent erased and the tree deleted, both under a deadline
// owned by the manager rather than the HTTP request, so a browser that
// goes away or a slow disk cannot cut the deletion between the two steps.
// The destructive phase (erase, then rm) runs on an SSH connection of its
// own, so nothing that closes the poller's (an edit, a reconnect, a probe
// failure) can cut it between the two steps. The caller gets the outcome
// when it is known, or, if its own context ends first, word that the
// deletion continues.
func (p *poller) rtRemoveData(ctx context.Context, cli *rtorrent.Client, run scriptRunner, ep, root, hash string, release func()) (err error) {
	if release == nil {
		release = func() {}
	}
	detachedOwnsLease := false
	defer func() {
		if !detachedOwnsLease {
			release() // refused or failed before the detached phase took it over
		}
	}()
	if run == nil {
		return errors.New("no SSH connection to the box")
	}
	if root == "" {
		return errors.New("the download directory is not known yet (no directory.default in the rTorrent config and no home path); not deleting anything")
	}
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	var base string
	if err := p.onRetired(actx, cli, func(c *rtorrent.Client) (err error) { base, err = c.BasePath(actx, hash); return err }); err != nil {
		return err
	}
	if base == "" {
		// A torrent that never started has no path yet (seen live on pm1
		// with a freshly added, stopped torrent): nothing to delete.
		return errors.New("rTorrent reports no download path for this torrent (never started?); nothing to delete, the torrent was left in place. Use remove instead")
	}
	// Absolute, bounded, no line breaks: anything else cannot even be handed
	// to the box (whose own check is the authority on what is inside).
	for _, s := range []string{base, root} {
		if !strings.HasPrefix(s, "/") || len(s) > 4096 || strings.ContainsAny(s, "\x00\n\r") {
			return errors.New("rTorrent reported a path that cannot be checked; not deleting anything")
		}
	}
	// 1. The box resolves both paths and checks containment; nothing has
	// changed if it refuses.
	cctx, ccancel := context.WithTimeout(ctx, 20*time.Second)
	out, stderr, err := run.Run(cctx, "sh", strings.NewReader(rtRemoveDataVars("check", root, base)+rtRemoveDataScript))
	ccancel()
	if err != nil {
		return fmt.Errorf("refusing to delete, the torrent was left in place: %s", clip(strings.TrimSpace(string(out)+" "+string(stderr)), 300))
	}
	// 2. Erase, then delete, detached from the request and on a transport
	// of their own. The lease travels with the job and is released when it
	// ends, so no other rTorrent mutation on this box runs meanwhile. The
	// job's deadline bounds the connection, the erase and the bookkeeping;
	// the rm then gets its own, from the job context itself.
	gen := p.generation() // the machine this job acts on
	res, admitted := p.m.detached(p.rmSetupTimeout(), func(dctx context.Context) (err error) {
		defer release()
		// The job answers for itself, whether or not the request is still
		// there to hear it: the outcome is logged, and a failure is written
		// to the box's state (shown under the client, raised as an alert)
		// until dismissed (Codex on PR 123: a request that ended first left
		// nobody to consume the result).
		defer func() { p.reportRtRemove(gen, hash, base, err) }()
		drun, ddial, dclose, err := p.detachedTransport(dctx, ep)
		if err != nil {
			return fmt.Errorf("could not open a connection for the deletion; nothing was changed: %w", err)
		}
		defer dclose()
		dcli := rtorrent.New(ddial)
		ectx, ecancel := context.WithTimeout(dctx, apiTimeout)
		defer ecancel()
		// The erase intent goes on record, durably, before the erase is
		// sent (see rtTorrentAction); withdrawn again if the command
		// provably never left.
		prevTomb, hadTomb := p.m.rtTombSnapshot(p.box.ID, hash)
		if err := p.m.rtTombstone(p.box.ID, gen, hash); err != nil {
			return fmt.Errorf("could not record the removal before sending it; nothing was changed: %w", err)
		}
		if err := dcli.Action(ectx, rtorrent.ActionRemove, hash); err != nil {
			if rtorrent.NotSent(err) {
				p.m.rtRestoreTomb(p.box.ID, hash, prevTomb, hadTomb)
			}
			return fmt.Errorf("rTorrent refused to remove the torrent; nothing was deleted: %w", err)
		}
		// The removed torrent is owed nothing any more, wherever the box's
		// pending labels live by now: this poller's client or its saved
		// copy, and, if an edit replaced this poller meanwhile, the current
		// poller's (which also persists the result). Under the edit lock,
		// so the cleanup cannot fall between an edit's restore of the owed
		// labels and its publish of the replacement poller, and only for a
		// poller of the same generation (compared through each poller's
		// synchronised view: conn pins the host key under its lock); fred
		// and the daybreak blue re-reviews on PR 123. The lock is waited
		// for under the job's deadline. The erase tombstone comes first,
		// before the lease can be released: should the lock not be free in
		// time (the forget then runs here only, unverified against the
		// current owner) or the forget be cut, the tombstone outlives this
		// job and the next poll settles it before any label is applied or
		// persisted, so the dead label cannot reach a re-added torrent
		// (fred on PR 123). It is settled only when the cleanup is known
		// complete on the current owner, and stays on record as the erase
		// time for late handoffs (adoptLate). The record itself was
		// written before the erase went out (above): nothing to add here.
		if unlock, ok := lockWithin(dctx, &p.m.boxMu); ok {
			// The cleaned label row is persisted by the current owner before
			// the tombstone is settled durably (fred on PR 123).
			settled := p.forgetRtLabel(dctx, hash, false)
			if cur := p.m.poller(p.box.ID); cur == p {
				if p.recordRtLabels(dctx) && settled {
					p.m.rtSettleTomb(p.box.ID, hash)
				}
			} else if cur != nil && cur.generation() == gen {
				onCur := cur.forgetRtLabel(dctx, hash, false)
				if cur.recordRtLabels(dctx) && settled && onCur {
					p.m.rtSettleTomb(p.box.ID, hash)
				}
			}
			unlock()
		} else {
			p.forgetRtLabel(dctx, hash, false)
			p.m.log.Warn("rtorrent remove-with-data: the edit lock was not free in time; the label is tombstoned and settled by the next poll", "box", p.box.Name)
		}
		// The rm gets its own, full budget from here, from the job context
		// itself rather than this job's deadline, which the connection, the
		// erase and the bookkeeping above have spent.
		rmctx, rmcancel := context.WithTimeout(p.m.jobBase(), rtRemoveTimeout)
		defer rmcancel()
		out, stderr, err := drun.Run(rmctx, "sh", strings.NewReader(rtRemoveDataVars("delete", root, base)+rtRemoveDataScript))
		if err != nil {
			why := clip(strings.TrimSpace(string(out)+" "+string(stderr)), 300)
			if errors.Is(rmctx.Err(), context.DeadlineExceeded) {
				why = fmt.Sprintf("the deletion did not finish within %s", rtRemoveTimeout)
			}
			return fmt.Errorf("the torrent was removed from rTorrent but its files were not (all) deleted: %s", why)
		}
		return nil
	})
	if !admitted {
		// Stop has begun: the job did not start, so the lease is still ours
		// to give back, and the answer is final whatever the request did.
		return <-res
	}
	detachedOwnsLease = true
	select {
	case err := <-res:
		return err
	case <-ctx.Done():
		return fmt.Errorf("the request ended before the deletion finished; it continues on the box: a failure is shown under rTorrent on this box and raises an alert: %w", ctx.Err())
	}
}

// reportRtRemove logs a detached remove-with-data's outcome and, on a
// failure, records it on the box's state for the UI and the alert rule,
// provided the state still belongs to the machine the job acted on (gen):
// an edit that moved the record to another host or account meanwhile
// cleared that machine's state and alerts, and its cleanup error must not
// reappear on the replacement (Codex on PR 123). The log line is kept
// either way.
func (p *poller) reportRtRemove(gen, hash, base string, err error) {
	if err == nil {
		p.m.log.Info("rtorrent remove-with-data done", "box", p.box.Name, "hash", hash, "path", base)
		return
	}
	p.m.log.Warn("rtorrent remove-with-data failed", "box", p.box.Name, "hash", hash, "path", base, "err", err)
	msg := clip(fmt.Sprintf("%s (%s…): %v", filepath.Base(base), strings.ToLower(hash[:8]), err), 400)
	now := time.Now()
	// Under the box's guard with the dismiss, and the durable row before the
	// live state: a restart finds the row whatever the state cache's copy
	// says (a poll writes that copy before it merges; fred on PR 123), and
	// Stop's drain, whose manager context is gone, writes it too (own
	// context). The row is written only when the live state still belongs
	// to gen, like the state itself.
	g := p.m.labelGuard(p.box.ID)
	g.Lock()
	defer g.Unlock()
	p.m.mu.RLock()
	stored := p.m.states[p.box.ID]
	same := stored != nil && stored.generation() == gen
	p.m.mu.RUnlock()
	if !same {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var werr error
	if p.m.cacheFault != nil {
		werr = p.m.cacheFault(rtRemoveCacheKind)
	}
	if werr == nil {
		werr = p.m.st.SetCache(ctx, p.box.ID, rtRemoveCacheKind, rtRemoveCache{Generation: gen, Message: msg, At: now})
	}
	if werr != nil {
		// The live notice is still published (the user must see it); the
		// dismiss compares against the live notice first, so the stale row
		// cannot be used to clear it (daybreak blue re-review 6 of PR 123).
		p.m.log.Warn("cache rtorrent removal outcome", "box", p.box.Name, "err", werr)
	}
	p.m.commit(p.box.ID, func(st *BoxState) *BoxState {
		if st == nil || st.generation() != gen {
			return nil
		}
		next := *st
		next.Rt.RemoveFailed, next.Rt.RemoveFailedAt = msg, now
		return &next
	}, commitOpts{hooks: true, publish: true})
}

// lockWithin takes mu unless ctx ends first. A mutex cannot be waited on
// with a context, so the lock is taken on a helper goroutine, and exactly
// one of two outcomes is chosen with one compare-and-swap: the helper hands
// the lock to the caller, or the caller gave up first and the helper lets
// the lock go at once. Nothing can be left holding it (Codex on PR 123: a
// check-then-send handoff could leak the mutex when the deadline fell
// between the check and the send).
func lockWithin(ctx context.Context, mu *sync.Mutex) (unlock func(), ok bool) {
	const waiting, handed, gaveUp = 0, 1, 2
	var state atomic.Int32
	got := make(chan struct{}, 1)
	go func() {
		mu.Lock()
		if state.CompareAndSwap(waiting, handed) {
			got <- struct{}{} // ownership passes to the caller (buffered: never blocks)
			return
		}
		mu.Unlock() // the caller gave up first
	}()
	select {
	case <-got:
		return mu.Unlock, true
	case <-ctx.Done():
		if state.CompareAndSwap(waiting, gaveUp) {
			return nil, false // the helper will unlock when it gets the lock
		}
		<-got // the helper won the race: the lock is ours after all
		return mu.Unlock, true
	}
}

// rmSetupTimeout is the detached job's deadline for connection, erase and
// bookkeeping (rtRemoveSetupTimeout outside tests).
func (p *poller) rmSetupTimeout() time.Duration {
	if p.jobTimeout > 0 {
		return p.jobTimeout
	}
	return rtRemoveSetupTimeout
}

// rtRemoveDataVars renders the script's inputs as shell-quoted assignments
// ahead of it (the shell may buffer stdin past the script, so they cannot
// follow it as data).
func rtRemoveDataVars(mode, root, target string) string {
	return "mode=" + shellQuote(mode) + "\nroot=" + shellQuote(root) + "\ntarget=" + shellQuote(target) + "\n"
}

// rtRemoveDataScript takes $mode (check | delete), $root and $target.
// Both paths are resolved through symlinks on the box; the root must lie
// strictly inside the home and the target strictly inside the root, or the
// script prints why and exits non-zero having changed nothing. In delete
// mode it then enters the target's resolved parent directory, verifies it
// is there, and removes the target by its base name relative to that
// directory: a directory held open as the working directory cannot be
// swapped for a symlink under the removal the way a pathname can, which
// narrows the check-then-remove window to the final component (the
// tenant's own concurrent renames; rm itself does not follow symlinks).
const rtRemoveDataScript = `LC_ALL=C; export LC_ALL
[ -n "$root" ] || { echo "no root"; exit 3; }
[ -n "$target" ] || { echo "no target"; exit 3; }
home=$(realpath -- "$HOME" 2>/dev/null) || { echo "cannot resolve HOME"; exit 4; }
r=$(realpath -e -- "$root" 2>/dev/null) || { echo "download root $root does not exist"; exit 4; }
t=$(realpath -e -- "$target" 2>/dev/null) || { echo "path does not exist (already deleted?)"; exit 5; }
case "$home" in /) echo "HOME is /"; exit 4;; esac
case "$r" in "$home"/?*) ;; *) echo "refusing: download root $r is not inside the home"; exit 6;; esac
case "$t" in "$r"/?*) ;; *) echo "refusing: $t is not inside the download root $r"; exit 6;; esac
[ "$mode" = delete ] || { echo "ok: $t"; exit 0; }
parent=$(dirname -- "$t"); name=$(basename -- "$t")
cd -- "$parent" || { echo "cannot enter $parent"; exit 7; }
[ "$(pwd -P)" = "$parent" ] || { echo "refusing: $parent moved"; exit 7; }
rm -rf -- "./$name"
`

// rtAddTorrents loads the request into rTorrent; the API shares its
// request shape with qBittorrent's.
func (p *poller) rtAddTorrents(ctx context.Context, req qbt.AddRequest) (rtorrent.AddResult, error) {
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	cli, _, _, err := p.rtReady(actx)
	if err != nil {
		return rtorrent.AddResult{}, err
	}
	release, err := p.m.rtLease(p.box.ID).Acquire(actx)
	if err != nil {
		return rtorrent.AddResult{}, err
	}
	defer release()
	// Tombstoned hashes are settled first, under the lease: a re-add of an
	// erased hash gets the label it asks for now, not the one it had. A
	// settlement that could not be made durable leaves the tombstone open,
	// and an add over an open tombstone would persist nothing for that hash
	// (recordRtLabels skips tombstoned hashes) while answering OK: refused
	// instead, until the records can be written again (fred on PR 123).
	p.settleRtTombs(actx, true)
	if open := p.m.rtTombList(p.box.ID, p.generation()); len(open) > 0 {
		return rtorrent.AddResult{}, fmt.Errorf("%w: a previous removal on this box is not yet settled in the dashboard's records, because the dashboard's database could not be written; nothing was added. This is a dashboard-side problem, not rTorrent's: check the dashboard's disk and database, then try again", ErrNotReady)
	}
	r := rtorrent.AddRequest{URLs: req.URLs, Label: req.Category, Stopped: req.Stopped}
	for _, f := range req.Files {
		r.Files = append(r.Files, f.Data)
	}
	var res rtorrent.AddResult
	err = p.onRetired(actx, cli, func(c *rtorrent.Client) (err error) { res, err = c.Add(actx, r); return err })
	if p.beforeRecord != nil {
		p.beforeRecord()
	}
	// The labels now owed to magnets are persisted at once, whatever the
	// outcome: a shutdown before the next poll must not lose them
	// (daybreak blue review of PR 123).
	rctx, rcancel := context.WithTimeout(context.Background(), 10*time.Second)
	p.recordRtLabels(rctx)
	rcancel()
	return res, err
}
