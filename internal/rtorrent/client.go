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

package rtorrent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Client talks to one rTorrent instance. Calls are serialised: rTorrent
// serves RPC on its single main loop, and one slow answer must not turn
// into a pile of open channels on the SSH transport.
type Client struct {
	dial     Dialer
	timeout  time.Duration
	maxBytes int64         // response byte cap, maxResponse outside tests
	maxRows  int           // torrent list cap, MaxTorrents outside tests
	calls    chan struct{} // capacity 1: the call token

	// pending are labels set on a magnet's metadata placeholder that must
	// be set again on the torrent rTorrent builds once the metadata
	// arrives (see Add); Torrents reconciles them. Guarded by pendMu.
	pendMu  sync.Mutex
	pending map[string]pendingLabel
	now     func() time.Time
}

// pendingLabel is a label owed to a hash until it is seen set or expires.
type pendingLabel struct {
	label string
	until time.Time
}

// Bounds on pending labels: a magnet whose metadata never arrives would
// otherwise hold its entry forever, and a flood of adds must not grow the
// map without limit.
const (
	pendingTTL = 24 * time.Hour
	pendingMax = 1000
)

// New returns a client over dial.
func New(dial Dialer) *Client {
	c := &Client{dial: dial, timeout: callTimeout, maxBytes: maxResponse, maxRows: MaxTorrents, calls: make(chan struct{}, 1), pending: map[string]pendingLabel{}, now: time.Now}
	c.calls <- struct{}{}
	return c
}

// MaxTorrents is the most torrents one list may hold; a longer list is an
// error (the view would be incomplete and the memory unbounded).
const MaxTorrents = 20000

// Torrent is one row of the main view as rTorrent reports it. Strings are
// clipped on the way in; the hash is upper-case hex as rTorrent uses it.
type Torrent struct {
	Hash      string
	Name      string
	Size      int64 // d.size_bytes
	Completed int64 // d.completed_bytes
	DownRate  int64 // bytes/s
	UpRate    int64
	Ratio     int64 // per mille
	Started   bool  // d.state: 1 started (possibly paused), 0 stopped
	Complete  bool
	Active    bool  // started and not paused
	Hashing   int64 // 0 none, 1 first check, 2 on completion, 3 rehash
	Open      bool
	Message   string // tracker/hash error text, empty when fine
	Label     string // d.custom1, ruTorrent's label
	Finished  int64  // epoch, 0 while incomplete
	Peers     int64  // connected peers
	Seeds     int64  // connected seeds (d.peers_complete)
	UpTotal   int64
	DownTotal int64
	Private   bool
}

// Clips for retained strings: a hostile client cannot grow the state.
const (
	maxName    = 400
	maxMessage = 200
	maxLabel   = 100
	maxVersion = 32
	maxPath    = 4096
)

// torrentFields are the d.multicall2 getters, in Torrent field order.
var torrentFields = []any{
	"d.hash=", "d.name=", "d.size_bytes=", "d.completed_bytes=", "d.down.rate=", "d.up.rate=",
	"d.ratio=", "d.state=", "d.complete=", "d.is_active=", "d.hashing=", "d.is_open=",
	"d.message=", "d.custom1=", "d.timestamp.finished=", "d.peers_connected=", "d.peers_complete=",
	"d.up.total=", "d.down.total=", "d.is_private=",
}

// Stats is the client-wide picture.
type Stats struct {
	Version    string // system.client_version, e.g. "0.9.8" or "0.16.25"
	Library    string // system.library_version
	DownBPS    int64
	UpBPS      int64
	ListenPort int
	DHT        bool
}

// Actions on one torrent. Stop and Start mirror ruTorrent: stop then
// close, open then start. Remove erases the torrent from the session and
// leaves its data alone: rTorrent has no delete-files command, ruTorrent
// does that itself in PHP, and this dashboard does not run rm on a path a
// torrent client handed it.
const (
	ActionStop   = "stop"
	ActionStart  = "start"
	ActionRemove = "remove"
)

// TorrentActions is the set Action accepts.
var TorrentActions = map[string]bool{ActionStop: true, ActionStart: true, ActionRemove: true}

var (
	// ErrBadAction is an action outside TorrentActions.
	ErrBadAction = errors.New("rtorrent: unknown torrent action")
	// ErrBadHash is a hash that is not 40 hex characters.
	ErrBadHash = errors.New("rtorrent: invalid torrent hash")
	// ErrUnexpected is a well-formed answer of the wrong shape (a string
	// where a list was asked for): the peer is not behaving like rTorrent.
	ErrUnexpected = errors.New("rtorrent: unexpected response shape")
)

// ValidHash reports whether h is a 40-character hex info hash.
func ValidHash(h string) bool {
	if len(h) != 40 {
		return false
	}
	for i := 0; i < len(h); i++ {
		c := h[i]
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F') {
			return false
		}
	}
	return true
}

// call performs one XML-RPC method call. Every rTorrent command takes a
// target as its first parameter ("" for global commands, the hash for d.*);
// 0.16 refuses calls without one, so callers always pass it.
func (c *Client) call(ctx context.Context, maxRows int, method string, params ...any) (Value, error) {
	body, err := encodeCall(method, params...)
	if err != nil {
		return Value{}, err
	}
	select {
	case <-c.calls:
	case <-ctx.Done():
		return Value{}, ctx.Err()
	}
	defer func() { c.calls <- struct{}{} }()
	r, rctx, done, err := roundTrip(ctx, c.dial, c.timeout, c.maxBytes, body)
	if err != nil {
		return Value{}, err
	}
	v, err := decodeResponse(r, maxRows)
	// Against the round trip's own context: its deadline, not only the
	// caller's, is what cuts a stalled body, and the cut then decodes as
	// malformed. Checked before done() cancels that context.
	err = deadlineErr(rctx, err)
	done()
	if err != nil {
		return Value{}, err
	}
	return v, nil
}

// BasePath returns d.base_path for hash: the directory (or single file)
// rTorrent writes the torrent's data to. Remove-with-data checks it on the
// box before deleting anything; see collect.
func (c *Client) BasePath(ctx context.Context, hash string) (string, error) {
	if !ValidHash(hash) {
		return "", ErrBadHash
	}
	v, err := c.call(ctx, 0, "d.base_path", strings.ToUpper(hash))
	if err != nil {
		return "", err
	}
	if v.Kind != KindString {
		return "", ErrUnexpected
	}
	return clip(v.Str, maxPath), nil
}

// DefaultDirectory returns directory.default, the download root
// (~/data on PMSS).
func (c *Client) DefaultDirectory(ctx context.Context) (string, error) {
	v, err := c.call(ctx, 0, "directory.default", "")
	if err != nil {
		return "", err
	}
	if v.Kind != KindString {
		return "", ErrUnexpected
	}
	return clip(v.Str, maxPath), nil
}

// Version returns system.client_version.
func (c *Client) Version(ctx context.Context) (string, error) {
	v, err := c.call(ctx, 0, "system.client_version", "")
	if err != nil {
		return "", err
	}
	if v.Kind != KindString {
		return "", ErrUnexpected
	}
	return clip(v.Str, maxVersion), nil
}

// Stats fetches the client-wide numbers in one system.multicall.
func (c *Client) Stats(ctx context.Context) (*Stats, error) {
	calls := []any{
		mc("system.client_version"), mc("system.library_version"),
		mc("throttle.global_down.rate"), mc("throttle.global_up.rate"),
		mc("network.listen.port"), mc("dht.statistics"),
	}
	v, err := c.call(ctx, 0, "system.multicall", calls)
	if err != nil {
		return nil, err
	}
	// Each result is a one-element array holding the value, or a fault
	// struct; a fault in one member leaves that field zero.
	if v.Kind != KindArray || len(v.Array) != len(calls) {
		return nil, ErrUnexpected
	}
	one := func(i int) Value {
		e := v.Array[i]
		if e.Kind == KindArray && len(e.Array) == 1 {
			return e.Array[0]
		}
		return Value{}
	}
	st := &Stats{
		Version: clip(one(0).Str, maxVersion),
		Library: clip(one(1).Str, maxVersion),
		DownBPS: nonNeg(one(2).Int),
		UpBPS:   nonNeg(one(3).Int),
	}
	if p := one(4).Int; p > 0 && p <= 65535 {
		st.ListenPort = int(p)
	}
	if d := one(5); d.Kind == KindStruct {
		// dht.statistics: "active" says whether DHT is running now; the
		// "dht" mode string has disable/off/auto/on and would read "off" as
		// on (Codex on PR 122).
		st.DHT = d.Struct["active"].Int == 1
	}
	return st, nil
}

// mc is one system.multicall member: method with the empty target.
func mc(method string) map[string]any {
	return map[string]any{"methodName": method, "params": []any{""}}
}

// Torrents lists the main view, then settles any label still owed to a
// magnet whose torrent has meanwhile been rebuilt from its metadata (see
// Add): a listed torrent with an empty label and a pending entry gets the
// label set, by hash, as a literal call; the rows returned already carry it.
func (c *Client) Torrents(ctx context.Context) ([]Torrent, error) {
	out, err := c.list(ctx)
	if err != nil {
		return nil, err
	}
	c.reconcileLabels(ctx, out)
	return out, nil
}

func (c *Client) list(ctx context.Context) ([]Torrent, error) {
	params := append([]any{"", "main"}, torrentFields...)
	v, err := c.call(ctx, c.maxRows, "d.multicall2", params...)
	if err != nil {
		return nil, err
	}
	if v.Kind != KindArray {
		return nil, ErrUnexpected
	}
	out := make([]Torrent, 0, len(v.Array))
	for _, row := range v.Array {
		if row.Kind != KindArray || len(row.Array) != len(torrentFields) {
			return nil, ErrUnexpected
		}
		r := row.Array
		h := strings.ToUpper(r[0].Str)
		if !ValidHash(h) {
			continue // not a torrent row; whatever it is, it gets no buttons and no slot
		}
		out = append(out, Torrent{
			Hash: h, Name: clip(r[1].Str, maxName),
			Size: nonNeg(r[2].Int), Completed: nonNeg(r[3].Int),
			DownRate: nonNeg(r[4].Int), UpRate: nonNeg(r[5].Int), Ratio: nonNeg(r[6].Int),
			Started: r[7].Int == 1, Complete: r[8].Int == 1, Active: r[9].Int == 1,
			Hashing: nonNeg(r[10].Int), Open: r[11].Int == 1,
			Message: clip(r[12].Str, maxMessage), Label: clip(r[13].Str, maxLabel),
			Finished: nonNeg(r[14].Int), Peers: nonNeg(r[15].Int), Seeds: nonNeg(r[16].Int),
			UpTotal: nonNeg(r[17].Int), DownTotal: nonNeg(r[18].Int), Private: r[19].Int == 1,
		})
	}
	return out, nil
}

// reconcileLabels applies pending labels to the listed torrents that lost
// theirs (rTorrent's process_meta_download builds a fresh download when a
// magnet's metadata arrives and does not copy custom1: fred and Codex on
// PR 122). An entry settles once the label is seen on a torrent that has
// its metadata (a size: the placeholder has none, and showing the label
// there proves nothing), or expires after pendingTTL.
func (c *Client) reconcileLabels(ctx context.Context, rows []Torrent) {
	c.pendMu.Lock()
	if len(c.pending) == 0 {
		c.pendMu.Unlock()
		return
	}
	now := c.now()
	type todo struct{ hash, label string }
	var set []todo
	seen := map[string]int{}
	for i, r := range rows {
		seen[r.Hash] = i
	}
	for h, p := range c.pending {
		if now.After(p.until) {
			delete(c.pending, h)
			continue
		}
		i, ok := seen[h]
		if !ok {
			continue // not listed yet (or removed): keep until it expires
		}
		if rows[i].Label == p.label && rows[i].Size > 0 {
			delete(c.pending, h) // settled on the real torrent
			continue
		}
		if rows[i].Label == "" {
			set = append(set, todo{h, p.label})
		}
	}
	c.pendMu.Unlock()
	for _, t := range set {
		if _, err := c.call(ctx, 0, "d.custom1.set", t.hash, t.label); err != nil {
			continue // tried again next poll; expiry bounds it
		}
		if i, ok := seen[t.hash]; ok {
			rows[i].Label = t.label
		}
	}
}

// remember records a label owed to hash (a magnet's). The oldest entry
// goes when the map is full.
func (c *Client) remember(hash, label string) {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	if len(c.pending) >= pendingMax {
		var oldest string
		var at time.Time
		for h, p := range c.pending {
			if oldest == "" || p.until.Before(at) {
				oldest, at = h, p.until
			}
		}
		delete(c.pending, oldest)
	}
	c.pending[hash] = pendingLabel{label: label, until: c.now().Add(pendingTTL)}
}

// Action runs one of TorrentActions on hash.
func (c *Client) Action(ctx context.Context, action, hash string) error {
	if !TorrentActions[action] {
		return ErrBadAction
	}
	if !ValidHash(hash) {
		return ErrBadHash
	}
	hash = strings.ToUpper(hash)
	var seq []string
	switch action {
	case ActionStop:
		seq = []string{"d.stop", "d.close"}
	case ActionStart:
		seq = []string{"d.open", "d.start"}
	case ActionRemove:
		seq = []string{"d.erase"}
	}
	for _, m := range seq {
		if _, err := c.call(ctx, 0, m, hash); err != nil {
			return err
		}
	}
	return nil
}

// AddRequest is a batch of torrents to load: URLs (magnet or http(s), which
// rTorrent fetches itself) and .torrent file contents. Label sets
// d.custom1 (ruTorrent's label) on each; Stopped loads without starting.
//
// The label is never passed along the load command: those trailing
// arguments go through rTorrent's command parser, which evaluates a leading
// "(" as a function and a leading "$" as a command even inside quotes
// (confirmed live on 0.9.8: the label "$system.client_version=" came back
// as "0.9.8"). Instead the torrent is loaded first and d.custom1.set is
// called by hash as its own XML-RPC method, whose parameters are literal
// values. That needs the hash up front, so a label is applied to .torrent
// files (hash computed here) and magnet links (hash in the link); an http(s)
// link loads without the label (the hash is not known until rTorrent
// fetched the file) and is counted in AddResult.Unlabelled.
type AddRequest struct {
	URLs    []string
	Files   [][]byte
	Label   string
	Stopped bool
}

// AddResult counts what was accepted. rTorrent answers a load command
// before it knows whether the torrent is valid, so Added is what it took,
// not what it will keep. Unlabelled counts loaded torrents whose label
// could not then be set (rTorrent refused the d.custom1.set).
type AddResult struct {
	Added      int
	Failed     int
	Unlabelled int
}

// Add loads every entry; one refused entry does not stop the rest.
func (c *Client) Add(ctx context.Context, req AddRequest) (AddResult, error) {
	var res AddResult
	label := clip(req.Label, maxLabel)
	type entry struct {
		method     string
		arg        any    // the URL or the file bytes
		hash       string // for the label; "" when none is wanted or known
		unlabelled bool   // a label was asked for but cannot be set by hash
		magnet     bool   // the label must be set again once the metadata arrives
	}
	urlMethod, rawMethod := "load.start", "load.raw_start"
	if req.Stopped {
		urlMethod, rawMethod = "load.normal", "load.raw"
	}
	var entries []entry
	for _, u := range req.URLs {
		if !validURL(u) {
			res.Failed++
			continue
		}
		e := entry{method: urlMethod, arg: u}
		if label != "" {
			if h, err := MagnetHash(u); err == nil {
				e.hash, e.magnet = h, true
			} else {
				e.unlabelled = true // an http(s) link: loaded, label left to ruTorrent
			}
		}
		entries = append(entries, e)
	}
	for _, f := range req.Files {
		e := entry{method: rawMethod, arg: f}
		if label != "" {
			h, err := InfoHash(f)
			if err != nil {
				res.Failed++ // not a torrent: rTorrent would refuse it too
				continue
			}
			e.hash = h
		}
		entries = append(entries, e)
	}
	var lastErr error
	for _, e := range entries {
		if _, err := c.call(ctx, 0, e.method, "", e.arg); err != nil {
			if isFault(err) {
				res.Failed++
				lastErr = err
				continue
			}
			return res, err
		}
		res.Added++
		if e.unlabelled {
			res.Unlabelled++
		}
		if e.hash != "" {
			// A literal XML-RPC string: rTorrent's command parser never sees
			// it. A fault here (the hash not present yet, which rTorrent
			// does not do for a synchronous load) leaves the torrent loaded
			// without its label and is counted.
			if _, err := c.call(ctx, 0, "d.custom1.set", e.hash, label); err != nil {
				if isFault(err) {
					res.Unlabelled++
					continue
				}
				return res, err
			}
			if e.magnet {
				// Set on the metadata placeholder only: rTorrent rebuilds the
				// torrent when the metadata arrives and drops custom1, so the
				// label is owed again then (reconciled by Torrents).
				c.remember(e.hash, label)
			}
		}
	}
	if res.Added == 0 && lastErr != nil {
		return res, lastErr
	}
	return res, nil
}

// validURL admits what rTorrent should fetch on the user's behalf: a magnet
// link or an http(s) URL. A bare path would make load.start read a file on
// the box, so paths are refused here even though the API layer checks too.
func validURL(u string) bool {
	l := strings.ToLower(u)
	return strings.HasPrefix(l, "magnet:?") || strings.HasPrefix(l, "http://") || strings.HasPrefix(l, "https://")
}

func isFault(err error) bool {
	var f *Fault
	return errors.As(err, &f)
}

func nonNeg(n int64) int64 {
	if n < 0 {
		return 0
	}
	return n
}

// Kind classifies a torrent the way the dashboard's qBittorrent rows are:
// seeding, leeching, paused, checking, error.
func (t Torrent) Kind() string {
	switch {
	case t.Hashing != 0:
		return "checking"
	case t.Message != "" && !strings.Contains(t.Message, "Tried all trackers"):
		return "error"
	case !t.Started || !t.Active:
		return "paused"
	case t.Complete:
		return "seeding"
	default:
		return "leeching"
	}
}

// State is a short human-readable state for the row.
func (t Torrent) State() string {
	switch {
	case t.Hashing != 0:
		return "checking"
	case !t.Open:
		return "closed"
	case !t.Started:
		return "stopped"
	case !t.Active:
		return "paused"
	case t.Complete:
		return "seeding"
	default:
		return "downloading"
	}
}

// String is for logs.
func (t Torrent) String() string { return fmt.Sprintf("%s %q", t.Hash, t.Name) }
