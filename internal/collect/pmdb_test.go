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
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"

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

	// A new key starts over; the catalogue now says 409 for odin: listed.
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
	s.PMDBUploadKey = "a-passphrase-of-words"
	m.SetSettings(s)
	m.SyncPMDB(ctx)
	v = m.PMDB()
	for _, h := range v.Hosts {
		byName[h.Hostname] = h
	}
	if v.Error != "" || byName["odin.pulsedmedia.com"].Status != "listed" || byName["loki.pulsedmedia.com"].Status != "listed" {
		t.Fatalf("409 not counted as listed: %+v", v)
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

