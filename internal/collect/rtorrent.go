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
	"sort"
	"strconv"
	"strings"
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
// config named, else PMSS's default socket under the home.
func rtEndpoint(b rtBox, snap *probe.Snapshot) string {
	if b.RtSCGI != "" {
		return b.RtSCGI
	}
	if snap.Rt.SCGI != "" {
		return snap.Rt.SCGI
	}
	return probe.DefaultSCGI(snap.Home)
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
// can bind a free port.
func (p *poller) rtClient(cli *sshx.Client, ep string) (*rtorrent.Client, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.rt != nil && p.rtEP == ep && p.rtSSH == cli {
		return p.rt, nil
	}
	var dial rtorrent.Dialer
	switch {
	case p.dialRt != nil:
		dial = p.dialRt(ep)
	case cli == nil:
		return nil, errors.New("rtorrent: no SSH connection")
	case strings.HasPrefix(ep, "unix:"):
		path := ep[len("unix:"):]
		dial = func(ctx context.Context) (netConn, error) { return cli.DialUnix(ctx, path) }
	case strings.HasPrefix(ep, "tcp:"):
		hp := ep[len("tcp:"):]
		_, portStr, _ := strings.Cut(hp, ":")
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return nil, errors.New("rtorrent: bad endpoint port")
		}
		dial = rtorrent.Dialer(verifiedDial(cli, hp, port))
	default:
		return nil, errors.New("rtorrent: no SCGI endpoint known (set one under edit, or check ~/.rtorrent.rc)")
	}
	nc := rtorrent.New(dial)
	// Labels still owed to magnets (see rtorrent.Client.Pending) belong to
	// the box, not to one client object: carry them over from the client
	// being replaced, or from the cache on the first client after a start.
	if p.rt != nil {
		nc.Restore(p.rt.Pending())
	} else {
		nc.Restore(p.rtLabels)
		p.rtLabels = nil
	}
	p.rt, p.rtEP, p.rtSSH = nc, ep, cli
	return p.rt, nil
}

// dropRtLocked forgets the rTorrent client (its dialer is bound to a
// transport being closed) but keeps the labels it still owes, so the next
// client inherits them; p.mu must be held. Without this a reconnect would
// lose the only in-memory copy and the next poll would persist an empty
// list over the cached one (Codex on PR 123).
func (p *poller) dropRtLocked() {
	if p.rt != nil {
		if owed := p.rt.Pending(); len(owed) > 0 {
			p.rtLabels = owed
		}
	}
	p.rt, p.rtSSH = nil, nil
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

// recordRtLabels writes the client's owed labels to the cache (an empty
// list when there is none, so a settled label does not come back).
func (p *poller) recordRtLabels(ctx context.Context) {
	p.mu.Lock()
	cli := p.rt
	p.mu.Unlock()
	if cli == nil {
		return
	}
	c := rtLabelsCache{Generation: boxGeneration(p.box), Labels: cli.Pending()}
	if err := p.m.st.SetCache(ctx, p.box.ID, rtLabelsCacheKind, c); err != nil {
		p.m.log.Warn("cache rtorrent labels", "box", p.box.Name, "err", err)
	}
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
		Version: st.Rt.Version, Library: st.Rt.Library}
	rs.Endpoint = clip(rtEndpoint(rtBox{p.box.RtSCGI}, snap), 600)
	defer func() { st.Rt = rs }()
	if known && !running {
		rs.Error = "rtorrent is not running"
		if rs.Disabled {
			rs.Error += " (~/.rtorrentDisable is set)"
		}
		return
	}
	if rs.Endpoint == "" {
		rs.Error = "no SCGI endpoint: the probe could not read the home path or ~/.rtorrent.rc"
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
	cli, err := p.rtClient(sshc, rs.Endpoint)
	if err != nil {
		rs.Error = describeErr(err)
		return
	}
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	stats, err := cli.Stats(actx)
	if err != nil {
		rs.Error = describeErr(err)
		return
	}
	rs.Version, rs.Library = clip(stats.Version, 32), clip(stats.Library, 32)
	rs.ListenPort, rs.DHT = stats.ListenPort, stats.DHT
	list, err := cli.Torrents(actx)
	if err != nil {
		rs.Error = describeErr(err)
		return
	}
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

// rtRemoveTimeout bounds the rm of a torrent's data on the box: a large
// tree on a slow disk takes a while, and the API handler allows 90 s.
const rtRemoveTimeout = 75 * time.Second

// RtActionRemoveData is the one torrent action rTorrent has no command
// for: remove the torrent, then delete its data. See rtRemoveData.
const RtActionRemoveData = "remove-data"

// RtTorrentActions is the set the API accepts for rTorrent.
var RtTorrentActions = map[string]bool{rtorrent.ActionStop: true, rtorrent.ActionStart: true, rtorrent.ActionRemove: true, RtActionRemoveData: true}

// rtReady returns the client after the checks that precede any rTorrent
// action: monitoring on, the box probed by this poller, the SSH connection
// up and, for a loopback endpoint, the port verified as this account's
// right now.
func (p *poller) rtReady(ctx context.Context) (*rtorrent.Client, *sshx.Client, error) {
	if !p.box.RtEnabled {
		return nil, nil, errors.New("rTorrent is not enabled for this box")
	}
	st, err := p.readyState()
	if err != nil {
		return nil, nil, err
	}
	if st.Rt.Endpoint == "" {
		return nil, nil, errors.New("no SCGI endpoint known for rTorrent yet")
	}
	var sshc *sshx.Client
	if p.dialRt == nil {
		if sshc, err = p.conn(ctx); err != nil {
			return nil, nil, err
		}
	}
	if strings.HasPrefix(st.Rt.Endpoint, "tcp:") {
		if !st.Rt.PortOK {
			return nil, nil, errors.New("the SCGI port has not been verified as owned by this account yet")
		}
		_, portStr, _ := strings.Cut(st.Rt.Endpoint[len("tcp:"):], ":")
		port, _ := strconv.Atoi(portStr)
		if sshc != nil { // nil only under the test hook, which has no box to ask
			if err := verifyPortNow(ctx, sshc, port); err != nil {
				return nil, nil, err
			}
		}
	}
	cli, err := p.rtClient(sshc, st.Rt.Endpoint)
	if err != nil {
		return nil, nil, err
	}
	return cli, sshc, nil
}

// rtTorrentAction runs one RtTorrentActions entry on hash.
func (p *poller) rtTorrentAction(ctx context.Context, hash, action string) error {
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	cli, sshc, err := p.rtReady(actx)
	if err != nil {
		return err
	}
	if action == RtActionRemoveData {
		// The caller's context: the rm inside gets its own, longer deadline
		// than the RPC phase (a cut rm leaves a half-deleted tree behind an
		// already erased torrent).
		return rtRemoveData(ctx, cli, sshc, hash)
	}
	return cli.Action(actx, action, hash)
}

// rtRemoveData is "remove torrent and data" for a client that has no such
// command (ruTorrent does the deletion itself in PHP). The dashboard asks
// rTorrent for the torrent's base path and its download root, erases the
// torrent, then runs rm on the box with BOTH paths checked there, after
// symlink resolution: the root must be strictly inside the home and the
// path strictly inside the root. A torrent client is not trusted with a
// path; the box's own view of the filesystem is what decides.
//
// ctx is the caller's: the RPC calls run under apiTimeout within it, the
// rm under rtRemoveTimeout (Codex on PR 123: a deletion capped by the RPC
// deadline could be cut mid-way).
func rtRemoveData(ctx context.Context, cli *rtorrent.Client, sshc *sshx.Client, hash string) error {
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	base, err := cli.BasePath(actx, hash)
	if err != nil {
		return err
	}
	root, err := cli.DefaultDirectory(actx)
	if err != nil {
		return err
	}
	// Refused here, before the erase, so a torrent whose path cannot even
	// be handed to the box is not removed either: absolute, bounded, no
	// line breaks (the box's own check is the authority on what is inside).
	for _, s := range []string{base, root} {
		if !strings.HasPrefix(s, "/") || len(s) > 4096 || strings.ContainsAny(s, "\x00\n\r") {
			return errors.New("rTorrent reported a path that cannot be checked; not deleting anything")
		}
	}
	if sshc == nil {
		return errors.New("no SSH connection to the box")
	}
	if err := cli.Action(actx, rtorrent.ActionRemove, hash); err != nil {
		return err
	}
	rctx, rcancel := context.WithTimeout(ctx, rtRemoveTimeout)
	defer rcancel()
	// The paths ride as quoted assignments ahead of the script (the shell
	// may buffer stdin past the script, so they cannot follow it as data).
	out, stderr, err := sshc.Run(rctx, "sh", strings.NewReader("root="+shellQuote(root)+"\ntarget="+shellQuote(base)+"\n"+rtRemoveDataScript))
	if err != nil {
		msg := strings.TrimSpace(string(out) + " " + string(stderr))
		return fmt.Errorf("the torrent was removed from rTorrent but its files were not deleted: %s", clip(msg, 300))
	}
	return nil
}

// rtRemoveDataScript takes the download root and the target path in $root
// and $target (shell-quoted assignments prepended by the caller), resolves
// both through symlinks and deletes the target only when root ⊂ $HOME and
// target ⊂ root, strictly. Any other case prints why and exits non-zero
// without touching the filesystem. Only rm -rf's own exit status is
// non-zero after a deletion began.
const rtRemoveDataScript = `LC_ALL=C; export LC_ALL
[ -n "$root" ] || { echo "no root"; exit 3; }
[ -n "$target" ] || { echo "no target"; exit 3; }
home=$(realpath -- "$HOME" 2>/dev/null) || { echo "cannot resolve HOME"; exit 4; }
r=$(realpath -e -- "$root" 2>/dev/null) || { echo "download root does not exist"; exit 4; }
t=$(realpath -e -- "$target" 2>/dev/null) || { echo "path does not exist (already deleted?)"; exit 5; }
case "$home" in /) echo "HOME is /"; exit 4;; esac
case "$r" in "$home"/?*) ;; *) echo "refusing: download root $r is not inside the home"; exit 6;; esac
case "$t" in "$r"/?*) ;; *) echo "refusing: $t is not inside the download root $r"; exit 6;; esac
rm -rf -- "$t"
`

// rtAddTorrents loads the request into rTorrent; the API shares its
// request shape with qBittorrent's.
func (p *poller) rtAddTorrents(ctx context.Context, req qbt.AddRequest) (rtorrent.AddResult, error) {
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	cli, _, err := p.rtReady(actx)
	if err != nil {
		return rtorrent.AddResult{}, err
	}
	r := rtorrent.AddRequest{URLs: req.URLs, Label: req.Category, Stopped: req.Stopped}
	for _, f := range req.Files {
		r.Files = append(r.Files, f.Data)
	}
	return cli.Add(actx, r)
}
