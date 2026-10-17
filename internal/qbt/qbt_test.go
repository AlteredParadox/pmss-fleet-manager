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

package qbt

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientFlow(t *testing.T) {
	loggedIn := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("username") == "admin" && r.FormValue("password") == "secret" {
			loggedIn = true
			http.SetCookie(w, &http.Cookie{Name: "SID", Value: "x"})
			w.Write([]byte("Ok."))
			return
		}
		w.Write([]byte("Fails."))
	})
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if c, _ := r.Cookie("SID"); c == nil || !loggedIn {
				w.WriteHeader(403)
				return
			}
			h(w, r)
		}
	}
	mux.HandleFunc("/api/v2/app/version", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.Host != "127.0.0.1:8080" { // qBittorrent's host header validation
			w.WriteHeader(401)
			return
		}
		w.Write([]byte("v5.1.0"))
	}))
	mux.HandleFunc("/api/v2/sync/maindata", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"server_state":{"dl_info_speed":100,"up_info_speed":2000,"global_ratio":"4.21"},"torrents":{"abc":{"name":"x","state":"uploading","size":10}}}`))
	}))
	mux.HandleFunc("/api/v2/torrents/pause", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.FormValue("hashes") != "all" {
			w.WriteHeader(400)
		}
	}))
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }, "127.0.0.1:8080", "admin", "secret")
	ctx := context.Background()
	v, err := c.Version(ctx)
	if err != nil || v != "v5.1.0" {
		t.Fatalf("version %q %v", v, err)
	}
	md, err := c.MainData(ctx)
	if err != nil || md.ServerState.UPInfoSpeed != 2000 || md.Torrents["abc"].Hash != "abc" {
		t.Fatalf("maindata %+v %v", md, err)
	}
	if err := c.PauseAll(ctx); err != nil { // /stop is 404 here, falls back to /pause
		t.Fatal(err)
	}
	bad := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }, "127.0.0.1:8080", "admin", "wrong")
	if _, err := bad.Version(ctx); err != ErrAuth {
		t.Fatalf("want ErrAuth, got %v", err)
	}
	if StateKind("stalledUP") != "seeding" || StateKind("stoppedDL") != "paused" {
		t.Fatal("state kinds")
	}
}

func TestLogin204(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("v5.1.2")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }, "127.0.0.1:1", "", "")
	if err := c.Login(context.Background()); err != nil {
		t.Fatalf("204 login must succeed: %v", err)
	}
	if v, err := c.Version(context.Background()); err != nil || v != "v5.1.2" {
		t.Fatalf("%q %v", v, err)
	}
}

func TestAuthBackoffAndBan(t *testing.T) {
	logins := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) { logins++; w.WriteHeader(401) })
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }, "127.0.0.1:1", "", "")
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if _, err := c.Version(ctx); err == nil {
			t.Fatal("want error")
		}
	}
	if logins != 1 {
		t.Fatalf("login attempted %d times; backoff must allow only one", logins)
	}
	if _, held := c.AuthBlocked(); !held {
		t.Fatal("expected auth hold")
	}

	mux2 := http.NewServeMux()
	mux2.HandleFunc("/api/v2/sync/maindata", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		w.Write([]byte("Your IP address has been banned after too many failed authentication attempts."))
	})
	srv2 := httptest.NewServer(mux2)
	defer srv2.Close()
	addr2 := srv2.Listener.Addr().String()
	c2 := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr2) }, "127.0.0.1:1", "", "")
	_, err := c2.MainData(ctx)
	if err != ErrBanned {
		t.Fatalf("want ErrBanned, got %v", err)
	}
}

func TestAuthGuardSurvivesClientRebuild(t *testing.T) {
	logins := 0
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) { logins++; w.WriteHeader(401) })
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(401) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	dial := func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }
	ctx := context.Background()
	g := &AuthGuard{}
	// A rebuilt client (SSH reconnect, restart) must not get a fresh attempt.
	for i := 0; i < 3; i++ {
		c := NewWithGuard(dial, "127.0.0.1:1", "u", "p", g)
		if _, err := c.Version(ctx); err == nil {
			t.Fatal("want error")
		}
	}
	if logins != 1 {
		t.Fatalf("login attempted %d times across rebuilds; want 1", logins)
	}
	// Persist and restore: still held.
	g2 := &AuthGuard{}
	g2.Restore(g.State())
	if err, held := g2.Blocked(CredsKey("u", "p")); !held || err != ErrAuth {
		t.Fatalf("restored guard not held: %v %v", err, held)
	}
	// Different credentials are a new attempt.
	if _, held := g2.Blocked(CredsKey("u", "other")); held {
		t.Fatal("changed credentials must clear the hold")
	}
	// Explicit clear (after a client restart) allows one more attempt.
	g.Clear()
	c := NewWithGuard(dial, "127.0.0.1:1", "u", "p", g)
	c.Version(ctx)
	if logins != 2 {
		t.Fatalf("after Clear, login attempts = %d, want 2", logins)
	}
}
