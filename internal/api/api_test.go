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

package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"pmss-fleet-manager/internal/collect"
	"pmss-fleet-manager/internal/store"
)

func newTestServer(t *testing.T) (*Server, *store.Store) {
	t.Helper()
	return newTestServerWith(t, "", "")
}

// newTestServerWith builds a server with basic auth and a base path set.
func newTestServerWith(t *testing.T, auth, basePath string) (*Server, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := collect.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	return New(st, m, auth, basePath), st
}

// put edits a box the way the page does: with the record's current
// revision spliced into the body.
func put(t *testing.T, h http.Handler, st *store.Store, id int64, body string, hdr map[string]string) (int, string) {
	t.Helper()
	b, err := st.GetBox(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	body = fmt.Sprintf(`{"revision":%d,%s`, b.Revision, strings.TrimPrefix(strings.TrimSpace(body), "{"))
	return do(t, h, "PUT", fmt.Sprintf("/api/boxes/%d", id), body, hdr)
}

func do(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) (int, string) {
	t.Helper()
	code, rr := doRec(t, h, method, path, body, hdr)
	return code, rr.Body.String()
}

// doRec is do with the whole recorded response, for header checks.
func doRec(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) (int, *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "dash.local:8080"
	if method != "GET" {
		req.Header.Set(MutationHeader, "1") // what the page sends; a test passes "" in hdr to drop it
	}
	for k, v := range hdr {
		if v == "" {
			req.Header.Del(k)
			continue
		}
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, rr
}

func TestAlertAckFlow(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	b := &store.Box{Name: "x", Host: "h", User: "u", Enabled: false}
	if err := st.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, err := st.RaiseAlert(ctx, b.ID, "disk", "warn", "disk high"); err != nil {
		t.Fatal(err)
	}
	code, body := do(t, srv, "GET", "/api/alerts", "", nil)
	var alerts []store.Alert
	if err := json.Unmarshal([]byte(body), &alerts); err != nil || code != 200 || len(alerts) != 1 || alerts[0].Acked {
		t.Fatalf("list: %d %s", code, body)
	}
	// Same-origin POST from the browser carries Origin matching Host.
	code, body = do(t, srv, "POST", "/api/alerts/1/ack", "", map[string]string{"Origin": "http://dash.local:8080"})
	if code != 200 {
		t.Fatalf("ack: %d %s", code, body)
	}
	// Browsers may send only Sec-Fetch-Site.
	code, _ = do(t, srv, "POST", "/api/alerts/ack-all", "", map[string]string{"Sec-Fetch-Site": "same-origin"})
	if code != 200 {
		t.Fatalf("ack-all: %d", code)
	}
	// Cross-site is refused.
	// Only http and https count as this page: an opaque or odd scheme on the
	// right host:port is not it.
	for _, o := range []string{"null", "ftp://dash.local:8080", "dash.local:8080"} {
		if code, _ = do(t, srv, "POST", "/api/alerts/1/ack", "", map[string]string{"Origin": o}); code != 403 {
			t.Fatalf("Origin %q = %d, want 403", o, code)
		}
	}
	code, _ = do(t, srv, "POST", "/api/alerts/1/ack", "", map[string]string{"Origin": "https://dash.local:8080"})
	if code == 403 {
		t.Fatal("https origin on this host must pass")
	}
	// The custom header is required on its own: a form post from a page on
	// the dashboard's hostname over plain http carries a matching Origin
	// host but can set no header.
	if code, _ = do(t, srv, "POST", "/api/alerts/1/ack", "", map[string]string{"Origin": "http://dash.local:8080", MutationHeader: ""}); code != 403 {
		t.Fatalf("without the mutation header: %d, want 403", code)
	}
	// Sec-Fetch-Site is honoured even when Origin is present and matches.
	if code, _ = do(t, srv, "POST", "/api/alerts/1/ack", "", map[string]string{"Origin": "http://dash.local:8080", "Sec-Fetch-Site": "cross-site"}); code != 403 {
		t.Fatalf("cross-site with matching Origin: %d, want 403", code)
	}
	code, _ = do(t, srv, "POST", "/api/alerts/1/ack", "", map[string]string{"Origin": "http://evil.example"})
	if code != 403 {
		t.Fatalf("csrf: %d", code)
	}
	_, body = do(t, srv, "GET", "/api/alerts", "", nil)
	_ = json.Unmarshal([]byte(body), &alerts)
	if !alerts[0].Acked || !alerts[0].Active {
		t.Fatalf("after ack: %+v", alerts[0])
	}
	n, _ := st.CountUnacked(ctx)
	if n != 0 {
		t.Fatalf("unacked %d", n)
	}
	_ = st.ResolveAlert(ctx, b.ID, "disk")
	code, _ = do(t, srv, "POST", "/api/alerts/clear-resolved", "", map[string]string{"Origin": "http://dash.local:8080"})
	if code != 200 {
		t.Fatalf("clear: %d", code)
	}
	_, body = do(t, srv, "GET", "/api/alerts", "", nil)
	_ = json.Unmarshal([]byte(body), &alerts)
	if len(alerts) != 0 {
		t.Fatalf("not cleared: %s", body)
	}
}

func TestSourceURLPinning(t *testing.T) {
	s := &Server{}
	cases := map[string]string{
		"dev":               sourceBaseURL,
		"":                  sourceBaseURL,
		"v1.2.3":            sourceBaseURL + "/tree/v1.2.3",
		"ac08e55":           sourceBaseURL + "/tree/ac08e55",
		"ac08e55-dirty":     sourceBaseURL,
		"v1.2.3-4-gabc1234": sourceBaseURL,
	}
	for v, want := range cases {
		s.SetVersion(v)
		if got := s.sourceURL(); got != want {
			t.Errorf("version %q: got %s, want %s", v, got, want)
		}
	}
}

func TestHostAllowList(t *testing.T) {
	s, _ := newTestServer(t)
	// No list: any Host is served (the test helper uses dash.local).
	if code, _ := do(t, s, "GET", "/api/state", "", nil); code != 200 {
		t.Fatalf("no allow-list: %d", code)
	}
	s.SetAllowedHosts([]string{"localhost", "127.0.0.1", "Dash.Local"}, false)
	cases := map[string]int{"dash.local:8080": 200, "localhost": 200, "127.0.0.1:9": 200, "attacker.example:8080": 421, "": 421}
	for host, want := range cases {
		req := httptest.NewRequest("GET", "/api/state", nil)
		req.Host = host
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Errorf("host %q: got %d want %d", host, rr.Code, want)
		}
	}
	// The legal routes are public but still subject to the Host check.
	req := httptest.NewRequest("GET", "/license", nil)
	req.Host = "attacker.example"
	rr := httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != 421 {
		t.Fatalf("license with bad host: %d", rr.Code)
	}
	if rr.Header().Get("Content-Security-Policy") != "" {
		t.Fatal("rejected request should not carry app headers")
	}
	// A specific bind lists its hosts, so an IP literal not among them is refused.
	req = httptest.NewRequest("GET", "/api/state", nil)
	req.Host = "192.168.1.5:8080"
	rr = httptest.NewRecorder()
	s.ServeHTTP(rr, req)
	if rr.Code != 421 {
		t.Fatalf("unlisted IP without a wildcard bind: %d", rr.Code)
	}
}

// A wildcard bind cannot list its addresses: IP literals and the loopback
// names are served, any other hostname only when -allowed-hosts names it.
func TestHostAllowListWildcard(t *testing.T) {
	s, _ := newTestServer(t)
	s.SetAllowedHosts([]string{"localhost", "127.0.0.1", "::1"}, true)
	served := []string{"192.168.1.5:8080", "192.168.1.5", "[fd00::1]:8080", "localhost:8080"}
	refused := []string{"evil.example:8080", "dash.local:8080", ""}
	for _, host := range append(served, refused...) {
		want := 200
		if !slices.Contains(served, host) {
			want = 421
		}
		req := httptest.NewRequest("GET", "/api/state", nil)
		req.Host = host
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Errorf("wildcard, host %q: got %d want %d", host, rr.Code, want)
		}
		if rr.Code == 421 && !strings.Contains(rr.Body.String(), "-allowed-hosts") {
			t.Errorf("wildcard, host %q: refusal should name -allowed-hosts: %s", host, rr.Body.String())
		}
	}
	// An -allowed-hosts entry is served alongside the IP literals.
	s.SetAllowedHosts([]string{"localhost", "127.0.0.1", "::1", "dash.local"}, true)
	for host, want := range map[string]int{"dash.local:8080": 200, "10.0.0.2:8080": 200, "evil.example:8080": 421} {
		req := httptest.NewRequest("GET", "/api/state", nil)
		req.Host = host
		rr := httptest.NewRecorder()
		s.ServeHTTP(rr, req)
		if rr.Code != want {
			t.Errorf("wildcard with extra name, host %q: got %d want %d", host, rr.Code, want)
		}
	}
}

func TestEditClearsHostKeyOnHostChangeOrForget(t *testing.T) {
	s, st := newTestServer(t)
	hdr := map[string]string{"Origin": "http://dash.local:8080", "Content-Type": "application/json"}
	code, body := do(t, s, "POST", "/api/boxes", `{"name":"b","host":"a.example","user":"u","host_key":"ssh-ed25519 AAAA"}`, hdr)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	boxes, _ := st.ListBoxes(context.Background())
	id := boxes[0].ID
	if boxes[0].HostKey == "" {
		t.Fatal("pin not stored")
	}
	// Edit without touching host/port and without the key: pin kept.
	code, body = put(t, s, st, id, `{"name":"b2","host":"a.example","user":"u"}`, hdr)
	if code != 200 {
		t.Fatalf("edit: %d %s", code, body)
	}
	b, _ := st.GetBox(context.Background(), id)
	if b.HostKey == "" {
		t.Fatal("pin must survive an unrelated edit")
	}
	// Host change: pin cleared.
	put(t, s, st, id, `{"name":"b2","host":"b.example","user":"u"}`, hdr)
	b, _ = st.GetBox(context.Background(), id)
	if b.HostKey != "" {
		t.Fatal("pin must clear when the host changes")
	}
	// Re-pin, then explicit forget.
	put(t, s, st, id, `{"name":"b2","host":"b.example","user":"u","host_key":"ssh-ed25519 BBBB"}`, hdr)
	put(t, s, st, id, `{"name":"b2","host":"b.example","user":"u","forget_host_key":true}`, hdr)
	b, _ = st.GetBox(context.Background(), id)
	if b.HostKey != "" {
		t.Fatal("forget_host_key must clear the pin")
	}
}

func TestWriteJSONRefusesNonFinite(t *testing.T) {
	rr := httptest.NewRecorder()
	writeJSON(rr, 200, map[string]float64{"x": math.Inf(1)})
	if rr.Code != 500 {
		t.Fatalf("got %d, want 500 for an unencodable value", rr.Code)
	}
	rr = httptest.NewRecorder()
	writeJSON(rr, 201, map[string]int{"ok": 1})
	if rr.Code != 201 || !strings.Contains(rr.Body.String(), `"ok":1`) {
		t.Fatalf("normal encode: %d %s", rr.Code, rr.Body.String())
	}
}

func TestDetectOnEditKeepsSecretsForSavedEndpointOnly(t *testing.T) {
	s, st := newTestServer(t)
	hdr := map[string]string{"Origin": "http://dash.local:8080", "Content-Type": "application/json"}
	// A box with a stored password, pinned to a bogus key, pointing at a port that refuses fast.
	code, body := do(t, s, "POST", "/api/boxes", `{"name":"b","host":"127.0.0.1","port":1,"user":"u","auth_method":"password","password":"S3cret","host_key":"ssh-ed25519 AAAA"}`, hdr)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	boxes, _ := st.ListBoxes(context.Background())
	id := boxes[0].ID
	// Same endpoint, blank password: allowed (stored secret reused under the pin); the
	// result must never contain the password.
	code, body = do(t, s, "POST", "/api/detect", fmt.Sprintf(`{"id":%d,"host":"127.0.0.1","port":1,"user":"u","auth_method":"password","password":""}`, id), hdr)
	if code != 200 || strings.Contains(body, "S3cret") {
		t.Fatalf("same endpoint: %d leak=%v %s", code, strings.Contains(body, "S3cret"), body[:min(200, len(body))])
	}
	// Different host, blank password: refused rather than sending the stored secret.
	code, body = do(t, s, "POST", "/api/detect", fmt.Sprintf(`{"id":%d,"host":"127.0.0.2","port":1,"user":"u","auth_method":"password","password":""}`, id), hdr)
	if code != 400 || !strings.Contains(body, "re-enter") {
		t.Fatalf("different host: %d %s", code, body)
	}
}

func TestEditClearsSecrets(t *testing.T) {
	s, st := newTestServer(t)
	hdr := map[string]string{"Origin": "http://dash.local:8080", "Content-Type": "application/json"}
	do(t, s, "POST", "/api/boxes", `{"name":"b","host":"a.example","user":"u","auth_method":"password","password":"pw","qbt_user":"q","qbt_pass":"qp"}`, hdr)
	boxes, _ := st.ListBoxes(context.Background())
	id := boxes[0].ID
	put(t, s, st, id, `{"name":"b","host":"a.example","user":"u","auth_method":"key","qbt_user":"q","clear_qbt_pass":true}`, hdr)
	b, _ := st.GetBox(context.Background(), id)
	if b.Password != "" || b.QbtPass != "" {
		t.Fatalf("secrets not cleared: ssh=%q qbt=%q", b.Password, b.QbtPass)
	}
}

func TestUpdateRulesForPinsAndSecrets(t *testing.T) {
	s, st := newTestServer(t)
	hdr := map[string]string{"Origin": "http://dash.local:8080", "Content-Type": "application/json"}
	do(t, s, "POST", "/api/boxes", `{"name":"b","host":"A.example","port":22,"user":"u","auth_method":"password","password":"pw","host_key":"ssh-ed25519 PIN"}`, hdr)
	boxes, _ := st.ListBoxes(context.Background())
	id := boxes[0].ID
	get := func() *store.Box { b, _ := st.GetBox(context.Background(), id); return b }
	// Changed host with a blank password: refused, nothing changes.
	code, body := put(t, s, st, id, `{"name":"b","host":"b.example","user":"u","auth_method":"password"}`, hdr)
	if code != 400 || !strings.Contains(body, "re-enter") || get().Host != "A.example" {
		t.Fatalf("host change without password: %d %s host=%s", code, body, get().Host)
	}
	// Changed user only: same refusal (secrets are bound to the user too).
	if code, _ = put(t, s, st, id, `{"name":"b","host":"a.example","user":"v","auth_method":"password"}`, hdr); code != 400 {
		t.Fatalf("user change without password: %d", code)
	}
	// Same endpoint (host case differs): a supplied key must not replace the pin.
	put(t, s, st, id, `{"name":"b2","host":"a.example","user":"u","auth_method":"password","host_key":"ssh-ed25519 OTHER"}`, hdr)
	if b := get(); b.HostKey != "ssh-ed25519 PIN" || b.Name != "b2" || b.Password != "pw" {
		t.Fatalf("unchanged endpoint must keep pin and password: %+v", b)
	}
	// Forget wins over a supplied key.
	put(t, s, st, id, `{"name":"b2","host":"a.example","user":"u","auth_method":"password","host_key":"ssh-ed25519 OTHER","forget_host_key":true}`, hdr)
	if get().HostKey != "" {
		t.Fatal("forget_host_key must clear the pin even when a key is supplied")
	}
	// Changed host WITH a typed password: allowed, and the fresh detect key is pinned.
	put(t, s, st, id, `{"name":"b2","host":"b.example","user":"u","auth_method":"password","password":"pw2","host_key":"ssh-ed25519 NEW"}`, hdr)
	if b := get(); b.Host != "b.example" || b.HostKey != "ssh-ed25519 NEW" || b.Password != "pw2" {
		t.Fatalf("host change with password: %+v", b)
	}
	// Create with key auth discards a typed password.
	do(t, s, "POST", "/api/boxes", `{"name":"k","host":"k.example","user":"u","auth_method":"key","password":"leftover"}`, hdr)
	boxes, _ = st.ListBoxes(context.Background())
	for _, b := range boxes {
		if b.Name == "k" && b.Password != "" {
			t.Fatal("key-auth create must not store a password")
		}
	}
}

func TestMoveRequiresWebUIPasswordToo(t *testing.T) {
	s, st := newTestServer(t)
	hdr := map[string]string{"Origin": "http://dash.local:8080", "Content-Type": "application/json"}
	do(t, s, "POST", "/api/boxes", `{"name":"b","host":"a.example","user":"u","auth_method":"key","qbt_user":"q","qbt_pass":"qp"}`, hdr)
	boxes, _ := st.ListBoxes(context.Background())
	id := boxes[0].ID
	code, body := put(t, s, st, id, `{"name":"b","host":"b.example","user":"u","auth_method":"key","qbt_user":"q"}`, hdr)
	if code != 400 || !strings.Contains(body, "WebUI password") {
		t.Fatalf("move with saved WebUI password and blank field: %d %s", code, body)
	}
	if code, _ = put(t, s, st, id, `{"name":"b","host":"b.example","user":"u","auth_method":"key","qbt_user":"q","clear_qbt_pass":true}`, hdr); code != 200 {
		t.Fatalf("move with remove ticked: %d", code)
	}
	if b, _ := st.GetBox(context.Background(), id); b.Host != "b.example" || b.QbtPass != "" {
		t.Fatalf("after move: %+v", b)
	}
}

func TestValidTorrentHash(t *testing.T) {
	v1 := "0123456789abcdef0123456789ABCDEF01234567"
	for h, ok := range map[string]bool{v1: true, v1 + v1[:24]: true, v1[:39]: false, v1[:39] + "g": false, "": false, "all": false, "../x": false} {
		if validTorrentHash(h) != ok {
			t.Errorf("%q: want %v", h, ok)
		}
	}
}

func multipartAdd(t *testing.T, fields map[string]string, files map[string][]byte) *http.Request {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	for k, v := range fields {
		mw.WriteField(k, v)
	}
	for name, data := range files {
		fw, _ := mw.CreateFormFile("torrents", name)
		fw.Write(data)
	}
	mw.Close()
	r := httptest.NewRequest("POST", "/api/boxes/1/qbt/torrents", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	return r
}

// The add handler's validation: only magnet/http(s) links, only bencoded
// files, sane names, and never an empty request.
func TestAddRequestFromForm(t *testing.T) {
	req, err := addRequestFromForm(multipartAdd(t, map[string]string{"urls": "magnet:?xt=urn:btih:abc\n\n  https://x/y.torrent  \n", "category": " tv ", "stopped": "true"}, map[string][]byte{"C:\\dl\\ok.torrent": []byte("d4:infoe")}))
	if err != nil {
		t.Fatal(err)
	}
	if len(req.URLs) != 2 || req.URLs[1] != "https://x/y.torrent" || req.Category != "tv" || !req.Stopped || len(req.Files) != 1 || req.Files[0].Name != "ok.torrent" {
		t.Fatalf("parsed %+v", req)
	}
	for name, r := range map[string]*http.Request{
		"javascript url": multipartAdd(t, map[string]string{"urls": "javascript:alert(1)"}, nil),
		"file url":       multipartAdd(t, map[string]string{"urls": "file:///etc/passwd"}, nil),
		"junk file":      multipartAdd(t, nil, map[string][]byte{"x.torrent": []byte("<html>")}),
		"empty":          multipartAdd(t, map[string]string{"urls": "\n \n"}, nil),
		"bad category":   multipartAdd(t, map[string]string{"urls": "magnet:?x", "category": "a\nb"}, nil),
	} {
		if _, err := addRequestFromForm(r); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// Uploads past the old 1 MiB in-memory budget need no temp directory (a
// read-only root filesystem has none writable): TMPDIR points nowhere and
// two 5 MiB files still parse. The per-file and file-count caps hold.
func TestAddRequestFromFormNoTempFiles(t *testing.T) {
	t.Setenv("TMPDIR", filepath.Join(t.TempDir(), "does-not-exist"))
	big := func(n int) []byte { return []byte("d4:info" + strings.Repeat("x", n) + "e") }
	req, err := addRequestFromForm(multipartAdd(t, nil, map[string][]byte{"a.torrent": big(5 << 20), "b.torrent": big(5 << 20)}))
	if err != nil || len(req.Files) != 2 || len(req.Files[0].Data) != len(big(5<<20)) {
		t.Fatalf("large upload: %d files, %v", len(req.Files), err)
	}
	if _, err := addRequestFromForm(multipartAdd(t, nil, map[string][]byte{"huge.torrent": big(addMaxFile)})); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Fatalf("over the per-file cap: %v", err)
	}
	many := map[string][]byte{}
	for i := 0; i <= addMaxFiles; i++ {
		many[fmt.Sprintf("%d.torrent", i)] = []byte("d4:infoe")
	}
	if _, err := addRequestFromForm(multipartAdd(t, nil, many)); err == nil || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("too many files: %v", err)
	}
	r := httptest.NewRequest("POST", "/api/boxes/1/qbt/torrents", strings.NewReader("urls=magnet:?x"))
	r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if _, err := addRequestFromForm(r); err == nil || !strings.Contains(err.Error(), "bad upload") {
		t.Fatalf("not multipart: %v", err)
	}
	// The first value of a repeated field counts, as FormValue did.
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("urls", "magnet:?xt=urn:btih:abc")
	mw.WriteField("urls", "javascript:alert(1)")
	mw.Close()
	r = httptest.NewRequest("POST", "/api/boxes/1/qbt/torrents", &buf)
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if req, err := addRequestFromForm(r); err != nil || len(req.URLs) != 1 {
		t.Fatalf("repeated field: %+v %v", req, err)
	}
}

// /healthz answers at the root and under the base path, without the login
// the rest of the API asks for, and without counting as a failed login.
func TestHealthz(t *testing.T) {
	s, _ := newTestServerWith(t, "me:pw", "/fleet")
	for i := 0; i < 10; i++ {
		for _, p := range []string{"/healthz", "/fleet/healthz"} {
			if code, body := do(t, s, "GET", p, "", nil); code != 200 || body != "ok\n" {
				t.Fatalf("%s: %d %q", p, code, body)
			}
		}
	}
	if code, _ := do(t, s, "GET", "/fleet/api/state", "", nil); code != 401 {
		t.Fatalf("api without login: %d", code)
	}
	if code, _ := do(t, s, "POST", "/healthz", "", map[string]string{MutationHeader: "1"}); code == 200 {
		t.Fatal("POST /healthz served")
	}
	plain, _ := newTestServer(t)
	if code, _ := do(t, plain, "GET", "/healthz", "", nil); code != 200 {
		t.Fatalf("no base path: %d", code)
	}
}

// The streaming parser's refusals: an oversize text field, a body cut off
// mid-part, and unknown parts skipped.
func TestAddRequestFromFormRefusals(t *testing.T) {
	long := strings.Repeat("m", addMaxField+1)
	if _, err := addRequestFromForm(multipartAdd(t, map[string]string{"urls": long}, nil)); err == nil || !strings.Contains(err.Error(), "too long") {
		t.Fatalf("long field: %v", err)
	}
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	mw.WriteField("other", "ignored")
	mw.WriteField("urls", "magnet:?xt=urn:btih:abc")
	mw.Close()
	r := httptest.NewRequest("POST", "/api/boxes/1/qbt/torrents", bytes.NewReader(buf.Bytes()))
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if req, err := addRequestFromForm(r); err != nil || len(req.URLs) != 1 {
		t.Fatalf("unknown part: %+v %v", req, err)
	}
	cut := buf.Bytes()[:buf.Len()/2]
	r = httptest.NewRequest("POST", "/api/boxes/1/qbt/torrents", bytes.NewReader(cut))
	r.Header.Set("Content-Type", mw.FormDataContentType())
	if _, err := addRequestFromForm(r); err == nil || !strings.Contains(err.Error(), "bad upload") {
		t.Fatalf("cut body: %v", err)
	}
	// "stopped" and the category come through.
	req, err := addRequestFromForm(multipartAdd(t, map[string]string{"urls": "magnet:?xt=urn:btih:abc", "stopped": "1", "category": "x"}, nil))
	if err != nil || !req.Stopped || req.Category != "x" {
		t.Fatalf("fields: %+v %v", req, err)
	}
}

// The fleet-wide torrent list is a JSON object with a boxes array, limited
// to boxes with qBittorrent monitoring on.
func TestTorrentsList(t *testing.T) {
	srv, st := newTestServer(t)
	ctx := context.Background()
	if err := st.CreateBox(ctx, &store.Box{Name: "x", Host: "h", User: "u", Enabled: false}); err != nil {
		t.Fatal(err)
	}
	code, body := do(t, srv, "GET", "/api/torrents", "", nil)
	var out struct {
		Boxes []struct {
			ID  int64 `json:"id"`
			Qbt struct {
				Enabled bool `json:"enabled"`
			} `json:"qbt"`
		} `json:"boxes"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || code != 200 {
		t.Fatalf("list: %d %s", code, body)
	}
	for _, b := range out.Boxes {
		if !b.Qbt.Enabled {
			t.Errorf("box %d listed without qBittorrent monitoring", b.ID)
		}
	}
}

func TestClipStr(t *testing.T) {
	if got := clipStr("abcdef", 3); got != "abc…" {
		t.Errorf("ascii clip = %q", got)
	}
	if got := clipStr("héllo", 2); got != "h…" { // 'é' is two bytes; never split it
		t.Errorf("rune clip = %q", got)
	}
	if got := clipStr("short", 10); got != "short" {
		t.Errorf("no clip = %q", got)
	}
}

// Host and user end up in https links the UI builds, so the server rejects
// characters that would change the link's origin or path, and a host whose
// first label is empty (it is the box's default name).
func TestBoxInputHostUser(t *testing.T) {
	ok := boxInput{Host: "lt5-1.pulsedmedia.com", User: "alice_01"}
	if err := ok.validate(); err != nil {
		t.Fatalf("valid input rejected: %v", err)
	}
	for _, in := range []boxInput{
		{Host: "a@evil.example", User: "u"}, {Host: "evil.example/#", User: "u"}, {Host: "a b", User: "u"}, {Host: "..", User: "u"}, {Host: "-x.example", User: "u"},
		{Host: "h", User: "../x"}, {Host: "h", User: "a b"}, {Host: "h", User: "a/b"},
	} {
		if err := in.validate(); err == nil {
			t.Errorf("%+v accepted", in)
		}
	}
}

// Box cost totals count each active service once; cancelled matches and
// their due dates are history.
func TestTotalsActiveServicesOnce(t *testing.T) {
	act := &collect.ServiceInfo{ID: 1, Status: "Active", Monthly: 10, Currency: "EUR", NextDue: "2027-01-01"}
	old := &collect.ServiceInfo{ID: 2, Status: "Cancelled", Monthly: 99, Currency: "EUR", NextDue: "2020-01-01"}
	tt := totals([]collect.BoxState{{Service: act}, {Service: act}, {Service: old}})
	if tt.MonthlyCost != 10 {
		t.Fatalf("monthly = %v, want 10 (shared service once, cancelled excluded)", tt.MonthlyCost)
	}
	if tt.NextRenew != "2027-01-01" {
		t.Fatalf("next renewal = %q; the cancelled service's old due date must not win", tt.NextRenew)
	}
}

func TestAppWatch(t *testing.T) {
	s, st := newTestServer(t)
	ctx := context.Background()
	b := &store.Box{Name: "x", Host: "h.example", User: "u", Port: 22, Enabled: false}
	if err := st.CreateBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	if err := s.m.AddBox(ctx, &store.Box{Name: "y", Host: "y.example", User: "u", Port: 22, Enabled: false}); err != nil {
		t.Fatal(err)
	}
	boxes, _ := st.ListBoxes(ctx)
	id := boxes[1].ID // the one the manager knows
	hdr := map[string]string{"Origin": "http://dash.local:8080", "Content-Type": "application/json"}
	for _, body := range []string{`{"key":"Radarr","on":true}`, `{"key":"","on":true}`, `{"key":"radarr","on":true,"x":1}`} {
		if code, _ := do(t, s, "POST", fmt.Sprintf("/api/boxes/%d/apps/watch", id), body, hdr); code != 400 {
			t.Fatalf("%s: code %d", body, code)
		}
	}
	if code, out := do(t, s, "POST", fmt.Sprintf("/api/boxes/%d/apps/watch", id), `{"key":"radarr","on":true}`, hdr); code != 200 {
		t.Fatalf("watch: %d %s", code, out)
	}
	if code, out := do(t, s, "POST", fmt.Sprintf("/api/boxes/%d/apps/watch", id), `{"key":"other:bot.py","on":true}`, hdr); code != 200 {
		t.Fatalf("watch: %d %s", code, out)
	}
	got, _ := st.GetBox(ctx, id)
	if !slices.Equal(got.AppsWatch, []string{"other:bot.py", "radarr"}) {
		t.Fatalf("persisted: %v", got.AppsWatch)
	}
	state, _ := s.m.State(id)
	if !slices.Equal(state.Apps.Watched, got.AppsWatch) || len(state.Apps.Apps) != 2 || state.Apps.Apps[0].Running {
		t.Fatalf("state: %+v", state.Apps)
	}
	if code, _ := do(t, s, "POST", fmt.Sprintf("/api/boxes/%d/apps/watch", id), `{"key":"radarr","on":false}`, hdr); code != 200 {
		t.Fatalf("unwatch: %d", code)
	}
	got, _ = st.GetBox(ctx, id)
	if !slices.Equal(got.AppsWatch, []string{"other:bot.py"}) {
		t.Fatalf("after unwatch: %v", got.AppsWatch)
	}
	if code, _ := do(t, s, "POST", "/api/boxes/999999/apps/watch", `{"key":"radarr","on":true}`, hdr); code != 404 {
		t.Fatalf("missing box: %d", code)
	}
}

func TestReadLogValidation(t *testing.T) {
	s, _ := newTestServer(t)
	ctx := context.Background()
	if err := s.m.AddBox(ctx, &store.Box{Name: "y", Host: "y.example", User: "u", Port: 22, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	boxes, _ := s.st.ListBoxes(ctx)
	id := boxes[0].ID
	hdr := map[string]string{"Origin": "http://dash.local:8080", "Content-Type": "application/json"}
	for _, body := range []string{`{"path":"/etc/passwd"}`, `{"path":"../x"}`, `{"path":".logs/../../x"}`, `{"path":""}`, `{"path":"a.log","x":1}`} {
		if code, out := do(t, s, "POST", fmt.Sprintf("/api/boxes/%d/logs/read", id), body, hdr); code != 400 {
			t.Fatalf("%s: code %d %s", body, code, out)
		}
	}
	// A well-formed path is refused before any SSH while no probe of the
	// box has listed its logs (409, like a disabled box); the listed-path
	// check itself is tested in collect.
	code, out := do(t, s, "POST", fmt.Sprintf("/api/boxes/%d/logs/read", id), `{"path":".logs/x.log"}`, hdr)
	if code != 409 || !strings.Contains(out, "not been probed") {
		t.Fatalf("unlisted path: %d %s", code, out)
	}
	if code, _ := do(t, s, "POST", fmt.Sprintf("/api/boxes/%d/containers/zz/logs", id), "", hdr); code != 400 {
		t.Fatalf("bad container id: %d", code)
	}
	// The poll lists full ids, so an abbreviated one is refused up front.
	if code, _ := do(t, s, "POST", fmt.Sprintf("/api/boxes/%d/containers/%s/logs", id, strings.Repeat("a", 12)), "", hdr); code != 400 {
		t.Fatalf("abbreviated container id: %d", code)
	}
	if code, _ := do(t, s, "POST", "/api/boxes/999999/logs/read", `{"path":".logs/x.log"}`, hdr); code != 404 {
		t.Fatalf("missing box: %d", code)
	}
}

// The basic-auth throttle slows presented wrong credentials per address
// and never the right one, so a shared address (every client behind a
// reverse proxy) cannot be locked out by a stranger; a request with no
// credential gets the challenge and is not counted (GPT audit 2026-10-22
// finding 3, with its availability-over-throttling trade-off).
func TestAuthThrottleAndSSECap(t *testing.T) {
	s, _ := newTestServerWith(t, "me:secret", "")
	bad := map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("me:wrong"))}
	good := map[string]string{"Authorization": "Basic " + base64.StdEncoding.EncodeToString([]byte("me:secret"))}
	challenge := func(code int, rr *httptest.ResponseRecorder) {
		t.Helper()
		if code != 401 || !strings.HasPrefix(rr.Header().Get("WWW-Authenticate"), "Basic ") {
			t.Fatalf("code %d, WWW-Authenticate %q; want the challenge", code, rr.Header().Get("WWW-Authenticate"))
		}
	}
	for i := 0; i < authFreeTries; i++ {
		if code, _ := do(t, s, "GET", "/api/state", "", bad); code != 401 {
			t.Fatalf("attempt %d: %d, want 401", i+1, code)
		}
	}
	code, rr := doRec(t, s, "GET", "/api/state", "", bad)
	if code != 429 || rr.Header().Get("Retry-After") != "30" {
		t.Fatalf("past the free tries: %d Retry-After %q, want 429 and 30", code, rr.Header().Get("Retry-After"))
	}
	// Before the wait is over: no credential is still the challenge, the
	// wrong one is still 429, the right one gets in and clears the budget.
	code, rr = doRec(t, s, "GET", "/api/state", "", nil)
	challenge(code, rr)
	if code, _ := do(t, s, "GET", "/api/state", "", bad); code != 429 {
		t.Fatalf("wrong credential during the wait: %d, want 429", code)
	}
	if code, _ := do(t, s, "GET", "/api/state", "", good); code != 200 {
		t.Fatalf("the right credential during the wait: %d, want 200", code)
	}
	if code, _ := do(t, s, "GET", "/api/state", "", bad); code != 401 {
		t.Fatal("a success clears the budget")
	}
	// Six wrong ones in a row from a proxy's address, then the user.
	for i := 0; i <= authFreeTries; i++ {
		do(t, s, "GET", "/api/state", "", bad)
	}
	if code, _ := do(t, s, "GET", "/api/state", "", good); code != 200 {
		t.Fatalf("locked out behind a shared address: %d", code)
	}
	// Uncounted challenges never start a wait.
	for i := 0; i < 3*authFreeTries; i++ {
		code, rr = doRec(t, s, "GET", "/api/state", "", nil)
		challenge(code, rr)
	}
	if code, _ := do(t, s, "GET", "/api/state", "", bad); code != 401 {
		t.Fatalf("after uncounted challenges one wrong credential is a plain 401, got %d", code)
	}
	// The wait is the remaining time rounded up to whole seconds.
	s.authMu.Lock()
	for _, f := range s.authFails {
		f.count, f.until = authFreeTries+1, time.Now().Add(1500*time.Millisecond)
	}
	s.authMu.Unlock()
	code, rr = doRec(t, s, "GET", "/api/state", "", bad)
	if code != 429 || rr.Header().Get("Retry-After") != "2" {
		t.Fatalf("remaining wait: %d Retry-After %q, want 429 and 2", code, rr.Header().Get("Retry-After"))
	}
	// Event streams are capped.
	for i := 0; i < maxSSEStreams; i++ {
		s.sseSlots <- struct{}{}
	}
	if code, _ := do(t, s, "GET", "/api/events", "", good); code != 503 {
		t.Fatalf("over the cap: %d, want 503", code)
	}
	<-s.sseSlots
	ctx, cancel := context.WithCancel(context.Background())
	req := httptest.NewRequest("GET", "/api/events", nil).WithContext(ctx)
	req.Host = "dash.local:8080"
	req.Header.Set("Authorization", good["Authorization"])
	rr = httptest.NewRecorder()
	done := make(chan struct{})
	go func() { s.ServeHTTP(rr, req); close(done) }()
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done
	if rr.Code != 200 || len(s.sseSlots) != maxSSEStreams-1 {
		t.Fatalf("stream: code %d slots %d", rr.Code, len(s.sseSlots))
	}
}

// Two tabs edit one box from the same snapshot: A moves it to endpoint B
// with a new password, then B (stale) saves an unrelated change. B must be
// refused with 409, so the record can never end up as endpoint A with B's
// password (external review 3, finding 1).
func TestStaleEditCannotRebindSecret(t *testing.T) {
	s, st := newTestServer(t)
	hdr := map[string]string{"Origin": "http://dash.local:8080", "Content-Type": "application/json"}
	do(t, s, "POST", "/api/boxes", `{"name":"b","host":"a.example","user":"u","auth_method":"password","password":"P_A","qbt_pass":"Q_A"}`, hdr)
	boxes, _ := st.ListBoxes(context.Background())
	id := boxes[0].ID
	snap, _ := st.GetBox(context.Background(), id) // both tabs loaded this
	tabA := fmt.Sprintf(`{"revision":%d,"name":"b","host":"b.example","user":"u","auth_method":"password","password":"P_B","qbt_pass":"Q_B"}`, snap.Revision)
	if code, body := do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), tabA, hdr); code != 200 {
		t.Fatalf("tab A: %d %s", code, body)
	}
	tabB := fmt.Sprintf(`{"revision":%d,"name":"renamed","host":"a.example","user":"u","auth_method":"password"}`, snap.Revision)
	code, body := do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), tabB, hdr)
	if code != 409 || !strings.Contains(body, `"box"`) {
		t.Fatalf("tab B must be refused as stale with the fresh record: %d %s", code, body)
	}
	got, _ := st.GetBox(context.Background(), id)
	if got.Host != "b.example" || got.Password != "P_B" || got.QbtPass != "Q_B" || got.Name != "b" || got.Revision != snap.Revision+1 {
		t.Fatalf("record must be exactly tab A's: %+v", got)
	}
	// Without a revision an edit is refused outright.
	if code, _ := do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), `{"name":"x","host":"b.example","user":"u"}`, hdr); code != 400 {
		t.Fatalf("no revision: %d", code)
	}
	// A pin a poll stored while the form was open survives the save.
	do(t, s, "POST", "/api/boxes", `{"name":"p","host":"p.example","user":"u"}`, hdr)
	boxes, _ = st.ListBoxes(context.Background())
	var pid int64
	for _, b := range boxes {
		if b.Name == "p" {
			pid = b.ID
		}
	}
	snap, _ = st.GetBox(context.Background(), pid)
	if pinned, _ := st.SetHostKey(context.Background(), pid, "p.example", 22, "ssh-ed25519 POLL"); !pinned {
		t.Fatal("pin")
	}
	if code, body := do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", pid), fmt.Sprintf(`{"revision":%d,"name":"p2","host":"p.example","user":"u"}`, snap.Revision), hdr); code != 200 {
		t.Fatalf("edit after a poll pinned: %d %s", code, body)
	}
	if got, _ := st.GetBox(context.Background(), pid); got.HostKey != "ssh-ed25519 POLL" || got.Name != "p2" {
		t.Fatalf("pin stored meanwhile must survive: %+v", got)
	}
}

// The deterministic barrier case: the competing edit lands after the
// handler has read and validated the record (so the fast revision check
// passed) and before its update. The store's compare-and-set must refuse
// it with 409 and the competing edit's record must stand.
func TestEditRacingAnotherEditIsRefused(t *testing.T) {
	s, st := newTestServer(t)
	hdr := map[string]string{"Origin": "http://dash.local:8080", "Content-Type": "application/json"}
	do(t, s, "POST", "/api/boxes", `{"name":"b","host":"a.example","user":"u","auth_method":"password","password":"P_A"}`, hdr)
	boxes, _ := st.ListBoxes(context.Background())
	id := boxes[0].ID
	snap, _ := st.GetBox(context.Background(), id)
	s.beforeBoxUpdate = func() {
		// Tab A, moving the box with a new password, commits first.
		other, _ := st.GetBox(context.Background(), id)
		other.Host, other.Password = "b.example", "P_B"
		if err := st.UpdateBox(context.Background(), other, store.Clears{ReplaceHostKey: true}); err != nil {
			t.Error(err)
		}
	}
	code, body := do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), fmt.Sprintf(`{"revision":%d,"name":"renamed","host":"a.example","user":"u","auth_method":"password"}`, snap.Revision), hdr)
	if code != 409 || !strings.Contains(body, `"host":"b.example"`) {
		t.Fatalf("racing edit: %d %s", code, body)
	}
	got, _ := st.GetBox(context.Background(), id)
	if got.Host != "b.example" || got.Password != "P_B" || got.Name != "b" || got.Revision != snap.Revision+1 {
		t.Fatalf("the committed edit must stand untouched: %+v", got)
	}
}

// flushWriter is a streaming ResponseWriter: it reports the first flush,
// which is when a browser's EventSource fires onopen.
type flushWriter struct {
	mu     sync.Mutex
	hdr    http.Header
	code   int
	body   strings.Builder
	opened chan struct{}
	once   sync.Once
}

func (f *flushWriter) Header() http.Header { return f.hdr }
func (f *flushWriter) WriteHeader(c int)   { f.code = c }
func (f *flushWriter) Write(b []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body.Write(b)
}
func (f *flushWriter) Flush() { f.once.Do(func() { close(f.opened) }) }
func (f *flushWriter) text() string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.body.String()
}

// The event stream is subscribed before its headers go out: a change made
// the moment the stream opens (when the page takes its snapshot) is
// delivered, so snapshot plus stream miss nothing (external review 3,
// finding 11). The snapshot carries the revision it was taken at.
func TestEventsSubscribedBeforeOpen(t *testing.T) {
	s, _ := newTestServer(t)
	hdr := map[string]string{"Origin": "http://dash.local:8080", "Content-Type": "application/json"}
	do(t, s, "POST", "/api/boxes", `{"name":"b","host":"a.example","user":"u","auth_method":"key","key_path":"/k"}`, hdr)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest("GET", "/api/events", nil).WithContext(ctx)
	req.Host = "dash.local:8080"
	w := &flushWriter{hdr: http.Header{}, opened: make(chan struct{})}
	done := make(chan struct{})
	go func() { s.ServeHTTP(w, req); close(done) }()
	select {
	case <-w.opened:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never opened")
	}
	// Published right after the stream opened, as the page's snapshot is
	// being taken: must arrive on this stream.
	code, body := do(t, s, "GET", "/api/state", "", nil)
	if code != 200 || !strings.Contains(body, `"rev":`) {
		t.Fatalf("snapshot: %d %s", code, body)
	}
	var snap struct {
		Rev   uint64 `json:"rev"`
		Boxes []struct {
			Rev uint64 `json:"rev"`
		} `json:"boxes"`
	}
	if err := json.Unmarshal([]byte(body), &snap); err != nil || len(snap.Boxes) != 1 || snap.Boxes[0].Rev == 0 || snap.Boxes[0].Rev > snap.Rev {
		t.Fatalf("snapshot revs: %+v %v", snap, err)
	}
	s.m.Publish(collect.Event{Type: "state", BoxID: 1, Rev: snap.Rev + 1, State: &collect.BoxState{ID: 1}})
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(w.text(), `event: state`) {
		if time.Now().After(deadline) {
			t.Fatalf("event published at open was not delivered: %q", w.text())
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(w.text(), fmt.Sprintf(`"rev":%d`, snap.Rev+1)) {
		t.Fatalf("event lacks its revision: %q", w.text())
	}
	cancel()
	<-done
}

// The download URL is saved under the same rule the install applies.
func TestSettingsDownloadURL(t *testing.T) {
	srv, _ := newTestServer(t)
	for body, want := range map[string]int{
		`{"qbt_download_url":"https://example.com/b?token=x"}`:                         200,
		`{"qbt_download_url":"http://example.com/b"}`:                                  400,
		`{"qbt_download_url":"https://example.com/a b"}`:                               400,
		`{"qbt_download_url":"https://example.com/` + strings.Repeat("a", 2048) + `"}`: 400,
	} {
		if code, out := do(t, srv, "PUT", "/api/settings", body, nil); code != want {
			t.Errorf("%.60s: %d %s", body, code, out)
		}
	}
	if got := srv.m.Settings().QbtDownloadURL; got != "https://example.com/b?token=x" {
		t.Fatalf("stored %q", got)
	}
}

// The pmdb upload key follows the PM API key's rules (kept on an empty
// PUT, never echoed, cleared by its flag), and enabling the catalogue
// without a key is refused rather than silently doing nothing.
func TestSettingsPMDB(t *testing.T) {
	srv, _ := newTestServer(t)
	code, body := do(t, srv, "PUT", "/api/settings", `{"pmdb_enabled":true}`, nil)
	if code != 400 || !strings.Contains(body, "upload key") {
		t.Fatalf("enable without a key: %d %s", code, body)
	}
	code, body = do(t, srv, "PUT", "/api/settings", `{"pmdb_enabled":true,"pmdb_upload_key":" some words that make a passphrase "}`, nil)
	if code != 200 || strings.Contains(body, "passphrase") || !strings.Contains(body, `"has_pmdb_upload_key":true`) || !strings.Contains(body, `"pmdb_enabled":true`) {
		t.Fatalf("enable: %d %s", code, body)
	}
	if code, body = do(t, srv, "PUT", "/api/settings", `{"pmdb_upload_key":"bad\nkey but long enough to pass"}`, nil); code != 400 {
		t.Fatalf("key with a line break: %d %s", code, body)
	}
	if code, body = do(t, srv, "PUT", "/api/settings", `{"pmdb_upload_key":"short"}`, nil); code != 400 || !strings.Contains(body, "too short") {
		t.Fatalf("short key: %d %s", code, body)
	}
	if got := srv.m.Settings(); got.PMDBUploadKey != "some words that make a passphrase" || !got.PMDBEnabled {
		t.Fatalf("stored: %+v", got)
	}
	if code, _ = do(t, srv, "PUT", "/api/settings", `{"theme":"light"}`, nil); code != 200 || srv.m.Settings().PMDBUploadKey != "some words that make a passphrase" {
		t.Fatalf("key not kept: %d", code)
	}
	// An empty key on a later save keeps the stored one; the flag removes
	// it, which switches the integration off with it.
	code, body = do(t, srv, "PUT", "/api/settings", `{"clear_pmdb_upload_key":true}`, nil)
	if code != 200 || srv.m.Settings().PMDBUploadKey != "" || srv.m.Settings().PMDBEnabled || !strings.Contains(body, `"has_pmdb_upload_key":false`) || !strings.Contains(body, `"pmdb_enabled":false`) {
		t.Fatalf("clear: %d %s", code, body)
	}
	if code, body = do(t, srv, "GET", "/api/pmdb", "", nil); code != 200 || !strings.Contains(body, `"enabled":false`) {
		t.Fatalf("view: %d %s", code, body)
	}
	if code, _ = do(t, srv, "POST", "/api/pmdb/sync", "", nil); code != 400 {
		t.Fatalf("sync while off: %d", code)
	}
}

func TestRtRoutesAndSettings(t *testing.T) {
	s, st := newTestServer(t)
	hdr := map[string]string{"Origin": "http://dash.local:8080"}
	// rt_scgi is validated like the other endpoint fields.
	code, body := do(t, s, "POST", "/api/boxes", `{"name":"b","host":"a.example","user":"u","rt_enabled":true,"rt_scgi":"tcp:0.0.0.0:5000"}`, hdr)
	if code != 400 || !strings.Contains(body, "rt_scgi") {
		t.Fatalf("bad rt_scgi: %d %s", code, body)
	}
	code, body = do(t, s, "POST", "/api/boxes", `{"name":"b","host":"a.example","user":"u","rt_enabled":true,"rt_scgi":" unix:/home/u/.rtorrent.socket "}`, hdr)
	if code != 201 {
		t.Fatalf("create: %d %s", code, body)
	}
	var created struct {
		ID int64 `json:"id"`
	}
	json.Unmarshal([]byte(body), &created)
	b, err := st.GetBox(context.Background(), created.ID)
	if err != nil || !b.RtEnabled || b.RtSCGI != "unix:/home/u/.rtorrent.socket" {
		t.Fatalf("stored %+v %v", b, err)
	}
	// The combined list carries the rtorrent block for a box with only rTorrent on.
	code, body = do(t, s, "GET", "/api/torrents", "", nil)
	var out struct {
		Boxes []struct {
			ID     int64 `json:"id"`
			BoxRev int64 `json:"box_rev"`
			Rt     struct {
				Enabled  bool   `json:"enabled"`
				Endpoint string `json:"endpoint"`
			} `json:"rtorrent"`
		} `json:"boxes"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil || code != 200 || len(out.Boxes) != 1 || out.Boxes[0].BoxRev != b.Revision || !out.Boxes[0].Rt.Enabled || out.Boxes[0].Rt.Endpoint != "" { // the endpoint comes from a poll, which checks it
		t.Fatalf("list: %d %s", code, body)
	}
	// Actions: the action and hash are checked before anything reaches the box.
	id := created.ID
	for _, tc := range []struct {
		path string
		want int
	}{
		{fmt.Sprintf("/api/boxes/%d/rtorrent/torrents/%s/resume", id, strings.Repeat("a", 40)), 400},                   // resume is part of start
		{fmt.Sprintf("/api/boxes/%d/rtorrent/torrents/%s/pause?rev=%d", id, strings.Repeat("a", 40), b.Revision), 409}, // valid for rTorrent; the box was never probed
		{fmt.Sprintf("/api/boxes/%d/qbt/torrents/%s/pause?rev=%d", id, strings.Repeat("a", 40), b.Revision), 400},      // qBittorrent has no pause of its own
		{fmt.Sprintf("/api/boxes/%d/rtorrent/torrents/%s/stop", id, strings.Repeat("a", 64)), 400},                     // v2 hashes are qBittorrent's
		{fmt.Sprintf("/api/boxes/%d/rtorrent/torrents/..%%2F/stop", id), 400},
		{fmt.Sprintf("/api/boxes/%d/rtorrent/torrents/%s/remove-data?rev=%d", id, strings.Repeat("A", 40), b.Revision), 409},
		{fmt.Sprintf("/api/boxes/%d/rtorrent/torrents/%s/stop", id, strings.Repeat("a", 40)), 400},                      // no box revision
		{fmt.Sprintf("/api/boxes/%d/rtorrent/torrents/%s/stop?rev=%d", id, strings.Repeat("a", 40), b.Revision+1), 409}, // drawn from another revision
		{fmt.Sprintf("/api/boxes/%d/qbt/torrents/%s/stop?rev=x", id, strings.Repeat("a", 40)), 400},                     // valid, but the box was never probed: not ready
		{fmt.Sprintf("/api/boxes/%d/rtorrent/torrents/%s/stop?rev=0", 999, strings.Repeat("a", 40)), 404},
		{fmt.Sprintf("/api/boxes/%d/rtorrent/remove-failure/dismiss", id), 400}, // the notice time is required
	} {
		code, body := do(t, s, "POST", tc.path, "", hdr)
		if code != tc.want {
			t.Errorf("%s: %d %s", tc.path, code, body)
		}
	}
	// A dismiss names the notice it saw (none on record here: nothing to
	// compare, the row is simply absent); an unknown box is 404.
	if code, body := do(t, s, "POST", fmt.Sprintf("/api/boxes/%d/rtorrent/remove-failure/dismiss", id), `{"at":"2026-10-25T10:00:00Z"}`, hdr); code != 200 {
		t.Errorf("dismiss: %d %s", code, body)
	}
	if code, _ := do(t, s, "POST", fmt.Sprintf("/api/boxes/%d/rtorrent/remove-failure/dismiss", 999), `{"at":"2026-10-25T10:00:00Z"}`, hdr); code != 404 {
		t.Errorf("dismiss unknown box: %d", code)
	}
	// The body is strict: one value, no unknown fields.
	for _, body := range []string{`{"at":"2026-10-25T10:00:00Z","x":1}`, `{"at":"2026-10-25T10:00:00Z"}{}`} {
		if code, _ := do(t, s, "POST", fmt.Sprintf("/api/boxes/%d/rtorrent/remove-failure/dismiss", id), body, hdr); code != 400 {
			t.Errorf("dismiss %s: %d", body, code)
		}
	}
	// Add: the shared form validation applies; a never-probed box is 409.
	code, body = do(t, s, "POST", fmt.Sprintf("/api/boxes/%d/rtorrent/torrents", id), "", hdr)
	if code != 400 {
		t.Fatalf("add without form: %d %s", code, body)
	}
	// Discovery: dht and pex only, a strict body; a never-probed box is
	// 409 and an unknown one 404.
	for _, tc := range []struct {
		id   int64
		body string
		want int
	}{
		{id, `{"lsd":true}`, 400},
		{id, `{}`, 400},
		{id, `{"dht":"on"}`, 400},
		{id, `not json`, 400},
		{id, `{"dht":true}`, 409},
		{999, `{"pex":false}`, 404},
	} {
		if code, body := do(t, s, "POST", fmt.Sprintf("/api/boxes/%d/rtorrent/discovery?rev=%d", tc.id, b.Revision), tc.body, hdr); code != tc.want {
			t.Errorf("discovery %s: %d %s", tc.body, code, body)
		}
	}
}

// A body is one JSON value: anything after it but whitespace is refused
// (daybreak blue audit of 7506bd4).
func TestDecodeOneValue(t *testing.T) {
	var v map[string]any
	for body, ok := range map[string]bool{`{"a":1}`: true, "{\"a\":1}\n \t": true, `{"a":1}{"b":2}`: false, `{"a":1} x`: false, `{"a":1}]`: false} {
		r := httptest.NewRequest("POST", "/", strings.NewReader(body))
		if err := decode(r, &v); (err == nil) != ok {
			t.Errorf("decode %q: %v", body, err)
		}
		r = httptest.NewRequest("POST", "/", strings.NewReader(body))
		if err := decodeStrict(r, &v); (err == nil) != ok {
			t.Errorf("decodeStrict %q: %v", body, err)
		}
	}
}

// Monitor-only mode refuses exactly the routes that could change a box, and
// any mutating route that changesNoBox does not list: the guarded set is
// pinned here, so classifying a new route is a reviewed decision. Routes
// that only touch the dashboard's records or read from a box still work.
func TestMonitorOnly(t *testing.T) {
	srv, st := newTestServer(t)
	hdr := map[string]string{"Origin": "http://dash.local:8080"}
	want := []string{
		"POST " + boxRoute + "/qbt/{action}",
		"POST " + boxRoute + "/qbt/torrents/{hash}/{action}",
		"POST " + boxRoute + "/qbt/torrents",
		"POST " + boxRoute + "/qbt/discovery",
		"POST " + boxRoute + "/rtorrent/torrents/{hash}/{action}",
		"POST " + boxRoute + "/rtorrent/torrents",
		"POST " + boxRoute + "/rtorrent/discovery",
		"POST " + boxRoute + "/containers/{cid}/{action}",
	}
	if !slices.Equal(srv.boxWrites, want) {
		t.Fatalf("guarded routes:\n%s\nwant:\n%s", strings.Join(srv.boxWrites, "\n"), strings.Join(want, "\n"))
	}
	if changesNoBox("POST /api/boxes/{id}/something-new") {
		t.Fatal("an unlisted route must count as box-changing")
	}
	b := &store.Box{Name: "x", Host: "h", User: "u", Enabled: true, QbtEnabled: true, RtEnabled: true}
	if err := st.CreateBox(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	h := strings.Repeat("a", 40)
	box := fmt.Sprintf("/api/boxes/%d", b.ID)
	blocked := []string{
		box + "/qbt/install", box + "/qbt/stop", box + "/qbt/pause",
		box + "/qbt/torrents/" + h + "/remove-data?rev=0", box + "/qbt/torrents", box + "/qbt/discovery",
		box + "/rtorrent/torrents/" + h + "/stop?rev=0", box + "/rtorrent/torrents", box + "/rtorrent/discovery",
		box + "/containers/c1/restart",
	}
	// Off: these reach their handlers (which then fail on the unprobed box,
	// never with the monitor-only refusal).
	for _, p := range blocked {
		if code, body := do(t, srv, "POST", p, `{}`, hdr); code == 403 {
			t.Errorf("off: %s refused: %s", p, body)
		}
	}
	srv.SetMonitorOnly(true)
	for _, p := range blocked {
		if code, body := do(t, srv, "POST", p, `{}`, hdr); code != 403 || !strings.Contains(body, "monitor-only") {
			t.Errorf("on: %s: %d %s", p, code, body)
		}
	}
	// Dashboard-side and read-only routes still work.
	if code, body := do(t, srv, "POST", box+"/refresh", "", hdr); code == 403 {
		t.Errorf("refresh refused: %s", body)
	}
	if code, body := do(t, srv, "POST", "/api/alerts/ack-all", "", hdr); code != 200 {
		t.Errorf("ack-all: %d %s", code, body)
	}
	code, body := do(t, srv, "GET", "/api/state", "", nil)
	var state struct {
		MonitorOnly bool `json:"monitor_only"`
	}
	if err := json.Unmarshal([]byte(body), &state); err != nil || code != 200 || !state.MonitorOnly {
		t.Fatalf("state: %d %s", code, body)
	}
}

// Every box-changing route names the box revision it was drawn from: none
// is refused (400), another is stale (409), both before the handler runs.
func TestBoxWritesNeedRevision(t *testing.T) {
	srv, _ := newTestServer(t)
	hdr := map[string]string{"Origin": "http://dash.local:8080"}
	b := &store.Box{Name: "x", Host: "x.invalid", User: "u", Port: 22, Enabled: true, QbtEnabled: true, RtEnabled: true}
	if err := srv.m.AddBox(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	h := strings.Repeat("a", 40)
	box := fmt.Sprintf("/api/boxes/%d", b.ID)
	cid := strings.Repeat("c", 64)
	for _, p := range []string{
		box + "/qbt/install", box + "/qbt/restart", box + "/qbt/pause",
		box + "/qbt/torrents/" + h + "/stop", box + "/qbt/torrents", box + "/qbt/discovery",
		box + "/rtorrent/torrents/" + h + "/stop", box + "/rtorrent/torrents", box + "/rtorrent/discovery",
		box + "/containers/" + cid + "/restart",
	} {
		if code, body := do(t, srv, "POST", p, `{}`, hdr); code != 400 || !strings.Contains(body, "rev") {
			t.Errorf("%s without rev: %d %s", p, code, body)
		}
		if code, body := do(t, srv, "POST", fmt.Sprintf("%s?rev=%d", p, b.Revision+1), `{}`, hdr); code != 409 || !strings.Contains(body, "edited since") {
			t.Errorf("%s with another rev: %d %s", p, code, body)
		}
		if code, body := do(t, srv, "POST", fmt.Sprintf("%s?rev=%d", p, b.Revision), `{}`, hdr); strings.Contains(body, "edited since") || strings.Contains(body, "rev (") {
			t.Errorf("%s with the current rev: %d %s", p, code, body)
		}
	}
	if code, body := do(t, srv, "POST", "/api/boxes/x/qbt/stop?rev=0", `{}`, hdr); code != 400 {
		t.Errorf("bad box id: %d %s", code, body)
	}
}

// The revision is checked again by the action itself, after the body was
// read: a box edited while a slow request body was still arriving is
// refused, not acted on as the edited box (fred on PR 136).
func TestBoxRevisionRecheckedAfterBody(t *testing.T) {
	srv, _ := newTestServer(t)
	ctx := context.Background()
	b := &store.Box{Name: "x", Host: "x.invalid", User: "u", Port: 22, Enabled: true, QbtEnabled: true}
	if err := srv.m.AddBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	pr, pw := io.Pipe()
	req := httptest.NewRequest("POST", fmt.Sprintf("/api/boxes/%d/qbt/discovery?rev=%d", b.ID, b.Revision), pr)
	req.Host = "dash.local:8080"
	req.Header.Set(MutationHeader, "1")
	req.Header.Set("Origin", "http://dash.local:8080")
	rr := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { srv.ServeHTTP(rr, req); close(done) }()
	// The handler is reading the body, so the admission check has passed.
	if _, err := pw.Write([]byte(`{"dht":`)); err != nil {
		t.Fatal(err)
	}
	ed := *b
	ed.Name = "edited"
	if err := srv.m.UpdateBox(ctx, &ed, store.Clears{}); err != nil {
		t.Fatal(err)
	}
	pw.Write([]byte(`true}`))
	pw.Close()
	<-done
	if rr.Code != 409 || !strings.Contains(rr.Body.String(), "edited since") {
		t.Fatalf("%d %s", rr.Code, rr.Body.String())
	}
}
