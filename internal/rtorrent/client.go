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

	// stateMu guards retired.
	stateMu sync.Mutex

	// retired is set by Retire, under the operation token: from then on
	// every operation fails with ErrRetired, so a caller still holding this
	// client after a handoff cannot change state the successor never sees.
	retired bool
}

// ErrRetired is an operation on a client after Retire: the caller must
// resolve the current client and decide whether to retry. Nothing was sent.
var ErrRetired = errors.New("rtorrent: client retired; resolve the current client")

// errNotSent marks a call that failed before any byte of the request was
// written (the dial failed): the command did not reach rTorrent.
var errNotSent = errors.New("rtorrent: request not sent")

// maxLabelErrors bounds AddResult.LabelErrors; LabelErrorsOmitted counts
// the rest.
const maxLabelErrors = 8

// New returns a client over dial.
func New(dial Dialer) *Client {
	c := &Client{dial: dial, timeout: callTimeout, maxBytes: maxResponse, maxRows: MaxTorrents, maxValues: maxValues, ops: make(chan struct{}, 1)}
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
}

// Clips for retained strings: a hostile client cannot grow the state.
const (
	maxName    = 400
	maxMessage = 200
	maxLabel   = 100
	maxVersion = 32
	maxPath    = 4096
)

// torrentFields are the d.multicall2 getters, in Torrent field order, all
// present on every rTorrent build PMSS tenants run (0.9.8 and 0.16).
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
// leaves its data alone: rTorrent has no delete-files command (ruTorrent
// does that itself in PHP). The collector's remove-with-data uses the path
// rTorrent reports only as a candidate, checked on the box against a
// download root it reads itself, never as the authority on what to delete.
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
	over := r.over()
	done()
	// The cap is exact: a document that decoded, as a value or as a fault,
	// but ran past maxBytes (trailing bytes read up to the sentinel) is
	// refused, so an oversized fault cannot steer a caller (daybreak blue
	// re-review 5 of PR 122, fred). A deadline keeps its own attribution.
	if over && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, context.Canceled) {
		return Value{}, ErrTooLarge
	}
	if err != nil {
		return Value{}, err
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

// Torrents lists the main view. It changes nothing on the box.
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
	return out, nil
}

// columnKinds are the XML-RPC kinds of torrentFields, in order: a value of
// another kind in a row is not rTorrent's answer and fails the listing
// rather than reading as zero.
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
	v, err := c.call(ctx, c.maxRows, c.maxValues, "d.multicall2", append([]any{"", "main"}, fields...)...)
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
	}
	return out, nil
}

// Retire ends the client: it marks the client retired, then waits for the
// operation in flight, if any, to complete its remote and local steps, so a
// replacement never overlaps it on the box. Every later error-returning
// operation fails with ErrRetired. Idempotent; honours ctx
// while an operation holds the token, and a timed-out Retire still retires.
func (c *Client) Retire(ctx context.Context) error {
	c.stateMu.Lock()
	c.retired = true
	c.stateMu.Unlock()
	select {
	case <-c.ops:
	case <-ctx.Done():
		return ctx.Err()
	}
	c.ops <- struct{}{}
	return nil
}

// Retired reports whether Retire has run.
func (c *Client) Retired() bool {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()
	return c.retired
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
// could not be applied or confirmed: rTorrent refused the d.custom1.set,
// or its answer was lost, in which case the label may well be set.
type AddResult struct {
	Added      int
	Failed     int
	Unlabelled int
	// LabelErrors says why labels were not applied or confirmed, one entry
	// per such torrent up to maxLabelErrors (LabelErrorsOmitted counts the
	// rest), so the caller can log it. Each is a fixed reason built here: a
	// hash prefix where there is one and a category with any fault code,
	// never the label, a URL, torrent contents or the peer's own fault text.
	LabelErrors        []string
	LabelErrorsOmitted int
	// Uncertain is 1 when the batch stopped on a load that was sent but got
	// no usable answer (a lost reply, a cut connection): that entry may or
	// may not have been loaded. A load that never left (the dial failed) is
	// not uncertain (daybreak blue re-review of PR 122). Set alongside the
	// error Add returns.
	Uncertain int
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
				e.hash = h
			} else {
				e.unlabelled = true // no single proven hash (an http(s) link, or an ambiguous magnet)
			}
		}
		entries = append(entries, e)
	}
	for _, f := range req.Files {
		// Loaded whatever the label: an optional label never decides
		// whether the torrent is added (daybreak blue re-review of PR 122).
		// rTorrent judges the file; the label is set only on a hash proven
		// unique here.
		e := entry{method: rawMethod, arg: f}
		if label != "" {
			if h, err := InfoHash(f); err == nil {
				e.hash = h
			} else {
				e.unlabelled = true
			}
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
			if !errors.Is(err, errNotSent) {
				res.Uncertain = 1
			}
			return res, err
		}
		res.Added++
		if e.unlabelled {
			res.Unlabelled++
			res.labelError("entry", "not attempted: no usable v1 info hash to set it by (an http(s) link, or a magnet without one)")
		}
		if e.hash != "" {
			// A literal XML-RPC string: rTorrent's command parser never sees
			// it. The torrent is loaded whatever happens here: a fault (the
			// hash not present yet, which rTorrent does not do for a
			// synchronous load) or a lost answer leaves it loaded with its
			// label unconfirmed, which is counted and reported as such,
			// never as a failed add (fred on PR 122). The label is attempted
			// exactly once: for a magnet it lands on the metadata placeholder
			// and rTorrent drops it when the metadata arrives and the torrent
			// is rebuilt; nothing is remembered to set it again, since a
			// later same-hash torrent cannot be told from this one and a
			// missing label beats a wrong one (the stated contract; daybreak
			// blue and fred on PR 122). The user sets it in ruTorrent then.
			if err := c.setLabel(ctx, e.hash, label); err != nil {
				res.Unlabelled++
				res.labelError(strings.ToLower(e.hash[:8])+"…", labelReason(err))
			}
		}
	}
	if res.Added == 0 && lastErr != nil {
		return res, lastErr
	}
	return res, nil
}

// labelError records one reason, within the bound.
func (r *AddResult) labelError(who, why string) {
	if len(r.LabelErrors) >= maxLabelErrors {
		r.LabelErrorsOmitted++
		return
	}
	r.LabelErrors = append(r.LabelErrors, who+": "+why)
}

// labelReason is a fixed category for a failed label set: the peer's own
// fault text is not repeated (a lower-trust peer controls it).
func labelReason(err error) string {
	var f *Fault
	switch {
	case errors.As(err, &f):
		return fmt.Sprintf("rTorrent refused the label (fault %d)", f.Code)
	case errors.Is(err, ErrUnexpected):
		return "rTorrent's answer was not the label sent"
	case errors.Is(err, context.DeadlineExceeded), errors.Is(err, context.Canceled):
		return "no answer in time (the label may or may not be set)"
	}
	return "the connection failed (the label may or may not be set)"
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
