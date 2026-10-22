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

// Package qbt is a minimal qBittorrent Web API v2 client that works over any
// dialer, so it can ride an SSH tunnel to 127.0.0.1:<port> on the box.
package qbt

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Dialer opens a connection to the WebUI listener.
type Dialer func(ctx context.Context) (net.Conn, error)

// Client talks to one qBittorrent instance.
type Client struct {
	http   *http.Client
	base   string // http://127.0.0.1:<port>, also used as the Host header qBittorrent validates
	user   string
	pass   string
	mu     sync.Mutex
	cookie string // session cookie header value, set by Login

	guard *AuthGuard

	// sync is the retained sync/maindata view, see MainData. syncMu
	// serialises MainData calls and is held across the request; mu is
	// never held with it. syncGen is bumped by Close/ResetSync without
	// any lock (Close is called under callers' own locks, which the
	// request's dialer may need): a view whose gen is behind it is stale
	// and the next MainData starts over.
	syncMu  sync.Mutex
	sync    syncState
	syncGen atomic.Uint64
}

// syncState is what incremental sync needs between two MainData calls:
// the server's response id and everything it has sent so far, merged
// delta by delta. torrents nil means "nothing retained": the next request
// is a full one (rid=0).
type syncState struct {
	gen      uint64 // Client.syncGen when the view was built
	rid      int64
	server   ServerState
	torrents map[string]Torrent
}

// banRetryAfter is the hold after qBittorrent reports the IP banned: its
// WebUI\BanDuration defaults to an hour.
const banRetryAfter = 65 * time.Minute

// ErrBanned is returned when qBittorrent reports the IP as banned.
var ErrBanned = errors.New("qbittorrent has banned the box's loopback IP after failed logins; it clears after about an hour or when qbittorrent-nox restarts")

// ErrAuth is returned when login is refused.
var ErrAuth = errors.New("qbittorrent rejected the WebUI username/password; login attempts for these credentials are suspended until they are changed or the client is restarted")

// AuthGuard remembers a rejected login so it is never retried on a timer.
//
// qBittorrent bans the client IP for an hour after five failed logins and
// its failure counter is only cleared by a successful login or an expired
// ban, never by elapsed time. The banned IP is the box's own loopback, which
// also locks the user out of the PMSS web path. So a rejected credential is
// held indefinitely, keyed to that credential: the hold ends only when the
// credentials change or the caller clears it (after restarting the client,
// which resets qBittorrent's counter). A guard outlives the Client it is
// attached to; the poller keeps one per box and persists it.
type AuthGuard struct {
	mu sync.Mutex
	// loginMu admits one login attempt at a time. Without it, callers that
	// all saw the guard open (a poll and a few torrent clicks) would each
	// send the same rejected credentials before the first failure was
	// recorded, and five of those is qBittorrent's ban threshold.
	loginMu sync.Mutex
	// Attempts are numbered (under mu) so a caller refused while another's
	// attempt was in flight can take that attempt's outcome. The guard is
	// shared by every client built for the box, so a failure is shared
	// across clients too; a success is only reusable by the client that
	// holds the resulting cookie (lastBy).
	loginGen uint64
	loginErr error
	loginAt  time.Time
	loginKey string // credentials the last attempt used; a failure is shared only with those
	lastBy   *Client
	err      error
	until    time.Time // zero = until cleared
	key      string    // credentials fingerprint the hold applies to
	// ambiguous counts outcome-less logins for ambKey (see noteAmbiguous).
	ambiguous int
	ambKey    string
}

// AuthState is the persistable form of a guard.
type AuthState struct {
	Error string    `json:"error,omitempty"`
	Until time.Time `json:"until,omitempty"`
	Key   string    `json:"key,omitempty"`
	// Ambiguous counts logins whose outcome was never seen (the connection
	// failed after the credentials were sent) for AmbiguousKey; see
	// ErrAmbiguous. Kept apart from Key: a ban hold has no key (it is the
	// IP that is banned) and must not become credential-scoped on restore.
	Ambiguous    int    `json:"ambiguous,omitempty"`
	AmbiguousKey string `json:"ambiguous_key,omitempty"`
	// Endpoint is the SSH endpoint the hold was recorded for (set by the
	// poller); a restore ignores a hold recorded for another one.
	Endpoint string `json:"endpoint,omitempty"`
}

// ambiguousBudget is how many logins may end without a visible outcome
// before further attempts are held. qBittorrent counts a failure it saw
// even when the reply never reached us, and five of those ban the box's
// loopback IP; the budget keeps the worst case well under that.
const ambiguousBudget = 3

// ErrAmbiguous is the hold after ambiguousBudget logins ended without an
// answer; it clears like ErrAuth (credentials re-entered or client restarted).
var ErrAmbiguous = errors.New("qbittorrent login attempts kept ending without an answer (the connection failed after the credentials were sent); further attempts are suspended until the password is re-entered or the client is restarted, to stay clear of qBittorrent's IP ban")

// noteAmbiguous records a login whose outcome was not seen and holds once
// the budget is spent.
func (g *AuthGuard) noteAmbiguous(key string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.ambKey != key {
		g.ambKey, g.ambiguous = key, 0
	}
	g.ambiguous++
	if g.ambiguous >= ambiguousBudget {
		g.err, g.until, g.key = ErrAmbiguous, time.Time{}, key
	}
}

// CredsKey fingerprints a username/password pair without revealing it.
func CredsKey(user, pass string) string {
	h := sha256.Sum256([]byte(user + "\x00" + pass))
	return hex.EncodeToString(h[:8])
}

func (g *AuthGuard) fail(err error, until time.Time, key string) {
	g.mu.Lock()
	g.err, g.until, g.key = err, until, key
	g.mu.Unlock()
}

// Clear ends any hold (call after the client was restarted) and forgets
// the last attempt's outcome, so the next refused request may log in.
func (g *AuthGuard) Clear() {
	g.mu.Lock()
	g.err, g.until, g.key = nil, time.Time{}, ""
	g.loginErr, g.loginAt, g.lastBy = nil, time.Time{}, nil
	g.ambiguous, g.ambKey = 0, ""
	g.mu.Unlock()
}

// Blocked reports whether logins with the given credentials are suspended.
func (g *AuthGuard) Blocked(key string) (error, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.err == nil {
		return nil, false
	}
	if g.key != "" && g.key != key { // credentials changed: the hold no longer applies
		g.err, g.until, g.key = nil, time.Time{}, ""
		return nil, false
	}
	if !g.until.IsZero() {
		if time.Now().After(g.until) {
			g.err, g.until, g.key = nil, time.Time{}, ""
			return nil, false
		}
		return fmt.Errorf("%w (next attempt in %s)", g.err, time.Until(g.until).Round(time.Minute)), true
	}
	return g.err, true
}

// State exports the hold for persistence.
func (g *AuthGuard) State() AuthState {
	g.mu.Lock()
	defer g.mu.Unlock()
	s := AuthState{Until: g.until, Key: g.key}
	if g.err != nil {
		s.Error = g.err.Error()
	}
	if g.ambiguous > 0 {
		s.Ambiguous, s.AmbiguousKey = g.ambiguous, g.ambKey
	}
	return s
}

// Restore loads a persisted hold. Known errors map back to their sentinels.
func (g *AuthGuard) Restore(s AuthState) {
	if s.Ambiguous > 0 {
		g.mu.Lock()
		g.ambiguous, g.ambKey = s.Ambiguous, s.AmbiguousKey
		g.mu.Unlock()
	}
	if s.Error == "" {
		return
	}
	err := errors.New(s.Error)
	switch s.Error {
	case ErrAuth.Error():
		err = ErrAuth
	case ErrBanned.Error():
		err = ErrBanned
	case ErrAmbiguous.Error():
		err = ErrAmbiguous
	}
	g.fail(err, s.Until, s.Key)
}

// New builds a client with its own guard. Test support: the poller uses
// NewWithGuard so the hold outlives the client. hostport is what qBittorrent
// should see in the Host header (its host-header validation accepts localhost/127.0.0.1 by default),
// e.g. "127.0.0.1:8080". user/pass may be empty when the WebUI bypasses
// authentication for localhost.
func New(dial Dialer, hostport, user, pass string) *Client {
	return NewWithGuard(dial, hostport, user, pass, &AuthGuard{})
}

// NewWithGuard builds a client whose auth holds live in a guard that
// survives the client being rebuilt (SSH reconnects, port changes).
func NewWithGuard(dial Dialer, hostport, user, pass string, g *AuthGuard) *Client {
	if g == nil {
		g = &AuthGuard{}
	}
	tr := &http.Transport{
		DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		MaxIdleConns:    2,
		IdleConnTimeout: 60 * time.Second,
	}
	if hostport == "" {
		hostport = "127.0.0.1"
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: 30 * time.Second}, base: "http://" + hostport, user: user, pass: pass, guard: g}
}

// Close releases idle connections and discards the retained sync view: it
// is called when whatever answers the port may be a different process,
// whose sync position means nothing to ours.
func (c *Client) Close() {
	if tr, ok := c.http.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
	c.ResetSync()
}

// doResp sends a URL-encoded form (or a bare request when form is nil).
func (c *Client) doResp(ctx context.Context, method, path string, form url.Values) (int, []byte, *http.Response, error) {
	if form == nil {
		return c.send(ctx, method, path, "", nil)
	}
	return c.send(ctx, method, path, "application/x-www-form-urlencoded", []byte(form.Encode()))
}

// send is the one place a request leaves the client: cookie, Referer and
// Origin are set here, and the response body is capped. body may be nil.
func (c *Client) send(ctx context.Context, method, path, ctype string, body []byte) (int, []byte, *http.Response, error) {
	var rd io.Reader
	if body != nil {
		rd = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return 0, nil, nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	req.Header.Set("Referer", c.base+"/")
	req.Header.Set("Origin", c.base)
	c.mu.Lock()
	if c.cookie != "" {
		req.Header.Set("Cookie", c.cookie)
	}
	c.mu.Unlock()
	resp, err := c.http.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	// One byte over the cap tells a body that was cut off from one that
	// fits: a truncated maindata would otherwise fail as malformed JSON.
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err == nil && len(b) > maxBody {
		return resp.StatusCode, nil, resp, fmt.Errorf("qbittorrent: response from %s is larger than %d MiB", path, maxBody>>20)
	}
	return resp.StatusCode, b, resp, err
}

// maxBody bounds any one response. A maindata for a few thousand torrents
// is a few MiB; every poller may hold one of these at once. A larger
// response is an error, never silently truncated.
const maxBody = 16 << 20

// Login authenticates. With localhost auth bypass qBittorrent answers Ok. to
// anything. One attempt runs at a time per guard.
func (c *Client) Login(ctx context.Context) error {
	c.guard.loginMu.Lock()
	defer c.guard.loginMu.Unlock()
	return c.attempt(ctx)
}

// attempt runs one login and publishes its outcome as a new generation.
// Caller holds guard.loginMu.
func (c *Client) attempt(ctx context.Context) error {
	err := c.login(ctx)
	g := c.guard
	g.mu.Lock()
	g.loginGen++
	g.loginErr, g.loginAt, g.loginKey, g.lastBy = err, time.Now(), c.credsKey(), c
	g.mu.Unlock()
	return err
}

// loginShareWindow: after a failed attempt, requests refused within this
// long take that failure too, even ones sent after it completed. Without
// it a burst of actions straddling a dropped login would each retry. A
// call after the window may try again. Variable so tests can shorten it.
var loginShareWindow = 2 * time.Second

func (c *Client) curGen() uint64 {
	c.guard.mu.Lock()
	defer c.guard.mu.Unlock()
	return c.guard.loginGen
}

// loginAfter logs in for a request that was refused, where gen is the login
// generation when that request was sent. If an attempt completed since
// (another caller's, which this one waited on), its outcome is shared: nil
// means the retry can use the new session, an error (rejection, or a
// connection that dropped mid-login) is returned without sending the
// credentials again. So is a failure from the last loginShareWindow, by
// any client on this guard. A success is reused only by the client that
// made it (the cookie is its own); another client logs in for itself,
// which is safe because the credentials are known good. Any other refused
// request makes its own attempt.
func (c *Client) loginAfter(ctx context.Context, gen uint64) error {
	c.guard.loginMu.Lock()
	defer c.guard.loginMu.Unlock()
	g := c.guard
	g.mu.Lock()
	cur, last, at, by, same := g.loginGen, g.loginErr, g.loginAt, g.lastBy, g.loginKey == c.credsKey()
	g.mu.Unlock()
	if last != nil && same && (cur != gen || time.Since(at) < loginShareWindow) {
		return last
	}
	if last == nil && cur != gen && by == c {
		return nil
	}
	return c.attempt(ctx)
}

func (c *Client) login(ctx context.Context) error {
	if err, held := c.guard.Blocked(c.credsKey()); held {
		return err
	}
	form := url.Values{"username": {c.user}, "password": {c.pass}}
	// Per-request write evidence: WroteRequest fires for THIS request once
	// headers and body (the credentials) were handed to the connection
	// without error. A dial failure, a request cancelled before that, or a
	// write that failed part-way sent nothing qBittorrent could count as a
	// login and must not spend the ambiguous budget, or a briefly
	// unreachable box would lock valid credentials; and another request's
	// traffic on the shared transport cannot be mistaken for this one's.
	var sent atomic.Bool
	ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{WroteRequest: func(i httptrace.WroteRequestInfo) {
		if i.Err == nil {
			sent.Store(true)
		}
	}})
	code, body, resp, err := c.doResp(ctx, http.MethodPost, "/api/v2/auth/login", form)
	if err != nil {
		if sent.Load() {
			c.guard.noteAmbiguous(c.credsKey())
		}
		return err
	}
	if code == http.StatusForbidden && strings.Contains(strings.ToLower(string(body)), "banned") {
		c.guard.fail(ErrBanned, time.Now().Add(banRetryAfter), "")
		return ErrBanned
	}
	var parts []string
	for _, ck := range resp.Cookies() {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	c.mu.Lock()
	c.cookie = strings.Join(parts, "; ")
	c.mu.Unlock()
	if code == http.StatusUnauthorized || code == http.StatusForbidden || strings.HasPrefix(string(body), "Fails") {
		c.guard.fail(ErrAuth, time.Time{}, c.credsKey())
		return ErrAuth
	}
	c.guard.Clear() // a seen outcome, accepted: the ambiguous count resets too
	if code < 200 || code >= 300 {
		return fmt.Errorf("qbittorrent: login http %d: %s", code, clipBody(body))
	}
	// 204 = no body, no cookie: qBittorrent did not need a new session (auth
	// already satisfied by a bypass rule). Later calls simply go without a cookie.
	return nil
}

func (c *Client) credsKey() string { return CredsKey(c.user, c.pass) }

// AuthBlocked reports whether login attempts are currently suspended after a failure.
func (c *Client) AuthBlocked() (error, bool) { return c.guard.Blocked(c.credsKey()) }

// call performs a form API request, logging in once on 401/403.
func (c *Client) call(ctx context.Context, method, path string, form url.Values, out any) error {
	ctype, payload := "", []byte(nil)
	if form != nil {
		ctype, payload = "application/x-www-form-urlencoded", []byte(form.Encode())
	}
	return c.callBody(ctx, method, path, ctype, payload, out)
}

// callBody is call for an arbitrary request body (multipart uploads).
func (c *Client) callBody(ctx context.Context, method, path, ctype string, payload []byte, out any) error {
	gen := c.curGen()
	code, body, _, err := c.send(ctx, method, path, ctype, payload)
	if err != nil {
		return err
	}
	if code == http.StatusForbidden || code == http.StatusUnauthorized {
		if code == http.StatusForbidden && strings.Contains(strings.ToLower(string(body)), "banned") {
			c.guard.fail(ErrBanned, time.Now().Add(banRetryAfter), "")
			return ErrBanned
		}
		if err := c.loginAfter(ctx, gen); err != nil {
			return err
		}
		code, body, _, err = c.send(ctx, method, path, ctype, payload)
		if err != nil {
			return err
		}
	}
	if code == http.StatusNotFound {
		return errNotFound
	}
	if code == http.StatusUnsupportedMediaType {
		return ErrBadTorrent
	}
	if code < 200 || code >= 300 {
		return &HTTPError{Path: path, Code: code, Body: clipBody(body)}
	}
	if out == nil {
		return nil
	}
	if s, ok := out.(*string); ok {
		*s = string(body)
		return nil
	}
	return json.Unmarshal(body, out)
}

var errNotFound = errors.New("qbittorrent: endpoint not found")

// Version returns the application version, e.g. "v5.1.0".
func (c *Client) Version(ctx context.Context) (string, error) {
	var s string
	err := c.call(ctx, http.MethodGet, "/api/v2/app/version", nil, &s)
	return strings.TrimSpace(s), err
}

// Torrent is the subset of torrent fields the dashboard uses.
type Torrent struct {
	Hash     string  `json:"hash"`
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
	DLSpeed  int64   `json:"dlspeed"`
	UPSpeed  int64   `json:"upspeed"`
	Ratio    float64 `json:"ratio"`
	State    string  `json:"state"`
	AddedOn  int64   `json:"added_on"`
	Category string  `json:"category"`
	// Lifetime transfer and swarm size. The *Swarm counts are what the
	// tracker/DHT reports and come back as -1 when unknown.
	Uploaded   int64 `json:"uploaded"`
	Downloaded int64 `json:"downloaded"`
	Seeds      int   `json:"num_seeds"`
	SeedsSwarm int   `json:"num_complete"`
	Peers      int   `json:"num_leechs"`
	PeersSwarm int   `json:"num_incomplete"`
}

// ServerState is the global state block from sync/maindata.
type ServerState struct {
	DLInfoSpeed int64  `json:"dl_info_speed"`
	UPInfoSpeed int64  `json:"up_info_speed"`
	GlobalRatio string `json:"global_ratio"`
	// ConnectionStatus is libtorrent's view of incoming connectivity:
	// "connected", "firewalled" (no peer has reached the listen port yet,
	// which after a start takes a while to clear) or "disconnected".
	ConnectionStatus string `json:"connection_status"`
}

// Prefs is the subset of app/preferences the dashboard shows: the three
// peer-discovery mechanisms private trackers forbid, and the listen port
// incoming peers must reach.
type Prefs struct {
	DHT        bool `json:"dht"`
	PeX        bool `json:"pex"`
	LSD        bool `json:"lsd"`
	ListenPort int  `json:"listen_port"`
}

// Preferences reads the client's settings.
func (c *Client) Preferences(ctx context.Context) (*Prefs, error) {
	var p Prefs
	if err := c.call(ctx, http.MethodGet, "/api/v2/app/preferences", nil, &p); err != nil {
		return nil, err
	}
	return &p, nil
}

// SetPreferences writes the given keys via app/setPreferences; keys not
// present are left as they are.
func (c *Client) SetPreferences(ctx context.Context, prefs map[string]any) error {
	b, err := json.Marshal(prefs)
	if err != nil {
		return err
	}
	return c.call(ctx, http.MethodPost, "/api/v2/app/setPreferences", url.Values{"json": {string(b)}}, nil)
}

// MainData is the client's current view after a sync: server state plus
// every torrent. Torrents is the client's retained map, not a copy: read
// it before the next MainData call on the same client and do not modify it.
type MainData struct {
	ServerState ServerState
	Torrents    map[string]Torrent
}

// MaxTorrents bounds the torrents one client view may hold, and the
// entries one sync response may carry. The decoder stops at the limit
// before materialising more, so a hostile or runaway WebUI cannot make the
// dashboard allocate without bound; a real list that large is reported as
// an error rather than shown partially.
const MaxTorrents = 20000

// MainData syncs with the WebUI: the first call (and any after Close or a
// failure that discarded the view) fetches everything with rid=0; later
// calls send the last response id and receive only what changed since,
// which is merged field by field into the retained view (a delta entry
// holds only the fields that changed; torrents_removed drops entries; a
// full_update replaces the view). A failed request or an undecodable
// response leaves the view and its id untouched, so the next call asks
// for the same delta again. qBittorrent keeps the sync position per
// session, and a re-login is a new session: it then answers with a full
// update, which is honoured like any other.
func (c *Client) MainData(ctx context.Context) (*MainData, error) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	gen := c.syncGen.Load()
	if c.sync.gen != gen {
		c.sync = syncState{} // reset since the view was built
	}
	rid := c.sync.rid
	if c.sync.torrents == nil {
		rid = 0
	}
	var raw string
	if err := c.call(ctx, http.MethodGet, "/api/v2/sync/maindata?rid="+strconv.FormatInt(rid, 10), nil, &raw); err != nil {
		return nil, err
	}
	d, err := decodeMainData(strings.NewReader(raw), MaxTorrents)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent: sync/maindata: %w", err)
	}
	next, err := c.sync.apply(d, rid == 0)
	if err != nil {
		return nil, fmt.Errorf("qbittorrent: sync/maindata: %w", err)
	}
	next.gen = gen // a reset during the request still wins: the view is dropped next time
	c.sync = next
	return &MainData{ServerState: next.server, Torrents: next.torrents}, nil
}

// ResetSync discards the retained sync view; the next MainData fetches
// everything. Close does this too. It takes no lock, so it is safe under
// any lock the in-flight request's dialer may be waiting for.
func (c *Client) ResetSync() {
	c.syncGen.Add(1)
}

// mainDelta is one decoded sync/maindata response, torrents still raw:
// each entry is merged into the retained Torrent by unmarshalling into a
// copy of it, which sets only the fields the entry carries.
type mainDelta struct {
	rid      int64
	full     bool
	server   json.RawMessage
	torrents map[string]json.RawMessage
	removed  []string
}

// decodeMainData reads a sync/maindata body as a stream, keeping the keys
// the dashboard uses and skipping the rest (categories, tags, trackers)
// without materialising them. It stops with an error once the torrents
// object or the removed list exceeds maxEntries.
func decodeMainData(r io.Reader, maxEntries int) (*mainDelta, error) {
	dec := json.NewDecoder(r)
	if err := expectDelim(dec, '{'); err != nil {
		return nil, err
	}
	d := &mainDelta{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		key, _ := tok.(string)
		switch key {
		case "rid":
			err = dec.Decode(&d.rid)
		case "full_update":
			err = dec.Decode(&d.full)
		case "server_state":
			err = dec.Decode(&d.server)
		case "torrents":
			d.torrents, err = decodeTorrentEntries(dec, maxEntries)
		case "torrents_removed":
			d.removed, err = decodeRemoved(dec, maxEntries)
		default:
			err = skipValue(dec)
		}
		if err != nil {
			return nil, err
		}
	}
	if err := expectDelim(dec, '}'); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF { // a prefix followed by anything else is not a response
		return nil, fmt.Errorf("trailing data after the sync object")
	}
	return d, nil
}

func decodeTorrentEntries(dec *json.Decoder, maxEntries int) (map[string]json.RawMessage, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok == nil { // "torrents": null
		return nil, nil
	}
	if tok != json.Delim('{') {
		return nil, fmt.Errorf("torrents is not an object")
	}
	out := map[string]json.RawMessage{}
	for dec.More() {
		if len(out) >= maxEntries {
			return nil, fmt.Errorf("more than %d torrents; not supported", maxEntries)
		}
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		hash, _ := tok.(string)
		if len(hash) > maxHashLen {
			return nil, fmt.Errorf("torrent key longer than %d bytes", maxHashLen)
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, err
		}
		out[hash] = raw
	}
	return out, expectDelim(dec, '}')
}

func decodeRemoved(dec *json.Decoder, maxEntries int) ([]string, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, nil
	}
	if tok != json.Delim('[') {
		return nil, fmt.Errorf("torrents_removed is not a list")
	}
	var out []string
	for dec.More() {
		if len(out) >= maxEntries {
			return nil, fmt.Errorf("more than %d removed torrents; not supported", maxEntries)
		}
		var h string
		if err := dec.Decode(&h); err != nil {
			return nil, err
		}
		if len(h) > maxHashLen {
			return nil, fmt.Errorf("removed key longer than %d bytes", maxHashLen)
		}
		out = append(out, h)
	}
	return out, expectDelim(dec, ']')
}

// Bounds on what one retained entry may hold, so the view's memory is
// bounded by MaxTorrents times a few hundred bytes however large the
// strings in individual responses are (each response is itself capped at
// maxBody, but deltas accumulate). Hash keys over the bound are refused
// as malformed (a v1 hash is 40 hex characters, a v2 hash 64); the other
// strings are clipped at a rune boundary.
const (
	maxHashLen     = 64
	maxNameLen     = 400
	maxStateLen    = 32
	maxCategoryLen = 100
	maxServerStr   = 32
)

// clipped returns the torrent with its strings within the retention bounds.
func (t Torrent) clipped() Torrent {
	t.Hash, t.Name, t.State, t.Category = clipStr(t.Hash, maxHashLen), clipStr(t.Name, maxNameLen), clipStr(t.State, maxStateLen), clipStr(t.Category, maxCategoryLen)
	return t
}

func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	if tok != want {
		return fmt.Errorf("expected %q, got %v", want, tok)
	}
	return nil
}

// skipValue consumes one JSON value, however nested, without keeping it.
func skipValue(dec *json.Decoder) error {
	depth := 0
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				depth++
			case '}', ']':
				depth--
			}
		}
		if depth == 0 {
			return nil
		}
	}
}

// apply merges a delta into the retained view and returns the result; the
// receiver is not modified, so a decode error part-way leaves it as it
// was. requestedFull is true when the request carried rid=0: a WebUI that
// omits full_update (older or fake ones) still answers that with a full
// list, never a delta.
func (s syncState) apply(d *mainDelta, requestedFull bool) (syncState, error) {
	full := d.full || requestedFull || s.torrents == nil
	next := syncState{rid: d.rid, server: s.server}
	if full {
		next.server = ServerState{}
	}
	if len(d.server) > 0 {
		if err := json.Unmarshal(d.server, &next.server); err != nil {
			return s, fmt.Errorf("server_state: %w", err)
		}
		next.server.GlobalRatio, next.server.ConnectionStatus = clipStr(next.server.GlobalRatio, maxServerStr), clipStr(next.server.ConnectionStatus, maxServerStr)
	}
	updates := make(map[string]Torrent, len(d.torrents))
	for hash, raw := range d.torrents {
		var t Torrent
		if !full {
			t = s.torrents[hash] // zero when new
		}
		if err := json.Unmarshal(raw, &t); err != nil {
			return s, fmt.Errorf("torrent %s: %w", hash, err)
		}
		if t.Hash == "" {
			t.Hash = hash
		}
		updates[hash] = t.clipped()
	}
	if full {
		next.torrents = updates
		return next, nil
	}
	next.torrents = make(map[string]Torrent, len(s.torrents)+len(updates))
	for h, t := range s.torrents {
		next.torrents[h] = t
	}
	for _, h := range d.removed {
		delete(next.torrents, h)
	}
	for h, t := range updates {
		next.torrents[h] = t
	}
	if len(next.torrents) > MaxTorrents {
		return s, fmt.Errorf("more than %d torrents; not supported", MaxTorrents)
	}
	return next, nil
}

// PauseAll stops every torrent (qBittorrent 5 "stop", 4.x "pause").
func (c *Client) PauseAll(ctx context.Context) error { return c.stopStart(ctx, false, "all") }

// ResumeAll starts every torrent.
func (c *Client) ResumeAll(ctx context.Context) error { return c.stopStart(ctx, true, "all") }

// stopStart hits the qBittorrent 5 endpoint and falls back to the 4.x name
// when the daemon does not know it. hashes is "all" or a '|'-joined list.
func (c *Client) stopStart(ctx context.Context, start bool, hashes string) error {
	form := url.Values{"hashes": {hashes}}
	v5, v4 := "/api/v2/torrents/stop", "/api/v2/torrents/pause"
	if start {
		v5, v4 = "/api/v2/torrents/start", "/api/v2/torrents/resume"
	}
	err := c.call(ctx, http.MethodPost, v5, form, nil)
	if errors.Is(err, errNotFound) {
		err = c.call(ctx, http.MethodPost, v4, form, nil)
	}
	return err
}

// clipBody keeps the start of a response body for an error message: the
// text reaches the UI as a toast, and a hostile or broken WebUI could send
// megabytes.
func clipBody(b []byte) string {
	s := strings.TrimSpace(string(b))
	if len(s) <= 300 {
		return s
	}
	cut := 300
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// HTTPError is a non-2xx answer from the WebUI that no sentinel covers.
type HTTPError struct {
	Path string
	Code int
	Body string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("qbittorrent: %s http %d: %s", e.Path, e.Code, e.Body)
}

// ErrBadTorrent is returned when qBittorrent rejects an upload as not a
// torrent file (HTTP 415).
var ErrBadTorrent = errors.New("qbittorrent: not a valid torrent file")

// ErrAddRejected is returned when qBittorrent added nothing: "Fails." from
// 4.x and 5.0/5.1, HTTP 409 from 5.2+. Both an unparsable link and a torrent
// that is already in the session end there, so the message names both.
var ErrAddRejected = errors.New("qbittorrent: nothing was added — the torrent is already present, or the link is not a valid magnet or torrent")

// ValidHash reports whether h is a v1 (40 hex) or v2 (64 hex) info-hash in
// either case. Everything that names a torrent to the WebUI goes through it.
func ValidHash(h string) bool {
	if len(h) != 40 && len(h) != 64 {
		return false
	}
	return strings.Trim(strings.ToLower(h), "0123456789abcdef") == ""
}

// TorrentFile is one uploaded .torrent for Add.
type TorrentFile struct {
	Name string
	Data []byte
}

// AddRequest is what Add sends: links and/or files, with the options the
// dashboard exposes. Category "" keeps qBittorrent's default.
type AddRequest struct {
	URLs     []string
	Files    []TorrentFile
	Category string
	Stopped  bool // add without starting (qBittorrent 5 "stopped", 4.x "paused"); false leaves the client's own default in force
}

// AddResult is what qBittorrent reported about an add. Counted is false on
// 4.x and 5.0/5.1, which only say "Ok." (at least one entry was accepted);
// 5.2+ returns per-entry counts.
type AddResult struct {
	Counted bool
	Added   int // torrents now in the session
	Failed  int // rejected: already present or not a torrent
	Pending int // http(s) links qBittorrent is still fetching
}

// Add posts torrents/add as multipart. Up to 5.1 qBittorrent answers 200
// "Ok." when it accepted at least one entry and 200 "Fails." when it added
// nothing; 5.2+ answers a JSON count object, or 409 when nothing was added.
func (c *Client) Add(ctx context.Context, req AddRequest) (AddResult, error) {
	var res AddResult
	if len(req.URLs) == 0 && len(req.Files) == 0 {
		return res, errors.New("nothing to add")
	}
	body, ctype, err := addBody(req)
	if err != nil {
		return res, err
	}
	var out string
	if err := c.callBody(ctx, http.MethodPost, "/api/v2/torrents/add", ctype, body, &out); err != nil {
		var he *HTTPError
		if errors.As(err, &he) && he.Code == http.StatusConflict {
			return res, ErrAddRejected // 5.2+: nothing was added
		}
		return res, err
	}
	return parseAddResult(out)
}

// addBody builds the multipart form for torrents/add.
func addBody(req AddRequest) ([]byte, string, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	if len(req.URLs) > 0 {
		_ = mw.WriteField("urls", strings.Join(req.URLs, "\n"))
	}
	if req.Category != "" {
		_ = mw.WriteField("category", req.Category)
	}
	if req.Stopped {
		// Only ever sent as true: an explicit false would override the
		// client's "add torrents stopped" preference, which is the user's.
		_ = mw.WriteField("stopped", "true") // 5.x name
		_ = mw.WriteField("paused", "true")  // 4.x name; unknown fields are ignored
	}
	for _, f := range req.Files {
		fw, err := mw.CreateFormFile("torrents", f.Name)
		if err != nil {
			return nil, "", err
		}
		if _, err := fw.Write(f.Data); err != nil {
			return nil, "", err
		}
	}
	if err := mw.Close(); err != nil {
		return nil, "", err
	}
	return buf.Bytes(), mw.FormDataContentType(), nil
}

// parseAddResult reads a 2xx torrents/add body: "Ok."/"Fails." up to 5.1,
// a JSON count object from 5.2 on. Anything else counts as an uncounted
// success, since the status already said the request was accepted.
func parseAddResult(out string) (AddResult, error) {
	var res AddResult
	out = strings.TrimSpace(out)
	if strings.HasPrefix(out, "Fails") {
		return res, ErrAddRejected
	}
	if !strings.HasPrefix(out, "{") {
		return res, nil
	}
	var counts struct {
		Success int `json:"success_count"`
		Failure int `json:"failure_count"`
		Pending int `json:"pending_count"`
	}
	if err := json.Unmarshal([]byte(out), &counts); err != nil {
		return res, nil
	}
	res = AddResult{Counted: true, Added: max(counts.Success, 0), Failed: max(counts.Failure, 0), Pending: max(counts.Pending, 0)}
	if res.Added == 0 && res.Pending == 0 {
		return res, ErrAddRejected
	}
	return res, nil
}

// Per-torrent operations TorrentAction accepts.
const (
	ActionStop       = "stop"
	ActionStart      = "start"
	ActionRemove     = "remove"      // keeps the downloaded files
	ActionRemoveData = "remove-data" // deletes them too
)

// TorrentActions is the set of accepted per-torrent operations.
var TorrentActions = map[string]bool{ActionStop: true, ActionStart: true, ActionRemove: true, ActionRemoveData: true}

// TorrentAction stops, starts or removes one torrent. "remove" keeps the
// downloaded files on disk; "remove-data" deletes them too. The caller has
// already checked that hash is a bare hex info-hash.
func (c *Client) TorrentAction(ctx context.Context, action, hash string) error {
	switch action {
	case ActionStop:
		return c.stopStart(ctx, false, hash)
	case ActionStart:
		return c.stopStart(ctx, true, hash)
	case ActionRemove, ActionRemoveData:
		form := url.Values{"hashes": {hash}, "deleteFiles": {strconv.FormatBool(action == ActionRemoveData)}}
		return c.call(ctx, http.MethodPost, "/api/v2/torrents/delete", form, nil)
	}
	return fmt.Errorf("unknown torrent action %q", action)
}

// Shutdown asks qBittorrent to exit.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/api/v2/app/shutdown", url.Values{}, nil)
}

// StateKind classifies a qBittorrent state string into seeding, leeching, paused, checking, error.
func StateKind(state string) string {
	switch state {
	case "uploading", "forcedUP", "stalledUP", "queuedUP":
		return "seeding"
	case "downloading", "forcedDL", "metaDL", "forcedMetaDL", "queuedDL", "stalledDL", "allocating":
		return "leeching"
	case "pausedUP", "pausedDL", "stoppedUP", "stoppedDL":
		return "paused"
	case "checkingUP", "checkingDL", "checkingResumeData", "moving":
		return "checking"
	case "error", "missingFiles":
		return "error"
	}
	return "unknown"
}
