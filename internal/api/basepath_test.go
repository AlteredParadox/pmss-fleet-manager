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
	"net/http/httptest"
	"strings"
	"testing"
)

// The served page must reference its assets relative to its own URL, so the
// same embedded files work at the root and under a base path.
func TestIndexAndAssetsAtRoot(t *testing.T) {
	srv, _ := newTestServer(t)
	code, body := do(t, srv, "GET", "/", "", nil)
	if code != 200 || !strings.Contains(body, `href="static/style.css"`) || !strings.Contains(body, `src="static/app.js"`) {
		t.Fatalf("index: %d %.200s", code, body)
	}
	if strings.Contains(body, "__BASE_PATH__") {
		t.Fatal("index still carries a placeholder")
	}
	code, body = do(t, srv, "GET", "/static/app.js", "", nil)
	if code != 200 || strings.Contains(body, "__BASE_PATH__") || !strings.Contains(body, "location.pathname") {
		t.Fatalf("app.js: %d (placeholder present %v)", code, strings.Contains(body, "__BASE_PATH__"))
	}
}

func TestBasePathRoutes(t *testing.T) {
	srv, _ := newTestServerWith(t, "u:p", "/fleet/") // trailing slash is normalised away
	srv.SetLegal([]byte("license text"), []byte("notices"))
	auth := map[string]string{"Authorization": "Basic dTpw"} // u:p
	with := func(extra map[string]string) map[string]string {
		h := map[string]string{"Authorization": "Basic dTpw"}
		for k, v := range extra {
			h[k] = v
		}
		return h
	}
	cases := []struct {
		name, method, path string
		hdr                map[string]string
		want               int
	}{
		{"page under prefix", "GET", "/fleet/", auth, 200},
		{"prefix without slash redirects", "GET", "/fleet", auth, 307},
		{"asset under prefix", "GET", "/fleet/static/style.css", auth, 200},
		{"api under prefix", "GET", "/fleet/api/state", auth, 200},
		{"api needs auth", "GET", "/fleet/api/state", nil, 401},
		{"license is public under prefix", "GET", "/fleet/license", nil, 200},
		{"source is public under prefix", "GET", "/fleet/source", nil, 302},
		{"no route at the root", "GET", "/", auth, 404},
		{"no api at the root", "GET", "/api/state", auth, 404},
		{"root license is not public", "GET", "/license", nil, 401},
		{"action named license is not public", "POST", "/fleet/api/boxes/1/qbt/license", nil, 401},
		{"action named source is not public", "POST", "/fleet/api/boxes/1/containers/x/source", nil, 401},
		{"cross-origin POST refused", "POST", "/fleet/api/detect", with(map[string]string{"Origin": "https://evil.example"}), 403},
		{"cross-site POST refused", "POST", "/fleet/api/detect", with(map[string]string{"Sec-Fetch-Site": "cross-site"}), 403},
		{"same-origin POST reaches the handler", "POST", "/fleet/api/alerts/ack-all", with(map[string]string{"Origin": "http://dash.local:8080"}), 200},
	}
	for _, c := range cases {
		code, body := do(t, srv, c.method, c.path, "", c.hdr)
		if code != c.want {
			t.Errorf("%s: %s %s = %d, want %d (%.120s)", c.name, c.method, c.path, code, c.want, body)
		}
	}
}

func TestNormalizeBasePath(t *testing.T) {
	ok := map[string]string{
		"": "", " ": "", "/": "", "//": "",
		"/fleet": "/fleet", "/fleet/": "/fleet", "fleet": "/fleet", "fleet/": "/fleet",
		"/user-vaina/fleet/": "/user-vaina/fleet", "/a//b/./c/": "/a/b/c", "/../x": "/x",
	}
	for in, want := range ok {
		got, err := NormalizeBasePath(in)
		if err != nil || got != want {
			t.Errorf("NormalizeBasePath(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"/fle et", "/x{y}", "/a?b", "/a#b", "/a\tb", "/a\x7fb", "/a}"} {
		if got, err := NormalizeBasePath(in); err == nil {
			t.Errorf("NormalizeBasePath(%q) = %q, want error", in, got)
		}
	}
}

func TestNewPanicsOnInvalidBasePath(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("New accepted a base path with a wildcard")
		}
	}()
	New(nil, nil, "", "/x{y}")
}

// The web font is served from the binary with a font MIME type, and neither
// the page nor the CSP refers to a third-party host any more.
func TestEmbeddedFont(t *testing.T) {
	srv, _ := newTestServer(t)
	req := httptest.NewRequest("GET", "/static/fonts/JetBrainsMono-Regular.woff2", nil)
	rr := httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if rr.Code != 200 || rr.Header().Get("Content-Type") != "font/woff2" || !strings.HasPrefix(rr.Body.String(), "wOF2") {
		t.Fatalf("font: %d %q %.4q", rr.Code, rr.Header().Get("Content-Type"), rr.Body.String())
	}
	if !strings.Contains(rr.Header().Get("Cache-Control"), "max-age") {
		t.Fatalf("font cache-control: %q", rr.Header().Get("Cache-Control"))
	}
	req = httptest.NewRequest("GET", "/", nil)
	rr = httptest.NewRecorder()
	srv.ServeHTTP(rr, req)
	if strings.Contains(rr.Body.String(), "googleapis") || strings.Contains(rr.Header().Get("Content-Security-Policy"), "google") {
		t.Fatal("page or CSP still refers to Google Fonts")
	}
}
