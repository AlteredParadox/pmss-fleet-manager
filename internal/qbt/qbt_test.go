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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"
	"unsafe"
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
		w.Write([]byte(`{"server_state":{"dl_info_speed":100,"up_info_speed":2000,"global_ratio":"4.21","connection_status":"firewalled"},"torrents":{"abc":{"name":"x","state":"uploading","size":10,"uploaded":30,"downloaded":10,"num_seeds":2,"num_complete":-1,"num_leechs":3,"num_incomplete":40}}}`))
	}))
	mux.HandleFunc("/api/v2/app/preferences", auth(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"dht":true,"pex":false,"lsd":true,"max_ratio":2,"listen_port":51413}`))
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
	if err != nil || md.ServerState.UPInfoSpeed != 2000 || md.ServerState.ConnectionStatus != "firewalled" || md.Torrents["abc"].Hash != "abc" {
		t.Fatalf("maindata %+v %v", md, err)
	}
	if tr := md.Torrents["abc"]; tr.Uploaded != 30 || tr.Downloaded != 10 || tr.Seeds != 2 || tr.SeedsSwarm != -1 || tr.Peers != 3 || tr.PeersSwarm != 40 {
		t.Fatalf("transfer/swarm fields %+v", tr)
	}
	if pr, err := c.Preferences(ctx); err != nil || !pr.DHT || pr.PeX || !pr.LSD || pr.ListenPort != 51413 {
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

// A response over maxBody is an explicit error, not a truncated body that
// then fails to decode; one of exactly maxBody still reads.
func TestOversizeResponse(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("v5.1.2" + strings.Repeat(" ", maxBody-len("v5.1.2"))))
	})
	mux.HandleFunc("/api/v2/sync/maindata", func(w http.ResponseWriter, r *http.Request) {
		// maxBody+1 bytes of valid JSON: cut at maxBody it would no longer parse.
		w.Write([]byte(`{"torrents":{` + strings.Repeat(" ", maxBody-len(`{"torrents":{`)-1) + "}}"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }, "127.0.0.1:1", "", "")
	if v, err := c.Version(context.Background()); err != nil || v != "v5.1.2" {
		t.Fatalf("body of exactly maxBody: %q %v", v, err)
	}
	if _, err := c.MainData(context.Background()); err == nil || !strings.Contains(err.Error(), "larger than 16 MiB") {
		t.Fatalf("want a size error, got %v", err)
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
// reads every answer shape: "Ok." (≤5.1), JSON counts and 409 (5.2+) as
// ErrAddRejected, 415 as ErrBadTorrent, other non-2xx as *HTTPError.
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
		switch answer {
		case "415":
			w.WriteHeader(http.StatusUnsupportedMediaType)
			return
		case "409":
			w.WriteHeader(http.StatusConflict)
			return
		case "500":
			w.WriteHeader(http.StatusInternalServerError)
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
	if res, err := c.Add(ctx, req); err != nil || res.Counted {
		t.Fatalf("Ok. → %+v %v", res, err)
	}
	want := map[string]string{"urls": "magnet:?xt=urn:btih:abc\nhttps://x/y.torrent", "category": "linux", "stopped": "true", "paused": "true", "file:a.torrent": "d4:infoe"}
	for k, v := range want {
		if seen[k] != v {
			t.Errorf("%s = %q, want %q", k, seen[k], v)
		}
	}
	// Not stopped: neither field may be sent, or the client's own
	// "add stopped" default would be overridden.
	seen = nil
	if _, err := c.Add(ctx, AddRequest{URLs: []string{"magnet:?xt=urn:btih:def"}}); err != nil {
		t.Fatal(err)
	}
	if seen["stopped"] != "" || seen["paused"] != "" {
		t.Errorf("unstopped add sent stopped=%q paused=%q", seen["stopped"], seen["paused"])
	}
	answer = "Fails."
	if _, err := c.Add(ctx, req); !errors.Is(err, ErrAddRejected) {
		t.Fatalf("Fails. → %v", err)
	}
	answer = "415"
	if _, err := c.Add(ctx, req); !errors.Is(err, ErrBadTorrent) {
		t.Fatalf("415 → %v", err)
	}
	// qBittorrent 5.2+: counts on success, 409 when nothing was added.
	answer = `{"success_count":1,"failure_count":2,"pending_count":0,"added_torrent_ids":["x"]}`
	if res, err := c.Add(ctx, req); err != nil || !res.Counted || res.Added != 1 || res.Failed != 2 {
		t.Fatalf("5.2 counts → %+v %v", res, err)
	}
	answer = `{"success_count":0,"failure_count":0,"pending_count":2}`
	if res, err := c.Add(ctx, req); err != nil || res.Pending != 2 {
		t.Fatalf("5.2 pending → %+v %v", res, err)
	}
	answer = "409"
	if _, err := c.Add(ctx, req); !errors.Is(err, ErrAddRejected) {
		t.Fatalf("409 → %v", err)
	}
	answer = "500"
	var he *HTTPError
	if _, err := c.Add(ctx, req); !errors.As(err, &he) || he.Code != 500 {
		t.Fatalf("500 → %v", err)
	}
	if _, err := c.Add(ctx, AddRequest{}); err == nil {
		t.Fatal("empty add accepted")
	}
}

func TestValidHash(t *testing.T) {
	v1 := "0123456789abcdef0123456789ABCDEF01234567"
	for h, ok := range map[string]bool{v1: true, v1 + v1[:24]: true, v1[:39]: false, v1 + "0": false, "..": false, "": false, v1[:39] + "…": false} {
		if ValidHash(h) != ok {
			t.Errorf("ValidHash(%q) = %v", h, !ok)
		}
	}
}

// Concurrent callers that all find the guard open must share one login
// attempt: each sending the rejected credentials would reach qBittorrent's
// ban threshold of five on its own.
func TestLoginSingleFlight(t *testing.T) {
	var logins atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) { logins.Add(1); w.WriteHeader(401) })
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }, "127.0.0.1:1", "admin", "wrong")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Version(context.Background()); !errors.Is(err, ErrAuth) {
				t.Errorf("want ErrAuth, got %v", err)
			}
		}()
	}
	wg.Wait()
	if n := logins.Load(); n != 1 {
		t.Fatalf("login attempted %d times by 8 concurrent calls; want exactly 1", n)
	}
}

// A caller refused while another caller's login is in flight retries with
// the new session instead of logging in again.
func TestLoginAfterReusesFreshSession(t *testing.T) {
	var logins atomic.Int32
	var mu sync.Mutex
	authed := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		logins.Add(1)
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: "s1"})
		mu.Lock()
		authed = true
		mu.Unlock()
		w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) {
		if c, _ := r.Cookie("SID"); c == nil || c.Value != "s1" {
			w.WriteHeader(403)
			return
		}
		w.Write([]byte("v5.2.4"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }, "127.0.0.1:1", "admin", "right")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if v, err := c.Version(context.Background()); err != nil || v != "v5.2.4" {
				t.Errorf("version: %q %v", v, err)
			}
		}()
	}
	wg.Wait()
	// Callers refused before the first login finished take its outcome;
	// only a request sent after it and still refused would log in again.
	if n := logins.Load(); n != 1 {
		t.Fatalf("logins = %d; want 1", n)
	}
	mu.Lock()
	defer mu.Unlock()
	if !authed {
		t.Fatal("never logged in")
	}
}

// A login whose connection drops after the credentials were sent is still
// one attempt against qBittorrent's counter: waiters share that failure
// instead of each sending the credentials again. A later, independent call
// may retry.
func TestLoginSharesDroppedAttempt(t *testing.T) {
	defer func(d time.Duration) { loginShareWindow = d }(loginShareWindow)
	loginShareWindow = 200 * time.Millisecond
	var logins atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		logins.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close() // qBittorrent saw the credentials; the dashboard sees no answer
	})
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }, "127.0.0.1:1", "admin", "pw")
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Version(context.Background()); err == nil {
				t.Error("want an error from the dropped login")
			}
		}()
	}
	wg.Wait()
	if n := logins.Load(); n != 1 {
		t.Fatalf("8 concurrent calls made %d login attempts; want 1", n)
	}
	time.Sleep(loginShareWindow + 50*time.Millisecond)
	if _, err := c.Version(context.Background()); err == nil {
		t.Fatal("want an error")
	}
	if n := logins.Load(); n != 2 {
		t.Fatalf("a later independent call must be allowed its own attempt; logins = %d", n)
	}
}

// Clients are rebuilt on reconnect and the wizard makes a fresh one per
// click, all on the box's one guard: a dropped login is shared across them
// too, so together they still make one attempt.
func TestDroppedLoginSharedAcrossClients(t *testing.T) {
	defer func(d time.Duration) { loginShareWindow = d }(loginShareWindow)
	loginShareWindow = 200 * time.Millisecond
	var logins atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		logins.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	})
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	dial := func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }
	g := &AuthGuard{}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		c := NewWithGuard(dial, "127.0.0.1:1", "admin", "pw", g) // a different client each time
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Version(context.Background()); err == nil {
				t.Error("want an error from the dropped login")
			}
		}()
	}
	wg.Wait()
	if n := logins.Load(); n != 1 {
		t.Fatalf("6 clients on one guard made %d login attempts; want 1", n)
	}
}

// A success is the cookie of the client that made it: another client on
// the same guard logs in for itself rather than retrying without a session.
func TestSuccessNotSharedAcrossClients(t *testing.T) {
	var logins atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/auth/login", func(w http.ResponseWriter, r *http.Request) {
		n := logins.Add(1)
		http.SetCookie(w, &http.Cookie{Name: "SID", Value: fmt.Sprintf("s%d", n)})
		w.Write([]byte("Ok."))
	})
	mux.HandleFunc("/api/v2/app/version", func(w http.ResponseWriter, r *http.Request) {
		if c, _ := r.Cookie("SID"); c == nil {
			w.WriteHeader(403)
			return
		}
		w.Write([]byte("v5.2.4"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	dial := func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }
	g := &AuthGuard{}
	a := NewWithGuard(dial, "127.0.0.1:1", "admin", "pw", g)
	b := NewWithGuard(dial, "127.0.0.1:1", "admin", "pw", g)
	for _, c := range []*Client{a, b} {
		if v, err := c.Version(context.Background()); err != nil || v != "v5.2.4" {
			t.Fatalf("version: %q %v", v, err)
		}
	}
	if n := logins.Load(); n != 2 {
		t.Fatalf("two clients need two sessions; logins = %d", n)
	}
}

// Logins that end without an answer spend a budget; once it is gone the
// guard holds like a rejection, and a seen outcome resets it.
func TestAmbiguousLoginBudget(t *testing.T) {
	g := &AuthGuard{}
	for i := 1; i < ambiguousBudget; i++ {
		g.noteAmbiguous("k")
		if _, held := g.Blocked("k"); held {
			t.Fatalf("held after %d ambiguous attempts", i)
		}
	}
	g.noteAmbiguous("k")
	if err, held := g.Blocked("k"); !held || err != ErrAmbiguous {
		t.Fatalf("budget spent: %v %v", err, held)
	}
	st := g.State()
	if st.Ambiguous != ambiguousBudget || st.AmbiguousKey != "k" || st.Error != ErrAmbiguous.Error() {
		t.Fatalf("state: %+v", st)
	}
	// A ban hold has no key (the IP is banned): the ambiguous count must
	// not make it credential-scoped through State/Restore.
	gb := &AuthGuard{}
	gb.noteAmbiguous("k")
	gb.fail(ErrBanned, time.Now().Add(time.Hour), "")
	sb := gb.State()
	if sb.Key != "" || sb.AmbiguousKey != "k" || sb.Ambiguous != 1 {
		t.Fatalf("ban state: %+v", sb)
	}
	gr := &AuthGuard{}
	gr.Restore(sb)
	if _, held := gr.Blocked("other"); !held {
		t.Fatal("a restored ban must hold for every credential")
	}
	if _, held := g.Blocked("other"); held { // other credentials end the hold, as for a rejection
		t.Fatal("other credentials are not held")
	}
	g2 := &AuthGuard{}
	g2.Restore(st)
	if err, held := g2.Blocked("k"); !held || err != ErrAmbiguous {
		t.Fatalf("restored: %v %v", err, held)
	}
	g2.Clear()
	if _, held := g2.Blocked("k"); held {
		t.Fatal("cleared")
	}
	// A dial failure sends nothing: it must not spend the budget, however
	// often it repeats, or a briefly unreachable box locks valid credentials.
	c := New(func(ctx context.Context) (net.Conn, error) { return nil, errors.New("reset") }, "127.0.0.1:8080", "admin", "secret")
	for i := 0; i < 2*ambiguousBudget; i++ {
		if err := c.Login(context.Background()); err == nil {
			t.Fatal("login must fail")
		}
	}
	if _, held := c.AuthBlocked(); held {
		t.Fatal("dial failures must not be charged as ambiguous")
	}
	// A server that reads the credentials and drops the connection is the
	// ambiguous case: charged, and the hold survives State/Restore.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			buf := make([]byte, 4096)
			conn.Read(buf)
			conn.Close()
		}
	}()
	addr := ln.Addr().String()
	c2 := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }, "127.0.0.1:8080", "admin", "secret")
	for i := 0; i < ambiguousBudget; i++ {
		if err := c2.Login(context.Background()); err == nil {
			t.Fatal("login must fail")
		}
	}
	if err, held := c2.AuthBlocked(); !held || err != ErrAmbiguous {
		t.Fatalf("after %d dropped answers: %v %v", ambiguousBudget, err, held)
	}
	g3 := &AuthGuard{}
	g3.Restore(c2.guard.State())
	if err, held := g3.Blocked(CredsKey("admin", "secret")); !held || err != ErrAmbiguous {
		t.Fatalf("restored: %v %v", err, held)
	}
}

// The ambiguous signal is per request: another request's traffic on the
// same client while a login fails before writing must not charge the
// budget.
func TestAmbiguousSignalIsPerRequest(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { // answers nothing, ever: every request times out
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				buf := make([]byte, 4096)
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}()
		}
	}()
	addr := ln.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }, "127.0.0.1:8080", "admin", "secret")
	// A concurrent non-login request that is still writing/waiting.
	bg, bgCancel := context.WithCancel(context.Background())
	defer bgCancel()
	go func() { _, _ = c.Version(bg) }()
	time.Sleep(50 * time.Millisecond)
	// Logins cancelled before they can be sent: a pre-cancelled context.
	done, cancel := context.WithCancel(context.Background())
	cancel()
	for i := 0; i < 2*ambiguousBudget; i++ {
		if err := c.Login(done); err == nil {
			t.Fatal("login must fail")
		}
	}
	if _, held := c.AuthBlocked(); held {
		t.Fatal("logins that never wrote must not be charged, whatever other requests did")
	}
}

// Incremental sync: the first request is rid=0 and every later one carries
// the last response id; a delta merges field by field into the retained
// view, torrents_removed drops entries, a failed request leaves the view
// and the id as they were, a full_update replaces the view, and Close
// starts over (external review 3, finding 8).
func TestIncrementalSync(t *testing.T) {
	var rids []string
	var step int
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/sync/maindata", func(w http.ResponseWriter, r *http.Request) {
		rids = append(rids, r.URL.Query().Get("rid"))
		switch step {
		case 0:
			w.Write([]byte(`{"rid":1,"full_update":true,"server_state":{"dl_info_speed":10,"up_info_speed":20,"connection_status":"connected"},"categories":{"tv":{"name":"tv","savePath":"/x"}},"tags":["a"],"trackers":{"http://t":["abc","def"]},"torrents":{"abc":{"name":"A","size":100,"upspeed":1,"state":"uploading"},"def":{"name":"D","size":200,"state":"stalledUP"}}}`))
		case 1:
			w.Write([]byte(`{"rid":2,"server_state":{"up_info_speed":99},"torrents":{"abc":{"upspeed":500},"ghi":{"name":"G","size":5}},"torrents_removed":["def"]}`))
		case 2:
			w.WriteHeader(500)
		case 3:
			w.Write([]byte(`{"rid":7,"full_update":true,"server_state":{"dl_info_speed":1},"torrents":{"zzz":{"name":"Z"}}}`))
		default:
			w.Write([]byte(`{"rid":8,"full_update":true,"torrents":{}}`))
		}
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }, "127.0.0.1:1", "", "")
	ctx := context.Background()

	md, err := c.MainData(ctx)
	if err != nil || len(md.Torrents) != 2 || md.Torrents["abc"].Hash != "abc" || md.ServerState.DLInfoSpeed != 10 {
		t.Fatalf("full: %+v %v", md, err)
	}
	step = 1
	md, err = c.MainData(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if a := md.Torrents["abc"]; a.UPSpeed != 500 || a.Name != "A" || a.Size != 100 || a.State != "uploading" {
		t.Fatalf("delta must change only the fields it carries: %+v", a)
	}
	if _, ok := md.Torrents["def"]; ok || md.Torrents["ghi"].Name != "G" || md.Torrents["ghi"].Hash != "ghi" || len(md.Torrents) != 2 {
		t.Fatalf("removed/added: %+v", md.Torrents)
	}
	if md.ServerState.UPInfoSpeed != 99 || md.ServerState.DLInfoSpeed != 10 || md.ServerState.ConnectionStatus != "connected" {
		t.Fatalf("server_state merges too: %+v", md.ServerState)
	}
	step = 2
	if _, err := c.MainData(ctx); err == nil {
		t.Fatal("want the 500")
	}
	step = 3
	md, err = c.MainData(ctx)
	if err != nil || len(md.Torrents) != 1 || md.Torrents["zzz"].Name != "Z" || md.ServerState.UPInfoSpeed != 0 || md.ServerState.DLInfoSpeed != 1 {
		t.Fatalf("full_update must replace the view: %+v %+v %v", md.Torrents, md.ServerState, err)
	}
	c.Close()
	step = 4
	if _, err := c.MainData(ctx); err != nil {
		t.Fatal(err)
	}
	// rid=0 first; then 1; the failed request asked with 2 and so does the
	// retry (nothing advanced), which got the full update; after Close, 0.
	if want := []string{"0", "1", "2", "2", "0"}; !slices.Equal(rids, want) {
		t.Fatalf("rids sent: %v, want %v", rids, want)
	}
}

// A response carrying more torrents than MaxTorrents is refused before the
// decoder materialises them, and an entry that does not decode leaves the
// retained view and its id untouched.
func TestSyncEntryCapAndBadDelta(t *testing.T) {
	var sb strings.Builder
	sb.WriteString(`{"rid":3,"full_update":true,"torrents":{`)
	for i := 0; i <= MaxTorrents; i++ {
		if i > 0 {
			sb.WriteByte(',')
		}
		fmt.Fprintf(&sb, `"%040d":{}`, i)
	}
	sb.WriteString(`}}`)
	if _, err := decodeMainData(strings.NewReader(sb.String()), MaxTorrents); err == nil || !strings.Contains(err.Error(), "more than") {
		t.Fatalf("want the cap, got %v", err)
	}
	d, err := decodeMainData(strings.NewReader(`{"rid":4,"torrents":{"abc":{"size":"not a number"}},"torrents_removed":null,"torrents":null}`), MaxTorrents)
	if err != nil {
		t.Fatal(err)
	}
	d, err = decodeMainData(strings.NewReader(`{"rid":4,"torrents":{"abc":{"size":"not a number"}},"torrents_removed":["x"],"trackers":{"u":[1,[2,{"a":null}]]}}`), MaxTorrents)
	if err != nil || d.rid != 4 || len(d.removed) != 1 {
		t.Fatalf("decode: %+v %v", d, err)
	}
	have := syncState{rid: 3, torrents: map[string]Torrent{"abc": {Hash: "abc", Size: 7}, "x": {Hash: "x"}}}
	next, err := have.apply(d, false)
	if err == nil || next.rid != 3 || next.torrents["abc"].Size != 7 || len(next.torrents) != 2 {
		t.Fatalf("a bad entry must not advance anything: %+v %v", next, err)
	}
	// Growing past the cap by deltas is refused as well.
	big := syncState{rid: 1, torrents: make(map[string]Torrent, MaxTorrents)}
	for i := 0; i < MaxTorrents; i++ {
		big.torrents[fmt.Sprintf("%040d", i)] = Torrent{}
	}
	if _, err := big.apply(&mainDelta{rid: 2, torrents: map[string]json.RawMessage{"new": json.RawMessage(`{}`)}}, false); err == nil {
		t.Fatal("want the cap")
	}
}

// Malformed sync bodies are errors, not partial views.
func TestDecodeMainDataMalformed(t *testing.T) {
	for _, body := range []string{
		``, `[]`, `{"torrents":[]}`, `{"torrents_removed":{}}`, `{"torrents":{"a":{}}`, `{"rid":"x"}`,
		`{"trackers":{"u":[1,2}}`, `{"torrents":{"a":1,}}`, `{"server_state":5`, `{"torrents_removed":[1]}`,
		`{"rid":1} x`, `{"rid":1}{"rid":2}`, `{"rid":1}]`,
		`{"torrents":{"` + strings.Repeat("a", maxHashLen+1) + `":{}}}`, `{"torrents_removed":["` + strings.Repeat("a", maxHashLen+1) + `"]}`,
	} {
		if _, err := decodeMainData(strings.NewReader(body), MaxTorrents); err == nil {
			t.Errorf("%q decoded", body)
		}
	}
	if d, err := decodeMainData(strings.NewReader("{\"rid\":1} \n\t"), MaxTorrents); err != nil || d.rid != 1 {
		t.Fatalf("trailing whitespace is fine: %+v %v", d, err)
	}
	// A server_state that is not an object fails at apply, leaving the view.
	d, err := decodeMainData(strings.NewReader(`{"rid":2,"server_state":[1]}`), MaxTorrents)
	if err != nil {
		t.Fatal(err)
	}
	have := syncState{rid: 1, torrents: map[string]Torrent{}}
	if next, err := have.apply(d, false); err == nil || next.rid != 1 {
		t.Fatalf("bad server_state: %+v %v", next, err)
	}
}

// A reset while a sync is in flight (Close is called under the poller's
// lock the request's dialer may need, so it must not wait for the sync
// lock) wins: the view built by that request is dropped by the next call.
func TestResetDuringSync(t *testing.T) {
	var mu sync.Mutex
	var rids []string
	release, arrived := make(chan struct{}), make(chan struct{}, 2)
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v2/sync/maindata", func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		rids = append(rids, r.URL.Query().Get("rid"))
		first := len(rids) == 1
		mu.Unlock()
		arrived <- struct{}{}
		if first {
			<-release
		}
		w.Write([]byte(`{"rid":5,"full_update":true,"torrents":{"a":{}}}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) }, "127.0.0.1:1", "", "")
	done := make(chan error, 1)
	go func() { _, err := c.MainData(context.Background()); done <- err }()
	select {
	case <-arrived:
	case <-time.After(5 * time.Second):
		t.Fatal("request never arrived")
	}
	closed := make(chan struct{})
	go func() { c.Close(); close(closed) }() // must return while the request is still blocked
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close waited for the in-flight sync")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := c.MainData(context.Background()); err != nil {
		t.Fatal(err)
	}
	<-arrived
	mu.Lock()
	defer mu.Unlock()
	if want := []string{"0", "0"}; !slices.Equal(rids, want) {
		t.Fatalf("rids sent: %v, want %v (the view from before the reset must not be used)", rids, want)
	}
}

// Retained memory is bounded by the entry count times fixed string bounds,
// not by what individual responses carry: a run of deltas that each add a
// torrent with a huge name retains only the clipped name each time, and
// the clip happens before anything is kept (fred review on PR 95).
func TestRetainedStringsBounded(t *testing.T) {
	huge := strings.Repeat("n", 64<<10) // 64 KiB name, state and category per entry
	view := syncState{}
	for i := 0; i < 20; i++ {
		raw := json.RawMessage(fmt.Sprintf(`{"name":"%s","state":"%s","category":"%s"}`, huge, huge, huge))
		d := &mainDelta{rid: int64(i + 1), full: i == 0, torrents: map[string]json.RawMessage{fmt.Sprintf("%040d", i): raw}}
		if i == 0 {
			d.server = json.RawMessage(`{"global_ratio":"` + huge + `","connection_status":"` + huge + `"}`)
		}
		next, err := view.apply(d, i == 0)
		if err != nil {
			t.Fatal(err)
		}
		view = next
	}
	total := len(view.server.GlobalRatio) + len(view.server.ConnectionStatus)
	for _, tr := range view.torrents {
		total += len(tr.Hash) + len(tr.Name) + len(tr.State) + len(tr.Category)
	}
	if bound := 4 * (20*(maxHashLen+maxNameLen+maxStateLen+maxCategoryLen) + 2*maxServerStr); total > bound || len(view.torrents) != 20 || view.rid != 20 {
		t.Fatalf("retained %d bytes of strings in %d torrents (rid %d), bound %d", total, len(view.torrents), view.rid, bound)
	}
	if n := view.torrents[fmt.Sprintf("%040d", 7)].Name; len(n) != maxNameLen {
		t.Fatalf("name clipped to %d, want %d", len(n), maxNameLen)
	}
	// The bounds are runes, as the display limits are: 400 emoji keep all
	// 400 (1600 bytes), and a 401st goes.
	emoji := strings.Repeat("\U0001F600", maxNameLen+1)
	if got := clipStr(emoji, maxNameLen); utf8.RuneCountInString(got) != maxNameLen || len(got) != 4*maxNameLen || !utf8.ValidString(got) {
		t.Fatalf("rune clip: %d runes, %d bytes", utf8.RuneCountInString(got), len(got))
	}
	if got := clipStr("ab\u00e9", 3); got != "ab\u00e9" {
		t.Fatalf("three runes fit: %q", got)
	}
	// A clipped prefix must not alias the oversized source, or the whole
	// source stays allocated for as long as the view keeps the prefix
	// (fred review on PR 95).
	src := strings.Repeat("x", 1<<16)
	if got := clipStr(src, 10); len(got) != 10 || unsafe.StringData(got) == unsafe.StringData(src) {
		t.Fatal("clipped prefix aliases the source allocation")
	}
	if got := clipStr(src[:10], 10); unsafe.StringData(got) != unsafe.StringData(src) {
		t.Fatal("an unclipped string need not be copied")
	}
}
