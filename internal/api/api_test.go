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
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"pmss-fleet-manager/internal/collect"
	"pmss-fleet-manager/internal/store"
)

func newTestServer(t *testing.T) (*Server, *store.Store) {
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
	return New(st, m, ""), st
}

func do(t *testing.T, h http.Handler, method, path, body string, hdr map[string]string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Host = "dash.local:8080"
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr.Code, rr.Body.String()
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
	s.SetAllowedHosts([]string{"localhost", "127.0.0.1", "Dash.Local"})
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
	code, body = do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), `{"name":"b2","host":"a.example","user":"u"}`, hdr)
	if code != 200 {
		t.Fatalf("edit: %d %s", code, body)
	}
	b, _ := st.GetBox(context.Background(), id)
	if b.HostKey == "" {
		t.Fatal("pin must survive an unrelated edit")
	}
	// Host change: pin cleared.
	do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), `{"name":"b2","host":"b.example","user":"u"}`, hdr)
	b, _ = st.GetBox(context.Background(), id)
	if b.HostKey != "" {
		t.Fatal("pin must clear when the host changes")
	}
	// Re-pin, then explicit forget.
	do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), `{"name":"b2","host":"b.example","user":"u","host_key":"ssh-ed25519 BBBB"}`, hdr)
	do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), `{"name":"b2","host":"b.example","user":"u","forget_host_key":true}`, hdr)
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
	do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), `{"name":"b","host":"a.example","user":"u","auth_method":"key","qbt_user":"q","clear_qbt_pass":true}`, hdr)
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
	code, body := do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), `{"name":"b","host":"b.example","user":"u","auth_method":"password"}`, hdr)
	if code != 400 || !strings.Contains(body, "re-enter") || get().Host != "A.example" {
		t.Fatalf("host change without password: %d %s host=%s", code, body, get().Host)
	}
	// Changed user only: same refusal (secrets are bound to the user too).
	if code, _ = do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), `{"name":"b","host":"a.example","user":"v","auth_method":"password"}`, hdr); code != 400 {
		t.Fatalf("user change without password: %d", code)
	}
	// Same endpoint (host case differs): a supplied key must not replace the pin.
	do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), `{"name":"b2","host":"a.example","user":"u","auth_method":"password","host_key":"ssh-ed25519 OTHER"}`, hdr)
	if b := get(); b.HostKey != "ssh-ed25519 PIN" || b.Name != "b2" || b.Password != "pw" {
		t.Fatalf("unchanged endpoint must keep pin and password: %+v", b)
	}
	// Forget wins over a supplied key.
	do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), `{"name":"b2","host":"a.example","user":"u","auth_method":"password","host_key":"ssh-ed25519 OTHER","forget_host_key":true}`, hdr)
	if get().HostKey != "" {
		t.Fatal("forget_host_key must clear the pin even when a key is supplied")
	}
	// Changed host WITH a typed password: allowed, and the fresh detect key is pinned.
	do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), `{"name":"b2","host":"b.example","user":"u","auth_method":"password","password":"pw2","host_key":"ssh-ed25519 NEW"}`, hdr)
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
	code, body := do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), `{"name":"b","host":"b.example","user":"u","auth_method":"key","qbt_user":"q"}`, hdr)
	if code != 400 || !strings.Contains(body, "WebUI password") {
		t.Fatalf("move with saved WebUI password and blank field: %d %s", code, body)
	}
	if code, _ = do(t, s, "PUT", fmt.Sprintf("/api/boxes/%d", id), `{"name":"b","host":"b.example","user":"u","auth_method":"key","qbt_user":"q","clear_qbt_pass":true}`, hdr); code != 200 {
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
