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

package collect

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pmss-fleet-manager/internal/store"
)

func TestPMDBRow(t *testing.T) {
	st := &BoxState{Hostname: "Le4-0-105-238louhi.pulsedmedia.com.", HostSys: HostState{
		CPUModel: "Intel(R) Core(TM) i7-12700", NCPU: 20, CPUMHz: 2100, MemTotal: 64 << 30, SwapTotal: 8 << 30, SwapKnown: true,
		HomeSize: 15000000000000, GPUPresent: true, GPUVendor: "intel"}}
	row, ok := pmdbRow(st)
	if !ok {
		t.Fatal("row refused")
	}
	if row.ServerName != "le4-0-105-238louhi.pulsedmedia.com" || row.CoreCount != 20 || row.RAMGiB != 64 || row.StorageTiB != 13.64 ||
		row.ProcessorMHz == nil || *row.ProcessorMHz != 2100 || row.SwapGiB == nil || *row.SwapGiB != 8 || !row.QSV {
		t.Fatalf("row: %+v", row)
	}
	// An AMD GPU is not QuickSync; unknown swap and clock go as null.
	st.HostSys.GPUVendor, st.HostSys.SwapKnown, st.HostSys.CPUMHz = "amd", false, 0
	row, _ = pmdbRow(st)
	if row.QSV || row.SwapGiB != nil || row.ProcessorMHz != nil {
		t.Fatalf("row: %+v", row)
	}
	// Missing required facts (no inventory yet) refuse the row.
	for _, fix := range []func(*BoxState){
		func(s *BoxState) { s.HostSys.CPUModel = "" },
		func(s *BoxState) { s.HostSys.NCPU = 0 },
		func(s *BoxState) { s.HostSys.MemTotal = 0 },
		func(s *BoxState) { s.HostSys.HomeSize = 0 },
		func(s *BoxState) { s.Hostname = "not a host" },
	} {
		c := *st
		fix(&c)
		if _, ok := pmdbRow(&c); ok {
			t.Errorf("row accepted for %+v", c.HostSys)
		}
	}
}

// Two boxes on one host and one on a listed host: one list fetch, one
// submission, a 409 counted as listed, a refused key stopping the run and
// shown once, and nothing sent while the integration is off.
func TestSyncPMDB(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var submitted []string
	var lists int
	status := http.StatusCreated
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/api/servers":
			lists++
			_, _ = io.WriteString(w, `[{"serverName":"thor.pulsedmedia.com"}]`)
		case "/api/servers/raw":
			if r.Header.Get("Authorization") != "Bearer a-passphrase-of-words" {
				w.WriteHeader(401)
				return
			}
			var row map[string]any
			_ = json.NewDecoder(r.Body).Decode(&row)
			submitted = append(submitted, row["serverName"].(string))
			w.WriteHeader(status)
		}
	}))
	defer srv.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmdb.URL = srv.URL
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	hw := HostState{CPUModel: "Intel(R) Core(TM) i7-12700", NCPU: 20, MemTotal: 64 << 30, HomeSize: 15 << 40, GPUPresent: true, GPUVendor: "intel"}
	for i, host := range []string{"odin.pulsedmedia.com", "ODIN.pulsedmedia.com", "thor.pulsedmedia.com", "loki.pulsedmedia.com"} {
		b := &store.Box{Name: string(rune('a' + i)), Host: "h" + string(rune('a'+i)) + ".example", User: "u", Port: 22, Enabled: false}
		if err := m.AddBox(ctx, b); err != nil {
			t.Fatal(err)
		}
		m.mutateState(b.ID, func(s *BoxState) { s.Ready, s.Hostname, s.HostSys = true, host, hw })
	}

	// Off: nothing leaves, and the view says so.
	m.SyncPMDB(ctx)
	if v := m.PMDB(); v.Enabled || v.Configured || len(v.Hosts) != 0 || lists != 0 {
		t.Fatalf("disabled view: %+v lists=%d", v, lists)
	}

	s := m.Settings()
	s.PMDBEnabled, s.PMDBUploadKey = true, "a-passphrase-of-words"
	m.SetSettings(s)
	m.SyncPMDB(ctx)
	mu.Lock()
	got := append([]string(nil), submitted...)
	mu.Unlock()
	if len(got) != 2 || got[0] != "loki.pulsedmedia.com" || got[1] != "odin.pulsedmedia.com" {
		t.Fatalf("submitted %v", got)
	}
	v := m.PMDB()
	if !v.Configured || v.Error != "" || len(v.Hosts) != 3 || v.CheckedAt == "" {
		t.Fatalf("view: %+v", v)
	}
	byName := map[string]pmdbStatus{}
	for _, h := range v.Hosts {
		byName[h.Hostname] = h
	}
	if byName["thor.pulsedmedia.com"].Status != "listed" || byName["odin.pulsedmedia.com"].Status != "submitted" || len(byName["odin.pulsedmedia.com"].BoxIDs) != 2 {
		t.Fatalf("hosts: %+v", byName)
	}

	// A second run submits nothing again: the submitted ones are remembered
	// even though the mock list still does not show them.
	m.SyncPMDB(ctx)
	mu.Lock()
	n := len(submitted)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("resubmitted: %d", n)
	}

	// A new key starts the display over but not the decisions: odin and
	// loki stay settled. A fresh host is what meets the refused key.
	fresh := &store.Box{Name: "e", Host: "he.example", User: "u", Port: 22, Enabled: false}
	if err := m.AddBox(ctx, fresh); err != nil {
		t.Fatal(err)
	}
	m.mutateState(fresh.ID, func(s *BoxState) { s.Ready, s.Hostname, s.HostSys = true, "freya.pulsedmedia.com", hw })
	status = http.StatusConflict
	s.PMDBUploadKey = "another-passphrase-of-words"
	m.SetSettings(s)
	if v := m.PMDB(); len(v.Hosts) != 0 {
		t.Fatalf("state not reset on key change: %+v", v)
	}
	m.SyncPMDB(ctx)
	v = m.PMDB()
	if v.Error == "" {
		t.Fatalf("refused key not reported: %+v", v)
	}
	mu.Lock()
	n = len(submitted)
	mu.Unlock()
	if n != 2 {
		t.Fatalf("the refused key kept submitting: %d", n)
	}
	// Later runs under the same refused key post nothing more and keep
	// showing the refusal, until the key changes.
	m.SyncPMDB(ctx)
	mu.Lock()
	n = len(submitted)
	mu.Unlock()
	if v := m.PMDB(); n != 2 || v.Error == "" {
		t.Fatalf("a run under the refused key went ahead: %d %+v", n, v)
	}
	// The good key again: only the fresh host is posted (and its 409
	// counts as listed); odin and loki were accepted earlier in this
	// process, so the reset did not unsettle them and they show as
	// submitted although the mock list still omits them.
	s.PMDBUploadKey = "a-passphrase-of-words"
	m.SetSettings(s)
	m.SyncPMDB(ctx)
	v = m.PMDB()
	for _, h := range v.Hosts {
		byName[h.Hostname] = h
	}
	mu.Lock()
	got = append([]string(nil), submitted...)
	mu.Unlock()
	if v.Error != "" || len(got) != 3 || got[2] != "freya.pulsedmedia.com" || byName["freya.pulsedmedia.com"].Status != "listed" ||
		byName["odin.pulsedmedia.com"].Status != "submitted" || byName["loki.pulsedmedia.com"].Status != "submitted" {
		t.Fatalf("after the key came back: %v %+v", got, v)
	}
}

// Opting out while a run is in flight posts nothing more: a list fetched
// before the opt-out is discarded instead of becoming state, and a run
// between two submissions stops before the second.
func TestSyncPMDBStopsOnOptOut(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var posts int
	listArrived, release := make(chan struct{}), make(chan struct{})
	var onPost func()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/servers":
			select {
			case listArrived <- struct{}{}:
			default:
			}
			<-release
			_, _ = io.WriteString(w, `[]`)
		case "/api/servers/raw":
			mu.Lock()
			posts++
			mu.Unlock()
			if onPost != nil {
				onPost()
			}
			w.WriteHeader(http.StatusCreated)
		}
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmdb.URL = srv.URL
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	hw := HostState{CPUModel: "Intel(R) N100", NCPU: 4, MemTotal: 32 << 30, HomeSize: 60 << 40}
	for i, host := range []string{"odin.pulsedmedia.com", "thor.pulsedmedia.com"} {
		b := &store.Box{Name: string(rune('a' + i)), Host: "h" + string(rune('a'+i)) + ".example", User: "u", Port: 22, Enabled: false}
		if err := m.AddBox(ctx, b); err != nil {
			t.Fatal(err)
		}
		m.mutateState(b.ID, func(s *BoxState) { s.Ready, s.Hostname, s.HostSys = true, host, hw })
	}
	s := m.Settings()
	s.PMDBEnabled, s.PMDBUploadKey = true, "a-passphrase-of-words"
	m.SetSettings(s)

	// 1. The list is still fetching when the user unticks the box.
	done := make(chan struct{})
	go func() { m.SyncPMDB(ctx); close(done) }()
	<-listArrived
	off := s
	off.PMDBEnabled = false
	m.SetSettings(off)
	close(release)
	<-done
	mu.Lock()
	n := posts
	mu.Unlock()
	if n != 0 {
		t.Fatalf("posted %d hosts after the opt-out", n)
	}
	if v := m.PMDB(); v.Enabled || len(v.Hosts) != 0 || v.CheckedAt != "" {
		t.Fatalf("stale list became state: %+v", v)
	}

	// 2. The opt-out lands between two submissions: the second never goes.
	onPost = func() {
		o := m.Settings()
		o.PMDBEnabled = false
		m.SetSettings(o)
	}
	m.SetSettings(s)
	m.SyncPMDB(ctx)
	mu.Lock()
	n = posts
	mu.Unlock()
	if n != 1 {
		t.Fatalf("posted %d hosts; the opt-out after the first must stop the second", n)
	}
	if v := m.PMDB(); v.Enabled || len(v.Hosts) != 0 {
		t.Fatalf("the stale run's outcome became state: %+v", v)
	}

	// 3. An answer that arrives just as a reset lands (display state gone,
	// generation moved on) still settles the host. The window cannot be
	// hit on purpose from outside, so the outcome is recorded under a
	// stale generation directly; re-enabling then posts only the other
	// host.
	onPost = nil
	m.SetSettings(s)
	m.pmdbRecord(&pmdbRun{gen: 0, key: s.PMDBUploadKey}, "odin.pulsedmedia.com", nil)
	m.SyncPMDB(ctx)
	mu.Lock()
	n = posts
	mu.Unlock()
	if n != 2 {
		t.Fatalf("after re-enable: %d posts; want 2 (the accepted host is not posted again)", n)
	}
	for _, h := range m.PMDB().Hosts {
		if h.Status != "submitted" {
			t.Fatalf("standing after re-enable: %+v", h)
		}
	}
}

// A rate-limited submission ends the run: the remaining hosts stay pending
// for the next one instead of piling more requests onto a throttle.
func TestSyncPMDBStopsOnRateLimit(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var posts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/servers" {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		mu.Lock()
		posts++
		mu.Unlock()
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmdb.URL = srv.URL
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	hw := HostState{CPUModel: "Intel(R) N100", NCPU: 4, MemTotal: 32 << 30, HomeSize: 60 << 40}
	for i, host := range []string{"odin.pulsedmedia.com", "thor.pulsedmedia.com", "loki.pulsedmedia.com"} {
		b := &store.Box{Name: string(rune('a' + i)), Host: "h" + string(rune('a'+i)) + ".example", User: "u", Port: 22, Enabled: false}
		if err := m.AddBox(ctx, b); err != nil {
			t.Fatal(err)
		}
		m.mutateState(b.ID, func(s *BoxState) { s.Ready, s.Hostname, s.HostSys = true, host, hw })
	}
	s := m.Settings()
	s.PMDBEnabled, s.PMDBUploadKey = true, "a-passphrase-of-words"
	m.SetSettings(s)
	m.SyncPMDB(ctx)
	mu.Lock()
	n := posts
	mu.Unlock()
	if n != 1 {
		t.Fatalf("%d submissions after a 429; want 1", n)
	}
	var errs, pending int
	for _, h := range m.PMDB().Hosts {
		switch h.Status {
		case "error":
			errs++
		case "pending":
			pending++
		}
	}
	if errs != 1 || pending != 2 {
		t.Fatalf("standing after a 429: %+v", m.PMDB().Hosts)
	}
}

// A hostile catalogue that echoes the upload key in a redirect or an error
// body cannot get it into the public view or the log.
func TestSyncPMDBNeverReflectsKey(t *testing.T) {
	ctx := context.Background()
	const key = "sentinel-passphrase-of-words"
	mode := "redirect"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/servers" {
			if mode == "listbody" { // the catalogue knows the shared key and can echo it on any response
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"`+key+`"}`)
				return
			}
			_, _ = io.WriteString(w, `[]`)
			return
		}
		if mode == "redirect" {
			w.Header().Set("Location", "https://invalid.example/%zz?upload_key="+key)
			w.WriteHeader(http.StatusFound)
			return
		}
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"rejected `+key+`"}`)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var logs bytes.Buffer
	m := New(st, slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	m.pmdb.URL = srv.URL
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	b := &store.Box{Name: "a", Host: "ha.example", User: "u", Port: 22, Enabled: false}
	if err := m.AddBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	m.mutateState(b.ID, func(s *BoxState) {
		s.Ready, s.Hostname, s.HostSys = true, "odin.pulsedmedia.com", HostState{CPUModel: "Intel(R) N100", NCPU: 4, MemTotal: 32 << 30, HomeSize: 60 << 40}
	})
	s := m.Settings()
	s.PMDBEnabled, s.PMDBUploadKey = true, key
	m.SetSettings(s)
	for _, mode = range []string{"redirect", "body", "listbody"} {
		m.resetPMDB()
		m.SyncPMDB(ctx)
		view, _ := json.Marshal(m.PMDB())
		if strings.Contains(string(view), key) {
			t.Fatalf("%s: key in the public view: %s", mode, view)
		}
		if strings.Contains(logs.String(), key) {
			t.Fatalf("%s: key in the log: %s", mode, logs.String())
		}
		if mode == "listbody" {
			if v := m.PMDB(); v.Error == "" || len(v.Hosts) != 0 {
				t.Fatalf("listbody: standing: %+v", v)
			}
			continue
		}
		if h := m.PMDB().Hosts; len(h) != 1 || h[0].Status != "error" || h[0].Detail == "" {
			t.Fatalf("%s: standing: %+v", mode, h)
		}
	}
}

// After a rate limit, every run is held for a while, not just the one host
// that was refused: the limit is per address. A run under a tiny total
// budget against a slow catalogue ends without posting and holds too.
func TestSyncPMDBHolds(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var posts, lists int
	slow, listStatus := false, http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/api/servers" {
			lists++
			if slow {
				mu.Unlock()
				time.Sleep(300 * time.Millisecond)
				mu.Lock()
			}
			w.WriteHeader(listStatus)
			_, _ = io.WriteString(w, `[]`)
			return
		}
		posts++
		w.WriteHeader(http.StatusTooManyRequests)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmdb.URL = srv.URL
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	hw := HostState{CPUModel: "Intel(R) N100", NCPU: 4, MemTotal: 32 << 30, HomeSize: 60 << 40}
	for i, host := range []string{"odin.pulsedmedia.com", "thor.pulsedmedia.com"} {
		b := &store.Box{Name: string(rune('a' + i)), Host: "h" + string(rune('a'+i)) + ".example", User: "u", Port: 22, Enabled: false}
		if err := m.AddBox(ctx, b); err != nil {
			t.Fatal(err)
		}
		m.mutateState(b.ID, func(s *BoxState) { s.Ready, s.Hostname, s.HostSys = true, host, hw })
	}
	s := m.Settings()
	s.PMDBEnabled, s.PMDBUploadKey = true, "a-passphrase-of-words"
	m.SetSettings(s)
	m.SyncPMDB(ctx)
	m.SyncPMDB(ctx) // held: no second list fetch, no second post
	mu.Lock()
	p, l := posts, lists
	mu.Unlock()
	if p != 1 || l != 1 {
		t.Fatalf("after a 429: posts=%d lists=%d; want 1 and 1", p, l)
	}
	if v := m.PMDB(); !strings.Contains(v.Error, "rate-limiting") {
		t.Fatalf("hold not shown: %+v", v)
	}
	// A new key lifts the hold; a slow catalogue under a tiny budget posts
	// nothing and is held as a service failure.
	s.PMDBUploadKey = "another-passphrase-of-words"
	m.SetSettings(s)
	prev := pmdbRunBudget
	pmdbRunBudget = 50 * time.Millisecond
	defer func() { pmdbRunBudget = prev }()
	mu.Lock()
	slow = true
	mu.Unlock()
	m.SyncPMDB(ctx)
	mu.Lock()
	p, l = posts, lists
	mu.Unlock()
	if p != 1 || l != 2 {
		t.Fatalf("slow run: posts=%d lists=%d; want 1 and 2", p, l)
	}
	if v := m.PMDB(); !strings.Contains(v.Error, "catalogue list") {
		t.Fatalf("budget failure not shown: %+v", v)
	}
	m.SyncPMDB(ctx) // held after the service failure: no third fetch
	mu.Lock()
	l = lists
	mu.Unlock()
	if l != 2 {
		t.Fatalf("not held after a service failure: lists=%d", l)
	}
	// A rate-limited list fetch holds every run too.
	pmdbRunBudget = prev
	s.PMDBUploadKey = "a-third-passphrase-of-words"
	m.SetSettings(s)
	mu.Lock()
	slow, listStatus = false, http.StatusTooManyRequests
	mu.Unlock()
	m.SyncPMDB(ctx)
	m.SyncPMDB(ctx)
	mu.Lock()
	l = lists
	mu.Unlock()
	if v := m.PMDB(); l != 3 || !strings.Contains(v.Error, "rate limit") {
		t.Fatalf("list 429: lists=%d view=%+v", l, v)
	}
}

// A box removed while the list was fetching is not submitted from stale
// facts.
func TestSyncPMDBRevalidatesAfterFetch(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var posts int
	listArrived, release := make(chan struct{}, 1), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/servers" {
			select {
			case listArrived <- struct{}{}:
			default:
			}
			<-release
			_, _ = io.WriteString(w, `[]`)
			return
		}
		mu.Lock()
		posts++
		mu.Unlock()
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmdb.URL = srv.URL
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	b := &store.Box{Name: "a", Host: "ha.example", User: "u", Port: 22, Enabled: false}
	if err := m.AddBox(ctx, b); err != nil {
		t.Fatal(err)
	}
	m.mutateState(b.ID, func(s *BoxState) {
		s.Ready, s.Hostname, s.HostSys = true, "odin.pulsedmedia.com", HostState{CPUModel: "Intel(R) N100", NCPU: 4, MemTotal: 32 << 30, HomeSize: 60 << 40}
	})
	s := m.Settings()
	s.PMDBEnabled, s.PMDBUploadKey = true, "a-passphrase-of-words"
	m.SetSettings(s)
	done := make(chan struct{})
	go func() { m.SyncPMDB(ctx); close(done) }()
	<-listArrived
	if err := m.RemoveBox(ctx, b.ID); err != nil {
		t.Fatal(err)
	}
	close(release)
	<-done
	mu.Lock()
	n := posts
	mu.Unlock()
	if n != 0 || len(m.PMDB().Hosts) != 0 {
		t.Fatalf("removed box submitted: posts=%d hosts=%+v", n, m.PMDB().Hosts)
	}
}

// A caller that goes away mid-submission (a manual check whose page was
// closed) is not a catalogue failure: nothing is recorded, no hold is set,
// and the next run proceeds.
func TestSyncPMDBCallerCancelIsNotAFailure(t *testing.T) {
	var mu sync.Mutex
	var posts, lists int
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/api/servers" {
			lists++
			_, _ = io.WriteString(w, `[]`)
			return
		}
		posts++
		if posts == 2 {
			// The page closed while the second submission was in flight;
			// the first was answered. This one gets no answer at all (the
			// connection is dropped), so the cancel is what the client
			// sees, every time.
			cancel()
			conn, _, err := w.(http.Hijacker).Hijack()
			if err == nil {
				conn.Close()
			}
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmdb.URL = srv.URL
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	hw := HostState{CPUModel: "Intel(R) N100", NCPU: 4, MemTotal: 32 << 30, HomeSize: 60 << 40}
	for i, host := range []string{"odin.pulsedmedia.com", "thor.pulsedmedia.com"} {
		b := &store.Box{Name: string(rune('a' + i)), Host: "h" + string(rune('a'+i)) + ".example", User: "u", Port: 22, Enabled: false}
		if err := m.AddBox(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		m.mutateState(b.ID, func(s *BoxState) { s.Ready, s.Hostname, s.HostSys = true, host, hw })
	}
	s := m.Settings()
	s.PMDBEnabled, s.PMDBUploadKey = true, "a-passphrase-of-words"
	m.SetSettings(s)
	m.SyncPMDB(ctx)
	if v := m.PMDB(); v.Error != "" {
		t.Fatalf("caller cancel recorded as a failure: %+v", v)
	}
	// The answer that arrived before the cancel still settles its host.
	var settled int
	for _, h := range m.PMDB().Hosts {
		if h.Status == "error" {
			t.Fatalf("caller cancel recorded on a host: %+v", h)
		}
		if h.Status == "submitted" {
			settled++
		}
	}
	if settled != 1 {
		t.Fatalf("the accepted submission was not recorded: %+v", m.PMDB().Hosts)
	}
	m.SyncPMDB(context.Background()) // not held: fetches again and finishes only the second host
	mu.Lock()
	p, l := posts, lists
	mu.Unlock()
	if l != 2 || p != 3 {
		t.Fatalf("after a caller cancel: lists=%d posts=%d; want 2 and 3 (the settled host is not posted again)", l, p)
	}
	if v := m.PMDB(); v.Error != "" {
		t.Fatalf("second run: %+v", v)
	}
}

// armedCtx reports itself cancelled once armed, without ever closing its
// Done channel: the way to put a run in the window where an answer has
// arrived and the caller is then found to be gone, deterministically.
type armedCtx struct {
	context.Context
	armed atomic.Bool
}

func (c *armedCtx) Err() error {
	if c.armed.Load() {
		return context.Canceled
	}
	return nil
}

// A 409 travels as an error but is an answer: one that arrives just as the
// caller goes away still settles the host, or the next run would post it
// again.
func TestSyncPMDBCallerCancelKeepsExists(t *testing.T) {
	var mu sync.Mutex
	var posts []string
	ctx := &armedCtx{Context: context.Background()}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/api/servers" {
			_, _ = io.WriteString(w, `[]`)
			return
		}
		var row struct{ ServerName string }
		_ = json.NewDecoder(r.Body).Decode(&row)
		posts = append(posts, row.ServerName)
		if row.ServerName == "odin.pulsedmedia.com" {
			ctx.armed.Store(true) // the caller is gone by the time this answer is read
			w.WriteHeader(http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusCreated)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmdb.URL = srv.URL
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	hw := HostState{CPUModel: "Intel(R) N100", NCPU: 4, MemTotal: 32 << 30, HomeSize: 60 << 40}
	for i, host := range []string{"odin.pulsedmedia.com", "thor.pulsedmedia.com"} {
		b := &store.Box{Name: string(rune('a' + i)), Host: "h" + string(rune('a'+i)) + ".example", User: "u", Port: 22, Enabled: false}
		if err := m.AddBox(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		m.mutateState(b.ID, func(s *BoxState) { s.Ready, s.Hostname, s.HostSys = true, host, hw })
	}
	s := m.Settings()
	s.PMDBEnabled, s.PMDBUploadKey = true, "a-passphrase-of-words"
	m.SetSettings(s)
	m.SyncPMDB(ctx) // odin is answered 409 and the caller found gone; the run ends there
	mu.Lock()
	p := append([]string(nil), posts...)
	mu.Unlock()
	if len(p) != 1 || p[0] != "odin.pulsedmedia.com" {
		t.Fatalf("first run posted %v", p)
	}
	m.SyncPMDB(context.Background())
	mu.Lock()
	p = append([]string(nil), posts...)
	mu.Unlock()
	if len(p) != 2 || p[1] != "thor.pulsedmedia.com" {
		t.Fatalf("after the cancelled 409, posts were %v; odin must not be posted again", p)
	}
	for _, h := range m.PMDB().Hosts {
		if h.Hostname == "odin.pulsedmedia.com" && h.Status != "listed" {
			t.Fatalf("odin: %+v", h)
		}
	}
}

// A caller that goes away during the list fetch is not a catalogue failure
// either: nothing is recorded and no hold is set.
func TestSyncPMDBCallerCancelDuringList(t *testing.T) {
	var lists atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if lists.Add(1) == 1 {
			cancel() // the page closed mid-fetch; the answer never comes
			if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
				conn.Close()
			}
			return
		}
		_, _ = io.WriteString(w, `[{"serverName":"odin.pulsedmedia.com"}]`)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmdb.URL = srv.URL
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	b := &store.Box{Name: "a", Host: "a.example", User: "u", Port: 22, Enabled: false}
	if err := m.AddBox(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	m.mutateState(b.ID, func(s *BoxState) {
		s.Ready, s.Hostname, s.HostSys = true, "odin.pulsedmedia.com", HostState{CPUModel: "Intel(R) N100", NCPU: 4, MemTotal: 32 << 30, HomeSize: 60 << 40}
	})
	s := m.Settings()
	s.PMDBEnabled, s.PMDBUploadKey = true, "a-passphrase-of-words"
	m.SetSettings(s)
	m.SyncPMDB(ctx)
	if v := m.PMDB(); v.Error != "" || v.CheckedAt != "" {
		t.Fatalf("caller cancel during the list recorded: %+v", v)
	}
	m.SyncPMDB(context.Background()) // not held
	if n := lists.Load(); n != 2 {
		t.Fatalf("lists=%d; want 2 (no hold after a caller cancel)", n)
	}
	if v := m.PMDB(); v.Error != "" || len(v.Hosts) != 1 || v.Hosts[0].Status != "listed" {
		t.Fatalf("second run: %+v", v)
	}
}

// Automatic runs are cheap whatever the catalogue sends: once every host
// is decided an inventory wake fetches nothing, a new host within
// pmdbListInterval of the last fetch is decided from that copy (posted,
// not listed again), a host backing off asks for nothing either, and only
// a check the user asks for (or the periodic one) always fetches.
func TestSyncPMDBAutomaticRunsAskLittle(t *testing.T) {
	var mu sync.Mutex
	var lists, posts int
	postStatus := http.StatusCreated
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/api/servers" {
			lists++
			_, _ = io.WriteString(w, `[{"serverName":"odin.pulsedmedia.com"},{"serverName":"thor.pulsedmedia.com"}]`)
			return
		}
		posts++
		w.WriteHeader(postStatus)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmdb.URL = srv.URL
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	hw := HostState{CPUModel: "Intel(R) N100", NCPU: 4, MemTotal: 32 << 30, HomeSize: 60 << 40}
	add := func(name, host string) {
		b := &store.Box{Name: name, Host: name + ".example", User: "u", Port: 22, Enabled: false}
		if err := m.AddBox(context.Background(), b); err != nil {
			t.Fatal(err)
		}
		m.mutateState(b.ID, func(s *BoxState) { s.Ready, s.Hostname, s.HostSys = true, host, hw })
	}
	add("a", "odin.pulsedmedia.com")
	add("b", "thor.pulsedmedia.com")
	s := m.Settings()
	s.PMDBEnabled, s.PMDBUploadKey = true, "a-passphrase-of-words"
	m.SetSettings(s)
	counts := func() (int, int) {
		mu.Lock()
		defer mu.Unlock()
		return lists, posts
	}
	for i := 0; i < 20; i++ {
		m.syncPMDB(context.Background(), false) // twenty inventory wakes
	}
	if l, p := counts(); l != 1 || p != 0 {
		t.Fatalf("decided fleet: lists=%d posts=%d; want 1 and 0", l, p)
	}
	// A new host within the interval: decided from the copy, no fetch.
	add("c", "freya.pulsedmedia.com")
	m.syncPMDB(context.Background(), false)
	if l, p := counts(); l != 1 || p != 1 {
		t.Fatalf("new host: lists=%d posts=%d; want 1 and 1", l, p)
	}
	// Past the interval, a host that needs deciding fetches again; one
	// refused with a 4xx backs off and asks for nothing meanwhile.
	pmdbListInterval = 0
	defer func() { pmdbListInterval = 10 * time.Minute }()
	mu.Lock()
	postStatus = http.StatusBadRequest
	mu.Unlock()
	add("d", "loki.pulsedmedia.com")
	m.syncPMDB(context.Background(), false)
	if l, p := counts(); l != 2 || p != 2 {
		t.Fatalf("undecided host past the interval: lists=%d posts=%d; want 2 and 2", l, p)
	}
	for i := 0; i < 5; i++ {
		m.syncPMDB(context.Background(), false)
	}
	if l, p := counts(); l != 2 || p != 2 {
		t.Fatalf("host backing off: lists=%d posts=%d; want 2 and 2 still", l, p)
	}
	// The check the user asks for always fetches (and the backing-off
	// host still waits its hour).
	m.SyncPMDB(context.Background())
	m.SyncPMDB(context.Background())
	if l, p := counts(); l != 4 || p != 2 {
		t.Fatalf("manual checks: lists=%d posts=%d; want 4 and 2", l, p)
	}
	if v := m.PMDB(); v.CheckedAt == "" {
		t.Fatalf("view: %+v", v)
	}
}

// An opt-out that lands between a run being asked for and the run starting
// is seen: the settings are read under the state lock, after the reset,
// so not even the one keyless list fetch goes out.
func TestSyncPMDBOptOutBeforeBegin(t *testing.T) {
	var lists atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		lists.Add(1)
		_, _ = io.WriteString(w, `[]`)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmdb.URL = srv.URL
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	b := &store.Box{Name: "a", Host: "a.example", User: "u", Port: 22, Enabled: false}
	if err := m.AddBox(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	m.mutateState(b.ID, func(s *BoxState) {
		s.Ready, s.Hostname, s.HostSys = true, "odin.pulsedmedia.com", HostState{CPUModel: "Intel(R) N100", NCPU: 4, MemTotal: 32 << 30, HomeSize: 60 << 40}
	})
	s := m.Settings()
	s.PMDBEnabled, s.PMDBUploadKey = true, "a-passphrase-of-words"
	m.SetSettings(s)
	pmdbBeginHook = func() {
		pmdbBeginHook = nil
		off := m.Settings()
		off.PMDBEnabled = false
		m.SetSettings(off) // the user opts out as the run is about to begin
	}
	defer func() { pmdbBeginHook = nil }()
	m.SyncPMDB(context.Background())
	if n := lists.Load(); n != 0 {
		t.Fatalf("%d list fetches after an opt-out", n)
	}
}

// A decision is terminal for the process: a catalogue that alternates its
// list, or answers 409 to everything, cannot make the fleet submit again.
func TestSyncPMDBDecisionsAreTerminal(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var posts int
	listBody, postStatus := `[]`, http.StatusConflict
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/api/servers" {
			_, _ = io.WriteString(w, listBody)
			return
		}
		posts++
		w.WriteHeader(postStatus)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmdb.URL = srv.URL
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	hw := HostState{CPUModel: "Intel(R) N100", NCPU: 4, MemTotal: 32 << 30, HomeSize: 60 << 40}
	for i, host := range []string{"odin.pulsedmedia.com", "thor.pulsedmedia.com"} {
		b := &store.Box{Name: string(rune('a' + i)), Host: "h" + string(rune('a'+i)) + ".example", User: "u", Port: 22, Enabled: false}
		if err := m.AddBox(ctx, b); err != nil {
			t.Fatal(err)
		}
		m.mutateState(b.ID, func(s *BoxState) { s.Ready, s.Hostname, s.HostSys = true, host, hw })
	}
	s := m.Settings()
	s.PMDBEnabled, s.PMDBUploadKey = true, "a-passphrase-of-words"
	m.SetSettings(s)
	count := func() int { mu.Lock(); defer mu.Unlock(); return posts }
	set := func(list string, code int) { mu.Lock(); defer mu.Unlock(); listBody, postStatus = list, code }

	// Sequence A: empty list, 409 to every submission, empty list again.
	m.SyncPMDB(ctx)
	if n := count(); n != 2 {
		t.Fatalf("first run: %d posts", n)
	}
	m.SyncPMDB(ctx)
	m.SyncPMDB(ctx)
	if n := count(); n != 2 {
		t.Fatalf("hosts told they exist were submitted again: %d posts", n)
	}
	for _, h := range m.PMDB().Hosts {
		if h.Status != "listed" {
			t.Fatalf("standing after 409: %+v", h)
		}
	}

	// A reset does not unsettle them either.
	m.resetPMDB()
	m.SyncPMDB(ctx)
	if n := count(); n != 2 {
		t.Fatalf("a reset unsettled decided hosts: %d posts", n)
	}

	// Sequence B, on two hosts this process has not decided: accepted,
	// then listed, then missing from the list.
	for i, host := range []string{"freya.pulsedmedia.com", "tyr.pulsedmedia.com"} {
		id := int64(i + 1)
		m.mutateState(id, func(s *BoxState) { s.Hostname = host })
	}
	set(`[]`, http.StatusCreated)
	m.SyncPMDB(ctx)
	if n := count(); n != 4 {
		t.Fatalf("new hosts: %d posts", n)
	}
	set(`[{"serverName":"freya.pulsedmedia.com"},{"serverName":"tyr.pulsedmedia.com"}]`, http.StatusCreated)
	m.SyncPMDB(ctx)
	set(`[]`, http.StatusCreated)
	m.SyncPMDB(ctx)
	m.SyncPMDB(ctx)
	if n := count(); n != 4 {
		t.Fatalf("hosts the list dropped again were resubmitted: %d posts", n)
	}
}

// Every list failure holds the next runs, and a redirect on a submission
// ends the run after the first host.
func TestSyncPMDBListFailuresHold(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var posts, lists int
	mode := ""
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/api/servers" {
			lists++
			switch mode {
			case "malformed":
				_, _ = io.WriteString(w, `{"servers":`)
			case "redirect":
				w.Header().Set("Location", "https://elsewhere.example/")
				w.WriteHeader(http.StatusFound)
			case "400":
				w.WriteHeader(http.StatusBadRequest)
			default:
				_, _ = io.WriteString(w, `[]`)
			}
			return
		}
		posts++
		w.Header().Set("Location", "https://elsewhere.example/")
		w.WriteHeader(http.StatusFound)
	}))
	defer srv.Close()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmdb.URL = srv.URL
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	defer m.Stop()
	hw := HostState{CPUModel: "Intel(R) N100", NCPU: 4, MemTotal: 32 << 30, HomeSize: 60 << 40}
	for i, host := range []string{"odin.pulsedmedia.com", "thor.pulsedmedia.com", "loki.pulsedmedia.com"} {
		b := &store.Box{Name: string(rune('a' + i)), Host: "h" + string(rune('a'+i)) + ".example", User: "u", Port: 22, Enabled: false}
		if err := m.AddBox(ctx, b); err != nil {
			t.Fatal(err)
		}
		m.mutateState(b.ID, func(s *BoxState) { s.Ready, s.Hostname, s.HostSys = true, host, hw })
	}
	s := m.Settings()
	s.PMDBEnabled, s.PMDBUploadKey = true, "a-passphrase-of-words"
	m.SetSettings(s)
	for _, md := range []string{"malformed", "redirect", "400"} {
		m.resetPMDB()
		mu.Lock()
		mode, lists = md, 0
		mu.Unlock()
		m.SyncPMDB(ctx)
		m.SyncPMDB(ctx)
		mu.Lock()
		l := lists
		mu.Unlock()
		if l != 1 {
			t.Fatalf("%s: %d list requests across two runs; want 1 (held)", md, l)
		}
		if v := m.PMDB(); !strings.HasPrefix(v.Error, "catalogue list:") {
			t.Fatalf("%s: hold not shown: %+v", md, v)
		}
	}
	// A redirect on a submission: one POST, then the run ends and holds.
	m.resetPMDB()
	mu.Lock()
	mode, lists = "", 0
	mu.Unlock()
	m.SyncPMDB(ctx)
	m.SyncPMDB(ctx)
	mu.Lock()
	p, l := posts, lists
	mu.Unlock()
	if p != 1 || l != 1 {
		t.Fatalf("submission redirect: posts=%d lists=%d; want 1 and 1", p, l)
	}
}
