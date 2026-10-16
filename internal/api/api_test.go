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
	"io"
	"log/slog"
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
	return New(st, m, slog.New(slog.NewTextHandler(io.Discard, nil)), ""), st
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
