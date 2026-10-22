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

// Package api serves the JSON API, the SSE stream and the embedded frontend.
package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"pmss-fleet-manager/internal/collect"
	"pmss-fleet-manager/internal/probe"
	"pmss-fleet-manager/internal/qbt"
	"pmss-fleet-manager/internal/store"
	"pmss-fleet-manager/internal/web"
)

// Server holds the dependencies for the handlers.
const (
	schemeHTTP      = "http://"
	schemeHTTPS     = "https://"
	hdrContentType  = "Content-Type"
	hdrCacheControl = "Cache-Control"
	noStore         = "no-store"
)

type Server struct {
	st   *store.Store
	m    *collect.Manager
	auth string // "user:pass" or ""
	// basePath is the URL prefix every route is served under when a reverse
	// proxy forwards the dashboard from a sub-path unchanged ("" or "/fleet";
	// see NormalizeBasePath). The SPA derives the same prefix from its own URL.
	basePath string
	mux      *http.ServeMux
	// instance identifies this process; the UI reloads itself when it
	// changes after an SSE reconnect so a rebuilt server never runs stale JS.
	instance string
	// settingsMu serialises the read-merge-save of PUT /api/settings so two
	// tabs saving at once cannot lose a field or leave memory and DB apart.
	settingsMu sync.Mutex

	license, thirdParty []byte
	version             string

	// allowedHosts, when set, is the only set of Host header values served.
	// Without it a page on an attacker's domain whose DNS is rebound to the
	// dashboard's IP is same-origin from the browser's point of view and
	// passes the Origin check with full API access on a no-auth loopback
	// install. Hostnames only, lower-case, no port.
	allowedHosts map[string]bool
	// allowIPHosts additionally serves any Host header that is an IP
	// literal. It is set for a wildcard bind, whose addresses cannot be
	// listed; a rebinding page always arrives with its own hostname, never
	// with an IP literal.
	allowIPHosts bool

	// beforeBoxUpdate, when set (tests only), runs after handleUpdateBox has
	// read and validated the record and before the update: the window a
	// concurrent edit races into.
	beforeBoxUpdate func()
	// sseSlots caps concurrent event streams (each holds a goroutine and a
	// bus subscription); a browser needs one per tab.
	sseSlots chan struct{}
	// authFails throttles failed basic-auth attempts per client address.
	authMu    sync.Mutex
	authFails map[string]*authFail
}

// authFail is one client's run of failed basic-auth attempts.
type authFail struct {
	count int
	until time.Time
}

const (
	maxSSEStreams   = 32
	authFreeTries   = 5                // failures before the throttle starts
	authThrottle    = 30 * time.Second // added per failure past the free ones, capped
	authThrottleMax = 10 * time.Minute
)

// authThrottled reports whether a failed-auth client must wait, and
// records a failure or a success. Keyed by the connection's address, so
// behind a reverse proxy it is one shared budget, which is what an
// internet-facing deployment without the recommended proxy-side limiting
// needs most.
func (s *Server) authThrottled(addr string, failed bool) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		host = addr
	}
	s.authMu.Lock()
	defer s.authMu.Unlock()
	if s.authFails == nil || len(s.authFails) > 4096 {
		s.authFails = map[string]*authFail{}
	}
	f := s.authFails[host]
	now := time.Now()
	if f != nil && now.Before(f.until) {
		return true // the wait applies to every attempt, right password included: that is what slows guessing
	}
	if !failed {
		delete(s.authFails, host)
		return false
	}
	if f == nil {
		f = &authFail{}
		s.authFails[host] = f
	}
	f.count++
	if f.count > authFreeTries {
		f.until = now.Add(min(time.Duration(f.count-authFreeTries)*authThrottle, authThrottleMax))
		return true
	}
	return false
}

// SetAllowedHosts restricts served Host headers to the given hostnames
// (ports are ignored), plus any IP literal when anyIP is set. An empty list
// without anyIP disables the check.
func (s *Server) SetAllowedHosts(hosts []string, anyIP bool) {
	s.allowedHosts = nil
	s.allowIPHosts = anyIP
	if anyIP {
		s.allowedHosts = map[string]bool{}
	}
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" {
			continue
		}
		if s.allowedHosts == nil {
			s.allowedHosts = map[string]bool{}
		}
		s.allowedHosts[h] = true
	}
}

// hostAllowed checks the request's Host header against the allow-list.
func (s *Server) hostAllowed(hostport string) bool {
	if s.allowedHosts == nil {
		return true
	}
	h := strings.ToLower(hostport)
	if hp, _, err := net.SplitHostPort(hostport); err == nil {
		h = strings.ToLower(hp)
	}
	h = strings.Trim(h, "[]")
	return s.allowedHosts[h] || (s.allowIPHosts && net.ParseIP(h) != nil)
}

// csp is the Content-Security-Policy for the embedded UI: own origin only,
// plus Google Fonts for the stylesheet and font files. Inline styles are
// used for widths and the like, hence 'unsafe-inline' on styles only.
const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"font-src 'self'; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// New builds the handler tree. basePath is validated with NormalizeBasePath;
// an invalid value is a programming error (main validates user input first).
func New(st *store.Store, m *collect.Manager, basicAuth string, basePath string) *Server {
	bp, err := NormalizeBasePath(basePath)
	if err != nil {
		panic("api.New: " + err.Error())
	}
	s := &Server{st: st, m: m, auth: basicAuth, basePath: bp, mux: http.NewServeMux(),
		instance: strconv.FormatInt(time.Now().UnixNano(), 36), sseSlots: make(chan struct{}, maxSSEStreams)}
	s.routes()
	return s
}

// NormalizeBasePath turns the -base-path flag into the form routes are
// registered under: "" for the root, otherwise a clean absolute path without
// a trailing slash ("/fleet", "/user/fleet"). A missing leading slash is
// added; "/" means the root. Characters that would be read as a pattern
// wildcard or that cannot appear in a request path are rejected rather than
// left to panic inside ServeMux.
func NormalizeBasePath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if strings.ContainsAny(p, "{}?# \t\n\r") {
		return "", fmt.Errorf("base path %q may not contain braces, '?', '#' or whitespace", p)
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("base path %q contains a control character", p)
		}
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = path.Clean(p)
	if p == "/" {
		return "", nil
	}
	return p, nil
}

// boxRoute is the per-box API prefix.
const boxRoute = "/api/boxes/{id}"

func (s *Server) routes() {
	bp := s.basePath
	mux := s.mux
	mux.HandleFunc("GET "+bp+"/api/state", s.handleState)
	mux.HandleFunc("GET "+bp+"/api/events", s.handleEvents)
	mux.HandleFunc("GET "+bp+"/api/boxes", s.handleListBoxes)
	mux.HandleFunc("POST "+bp+"/api/boxes", s.handleCreateBox)
	mux.HandleFunc("GET "+bp+boxRoute, s.handleGetBox)
	mux.HandleFunc("GET "+bp+"/api/torrents", s.handleTorrents)
	mux.HandleFunc("PUT "+bp+boxRoute, s.handleUpdateBox)
	mux.HandleFunc("DELETE "+bp+boxRoute, s.handleDeleteBox)
	mux.HandleFunc("POST "+bp+boxRoute+"/probe", s.handleProbe) // runs the probe over SSH: POST so the CSRF check applies
	mux.HandleFunc("POST "+bp+boxRoute+"/refresh", s.handleRefresh)
	mux.HandleFunc("POST "+bp+boxRoute+"/du", s.handleDu)
	mux.HandleFunc("POST "+bp+boxRoute+"/qbt/{action}", s.handleQbtAction)
	mux.HandleFunc("POST "+bp+boxRoute+"/qbt/torrents/{hash}/{action}", s.handleTorrentAction)
	mux.HandleFunc("POST "+bp+boxRoute+"/qbt/torrents", s.handleAddTorrents) // literal segment wins over {action}
	mux.HandleFunc("POST "+bp+boxRoute+"/qbt/discovery", s.handleSetDiscovery)
	mux.HandleFunc("POST "+bp+boxRoute+"/containers/{cid}/{action}", s.handleContainerAction)
	mux.HandleFunc("POST "+bp+boxRoute+"/apps/watch", s.handleAppWatch)
	mux.HandleFunc("POST "+bp+boxRoute+"/logs/read", s.handleReadLog)                   // runs tail over SSH: POST so the CSRF check applies
	mux.HandleFunc("POST "+bp+boxRoute+"/containers/{cid}/logs", s.handleContainerLogs) // literal segment wins over {action}
	mux.HandleFunc("POST "+bp+"/api/detect", s.handleDetect)
	mux.HandleFunc("GET "+bp+"/api/alerts", s.handleAlerts)
	mux.HandleFunc("POST "+bp+"/api/alerts/ack-all", s.handleAckAll)
	mux.HandleFunc("POST "+bp+"/api/alerts/{id}/ack", s.handleAck)
	mux.HandleFunc("DELETE "+bp+"/api/alerts/{id}", s.handleDeleteAlert)
	mux.HandleFunc("POST "+bp+"/api/alerts/clear-resolved", s.handleClearResolved)
	mux.HandleFunc("GET "+bp+"/api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT "+bp+"/api/settings", s.handleSetSettings)
	// AGPL §13: the license, the third-party notices and the Corresponding
	// Source offer are served WITHOUT basic auth (see isPublicPath).
	mux.HandleFunc("GET "+bp+"/license", s.serveLegalText(func() []byte { return s.license }, "license text unavailable"))
	mux.HandleFunc("GET "+bp+"/third-party-licenses", s.serveLegalText(func() []byte { return s.thirdParty }, "third-party notices unavailable"))
	mux.HandleFunc("GET "+bp+"/source", s.handleSource)
	mux.HandleFunc("GET "+bp+"/api/services", s.handleServices)
	mux.HandleFunc("POST "+bp+"/api/services/sync", s.handleServicesSync)

	static, _ := fs.Sub(web.FS, "static")
	fileServer := http.FileServer(http.FS(static))
	mux.Handle("GET "+bp+"/static/", http.StripPrefix(bp+"/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") { // no directory listings, no second copy of the page
			http.NotFound(w, r)
			return
		}
		if strings.HasSuffix(r.URL.Path, ".woff2") {
			// Not in Go's built-in MIME table, and the embedded files carry no
			// modification time to revalidate against, so a day's caching
			// instead of a 90 KB re-download per page view.
			w.Header().Set(hdrContentType, "font/woff2")
			w.Header().Set(hdrCacheControl, "public, max-age=86400")
		} else {
			w.Header().Set(hdrCacheControl, "no-cache")
		}
		fileServer.ServeHTTP(w, r)
	})))
	// The page lives only at <base-path>/ (ServeMux redirects <base-path> to
	// it). index.html and app.js refer to static/ and api/ relative to that
	// URL, so no prefix is written into the served files.
	mux.HandleFunc("GET "+bp+"/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != bp+"/" {
			http.NotFound(w, r)
			return
		}
		b, err := fs.ReadFile(static, "index.html")
		if err != nil {
			http.Error(w, "index missing", 500)
			return
		}
		w.Header().Set(hdrContentType, "text/html; charset=utf-8")
		w.Header().Set(hdrCacheControl, noStore)
		w.Write(b)
	})
}

// ServeHTTP applies auth and security headers then dispatches.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.hostAllowed(r.Host) {
		http.Error(w, "unexpected Host header; start with -allowed-hosts to serve this name", http.StatusMisdirectedRequest)
		return
	}
	if s.auth != "" && !s.isPublicPath(r.URL.Path) {
		u, p, ok := r.BasicAuth()
		good := ok && subtle.ConstantTimeCompare([]byte(u+":"+p), []byte(s.auth)) == 1
		if s.authThrottled(r.RemoteAddr, !good) {
			http.Error(w, "too many failed logins from this address; try again later", http.StatusTooManyRequests)
			return
		}
		if !good {
			w.Header().Set("WWW-Authenticate", `Basic realm="pmss-fleet-manager"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", csp)
	if strings.HasPrefix(r.URL.Path, s.basePath+"/api/") && r.Method != http.MethodGet {
		if err := csrfCheck(r); err != nil {
			http.Error(w, err.Error(), http.StatusForbidden)
			return
		}
		// The server has no WriteTimeout (the SSE stream must live), so
		// bound the body read here: a client trickling a capped body must
		// not hold a handler forever. Two minutes covers a 40 MB torrent
		// upload on a slow uplink.
		_ = http.NewResponseController(w).SetReadDeadline(time.Now().Add(2 * time.Minute))
	}
	s.mux.ServeHTTP(w, r)
}

// MutationHeader must be present on every mutating API request. An HTML
// form cannot set a custom header and a cross-origin script would need a
// CORS preflight the server never grants, so this stops CSRF on its own,
// including from a plain-http page on the dashboard's own hostname, which
// the scheme-agnostic Origin check below cannot tell apart from the page.
const MutationHeader = "X-PMSS-Request"

// csrfCheck applies the three independent CSRF guards to a mutating
// request: the custom header, Sec-Fetch-Site (checked whenever a browser
// sends it, not only when Origin is absent) and the Origin host.
func csrfCheck(r *http.Request) error {
	if r.Header.Get(MutationHeader) == "" {
		return errors.New("missing " + MutationHeader + " header: mutating requests are accepted from the dashboard page only")
	}
	if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
		return errors.New("cross-site request rejected")
	}
	if origin := r.Header.Get("Origin"); origin != "" && !sameOrigin(origin, r.Host) {
		return errors.New("cross-origin request rejected")
	}
	return nil
}

// sameOrigin accepts an Origin of this host over http or https. The
// dashboard never terminates TLS itself, so the scheme the browser saw is
// not knowable here and both are allowed; any other scheme, or an opaque
// "null" origin, is not this page.
func sameOrigin(origin, host string) bool {
	scheme, rest, ok := strings.Cut(origin, "://")
	if !ok || (scheme != "http" && scheme != "https") {
		return false
	}
	return strings.EqualFold(rest, host)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	// Encode first: a value json cannot represent (NaN/Inf from a bad
	// number on one box) must be a visible 500, not a 200 with an empty body.
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set(hdrContentType, "application/json")
	w.Header().Set(hdrCacheControl, noStore)
	w.WriteHeader(code)
	w.Write(append(b, '\n'))
}

// writeErr sends an error message. It is clipped: action errors can carry
// output from the box's own docker daemon or start command, and the text
// ends up in a toast.
func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": clipStr(err.Error(), 600)})
}

func idParam(r *http.Request) (int64, error) {
	return strconv.ParseInt(r.PathValue("id"), 10, 64)
}

// Totals are the fleet-wide headline numbers.
type Totals struct {
	Count      int     `json:"count"`
	Online     int     `json:"online"`
	DiskUsed   int64   `json:"disk_used"`
	DiskQuota  int64   `json:"disk_quota"`
	TrafficOut int64   `json:"traffic_out"`
	TrafficIn  int64   `json:"traffic_in"`
	UpBps      float64 `json:"up_bps"`
	DownBps    float64 `json:"down_bps"`
	Torrents   int     `json:"torrents"`
	Seeding    int     `json:"seeding"`
	Containers int     `json:"containers"`
	NextRenew  string  `json:"next_renewal"`
	LastPoll   string  `json:"last_poll"`
	// From the Pulsed Media customer API when configured: monthly cost of
	// the matched boxes and of every active service on the account.
	MonthlyCost      float64 `json:"monthly_cost"`
	MonthlyCostFleet float64 `json:"monthly_cost_fleet"`
	Currency         string  `json:"currency,omitempty"`
}

func totals(states []collect.BoxState) Totals {
	var t Totals
	seenSvc := map[int64]bool{}
	var last time.Time
	for _, b := range states {
		t.Count++
		if b.Status == "online" || b.Status == "degraded" {
			t.Online++
		}
		t.DiskUsed += b.Disk.UsedBytes
		t.DiskQuota += b.Disk.QuotaBytes
		t.TrafficOut += b.Traffic.OutMonthBytes
		t.TrafficIn += b.Traffic.InMonthBytes
		t.UpBps += b.Rates.UpBps
		t.DownBps += b.Rates.DownBps
		t.Torrents += b.Qbt.Total
		t.Seeding += b.Qbt.Seeding
		t.Containers += b.Docker.Running
		if r := b.EffectiveRenewal(); r != "" && (t.NextRenew == "" || r < t.NextRenew) {
			t.NextRenew = r
		}
		if b.Service != nil && collect.ServiceActive(b.Service.Status) && !seenSvc[b.Service.ID] {
			seenSvc[b.Service.ID] = true // two boxes on one service count it once; inactive ones are history
			t.MonthlyCost += b.Service.Monthly
			t.Currency = b.Service.Currency
		}
		if b.LastPoll.After(last) {
			last = b.LastPoll
		}
	}
	if !last.IsZero() {
		t.LastPoll = last.UTC().Format(time.RFC3339)
	}
	return t
}

func (s *Server) handleState(w http.ResponseWriter, r *http.Request) {
	states, rev := s.m.Snapshot()
	sums := make([]collect.BoxState, len(states))
	for i, st := range states {
		sums[i] = st.Summary()
	}
	unacked, _ := s.st.CountUnacked(r.Context())
	tot := totals(states)
	svc := s.m.Services()
	tot.MonthlyCostFleet = svc.MonthlyCost
	if tot.Currency == "" {
		tot.Currency = svc.Currency
	}
	writeJSON(w, 200, map[string]any{
		"boxes":          sums,
		"rev":            rev, // see collect.Manager.Snapshot: lets the client merge this with the events around it
		"totals":         tot,
		"alerts_unacked": unacked,
		"settings":       s.m.Settings().Public(),
		"instance":       s.instance,
		"version":        s.Version(),
		"source_url":     s.sourceURL(),
	})
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", 500)
		return
	}
	select {
	case s.sseSlots <- struct{}{}:
		defer func() { <-s.sseSlots }()
	default:
		http.Error(w, "too many open event streams; close some dashboard tabs", http.StatusServiceUnavailable)
		return
	}
	// Subscribed before the headers go out: the client resyncs when the
	// stream opens, and every change from that moment on must reach it.
	ch, cancel := s.m.Bus().Subscribe()
	defer cancel()
	w.Header().Set(hdrContentType, "text/event-stream")
	w.Header().Set(hdrCacheControl, noStore)
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()
	ping := time.NewTicker(25 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case ev, ok := <-ch:
			if !ok {
				return // dropped as a slow subscriber: the client reconnects and resyncs
			}
			b, err := json.Marshal(ev)
			if err != nil {
				// An unencodable state (should be impossible after sanitising)
				// must not become an empty frame the client cannot parse.
				fmt.Fprint(w, ": skipped unencodable event\n\n")
				fl.Flush()
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.Type, b)
			fl.Flush()
		}
	}
}

// boxInput is the writable box shape, including secrets.
type boxInput struct {
	Name          string `json:"name"`
	Host          string `json:"host"`
	Port          int    `json:"port"`
	User          string `json:"user"`
	AuthMethod    string `json:"auth_method"`
	KeyPath       string `json:"key_path"`
	Password      string `json:"password"`
	Type          string `json:"type"`
	Plan          string `json:"plan"`
	Datacenter    string `json:"datacenter"`
	Renewal       string `json:"renewal"`
	QbtEnabled    bool   `json:"qbt_enabled"`
	QbtPort       int    `json:"qbt_port"`
	QbtUser       string `json:"qbt_user"`
	QbtPass       string `json:"qbt_pass"`
	QbtStartCmd   string `json:"qbt_start_cmd"`
	DockerEnabled bool   `json:"docker_enabled"`
	DuEnabled     *bool  `json:"du_enabled"`
	Enabled       *bool  `json:"enabled"`
	HostKey       string `json:"host_key"`
	// ID is set by the edit wizard so detect can use the stored secrets
	// when the password fields were left blank ("blank keeps saved").
	ID int64 `json:"id"`
	// ForgetHostKey drops the pinned SSH host key so the next connect
	// re-pins (trust on first use); needed after a box is reinstalled.
	ForgetHostKey bool `json:"forget_host_key"`
	// ClearQbtPass removes the saved WebUI password (a blank field keeps it).
	ClearQbtPass bool `json:"clear_qbt_pass"`
	// Revision is the record revision the edit was based on (from GET);
	// required on PUT, and a stale one is refused with 409.
	Revision *int64 `json:"revision"`
}

func (in boxInput) validate() error {
	in.Host = strings.TrimSpace(in.Host)
	if in.Host == "" || !hostChars(in.Host) {
		return errors.New("hostname is required: letters, digits, dots and dashes only, not starting with a dot or dash")
	}
	if u := strings.TrimSpace(in.User); u == "" || !userChars(u) {
		return errors.New("username is required: letters, digits, dot, dash and underscore only")
	}
	if in.Port < 0 || in.Port > 65535 {
		return errors.New("invalid port")
	}
	if in.QbtPort < 0 || in.QbtPort > 65535 {
		return errors.New("qbt_port must be 0..65535")
	}
	switch in.Type {
	case "", "seedbox", "storage":
	default:
		return errors.New("type must be seedbox or storage")
	}
	switch in.AuthMethod {
	case "", "key", "agent", "password":
	default:
		return errors.New("auth_method must be key, agent or password")
	}
	if in.Renewal != "" {
		if _, err := time.Parse("2006-01-02", in.Renewal); err != nil {
			return errors.New("renewal must be YYYY-MM-DD")
		}
	}
	return nil
}

func (in boxInput) apply(b *store.Box) {
	b.Name = strings.TrimSpace(in.Name)
	b.Host = strings.TrimSpace(in.Host)
	b.Port = in.Port
	if b.Port == 0 {
		b.Port = 22
	}
	b.User = strings.TrimSpace(in.User)
	b.AuthMethod = in.AuthMethod
	b.KeyPath = strings.TrimSpace(in.KeyPath)
	b.Password = in.Password
	b.Type = in.Type
	if b.Type == "" {
		b.Type = "seedbox"
	}
	b.Plan, b.Datacenter, b.Renewal = strings.TrimSpace(in.Plan), strings.TrimSpace(in.Datacenter), in.Renewal
	b.QbtEnabled, b.QbtPort, b.QbtUser, b.QbtPass, b.QbtStartCmd = in.QbtEnabled, in.QbtPort, in.QbtUser, in.QbtPass, in.QbtStartCmd
	b.DockerEnabled = in.DockerEnabled
	if in.DuEnabled != nil {
		b.DuEnabled = *in.DuEnabled
	}
	if in.Enabled != nil {
		b.Enabled = *in.Enabled
	}
	if in.HostKey != "" {
		b.HostKey = in.HostKey
	}
	if b.Name == "" {
		b.Name = strings.SplitN(b.Host, ".", 2)[0]
		if net.ParseIP(b.Host) != nil { // "192" is no name for 192.0.2.1
			b.Name = b.Host
		}
	}
}

// endpointChanged compares a stored box with an edited one: hostChanged
// covers host (DNS is case-insensitive) and port, which is what the SSH host
// key is bound to; userChanged is additionally what stored secrets are bound
// to.
func endpointChanged(stored, edited *store.Box) (hostChanged, userChanged bool) {
	hostChanged = !strings.EqualFold(stored.Host, edited.Host) || stored.Port != edited.Port
	userChanged = stored.User != edited.User
	return
}

func decode(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	return dec.Decode(v)
}

// decodeStrict reads a small JSON body (4 KiB) and refuses unknown fields:
// for the action endpoints whose whole input is a few named fields.
func decodeStrict(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("bad body: %v", err)
	}
	return nil
}

func (s *Server) handleListBoxes(w http.ResponseWriter, r *http.Request) {
	boxes, err := s.st.ListBoxes(r.Context())
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, boxes)
}

func (s *Server) handleCreateBox(w http.ResponseWriter, r *http.Request) {
	var in boxInput
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, 400, err)
		return
	}
	b := &store.Box{Enabled: true, DuEnabled: true}
	in.apply(b)
	if b.AuthMethod == "key" || b.AuthMethod == "agent" {
		b.Password = "" // typed before the auth method was switched; not used, not kept
	}
	if err := s.m.AddBox(r.Context(), b); err != nil {
		writeErr(w, 500, err)
		return
	}
	b.HasPassword, b.HasQbtPass = b.Password != "", b.QbtPass != ""
	writeJSON(w, 201, b)
}

// torrentsBox is one box in the fleet-wide torrent list: identity and
// status, plus the full qBittorrent view including its torrents.
type torrentsBox struct {
	ID           int64            `json:"id"`
	Name         string           `json:"name"`
	Status       string           `json:"status"`
	StatusReason string           `json:"status_reason,omitempty"`
	Qbt          collect.QbtState `json:"qbt"`
}

// handleTorrents returns every box that has qBittorrent monitoring on,
// with its torrents, for the combined view. Boxes without it are left out.
func (s *Server) handleTorrents(w http.ResponseWriter, r *http.Request) {
	states := s.m.States()
	out := make([]torrentsBox, 0, len(states))
	for _, st := range states {
		if !st.Qbt.Enabled {
			continue
		}
		out = append(out, torrentsBox{ID: st.ID, Name: st.Name, Status: st.Status, StatusReason: st.StatusReason, Qbt: st.Qbt})
	}
	writeJSON(w, 200, map[string]any{"boxes": out})
}

func (s *Server) handleGetBox(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	b, err := s.st.GetBox(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, err)
		return
	}
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	st, _ := s.m.State(id)
	writeJSON(w, 200, map[string]any{"box": b, "state": st})
}

func (s *Server) handleUpdateBox(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	b, err := s.st.GetBox(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, err)
		return
	}
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	var in boxInput
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, 400, err)
		return
	}
	if in.Revision == nil {
		writeErr(w, 400, errors.New("revision is required: send the box's current revision from GET /api/boxes/{id}"))
		return
	}
	if *in.Revision != b.Revision {
		// A fast answer for the common case; the store's compare-and-set is
		// what makes it airtight against a concurrent edit.
		writeJSON(w, http.StatusConflict, map[string]any{"error": store.ErrStale.Error(), "box": b})
		return
	}
	stored := *b
	in.apply(b)
	b.Revision = *in.Revision
	// The rules shared with detect (endpointChanged): the host-key pin
	// belongs to host:port and a supplied key never replaces an existing
	// pin; stored secrets are only ever used for the endpoint they were
	// saved for, so a changed host/port/user with a blank password is
	// refused rather than sent to the new host on trust-on-first-use.
	// Both rules are evaluated against the revision the edit names, and the
	// update applies only if the row still is that revision.
	hostChanged, userChanged := endpointChanged(&stored, b)
	if hostChanged || userChanged {
		if b.Password == "" && stored.Password != "" && (b.AuthMethod == "password" || b.AuthMethod == "") {
			writeErr(w, 400, errors.New("re-enter the SSH password to move this box to a different host, port or user; the saved one is only used for the saved endpoint"))
			return
		}
		if b.QbtPass == "" && stored.QbtPass != "" && !in.ClearQbtPass {
			writeErr(w, 400, errors.New("re-enter the WebUI password (or tick remove) to move this box to a different host, port or user; the saved one is only used for the saved endpoint"))
			return
		}
	}
	// The pin is decided inside the update statement (store.Clears): forget
	// clears it, a changed host:port replaces it with the wizard's fresh
	// detect result (or nothing), otherwise a pin already in the row is
	// kept, even one a poll stored while this edit was open, and an empty
	// one is filled from the wizard.
	b.HostKey = in.HostKey
	// Secrets: a blank field keeps the saved one, so clearing is explicit.
	// The SSH password is dropped when the box no longer authenticates with
	// one; the WebUI password on request, unless a new one was typed.
	clears := store.Clears{
		Password:       in.AuthMethod == "key" || in.AuthMethod == "agent",
		QbtPass:        in.ClearQbtPass && in.QbtPass == "",
		ForgetHostKey:  in.ForgetHostKey,
		ReplaceHostKey: hostChanged,
	}
	if s.beforeBoxUpdate != nil {
		s.beforeBoxUpdate()
	}
	if err := s.m.UpdateBox(r.Context(), b, clears); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			writeErr(w, 404, err)
		case errors.Is(err, store.ErrStale):
			fresh, _ := s.st.GetBox(r.Context(), id)
			writeJSON(w, http.StatusConflict, map[string]any{"error": err.Error(), "box": fresh})
		default:
			writeErr(w, 500, err)
		}
		return
	}
	fresh, _ := s.st.GetBox(r.Context(), id)
	writeJSON(w, 200, fresh)
}

func (s *Server) handleDeleteBox(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.m.RemoveBox(r.Context(), id); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleProbe(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	secs, err := s.m.RawProbe(ctx, id)
	if err != nil {
		writeErr(w, actionCode(err), err)
		return
	}
	writeJSON(w, 200, secs)
}

// actionCode maps a box action's failure to a status: the box is missing
// (404), its polling is off or it has not been probed yet (409), or the
// box itself did not cooperate (502).
func actionCode(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return 404
	case errors.Is(err, collect.ErrDisabled), errors.Is(err, collect.ErrNotReady):
		return 409
	}
	return 502
}

func (s *Server) handleRefresh(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.m.Refresh(id); err != nil {
		writeErr(w, actionCode(err), err)
		return
	}
	writeJSON(w, 202, map[string]bool{"ok": true})
}

func (s *Server) handleDu(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.m.RunDu(id); err != nil {
		writeErr(w, actionCode(err), err)
		return
	}
	writeJSON(w, 202, map[string]bool{"ok": true})
}

func (s *Server) handleQbtAction(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	action := r.PathValue("action")
	switch action {
	case "pause", "resume", "start", "stop", "restart", "install", "autostart":
	default:
		writeErr(w, 400, fmt.Errorf("unknown action %q", action))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 6*time.Minute)
	defer cancel()
	out, err := s.m.QbtAction(ctx, id, action)
	if err != nil {
		// Same clip as writeErr: the start command's stderr rides in err.
		writeJSON(w, actionCode(err), map[string]string{"error": clipStr(err.Error(), 600), "output": clipStr(out, 4000)})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "output": out})
}

// Limits on one add request. A .torrent is normally tens of kilobytes; the
// per-file cap leaves room for the piece tables of multi-terabyte and hybrid
// v1+v2 torrents, and the body cap (framing included) bounds what one
// request pushes through the box's WebUI within the action timeout.
const (
	addMaxBody    = 40 << 20
	addMaxFile    = 8 << 20
	addMaxFiles   = 20
	addMaxURLs    = 50
	addMaxURLLen  = 4000
	addMaxCatLen  = 100
	addMaxNameLen = 200
)

// validTorrentURL accepts magnet links and http(s) URLs to .torrent files,
// which is what qBittorrent's torrents/add fetches itself.
func validTorrentURL(u string) bool {
	if len(u) > addMaxURLLen || strings.ContainsAny(u, " \t\r\n\x00") {
		return false
	}
	l := strings.ToLower(u)
	return strings.HasPrefix(l, "magnet:?") || strings.HasPrefix(l, schemeHTTP) || strings.HasPrefix(l, schemeHTTPS)
}

// looksLikeTorrent is a cheap shape check (a bencoded dictionary) so junk
// never reaches the WebUI; qBittorrent does the real parse.
func looksLikeTorrent(b []byte) bool {
	return len(b) > 2 && b[0] == 'd' && b[len(b)-1] == 'e'
}

// addRequestFromForm validates a multipart add: "urls" (one per line),
// "torrents" files, "category", "stopped".
func addRequestFromForm(r *http.Request) (qbt.AddRequest, error) {
	var req qbt.AddRequest
	r.Body = http.MaxBytesReader(nil, r.Body, addMaxBody)
	if err := r.ParseMultipartForm(1 << 20); err != nil {
		return req, fmt.Errorf("bad upload: %v", err)
	}
	for _, line := range strings.Split(r.FormValue("urls"), "\n") {
		u := strings.TrimSpace(line)
		if u == "" {
			continue
		}
		if !validTorrentURL(u) {
			return req, fmt.Errorf("not a magnet or http(s) link: %q", clipStr(u, 60))
		}
		req.URLs = append(req.URLs, u)
	}
	if len(req.URLs) > addMaxURLs {
		return req, fmt.Errorf("at most %d links per request", addMaxURLs)
	}
	files := r.MultipartForm.File["torrents"]
	if len(files) > addMaxFiles {
		return req, fmt.Errorf("at most %d files per request", addMaxFiles)
	}
	for _, fh := range files {
		if fh.Size > addMaxFile {
			return req, fmt.Errorf("%s: larger than %d MB", clipStr(fh.Filename, 60), addMaxFile>>20)
		}
		f, err := fh.Open()
		if err != nil {
			return req, err
		}
		data, err := io.ReadAll(io.LimitReader(f, addMaxFile+1))
		f.Close()
		if err != nil {
			return req, err
		}
		if !looksLikeTorrent(data) {
			return req, fmt.Errorf("%s is not a .torrent file", clipStr(fh.Filename, 60))
		}
		name := clipStr(path.Base(strings.ReplaceAll(fh.Filename, "\\", "/")), addMaxNameLen)
		if name == "" || name == "." || name == "/" {
			name = "upload.torrent"
		}
		req.Files = append(req.Files, qbt.TorrentFile{Name: name, Data: data})
	}
	req.Category = strings.TrimSpace(r.FormValue("category"))
	if utf8.RuneCountInString(req.Category) > addMaxCatLen || strings.ContainsAny(req.Category, "\x00\n\r") {
		return req, errors.New("invalid category")
	}
	switch r.FormValue("stopped") {
	case "true", "1", "on":
		req.Stopped = true
	}
	if len(req.URLs) == 0 && len(req.Files) == 0 {
		return req, errors.New("paste at least one magnet or link, or choose a .torrent file")
	}
	return req, nil
}

func clipStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut := n
	for cut > 0 && !utf8.RuneStart(s[cut]) { // never split a rune
		cut--
	}
	return s[:cut] + "…"
}

// handleSetDiscovery takes {"dht":bool,"pex":bool,"lsd":bool} (any subset).
func (s *Server) handleSetDiscovery(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var want map[string]bool
	if err := decodeStrict(r, &want); err != nil {
		writeErr(w, 400, err)
		return
	}
	for k := range want {
		if !collect.DiscoveryKeys[k] {
			writeErr(w, 400, fmt.Errorf("unknown setting %q", k))
			return
		}
	}
	if len(want) == 0 {
		writeErr(w, 400, errors.New("nothing to change"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.m.SetDiscovery(ctx, id, want); err != nil {
		writeErr(w, actionCode(err), err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleAddTorrents(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	req, err := addRequestFromForm(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	res, err := s.m.AddTorrents(ctx, id, req)
	if err != nil {
		code := actionCode(err)
		if errors.Is(err, qbt.ErrBadTorrent) {
			code = 422
		} else if errors.Is(err, qbt.ErrAddRejected) {
			code = 409
		}
		writeErr(w, code, err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "links": len(req.URLs), "files": len(req.Files),
		"counted": res.Counted, "added": res.Added, "failed": res.Failed, "pending": res.Pending})
}

// hostChars accepts a DNS-shaped host name; the UI builds https links from
// it, so '@', '/', '#', '?' and quotes must never get in.
func hostChars(h string) bool {
	if h == "" || len(h) > 253 || h[0] == '.' || h[0] == '-' { // the first label is the box's default name
		return false
	}
	for _, r := range h {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '-' {
			return false
		}
	}
	return true
}

// userChars accepts a Unix account name as used in the user-<name> path.
func userChars(u string) bool {
	if len(u) > 64 {
		return false
	}
	for _, r := range u {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '.' && r != '-' && r != '_' {
			return false
		}
	}
	return true
}

// validTorrentHash accepts a v1 (40 hex) or v2 (64 hex) info-hash, in either
// case; anything else never reaches the WebUI.
func validTorrentHash(h string) bool { return qbt.ValidHash(h) }

func (s *Server) handleTorrentAction(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	hash, action := strings.ToLower(r.PathValue("hash")), r.PathValue("action")
	if !qbt.TorrentActions[action] {
		writeErr(w, 400, fmt.Errorf("unknown action %q", action))
		return
	}
	if !validTorrentHash(hash) {
		writeErr(w, 400, errors.New("invalid torrent hash"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.m.TorrentAction(ctx, id, hash, action); err != nil {
		writeErr(w, actionCode(err), err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

// handleReadLog takes {"path": "<relative path from the box's log list>"}
// and returns the file's last lines.
func (s *Server) handleReadLog(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var in struct {
		Path string `json:"path"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if !probe.ValidLogPath(in.Path) {
		writeErr(w, 400, errors.New("invalid log path"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	text, err := s.m.ReadLog(ctx, id, in.Path)
	if err != nil {
		writeErr(w, actionCode(err), err)
		return
	}
	writeJSON(w, 200, map[string]any{"path": in.Path, "text": text, "lines": collect.LogTailLines})
}

func (s *Server) handleContainerLogs(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	cid := r.PathValue("cid")
	if !validContainerID(cid) {
		writeErr(w, 400, errors.New("invalid container id"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	text, err := s.m.ContainerLogs(ctx, id, cid)
	if err != nil {
		writeErr(w, actionCode(err), err)
		return
	}
	writeJSON(w, 200, map[string]any{"id": cid, "text": text, "lines": collect.LogTailLines})
}

// validContainerID accepts a full or abbreviated hex container id.
func validContainerID(cid string) bool {
	return len(cid) >= 12 && len(cid) <= 64 && strings.Trim(cid, "0123456789abcdef") == ""
}

func (s *Server) handleContainerAction(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	cid, action := r.PathValue("cid"), r.PathValue("action")
	switch action {
	case "start", "stop", "restart":
	default:
		writeErr(w, 400, fmt.Errorf("unknown action %q", action))
		return
	}
	if !validContainerID(cid) {
		writeErr(w, 400, errors.New("invalid container id"))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	if err := s.m.ContainerAction(ctx, id, cid, action); err != nil {
		writeErr(w, actionCode(err), err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

// handleAppWatch takes {"key": "<app key>", "on": bool} and adds the app to
// or removes it from the box's watch list.
func (s *Server) handleAppWatch(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	var in struct {
		Key string `json:"key"`
		On  bool   `json:"on"`
	}
	if err := decodeStrict(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if !collect.ValidAppKey(in.Key) {
		writeErr(w, 400, errors.New("invalid app key"))
		return
	}
	if err := s.m.SetAppWatch(r.Context(), id, in.Key, in.On); err != nil {
		writeErr(w, actionCode(err), err)
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true})
}

func (s *Server) handleDetect(w http.ResponseWriter, r *http.Request) {
	var in boxInput
	if err := decode(r, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := in.validate(); err != nil {
		writeErr(w, 400, err)
		return
	}
	b := store.Box{Enabled: true, DuEnabled: true}
	in.apply(&b)
	if in.ID > 0 {
		// Edit mode: blank password fields mean "keep the saved one". The
		// pin belongs to host:port and is enforced whenever that endpoint
		// is unchanged; stored secrets are reused only for the saved
		// host:port AND user (the poller enforces the pin; the wizard must
		// not be a way around it).
		if stored, err := s.st.GetBox(r.Context(), in.ID); err == nil {
			hostChanged, userChanged := endpointChanged(stored, &b)
			if !hostChanged && !in.ForgetHostKey {
				b.HostKey = stored.HostKey
			}
			if !hostChanged && !userChanged {
				if b.Password == "" {
					b.Password = stored.Password
				}
				if b.QbtPass == "" && !in.ClearQbtPass {
					b.QbtPass = stored.QbtPass
				}
			} else if b.Password == "" && (b.AuthMethod == "password" || b.AuthMethod == "") && stored.Password != "" {
				writeErr(w, 400, errors.New("re-enter the SSH password to test a different host, port or user; the saved one is only used for the saved endpoint"))
				return
			} else if b.QbtPass == "" && stored.QbtPass != "" && !in.ClearQbtPass {
				// Same rule as saving: otherwise the test would log in to the
				// new host's WebUI with an empty password (a strike toward a ban).
				writeErr(w, 400, errors.New("re-enter the WebUI password (or tick remove) to test a different host, port or user; the saved one is only used for the saved endpoint"))
				return
			}
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 90*time.Second)
	defer cancel()
	res := collect.Detect(ctx, b)
	writeJSON(w, 200, res)
}

func (s *Server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	al, err := s.st.ListAlerts(r.Context(), 200)
	if err != nil {
		writeErr(w, 500, err)
		return
	}
	if al == nil {
		al = []store.Alert{}
	}
	writeJSON(w, 200, al)
}

func (s *Server) handleAck(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.st.AckAlert(r.Context(), id); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.m.Publish(collect.Event{Type: "alerts"})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteAlert(w http.ResponseWriter, r *http.Request) {
	id, err := idParam(r)
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	if err := s.st.DeleteAlert(r.Context(), id); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.m.Publish(collect.Event{Type: "alerts"})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleClearResolved(w http.ResponseWriter, r *http.Request) {
	if err := s.st.ClearResolved(r.Context()); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.m.Publish(collect.Event{Type: "alerts"})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleAckAll(w http.ResponseWriter, r *http.Request) {
	if err := s.st.AckAll(r.Context()); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.m.Publish(collect.Event{Type: "alerts"})
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.m.Settings().Public())
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, s.m.Services())
}

func (s *Server) handleServicesSync(w http.ResponseWriter, r *http.Request) {
	if err := s.m.SyncServices(r.Context()); err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, s.m.Services())
}

func (s *Server) handleSetSettings(w http.ResponseWriter, r *http.Request) {
	// Read the body first: a client trickling bytes must not hold the lock
	// that every other settings save waits on.
	body, err := io.ReadAll(http.MaxBytesReader(nil, r.Body, 1<<20))
	if err != nil {
		writeErr(w, 400, err)
		return
	}
	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	in := s.m.Settings()
	prevKey := in.PMAPIKey
	in.PMAPIKey = ""
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, 400, err)
		return
	}
	// The browser never sees the stored key, so an empty field means "keep".
	in.PMAPIKey = strings.TrimSpace(in.PMAPIKey)
	switch {
	case in.ClearPMAPIKey:
		in.PMAPIKey = ""
	case in.PMAPIKey == "":
		in.PMAPIKey = prevKey
	case strings.ContainsAny(in.PMAPIKey, " \t\r\n\"'"):
		writeErr(w, 400, errors.New("API key contains whitespace or quotes"))
		return
	}
	in.ClearPMAPIKey = false
	if in.PollIntervalSec < 5 || in.PollIntervalSec > 3600 {
		writeErr(w, 400, errors.New("poll interval must be 5..3600 seconds"))
		return
	}
	for _, v := range []int{in.DiskThresholdPct, in.TrafficThreshold} {
		if v < 1 || v > 100 {
			writeErr(w, 400, errors.New("thresholds must be 1..100"))
			return
		}
	}
	if in.RenewalWarnDays < 0 || in.RenewalWarnDays > 365 || in.DuIntervalHours < 0 || in.DuIntervalHours > 720 {
		writeErr(w, 400, errors.New("out of range"))
		return
	}
	if in.QbtDownloadURL == "" {
		in.QbtDownloadURL = store.DefaultQbtDownloadURL
	}
	if !strings.HasPrefix(in.QbtDownloadURL, schemeHTTPS) || strings.ContainsAny(in.QbtDownloadURL, " '\"") {
		writeErr(w, 400, errors.New("qbt_download_url must be an https URL"))
		return
	}
	if in.CardMinPx < 240 || in.CardMinPx > 900 {
		in.CardMinPx = 340
	}
	if in.Theme != "dark" && in.Theme != "light" {
		in.Theme = "dark"
	}
	if in.FleetView != "cards" && in.FleetView != "table" {
		in.FleetView = "cards"
	}
	if err := s.st.SaveSettings(r.Context(), in); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.m.SetSettings(in)
	s.m.Publish(collect.Event{Type: "settings"}) // other tabs refresh their copy
	writeJSON(w, 200, in.Public())
}
