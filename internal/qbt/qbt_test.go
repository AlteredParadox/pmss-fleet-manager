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
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
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
		w.Write([]byte(`{"server_state":{"dl_info_speed":100,"up_info_speed":2000,"global_ratio":"4.21"},"torrents":{"abc":{"name":"x","state":"uploading","size":10,"uploaded":30,"downloaded":10,"num_seeds":2,"num_complete":-1,"num_leechs":3,"num_incomplete":40}}}`))
	}))
	mux.HandleFunc("/api/v2/app/preferences", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"dht":true,"pex":false,"lsd":true,"max_ratio":2}`))
	}))
	var setPrefs string
	mux.HandleFunc("/api/v2/app/setPreferences", auth(func(w http.ResponseWriter, r *http.Request) { setPrefs = r.FormValue("json") }))
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
	if tr := md.Torrents["abc"]; tr.Uploaded != 30 || tr.Downloaded != 10 || tr.Seeds != 2 || tr.SeedsSwarm != -1 || tr.Peers != 3 || tr.PeersSwarm != 40 {
		t.Fatalf("transfer/swarm fields %+v", tr)
	}
	if pr, err := c.Preferences(ctx); err != nil || !pr.DHT || pr.PeX || !pr.LSD {
		t.Fatalf("preferences %+v %v", pr, err)
	}
	if err := c.SetPreferences(ctx, map[string]any{"dht": false}); err != nil || setPrefs != `{"dht":false}` {
		t.Fatalf("setPreferences sent %q, %v", setPrefs, err)
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

func TestAuthHoldAndBan(t *testing.T) {
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
		t.Fatalf("login attempted %d times; the hold must allow only one", logins)
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

// Per-torrent actions address one hash, fall back from the qBittorrent 5
// endpoint names to the 4.x ones, and only "remove-data" deletes files.
func TestTorrentAction(t *testing.T) {
	var got []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("v4.6.0")) })
	for _, p := range []string{"pause", "resume", "delete"} {
		p := p
		mux.HandleFunc("/api/v2/torrents/"+p, func(w http.ResponseWriter, r *http.Request) {
			got = append(got, p+" "+r.FormValue("hashes")+" "+r.FormValue("deleteFiles"))
		})
	}
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }, "127.0.0.1:8080", "admin", "secret")
	ctx := context.Background()
	h := "0123456789abcdef0123456789abcdef01234567"
	for _, a := range []string{"stop", "start", "remove", "remove-data"} {
		if err := c.TorrentAction(ctx, a, h); err != nil {
			t.Fatalf("%s: %v", a, err)
		}
	}
	want := []string{"pause " + h + " ", "resume " + h + " ", "delete " + h + " false", "delete " + h + " true"}
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Fatalf("calls %q, want %q", got, want)
	}
	if err := c.TorrentAction(ctx, "recheck", h); err == nil {
		t.Fatal("unknown action accepted")
	}
}

// Add posts a multipart torrents/add with links, files and options, and
// reads qBittorrent's "Fails." / 415 answers as ErrBadTorrent.
func TestAdd(t *testing.T) {
	answer := "Ok."
	var seen map[string]string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("/api/v2/torrents/add", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseMultipartForm(1 << 20); err != nil {
			w.WriteHeader(400)
			return
		}
		seen = map[string]string{"urls": r.FormValue("urls"), "category": r.FormValue("category"), "stopped": r.FormValue("stopped"), "paused": r.FormValue("paused")}
		for _, fh := range r.MultipartForm.File["torrents"] {
			f, _ := fh.Open()
			b, _ := io.ReadAll(f)
			f.Close()
			seen["file:"+fh.Filename] = string(b)
		}
		if answer == "415" {
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		}
		w.Write([]byte(answer))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }, "127.0.0.1:8080", "admin", "secret")
	ctx := context.Background()
	req := AddRequest{URLs: []string{"magnet:?xt=urn:btih:abc", "https://x/y.torrent"}, Files: []TorrentFile{{Name: "a.torrent", Data: []byte("d4:infoe")}}, Category: "linux", Stopped: true}
	if err := c.Add(ctx, req); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"urls": "magnet:?xt=urn:btih:abc\nhttps://x/y.torrent", "category": "linux", "stopped": "true", "paused": "true", "file:a.torrent": "d4:infoe"}
	for k, v := range want {
		if seen[k] != v {
			t.Errorf("%s = %q, want %q", k, seen[k], v)
		}
	}
	answer = "Fails."
	if err := c.Add(ctx, req); !errors.Is(err, ErrBadTorrent) {
		t.Fatalf("Fails. → %v", err)
	}
	answer = "415"
	if err := c.Add(ctx, req); !errors.Is(err, ErrBadTorrent) {
		t.Fatalf("415 → %v", err)
	}
	if err := c.Add(ctx, AddRequest{}); err == nil {
		t.Fatal("empty add accepted")
	}
}
