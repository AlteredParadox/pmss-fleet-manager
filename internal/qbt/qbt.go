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
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
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
	mu    sync.Mutex
	err   error
	until time.Time // zero = until cleared
	key   string    // credentials fingerprint the hold applies to
}

// AuthState is the persistable form of a guard.
type AuthState struct {
	Error string    `json:"error,omitempty"`
	Until time.Time `json:"until,omitempty"`
	Key   string    `json:"key,omitempty"`
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

// Clear ends any hold (call after the client was restarted).
func (g *AuthGuard) Clear() { g.fail(nil, time.Time{}, "") }

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
	return s
}

// Restore loads a persisted hold. Known errors map back to their sentinels.
func (g *AuthGuard) Restore(s AuthState) {
	if s.Error == "" {
		return
	}
	err := errors.New(s.Error)
	switch s.Error {
	case ErrAuth.Error():
		err = ErrAuth
	case ErrBanned.Error():
		err = ErrBanned
	}
	g.fail(err, s.Until, s.Key)
}

// New builds a client. hostport is what qBittorrent should see in the Host
// header (its host-header validation accepts localhost/127.0.0.1 by default),
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

// Close releases idle connections.
func (c *Client) Close() {
	if tr, ok := c.http.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

func (c *Client) do(ctx context.Context, method, path string, form url.Values) (int, []byte, error) {
	code, body, _, err := c.doResp(ctx, method, path, form)
	return code, body, err
}

func (c *Client) doResp(ctx context.Context, method, path string, form url.Values) (int, []byte, *http.Response, error) {
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, body)
	if err != nil {
		return 0, nil, nil, err
	}
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
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
	b, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	return resp.StatusCode, b, resp, err
}

// Login authenticates. With localhost auth bypass qBittorrent answers Ok. to anything.
func (c *Client) Login(ctx context.Context) error {
	if err, held := c.guard.Blocked(c.credsKey()); held {
		return err
	}
	form := url.Values{"username": {c.user}, "password": {c.pass}}
	code, body, resp, err := c.doResp(ctx, http.MethodPost, "/api/v2/auth/login", form)
	if err != nil {
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
	c.guard.Clear()
	if code < 200 || code >= 300 {
		return fmt.Errorf("qbittorrent: login http %d: %s", code, strings.TrimSpace(string(body)))
	}
	// 204 = no body, no cookie: qBittorrent did not need a new session (auth
	// already satisfied by a bypass rule). Later calls simply go without a cookie.
	return nil
}

func (c *Client) credsKey() string { return CredsKey(c.user, c.pass) }

// AuthBlocked reports whether login attempts are currently suspended after a failure.
func (c *Client) AuthBlocked() (error, bool) { return c.guard.Blocked(c.credsKey()) }

// call performs an API request, logging in once on 401/403.
func (c *Client) call(ctx context.Context, method, path string, form url.Values, out any) error {
	code, body, err := c.do(ctx, method, path, form)
	if err != nil {
		return err
	}
	if code == http.StatusForbidden || code == http.StatusUnauthorized {
		if code == http.StatusForbidden && strings.Contains(strings.ToLower(string(body)), "banned") {
			c.guard.fail(ErrBanned, time.Now().Add(banRetryAfter), "")
			return ErrBanned
		}
		if err := c.Login(ctx); err != nil {
			return err
		}
		code, body, err = c.do(ctx, method, path, form)
		if err != nil {
			return err
		}
	}
	if code == http.StatusNotFound {
		return errNotFound
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("qbittorrent: %s http %d: %s", path, code, strings.TrimSpace(string(body)))
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
}

// ServerState is the global state block from sync/maindata.
type ServerState struct {
	DLInfoSpeed int64  `json:"dl_info_speed"`
	UPInfoSpeed int64  `json:"up_info_speed"`
	GlobalRatio string `json:"global_ratio"`
}

// MainData is one full sync snapshot.
type MainData struct {
	ServerState ServerState        `json:"server_state"`
	Torrents    map[string]Torrent `json:"torrents"`
}

// MainData fetches a full (rid=0) sync snapshot: server state plus every torrent.
func (c *Client) MainData(ctx context.Context) (*MainData, error) {
	var md MainData
	if err := c.call(ctx, http.MethodGet, "/api/v2/sync/maindata?rid=0", nil, &md); err != nil {
		return nil, err
	}
	for h, t := range md.Torrents {
		if t.Hash == "" {
			t.Hash = h
			md.Torrents[h] = t
		}
	}
	return &md, nil
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

// TorrentActions are the per-torrent operations TorrentAction accepts.
var TorrentActions = map[string]bool{"stop": true, "start": true, "remove": true, "remove-data": true}

// TorrentAction stops, starts or removes one torrent. "remove" keeps the
// downloaded files on disk; "remove-data" deletes them too. The caller has
// already checked that hash is a bare hex info-hash.
func (c *Client) TorrentAction(ctx context.Context, action, hash string) error {
	switch action {
	case "stop":
		return c.stopStart(ctx, false, hash)
	case "start":
		return c.stopStart(ctx, true, hash)
	case "remove", "remove-data":
		form := url.Values{"hashes": {hash}, "deleteFiles": {strconv.FormatBool(action == "remove-data")}}
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
