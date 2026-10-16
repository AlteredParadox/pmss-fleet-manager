// Package qbt is a minimal qBittorrent Web API v2 client that works over any
// dialer, so it can ride an SSH tunnel to 127.0.0.1:<port> on the box.
package qbt

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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

	authFailErr   error     // last auth failure, replayed until authFailUntil
	authFailUntil time.Time // no login attempts before this; protects against qBittorrent's IP ban
}

// Cooldowns applied after auth failures. qBittorrent bans the client IP for an
// hour (WebUI\BanDuration) after five failed logins, and that IP is the box's
// own loopback, which would also lock the user out of the PMSS web path.
const (
	authRetryAfter = 20 * time.Minute
	banRetryAfter  = 65 * time.Minute
)

// ErrBanned is returned when qBittorrent reports the IP as banned.
var ErrBanned = errors.New("qbittorrent has banned the box's loopback IP after failed logins; it clears after about an hour or when qbittorrent-nox restarts")

// ErrAuth is returned when login is refused.
var ErrAuth = errors.New("qbittorrent rejected the WebUI username/password (set them in the box options, or enable 'Bypass authentication for clients on localhost' in qBittorrent)")

// New builds a client. hostport is what qBittorrent should see in the Host
// header (its host-header validation accepts localhost/127.0.0.1 by default),
// e.g. "127.0.0.1:8080". user/pass may be empty when the WebUI bypasses
// authentication for localhost.
func New(dial Dialer, hostport, user, pass string) *Client {
	tr := &http.Transport{
		DialContext:       func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		DisableKeepAlives: false,
		MaxIdleConns:      2,
		IdleConnTimeout:   60 * time.Second,
	}
	if hostport == "" {
		hostport = "127.0.0.1"
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: 30 * time.Second}, base: "http://" + hostport, user: user, pass: pass}
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
	c.mu.Lock()
	if c.authFailErr != nil && time.Now().Before(c.authFailUntil) {
		err := fmt.Errorf("%w (next attempt in %s)", c.authFailErr, time.Until(c.authFailUntil).Round(time.Minute))
		c.mu.Unlock()
		return err
	}
	c.mu.Unlock()
	form := url.Values{"username": {c.user}, "password": {c.pass}}
	code, body, resp, err := c.doResp(ctx, http.MethodPost, "/api/v2/auth/login", form)
	if err != nil {
		return err
	}
	if code == http.StatusForbidden && strings.Contains(strings.ToLower(string(body)), "banned") {
		c.failAuth(ErrBanned, banRetryAfter)
		return ErrBanned
	}
	var parts []string
	for _, ck := range resp.Cookies() {
		parts = append(parts, ck.Name+"="+ck.Value)
	}
	c.mu.Lock()
	c.cookie = strings.Join(parts, "; ")
	c.authFailErr, c.authFailUntil = nil, time.Time{}
	c.mu.Unlock()
	if code == http.StatusUnauthorized || code == http.StatusForbidden || strings.HasPrefix(string(body), "Fails") {
		c.failAuth(ErrAuth, authRetryAfter)
		return ErrAuth
	}
	if code < 200 || code >= 300 {
		return fmt.Errorf("qbittorrent: login http %d: %s", code, strings.TrimSpace(string(body)))
	}
	// 204 = no body, no cookie: qBittorrent did not need a new session (auth
	// already satisfied by a bypass rule). Later calls simply go without a cookie.
	return nil
}

func (c *Client) failAuth(err error, wait time.Duration) {
	c.mu.Lock()
	c.authFailErr, c.authFailUntil = err, time.Now().Add(wait)
	c.mu.Unlock()
}

// AuthBlocked reports whether login attempts are currently suspended after a failure.
func (c *Client) AuthBlocked() (error, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.authFailErr != nil && time.Now().Before(c.authFailUntil) {
		return c.authFailErr, true
	}
	return nil, false
}

// call performs an API request, logging in once on 401/403.
func (c *Client) call(ctx context.Context, method, path string, form url.Values, out any) error {
	code, body, err := c.do(ctx, method, path, form)
	if err != nil {
		return err
	}
	if code == http.StatusForbidden || code == http.StatusUnauthorized {
		if code == http.StatusForbidden && strings.Contains(strings.ToLower(string(body)), "banned") {
			c.failAuth(ErrBanned, banRetryAfter)
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
	Hash       string  `json:"hash"`
	Name       string  `json:"name"`
	Size       int64   `json:"size"`
	Progress   float64 `json:"progress"`
	DLSpeed    int64   `json:"dlspeed"`
	UPSpeed    int64   `json:"upspeed"`
	Ratio      float64 `json:"ratio"`
	State      string  `json:"state"`
	ETA        int64   `json:"eta"`
	AddedOn    int64   `json:"added_on"`
	Uploaded   int64   `json:"uploaded"`
	Downloaded int64   `json:"downloaded"`
	Category   string  `json:"category"`
	NumSeeds   int     `json:"num_seeds"`
	NumLeechs  int     `json:"num_leechs"`
	Tracker    string  `json:"tracker"`
}

// ServerState is the global state block from sync/maindata.
type ServerState struct {
	DLInfoSpeed      int64  `json:"dl_info_speed"`
	UPInfoSpeed      int64  `json:"up_info_speed"`
	DLInfoData       int64  `json:"dl_info_data"`
	UPInfoData       int64  `json:"up_info_data"`
	AllTimeDL        int64  `json:"alltime_dl"`
	AllTimeUL        int64  `json:"alltime_ul"`
	GlobalRatio      string `json:"global_ratio"`
	FreeSpaceOnDisk  int64  `json:"free_space_on_disk"`
	ConnectionStatus string `json:"connection_status"`
	DHTNodes         int    `json:"dht_nodes"`
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
func (c *Client) PauseAll(ctx context.Context) error {
	form := url.Values{"hashes": {"all"}}
	err := c.call(ctx, http.MethodPost, "/api/v2/torrents/stop", form, nil)
	if errors.Is(err, errNotFound) {
		err = c.call(ctx, http.MethodPost, "/api/v2/torrents/pause", form, nil)
	}
	return err
}

// ResumeAll starts every torrent.
func (c *Client) ResumeAll(ctx context.Context) error {
	form := url.Values{"hashes": {"all"}}
	err := c.call(ctx, http.MethodPost, "/api/v2/torrents/start", form, nil)
	if errors.Is(err, errNotFound) {
		err = c.call(ctx, http.MethodPost, "/api/v2/torrents/resume", form, nil)
	}
	return err
}

// Shutdown asks qBittorrent to exit.
func (c *Client) Shutdown(ctx context.Context) error {
	return c.call(ctx, http.MethodPost, "/api/v2/app/shutdown", url.Values{}, nil)
}

// StateKind classifies a qBittorrent state string into seeding, leeching, paused, checking, error or stalled.
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
