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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Client talks to one rTorrent instance. Operations are serialised as a
// whole, not call by call: rTorrent serves RPC on its single main loop, one
// slow answer must not turn into a pile of open channels on the SSH
// transport, and a two-call transition (stop = d.stop + d.close) must not
// interleave with another (start = d.open + d.start) into a state that is
// neither.
type Client struct {
	dial      Dialer
	timeout   time.Duration
	maxBytes  int64         // response byte cap, maxResponse outside tests
	maxRows   int           // torrent list cap, MaxTorrents outside tests
	maxValues int           // retained-value budget per response, maxValues outside tests
	ops       chan struct{} // capacity 1: the operation token

	// pending are labels set on a magnet's metadata placeholder that must
	// be set again on the torrent rTorrent builds once the metadata
	// arrives (see Add); Torrents reconciles them. Guarded by pendMu.
	pendMu  sync.Mutex
	pending map[string]pendingLabel
	now     func() time.Time

	// noMeta is set once a build answered the listing with d.is_meta in it
	// with an unknown-method fault (a d.multicall2 is all or nothing): later
	// listings leave the getter out and Torrent.MetaKnown is false.
	noMeta atomic.Bool
	// retired is set by Retire, under the operation token: from then on
	// every operation fails with ErrRetired, so a caller still holding this
	// client after a handoff cannot change state the successor never sees.
	retired bool
	// erased are the hashes this client removed (d.erase), with the time:
	// a barrier against a predecessor's late snapshot (Adopt) reviving a
	// label for a torrent erased since (Codex on PR 122). Bounded like
	// pending: pendingTTL, pendingMax entries, oldest out.
	erased map[string]time.Time
}

// ErrRetired is an operation on a client after Retire: the caller must
// resolve the current client and decide whether to retry.
var ErrRetired = errors.New("rtorrent: client retired; resolve the current client")

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
	c := &Client{dial: dial, timeout: callTimeout, maxBytes: maxResponse, maxRows: MaxTorrents, maxValues: maxValues, ops: make(chan struct{}, 1), pending: map[string]pendingLabel{}, now: time.Now}
	c.ops <- struct{}{}
	return c
}

// acquire takes the operation token for one public operation; release
// returns it. Internal helpers never acquire, so an operation's calls are
// one unit against every other operation on the client.
func (c *Client) acquire(ctx context.Context) (release func(), err error) {
	if c.Retired() {
		return nil, ErrRetired // no point queueing behind the operation in flight
	}
	select {
	case <-c.ops:
		// Checked again after taking the token: a caller queued behind the
		// Retire itself must see the retirement too.
		if c.Retired() {
			c.ops <- struct{}{}
			return nil, ErrRetired
		}
		return func() { c.ops <- struct{}{} }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
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
	// Meta is a magnet's metadata placeholder (d.is_meta): rTorrent replaces
	// it with the real torrent once the metadata arrives. MetaKnown is false
	// when the build did not answer d.is_meta (see list): the placeholder
	// cannot then be told from the real torrent, by size or otherwise, since
	// the placeholder grows to the advertised metadata size before it is
	// replaced (fred on PR 122).
	Meta      bool
	MetaKnown bool
}

// Clips for retained strings: a hostile client cannot grow the state.
const (
	maxName    = 400
	maxMessage = 200
	maxLabel   = 100
	maxVersion = 32
	maxPath    = 4096
)

// torrentFields are the d.multicall2 getters, in Torrent field order. The
// last one, d.is_meta, is the only one not confirmed on every rTorrent
// build PMSS tenants may run (0.9.8 answers it; 0.16 registers it too, but
// no 0.16 box was available): see list for the fallback without it.
var torrentFields = []any{
	"d.hash=", "d.name=", "d.size_bytes=", "d.completed_bytes=", "d.down.rate=", "d.up.rate=",
	"d.ratio=", "d.state=", "d.complete=", "d.is_active=", "d.hashing=", "d.is_open=",
	"d.message=", "d.custom1=", "d.timestamp.finished=", "d.peers_connected=", "d.peers_complete=",
	"d.up.total=", "d.down.total=", "d.is_private=", "d.is_meta=",
}

// metaField is the index of d.is_meta in torrentFields.
const metaField = 20

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

// ValidAction reports whether a is one of the actions Action runs. A
// closed switch rather than a map: nothing can add a key that validates
// but matches no case.
func ValidAction(a string) bool {
	switch a {
	case ActionStop, ActionStart, ActionRemove:
		return true
	}
	return false
}

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

// Per-call answer shapes: how many entries the outermost array may hold
// and how many Values the whole answer may retain. A scalar answer (a
// version, a path, a command's 0) needs neither an array nor more than a
// few values; the stats multicall has exactly six members; only the torrent
// listing needs the large ceilings (daybreak blue re-review of PR 122).
const (
	scalarRows, scalarValues = 4, 64
	statsRows, statsValues   = 6, 256
)

// call performs one XML-RPC method call under the operation the caller
// holds. Every rTorrent command takes a target as its first parameter (""
// for global commands, the hash for d.*); 0.16 refuses calls without one,
// so callers always pass it. maxRows and budget are the answer's shape.
func (c *Client) call(ctx context.Context, maxRows, budget int, method string, params ...any) (Value, error) {
	body, err := encodeCall(method, params...)
	if err != nil {
		return Value{}, err
	}
	r, rctx, done, err := roundTrip(ctx, c.dial, c.timeout, c.maxBytes, body)
	if err != nil {
		return Value{}, err
	}
	v, err := decodeResponse(r, maxRows, budget)
	// Against the round trip's own context: its deadline, not only the
	// caller's, is what cuts a stalled body, and the cut then decodes as
	// malformed. Checked before done() cancels that context.
	err = deadlineErr(rctx, err)
	done()
	if err != nil {
		return Value{}, err
	}
	// The cap is exact: a document that decoded but ran past maxBytes
	// (trailing bytes read up to the sentinel) is refused too (daybreak blue
	// re-review 5 of PR 122).
	if r.over() {
		return Value{}, ErrTooLarge
	}
	return v, nil
}

// setLabel runs d.custom1.set by hash. rTorrent registers d.custom1 as a
// string variable, and a string variable's setter answers with the string
// it assigned, not a status (fred and the daybreak blue re-review on PR
// 122: a status check here rejected every real labelled add). The answer
// must be exactly the label sent; anything else is not rTorrent's answer.
func (c *Client) setLabel(ctx context.Context, hash, label string) error {
	v, err := c.call(ctx, scalarRows, scalarValues, "d.custom1.set", hash, label)
	if err != nil {
		return err
	}
	if v.Kind != KindString || v.Str != label {
		return ErrUnexpected
	}
	return nil
}

// command runs a method whose answer is rTorrent's integer status, 0 for a
// command that ran: anything else, another integer included, is not
// rTorrent's answer to a command and must not read as success (daybreak
// blue re-review of PR 122).
func (c *Client) command(ctx context.Context, method string, params ...any) error {
	v, err := c.call(ctx, scalarRows, scalarValues, method, params...)
	if err != nil {
		return err
	}
	if v.Kind != KindInt || v.Int != 0 {
		return ErrUnexpected
	}
	return nil
}

// BasePath returns d.base_path for hash: the directory (or single file)
// rTorrent writes the torrent's data to. Remove-with-data checks it on the
// box before deleting anything; see collect.
func (c *Client) BasePath(ctx context.Context, hash string) (string, error) {
	if !ValidHash(hash) {
		return "", ErrBadHash
	}
	release, err := c.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	v, err := c.call(ctx, scalarRows, scalarValues, "d.base_path", strings.ToUpper(hash))
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
	release, err := c.acquire(ctx)
	if err != nil {
		return "", err
	}
	defer release()
	v, err := c.call(ctx, scalarRows, scalarValues, "system.client_version", "")
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
	release, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	calls := []any{
		mc("system.client_version"), mc("system.library_version"),
		mc("throttle.global_down.rate"), mc("throttle.global_up.rate"),
		mc("network.listen.port"), mc("dht.statistics"),
	}
	v, err := c.call(ctx, statsRows, statsValues, "system.multicall", calls)
	if err != nil {
		return nil, err
	}
	// Each result is a one-element array holding the value, or a fault
	// struct. Only an unknown-method fault, with its typed code and string,
	// and only for the two members a build may lack (the listen port and
	// DHT statistics) reads as "missing"; a fault on the version or the
	// rates, or a malformed fault, is an error, not a zero (daybreak blue
	// re-review of PR 122). A value of the wrong kind fails the call.
	if v.Kind != KindArray || len(v.Array) != len(calls) {
		return nil, ErrUnexpected
	}
	methods := []string{"system.client_version", "system.library_version", "throttle.global_down.rate", "throttle.global_up.rate", "network.listen.port", "dht.statistics"}
	member := func(i int, want Kind, optional bool) (Value, error) {
		e := v.Array[i]
		if e.Kind == KindStruct {
			code, str := e.Struct["faultCode"], e.Struct["faultString"]
			if code.Kind != KindInt || str.Kind != KindString {
				return Value{}, ErrUnexpected // not a fault, not a value
			}
			f := &Fault{Code: code.Int, String: clip(str.Str, 300)}
			if optional && unknownMethodFault(f, methods[i]) {
				return Value{}, nil // this method is missing on the build
			}
			return Value{}, f
		}
		if e.Kind != KindArray || len(e.Array) != 1 {
			return Value{}, ErrUnexpected
		}
		if e.Array[0].Kind != want {
			return Value{}, ErrUnexpected
		}
		return e.Array[0], nil
	}
	var st Stats
	var vals [6]Value
	for i, want := range []Kind{KindString, KindString, KindInt, KindInt, KindInt, KindStruct} {
		var err error
		if vals[i], err = member(i, want, i >= 4); err != nil {
			return nil, err
		}
	}
	st.Version, st.Library = clip(vals[0].Str, maxVersion), clip(vals[1].Str, maxVersion)
	st.DownBPS, st.UpBPS = nonNeg(vals[2].Int), nonNeg(vals[3].Int)
	if p := vals[4].Int; p > 0 && p <= 65535 {
		st.ListenPort = int(p)
	}
	if d := vals[5]; d.Kind == KindStruct {
		// dht.statistics: "active" says whether DHT is running now; the
		// "dht" mode string has disable/off/auto/on and would read "off" as
		// on (Codex on PR 122). A non-integer active is not rTorrent.
		a, ok := d.Struct["active"]
		if ok && a.Kind != KindInt {
			return nil, ErrUnexpected
		}
		st.DHT = ok && a.Int == 1
	}
	return &st, nil
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
	release, err := c.acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer release()
	out, err := c.list(ctx)
	if err != nil {
		return nil, err
	}
	c.reconcileLabels(ctx, out)
	return out, nil
}

// columnKinds are the XML-RPC kinds of torrentFields, in order: a value of
// another kind in a row is not rTorrent's answer and fails the listing
// rather than reading as zero (a false d.is_meta = 0 would otherwise spend
// a pending label on the placeholder).
var columnKinds = func() []Kind {
	k := make([]Kind, len(torrentFields))
	for i := range k {
		k[i] = KindInt
	}
	for _, i := range []int{0, 1, 12, 13} { // hash, name, message, label
		k[i] = KindString
	}
	return k
}()

func (c *Client) list(ctx context.Context) ([]Torrent, error) {
	fields := torrentFields
	if c.noMeta.Load() {
		fields = torrentFields[:metaField]
	}
	v, err := c.call(ctx, c.maxRows, c.maxValues, "d.multicall2", append([]any{"", "main"}, fields...)...)
	if err != nil && !c.noMeta.Load() && unknownMethodFault(err, "d.is_meta") {
		// A build without d.is_meta refuses the whole listing with an
		// unknown-method fault naming it: ask once more without it and
		// remember. Any other fault stands (fred on PR 122).
		c.noMeta.Store(true)
		fields = torrentFields[:metaField]
		v, err = c.call(ctx, c.maxRows, c.maxValues, "d.multicall2", append([]any{"", "main"}, fields...)...)
	}
	if err != nil {
		return nil, err
	}
	if v.Kind != KindArray {
		return nil, ErrUnexpected
	}
	out := make([]Torrent, 0, len(v.Array))
	for _, row := range v.Array {
		if row.Kind != KindArray || len(row.Array) != len(fields) {
			return nil, ErrUnexpected
		}
		r := row.Array
		for i, e := range r {
			if e.Kind != columnKinds[i] {
				return nil, ErrUnexpected
			}
		}
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
		t := &out[len(out)-1]
		if len(r) > metaField {
			t.Meta, t.MetaKnown = r[metaField].Int == 1, true
		}
	}
	return out, nil
}

// reconcileLabels applies pending labels to the listed torrents that lost
// theirs (rTorrent's process_meta_download builds a fresh download when a
// magnet's metadata arrives and does not copy custom1: fred and Codex on
// PR 122). An entry lives through the placeholder phase (d.is_meta; the
// placeholder's size is not a tell, libtorrent gives it a one-byte file).
// The first time its torrent is listed with metadata: a torrent that already
// carries a label (ours or one the user set) spends the entry untouched,
// so a label changed or cleared in ruTorrent afterwards is left alone; a
// torrent without one gets the label, and the entry is spent only once
// that set succeeded, so a failed call (deadline, dropped connection) is
// retried next poll (Codex on PR 122). Entries also expire after
// pendingTTL.
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
		if rows[i].Meta {
			// Still the placeholder. The user may have edited its label in
			// ruTorrent meanwhile: a cleared label retires the entry, a
			// changed one becomes what is owed after the rebuild.
			switch rows[i].Label {
			case p.label:
			case "":
				delete(c.pending, h)
			default:
				c.pending[h] = pendingLabel{label: rows[i].Label, until: p.until}
			}
			continue
		}
		if rows[i].Label != "" {
			if rows[i].MetaKnown {
				delete(c.pending, h) // the real torrent carries a label: nothing owed
			}
			// Unknown whether this is still the placeholder (a build without
			// d.is_meta): keep the entry until the label is seen missing,
			// which is the rebuild, or it expires; a label the user clears
			// in ruTorrent meanwhile is set once more on such a build.
			continue
		}
		set = append(set, todo{h, p.label})
	}
	c.pendMu.Unlock()
	for _, t := range set {
		if err := c.setLabel(ctx, t.hash, t.label); err != nil {
			continue // kept: tried again next poll; expiry bounds it
		}
		c.pendMu.Lock()
		delete(c.pending, t.hash) // spent on success only
		c.pendMu.Unlock()
		if i, ok := seen[t.hash]; ok {
			rows[i].Label = t.label
		}
	}
}

// PendingLabel is a label owed to a magnet's torrent until its metadata
// arrives, as persisted by the caller across restarts (see Pending and
// Restore): the client's memory is otherwise the process's.
type PendingLabel struct {
	Hash  string    `json:"hash"`
	Label string    `json:"label"`
	Until time.Time `json:"until"`
}

// Retire ends the client: it marks the client retired, waits for the
// operation in flight, if any, to complete its remote and local steps, and
// returns the labels still owed so the caller can hand them to the next
// client. Without the wait, a reconnect could snapshot between
// an Add's remote label setter and its remember (losing the obligation) or
// between a remove's erase and its forget (keeping a dead one); without the
// terminal mark, a caller still holding the client could change state
// after the snapshot (daybreak blue re-reviews of PR 122/123). Every later
// error-returning operation fails with ErrRetired, and Restore and Adopt
// take nothing (they return no error; the caller resolves the current
// client). Retire itself is
// idempotent and returns the snapshot again.
func (c *Client) Retire(ctx context.Context) ([]PendingLabel, error) {
	// Retired first, then the wait: should the wait give up, the client is
	// still closed to new operations, the one in flight completes, and a
	// later Retire collects the final snapshot.
	c.pendMu.Lock()
	c.retired = true
	c.pendMu.Unlock()
	select {
	case <-c.ops:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	defer func() { c.ops <- struct{}{} }()
	return c.Pending(), nil
}

// Adopt adds owed labels arriving late from a retired predecessor (its
// Retire outlived the handoff's wait): only hashes this client does not
// owe yet, so a label set since is never overwritten, and none this client
// erased since, so a snapshot older than the erase cannot revive the label
// (Codex on PR 122); a retired client takes nothing. The caller is
// expected to apply the same barrier across clients (an erase through a
// predecessor of this one is not known here).
func (c *Client) Adopt(labels []PendingLabel) {
	now := c.now()
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	if c.retired {
		return
	}
	for _, l := range labels {
		h := strings.ToUpper(l.Hash)
		if !ValidHash(l.Hash) || l.Label == "" || !now.Before(l.Until) {
			continue
		}
		if _, owed := c.pending[h]; owed {
			continue
		}
		if c.erasedLocked(h) {
			continue
		}
		c.setPendingLocked(h, pendingLabel{label: clip(l.Label, maxLabel), until: l.Until})
	}
}

// Retired reports whether Retire has run.
func (c *Client) Retired() bool {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	return c.retired
}

// Pending returns the labels still owed, for the caller to persist.
func (c *Client) Pending() []PendingLabel {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	out := make([]PendingLabel, 0, len(c.pending))
	for h, p := range c.pending {
		out = append(out, PendingLabel{Hash: h, Label: p.label, Until: p.until})
	}
	return out
}

// Restore adds persisted pending labels (expired ones and malformed hashes
// are dropped; the cap applies to new entries, an existing hash is updated
// regardless). A retired client takes nothing.
func (c *Client) Restore(labels []PendingLabel) {
	now := c.now()
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	if c.retired {
		return
	}
	for _, l := range labels {
		if !ValidHash(l.Hash) || l.Label == "" || !now.Before(l.Until) {
			continue
		}
		c.setPendingLocked(strings.ToUpper(l.Hash), pendingLabel{label: clip(l.Label, maxLabel), until: l.Until})
	}
}

// remember records a label owed to hash (a magnet's).
func (c *Client) remember(hash, label string) {
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	c.setPendingLocked(strings.ToUpper(hash), pendingLabel{label: label, until: c.now().Add(pendingTTL)})
}

// setPendingLocked stores an entry; only a NEW hash at capacity evicts the
// entry nearest expiry (updating an existing one at capacity must not drop
// an unrelated label; daybreak blue re-review of PR 122). pendMu held.
func (c *Client) setPendingLocked(hash string, p pendingLabel) {
	if _, exists := c.pending[hash]; !exists && len(c.pending) >= pendingMax {
		var oldest string
		var at time.Time
		for h, e := range c.pending {
			if oldest == "" || e.until.Before(at) {
				oldest, at = h, e.until
			}
		}
		delete(c.pending, oldest)
	}
	c.pending[hash] = p
}

// Action runs one of the valid actions (see ValidAction) on hash.
func (c *Client) Action(ctx context.Context, action, hash string) error {
	if !ValidAction(action) {
		return ErrBadAction
	}
	if !ValidHash(hash) {
		return ErrBadHash
	}
	release, err := c.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
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
		if err := c.command(ctx, m, hash); err != nil {
			return err
		}
	}
	if action == ActionRemove {
		c.eraseLocked(hash) // a removed magnet owes nothing any more, now or from a late snapshot (Codex on PR 122)
	}
	return nil
}

// eraseLocked forgets hash and records the erase as a barrier for Adopt.
func (c *Client) eraseLocked(hash string) {
	hash = strings.ToUpper(hash)
	now := c.now()
	c.pendMu.Lock()
	defer c.pendMu.Unlock()
	delete(c.pending, hash)
	if c.erased == nil {
		c.erased = map[string]time.Time{}
	}
	if _, ok := c.erased[hash]; !ok && len(c.erased) >= pendingMax {
		var oldest string
		for h, t := range c.erased {
			if now.Sub(t) > pendingTTL {
				delete(c.erased, h)
				continue
			}
			if oldest == "" || t.Before(c.erased[oldest]) {
				oldest = h
			}
		}
		if len(c.erased) >= pendingMax && oldest != "" {
			delete(c.erased, oldest)
		}
	}
	c.erased[hash] = now
}

// erasedLocked reports whether hash was erased through this client within
// pendingTTL (the longest a late snapshot could still owe it a label).
func (c *Client) erasedLocked(hash string) bool {
	t, ok := c.erased[hash]
	return ok && c.now().Sub(t) <= pendingTTL
}

// Forget drops any label still owed to hash: for a torrent removed through
// another client over the same box, so a later add of the same hash
// without a label does not inherit the old one. A public operation like
// the others, it waits for the operation token, so it cannot slip between
// an Add's label setter and its remember (daybreak blue re-review of PR
// 122): whichever of the two runs later decides.
func (c *Client) Forget(ctx context.Context, hash string) error {
	release, err := c.acquire(ctx)
	if err != nil {
		return err
	}
	defer release()
	// A Forget means the torrent was removed through another path (another
	// client, a detached job): the same barrier applies, or a late
	// predecessor snapshot could restore the label (Codex on PR 122).
	c.eraseLocked(hash)
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
	// Deferred counts magnets whose label is set now on the placeholder and
	// again once the metadata arrives (see reconcileLabels).
	Deferred int
}

// Add loads every entry; one refused entry does not stop the rest.
func (c *Client) Add(ctx context.Context, req AddRequest) (AddResult, error) {
	var res AddResult
	release, err := c.acquire(ctx)
	if err != nil {
		return res, err
	}
	defer release()
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
		if err := c.command(ctx, e.method, "", e.arg); err != nil {
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
			if err := c.setLabel(ctx, e.hash, label); err != nil {
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
				res.Deferred++
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

// unknownMethodFault reports whether err is rTorrent saying it does not know
// method: xmlrpc-c's "Method 'x' not defined" (code -506) on 0.9.8, "Command
// "x" does not exist" or "Unsupported" style texts on other builds. The
// method name must appear, so a fault about anything else is not mistaken
// for it.
func unknownMethodFault(err error, method string) bool {
	var f *Fault
	if !errors.As(err, &f) {
		return false
	}
	msg := strings.ToLower(f.String)
	if !strings.Contains(msg, strings.ToLower(method)) {
		return false
	}
	return f.Code == -506 || strings.Contains(msg, "not defined") || strings.Contains(msg, "does not exist") ||
		strings.Contains(msg, "unsupported") || strings.Contains(msg, "unknown") || strings.Contains(msg, "not found")
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
