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
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
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
	if st.Ambiguous != ambiguousBudget || st.Error != ErrAmbiguous.Error() {
		t.Fatalf("state: %+v", st)
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
	// Transport failure during login counts as ambiguous.
	c := New(func(ctx context.Context) (net.Conn, error) { return nil, errors.New("reset") }, "127.0.0.1:8080", "admin", "secret")
	for i := 0; i < ambiguousBudget; i++ {
		if err := c.Login(context.Background()); err == nil {
			t.Fatal("login must fail")
		}
	}
	if err, held := c.AuthBlocked(); !held || err != ErrAmbiguous {
		t.Fatalf("after %d failed transports: %v %v", ambiguousBudget, err, held)
	}
}
