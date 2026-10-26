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

package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"pmss-fleet-manager/internal/api"
	"pmss-fleet-manager/internal/collect"
	"pmss-fleet-manager/internal/store"
)

// The URL comes from the listen setting: a wildcard is reached on
// loopback, IPv6 is bracketed, a list prefers its first loopback or
// wildcard entry; the path is always the root /healthz.
func TestHealthURL(t *testing.T) {
	for _, tc := range []struct{ listen, want string }{
		{"127.0.0.1:8080", "http://127.0.0.1:8080/healthz"},
		{"0.0.0.0:8080", "http://127.0.0.1:8080/healthz"},
		{":8080", "http://127.0.0.1:8080/healthz"},
		{"[::]:9000", "http://[::1]:9000/healthz"},
		{"[::1]:9000", "http://[::1]:9000/healthz"},
		{"localhost:8080", "http://localhost:8080/healthz"},
		{"10.11.0.13:8080, 127.0.0.1:8081", "http://127.0.0.1:8081/healthz"},
		{"10.11.0.13:8080,192.168.1.2:8080", "http://10.11.0.13:8080/healthz"},
		{"[fd00::5]:8080", "http://[fd00::5]:8080/healthz"},
	} {
		if got, err := healthURL(tc.listen); err != nil || got != tc.want {
			t.Errorf("%q: %q %v, want %q", tc.listen, got, err, tc.want)
		}
	}
	for _, bad := range []string{"", " , ", "8080", "host"} {
		if _, err := healthURL(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// Against the real handler with a login and a base path set, as a server
// started with -base-path would be while the check (its own process) knows
// nothing of it: the root /healthz answers without a login (and without
// counting a failure), so does the one under the base path, the API still
// asks for the login, and a dead port or a non-200 is a failure.
func TestHealthcheckAgainstServer(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := collect.New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	srv := httptest.NewServer(api.New(st, m, "me:pw", "/fleet"))
	defer srv.Close()
	_, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	u, err := healthURL("0.0.0.0:" + port)
	if err != nil {
		t.Fatal(err)
	}
	if err := probeHealth(srv.URL+"/fleet/healthz", 2*time.Second); err != nil {
		t.Fatalf("under the base path: %v", err)
	}
	for i := 0; i < 20; i++ { // well past the failed-login throttle's free tries
		if err := probeHealth(u, 2*time.Second); err != nil {
			t.Fatalf("probe %d: %v", i, err)
		}
	}
	if resp, err := http.Get(srv.URL + "/fleet/api/state"); err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("api without login: %v %v", resp, err)
	}
	if code := healthcheck([]string{"-healthcheck-url", srv.URL + "/fleet/nope"}); code != 1 {
		t.Fatalf("non-200: exit %d", code)
	}
	if code := healthcheck([]string{"-listen", "127.0.0.1:" + port}); code != 0 {
		t.Fatalf("derived URL: exit %d", code)
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	dead := ln.Addr().String()
	ln.Close()
	if code := healthcheck([]string{"-listen", dead, "-timeout", "1s"}); code != 1 {
		t.Fatalf("dead port: exit %d", code)
	}
}
