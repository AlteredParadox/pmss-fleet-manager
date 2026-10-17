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
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"pmss-fleet-manager/internal/collect"
	"pmss-fleet-manager/internal/store"
	"pmss-fleet-manager/internal/web"
)

// Server holds the dependencies for the handlers.
type Server struct {
	st   *store.Store
	m    *collect.Manager
	auth string // "user:pass" or ""
	mux  *http.ServeMux
	// instance identifies this process; the UI reloads itself when it
	// changes after an SSE reconnect so a rebuilt server never runs stale JS.
	instance string

	license, thirdParty []byte
	version             string

	// allowedHosts, when set, is the only set of Host header values served.
	// Without it a page on an attacker's domain whose DNS is rebound to the
	// dashboard's IP is same-origin from the browser's point of view and
	// passes the Origin check with full API access on a no-auth loopback
	// install. Hostnames only, lower-case, no port.
	allowedHosts map[string]bool
}

// SetAllowedHosts restricts served Host headers to the given hostnames
// (ports are ignored). An empty list disables the check.
func (s *Server) SetAllowedHosts(hosts []string) {
	s.allowedHosts = nil
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
	return s.allowedHosts[h]
}

// csp is the Content-Security-Policy for the embedded UI: own origin only,
// plus Google Fonts for the stylesheet and font files. Inline styles are
// used for widths and the like, hence 'unsafe-inline' on styles only.
const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; " +
	"font-src https://fonts.gstatic.com; img-src 'self' data:; connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'"

// New builds the handler tree.
func New(st *store.Store, m *collect.Manager, basicAuth string) *Server {
	s := &Server{st: st, m: m, auth: basicAuth, mux: http.NewServeMux(),
		instance: strconv.FormatInt(time.Now().UnixNano(), 36)}
	s.routes()
	return s
}

func (s *Server) routes() {
	mux := s.mux
	mux.HandleFunc("GET /api/state", s.handleState)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.HandleFunc("GET /api/boxes", s.handleListBoxes)
	mux.HandleFunc("POST /api/boxes", s.handleCreateBox)
	mux.HandleFunc("GET /api/boxes/{id}", s.handleGetBox)
	mux.HandleFunc("PUT /api/boxes/{id}", s.handleUpdateBox)
	mux.HandleFunc("DELETE /api/boxes/{id}", s.handleDeleteBox)
	mux.HandleFunc("POST /api/boxes/{id}/probe", s.handleProbe) // runs the probe over SSH: POST so the CSRF check applies
	mux.HandleFunc("POST /api/boxes/{id}/refresh", s.handleRefresh)
	mux.HandleFunc("POST /api/boxes/{id}/du", s.handleDu)
	mux.HandleFunc("POST /api/boxes/{id}/qbt/{action}", s.handleQbtAction)
	mux.HandleFunc("POST /api/boxes/{id}/containers/{cid}/{action}", s.handleContainerAction)
	mux.HandleFunc("POST /api/detect", s.handleDetect)
	mux.HandleFunc("GET /api/alerts", s.handleAlerts)
	mux.HandleFunc("POST /api/alerts/ack-all", s.handleAckAll)
	mux.HandleFunc("POST /api/alerts/{id}/ack", s.handleAck)
	mux.HandleFunc("DELETE /api/alerts/{id}", s.handleDeleteAlert)
	mux.HandleFunc("POST /api/alerts/clear-resolved", s.handleClearResolved)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handleSetSettings)
	// AGPL §13: the license, the third-party notices and the Corresponding
	// Source offer are served WITHOUT basic auth (see isPublicPath).
	mux.HandleFunc("GET /license", s.serveLegalText(func() []byte { return s.license }, "license text unavailable"))
	mux.HandleFunc("GET /third-party-licenses", s.serveLegalText(func() []byte { return s.thirdParty }, "third-party notices unavailable"))
	mux.HandleFunc("GET /source", s.handleSource)
	mux.HandleFunc("GET /api/services", s.handleServices)
	mux.HandleFunc("POST /api/services/sync", s.handleServicesSync)

	static, _ := fs.Sub(web.FS, "static")
	fileServer := http.FileServer(http.FS(static))
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "" || strings.HasSuffix(r.URL.Path, "/") { // no directory listings, no second copy of the page
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Cache-Control", "no-cache")
		fileServer.ServeHTTP(w, r)
	})))
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		b, err := fs.ReadFile(static, "index.html")
		if err != nil {
			http.Error(w, "index missing", 500)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write(b)
	})
}

// ServeHTTP applies auth and security headers then dispatches.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.hostAllowed(r.Host) {
		http.Error(w, "unexpected Host header; start with -allowed-hosts to serve this name", http.StatusMisdirectedRequest)
		return
	}
	if s.auth != "" && !isPublicPath(r.URL.Path) {
		u, p, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(u+":"+p), []byte(s.auth)) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="pmss-fleet-manager"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Referrer-Policy", "no-referrer")
	w.Header().Set("Content-Security-Policy", csp)
	if strings.HasPrefix(r.URL.Path, "/api/") && r.Method != http.MethodGet {
		// Same-origin check against CSRF from other sites in the browser.
		if origin := r.Header.Get("Origin"); origin != "" {
			if !sameOrigin(origin, r.Host) {
				http.Error(w, "cross-origin request rejected", http.StatusForbidden)
				return
			}
		} else if sfs := r.Header.Get("Sec-Fetch-Site"); sfs != "" && sfs != "same-origin" && sfs != "none" {
			http.Error(w, "cross-site request rejected", http.StatusForbidden)
			return
		}
	}
	s.mux.ServeHTTP(w, r)
}

func sameOrigin(origin, host string) bool {
	origin = strings.TrimPrefix(strings.TrimPrefix(origin, "http://"), "https://")
	return strings.EqualFold(origin, host)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	// Encode first: a value json cannot represent (NaN/Inf from a bad
	// number on one box) must be a visible 500, not a 200 with an empty body.
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "internal error: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	w.Write(append(b, '\n'))
}

func writeErr(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
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
		if b.Service != nil && !seenSvc[b.Service.ID] {
			seenSvc[b.Service.ID] = true // two boxes on one service count it once
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
	states := s.m.States()
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
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(200)
	fmt.Fprint(w, "retry: 3000\n\n")
	fl.Flush()
	ch, cancel := s.m.Bus().Subscribe()
	defer cancel()
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
}

func (in boxInput) validate() error {
	in.Host = strings.TrimSpace(in.Host)
	if in.Host == "" || strings.ContainsAny(in.Host, " /") {
		return errors.New("hostname is required")
	}
	if strings.TrimSpace(in.User) == "" {
		return errors.New("username is required")
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
	stored := *b
	in.apply(b)
	// The rules shared with detect (endpointChanged): the host-key pin
	// belongs to host:port and a supplied key never replaces an existing
	// pin; stored secrets are only ever used for the endpoint they were
	// saved for, so a changed host/port/user with a blank password is
	// refused rather than sent to the new host on trust-on-first-use.
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
	switch {
	case in.ForgetHostKey:
		b.HostKey = ""
	case hostChanged:
		b.HostKey = in.HostKey // the wizard's fresh detect result, or nothing
	case stored.HostKey == "":
		b.HostKey = in.HostKey // nothing pinned yet: the wizard's fresh key is as good as the poller's first contact
	default:
		b.HostKey = stored.HostKey
	}
	// Secrets: a blank field keeps the saved one, so clearing is explicit.
	// The SSH password is dropped when the box no longer authenticates with
	// one; the WebUI password on request, unless a new one was typed.
	clearSSH := in.AuthMethod == "key" || in.AuthMethod == "agent"
	clearQbt := in.ClearQbtPass && in.QbtPass == ""
	// Two writes: a client that drops (or a shutdown) between them must not
	// leave the secret cleared and the rest of the edit unsaved.
	ctx := context.WithoutCancel(r.Context())
	if clearSSH || clearQbt {
		if err := s.st.ClearSecrets(ctx, id, clearSSH, clearQbt); err != nil {
			writeErr(w, 500, err)
			return
		}
		if clearSSH {
			b.Password = ""
		}
		if clearQbt {
			b.QbtPass = ""
		}
	}
	if err := s.m.UpdateBox(ctx, b); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, 404, err)
			return
		}
		writeErr(w, 500, err)
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
// (404), its polling is off (409), or the box itself did not cooperate (502).
func actionCode(err error) int {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return 404
	case errors.Is(err, collect.ErrDisabled):
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
		writeJSON(w, actionCode(err), map[string]string{"error": err.Error(), "output": out})
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "output": out})
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
	if len(cid) < 12 || len(cid) > 64 || strings.Trim(cid, "0123456789abcdef") != "" {
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
	in := s.m.Settings()
	prevKey := in.PMAPIKey
	in.PMAPIKey = ""
	if err := decode(r, &in); err != nil {
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
	if !strings.HasPrefix(in.QbtDownloadURL, "https://") || strings.ContainsAny(in.QbtDownloadURL, " '\"") {
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
