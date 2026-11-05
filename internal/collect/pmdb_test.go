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
	"fmt"
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

	"pmss-fleet-manager/internal/pmdb"
	"pmss-fleet-manager/internal/store"
)

// pmdbMock stands in for the catalogue: the four routes, with bodies and
// statuses the test sets, counting what was asked and keeping every row
// posted.
type pmdbMock struct {
	srv                                  *httptest.Server
	mu                                   sync.Mutex
	lists, configs, nodes, posts         int
	rows                                 []map[string]any
	listBody, configBody, nodeBody       string
	listStatus, configStatus, nodeStatus int
	postStatus                           int
	onPost                               func(row map[string]any) // called with mu released
}

func newPMDBMock(t *testing.T) *pmdbMock {
	k := &pmdbMock{listBody: `[]`, configBody: `{"uploadsEnabled":true,"plans":["M10G S","Dragon-R Mushu"]}`, nodeBody: `{"server":{},"system":{}}`,
		listStatus: http.StatusOK, configStatus: http.StatusOK, nodeStatus: http.StatusNotFound, postStatus: http.StatusCreated}
	k.srv = httptest.NewServer(http.HandlerFunc(k.serve))
	t.Cleanup(k.srv.Close)
	return k
}

func (k *pmdbMock) serve(w http.ResponseWriter, r *http.Request) {
	k.mu.Lock()
	switch {
	case r.URL.Path == "/api/servers" && r.Method == http.MethodGet:
		k.lists++
		st, body := k.listStatus, k.listBody
		k.mu.Unlock()
		w.WriteHeader(st)
		_, _ = io.WriteString(w, body)
	case r.URL.Path == "/api/config" && r.Method == http.MethodGet:
		k.configs++
		st, body := k.configStatus, k.configBody
		k.mu.Unlock()
		w.WriteHeader(st)
		_, _ = io.WriteString(w, body)
	case strings.HasPrefix(r.URL.Path, "/api/nodes/") && r.Method == http.MethodGet:
		k.nodes++
		st, body := k.nodeStatus, k.nodeBody
		k.mu.Unlock()
		w.WriteHeader(st)
		_, _ = io.WriteString(w, body)
	case r.URL.Path == "/api/servers/raw" && r.Method == http.MethodPost:
		var row map[string]any
		_ = json.NewDecoder(r.Body).Decode(&row)
		k.posts++
		k.rows = append(k.rows, row)
		st, onPost := k.postStatus, k.onPost
		k.mu.Unlock()
		if onPost != nil {
			onPost(row)
		}
		w.WriteHeader(st)
	default:
		k.mu.Unlock()
		w.WriteHeader(http.StatusNotFound)
	}
}

func (k *pmdbMock) set(fn func(*pmdbMock)) { k.mu.Lock(); defer k.mu.Unlock(); fn(k) }

func (k *pmdbMock) counts() (lists, configs, nodes, posts int) {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.lists, k.configs, k.nodes, k.posts
}

// names is every hostname posted, in order.
func (k *pmdbMock) names() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	var out []string
	for _, r := range k.rows {
		n, _ := r["serverName"].(string)
		out = append(out, n)
	}
	return out
}

// row is the last row posted for a host, or nil.
func (k *pmdbMock) row(host string) map[string]any {
	k.mu.Lock()
	defer k.mu.Unlock()
	for i := len(k.rows) - 1; i >= 0; i-- {
		if k.rows[i]["serverName"] == host {
			return k.rows[i]
		}
	}
	return nil
}

// pmdbFullHW is a host inventory with every optional known, so no row
// needs the catalogue's help and no node request goes out.
var pmdbFullHW = HostState{CPUModel: "Intel(R) N100", NCPU: 4, MemTotal: 32 << 30, HomeSize: 60 << 40, CPUMHz: 2100, SwapKnown: true,
	Virt: "none", Distro: "Debian GNU/Linux 12 (bookworm)", Kernel: "6.1.0-37-amd64", Arch: "x86_64", Users: 17, UsersKnown: true}

func newPMDBManager(t *testing.T, url string) (*Manager, *store.Store) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m.pmdb.URL = url
	if err := m.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Stop)
	return m, st
}

// pmdbAddHost adds a box whose state says it runs on host, with hw, and
// returns its id. fix, when given, adjusts the state further.
func pmdbAddHost(t *testing.T, m *Manager, name, host string, hw HostState, fix ...func(*BoxState)) int64 {
	t.Helper()
	b := &store.Box{Name: name, Host: name + ".example", User: "u", Port: 22, Enabled: false}
	if err := m.AddBox(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	m.mutateState(b.ID, func(s *BoxState) {
		s.Ready, s.Hostname, s.HostSys = true, host, hw
		for _, f := range fix {
			f(s)
		}
	})
	return b.ID
}

func pmdbEnable(m *Manager, username string) {
	s := m.Settings()
	s.PMDBEnabled, s.PMDBUsername = true, username
	m.SetSettings(s)
}

func pmdbHosts(m *Manager) map[string]pmdbStatus {
	out := map[string]pmdbStatus{}
	for _, h := range m.PMDB().Hosts {
		out[h.Hostname] = h
	}
	return out
}

func TestPMDBRow(t *testing.T) {
	a := &BoxState{ID: 2, Hostname: "Le4-0-105-238louhi.pulsedmedia.com.", HostSys: HostState{
		CPUModel: "Intel(R) Core(TM) i7-12700", NCPU: 20, CPUMHz: 2100, MemTotal: 64 << 30, SwapTotal: 1 << 20, SwapKnown: true,
		HomeSize: 15000000000000, Virt: "kvm", Distro: "Debian GNU/Linux 12 (bookworm)", Kernel: "6.1.0-37-amd64", Arch: "x86_64", Users: 17, UsersKnown: true}}
	a.Plan = "M10G S"
	// A second box on the host with no inventory yet and a plan of its
	// own from the services feed: the lower id wins every pick it can make.
	b := &BoxState{ID: 1, Hostname: "le4-0-105-238louhi.pulsedmedia.com"}
	b.Service = &ServiceInfo{Product: "Dragon-R Mushu"}
	const name = "le4-0-105-238louhi.pulsedmedia.com"
	c, ok := pmdbRow(name, []*BoxState{b, a})
	if !ok {
		t.Fatal("row refused")
	}
	row := c.row
	if row.ServerName != name || row.CoreCount != 20 || row.RAMGiB != 64 || row.StorageTiB != 13.64 || row.ProcessorName != "Intel(R) Core(TM) i7-12700" ||
		row.ProcessorMHz == nil || *row.ProcessorMHz != 2100 || row.SwapGiB == nil || *row.SwapGiB != 1.0/1024 ||
		row.KVM == nil || !*row.KVM || row.UserCount == nil || *row.UserCount != 17 || row.Username != "" || row.Plan != nil ||
		row.System == nil || *row.System != (pmdb.System{Distro: "Debian GNU/Linux 12 (bookworm)", Kernel: "6.1.0-37-amd64", Arch: "x86_64", VM: "KVM"}) {
		t.Fatalf("row: %+v system=%+v", row, row.System)
	}
	if c.plan != "Dragon-R Mushu" || c.skip != "" || len(c.ids) != 2 || c.ids[0] != 1 || c.hash == "" {
		t.Fatalf("candidate: %+v", c)
	}
	// The hash is of the facts: the same facts hash the same; a changed
	// fact or a changed plan claim hashes differently; the catalogue's
	// plan list and identifier are not in it.
	again, _ := pmdbRow(name, []*BoxState{b, a})
	if again.hash != c.hash {
		t.Fatal("hash not stable")
	}
	a2 := *a
	a2.HostSys.MemTotal = 65 << 30
	if changed, _ := pmdbRow(name, []*BoxState{b, &a2}); changed.hash == c.hash {
		t.Fatal("a changed fact must change the hash")
	}
	b2 := *b
	b2.Service = nil
	if changed, _ := pmdbRow(name, []*BoxState{&b2, a}); changed.hash == c.hash || changed.plan != "M10G S" {
		t.Fatalf("a changed plan claim must change the hash: %+v", changed)
	}
	// Nothing the catalogue would not take goes out: no qsv, no extras.
	js, _ := json.Marshal(row)
	for _, k := range []string{`"qsv"`, `"replace"`, `"benchmarks"`} {
		if bytes.Contains(js, []byte(k)) {
			t.Errorf("%s in the row: %s", k, js)
		}
	}
	// Unknown optionals go as nil, never as 0; a virtualisation answer that
	// is neither KVM nor none is kept as the string and no boolean.
	a3 := *a
	a3.HostSys.SwapKnown, a3.HostSys.CPUMHz, a3.HostSys.UsersKnown, a3.HostSys.Virt, a3.HostSys.Distro = false, 0, false, "lxc", ""
	c3, _ := pmdbRow(name, []*BoxState{&a3})
	if c3.row.SwapGiB != nil || c3.row.ProcessorMHz != nil || c3.row.UserCount != nil || c3.row.KVM != nil || c3.row.System == nil ||
		c3.row.System.VM != "LXC" || c3.row.System.Distro != "" || c3.plan != "M10G S" {
		t.Fatalf("row: %+v %+v", c3.row, c3.row.System)
	}
	a4 := *a
	a4.HostSys.Virt, a4.HostSys.Distro, a4.HostSys.Kernel, a4.HostSys.Arch = "", "", "", ""
	if c4, _ := pmdbRow(name, []*BoxState{&a4}); c4.row.System != nil || c4.row.KVM != nil {
		t.Fatalf("no system facts must send no system object: %+v", c4.row.System)
	}
	// Strings the catalogue would refuse are unknown, never cut; a
	// processor name it would refuse cannot be posted at all.
	a5 := *a
	a5.HostSys.Distro, a5.HostSys.Kernel = strings.Repeat("d", 201), "bad\tkernel"
	a5.Plan = strings.Repeat("p", 101)
	if c5, _ := pmdbRow(name, []*BoxState{&a5}); c5.row.System.Distro != "" || c5.row.System.Kernel != "" || c5.plan != "" || c5.skip != "" {
		t.Fatalf("refused strings: %+v plan=%q skip=%q", c5.row.System, c5.plan, c5.skip)
	}
	a6 := *a
	a6.HostSys.CPUModel = strings.Repeat("c", 301)
	if c6, ok := pmdbRow(name, []*BoxState{&a6}); !ok || c6.skip != pmdbSkipCPU {
		t.Fatalf("long processor name: %v %+v", ok, c6)
	}
	if c7, ok := pmdbRow("odin.example.org", []*BoxState{a}); !ok || c7.skip != pmdbSkipForeign {
		t.Fatalf("foreign hostname: %v %+v", ok, c7)
	}
	// Missing required facts (no inventory yet) refuse the row.
	for _, fix := range []func(*BoxState){
		func(s *BoxState) { s.HostSys.CPUModel = "" },
		func(s *BoxState) { s.HostSys.NCPU = 0 },
		func(s *BoxState) { s.HostSys.MemTotal = 0 },
		func(s *BoxState) { s.HostSys.HomeSize = 0 },
	} {
		s := *a
		fix(&s)
		if _, ok := pmdbRow(name, []*BoxState{&s}); ok {
			t.Errorf("row accepted for %+v", s.HostSys)
		}
	}
}

func TestPMDBDue(t *testing.T) {
	now := time.Now()
	rec := store.PMDBSubmission{Username: "u", RowHash: "h", SubmittedAt: now.Add(-time.Hour)}
	for _, c := range []struct {
		rec     store.PMDBSubmission
		found   bool
		user, h string
		due     bool
	}{
		{rec, false, "u", "h", true},
		{rec, true, "u", "h", false},
		{rec, true, "v", "h", true},
		{rec, true, "u", "g", true},
		{store.PMDBSubmission{Username: "u", RowHash: "h", SubmittedAt: now.Add(-pmdbRefresh)}, true, "u", "h", true},
		{store.PMDBSubmission{Username: "u", RowHash: "h", SubmittedAt: now.Add(-pmdbRefresh + time.Minute)}, true, "u", "h", false},
	} {
		if due, why := pmdbDue(c.rec, c.found, c.user, c.h, now); due != c.due || (due && why == "") {
			t.Errorf("pmdbDue(%+v, %v, %q, %q) = %v %q", c.rec, c.found, c.user, c.h, due, why)
		}
	}
}

// Two boxes on one host, one listed host and one more: one list fetch,
// one plan-list fetch, every host posted once under the identifier with
// exactly the catalogue's fields, nothing posted again while the facts
// stand, everything posted once more under a new identifier, a 409
// recorded as an error, and nothing sent while the integration is off.
func TestSyncPMDB(t *testing.T) {
	ctx := context.Background()
	k := newPMDBMock(t)
	k.set(func(k *pmdbMock) { k.listBody = `[{"serverName":"thor.pulsedmedia.com"}]` })
	m, _ := newPMDBManager(t, k.srv.URL)
	pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", pmdbFullHW)
	pmdbAddHost(t, m, "b", "ODIN.pulsedmedia.com", pmdbFullHW)
	pmdbAddHost(t, m, "c", "thor.pulsedmedia.com", pmdbFullHW, func(s *BoxState) { s.Plan = "M10G S" })
	pmdbAddHost(t, m, "d", "loki", pmdbFullHW, func(s *BoxState) { s.Plan = "Not A Plan" })

	// Off: nothing leaves, and the view says so.
	m.SyncPMDB(ctx)
	if v := m.PMDB(); v.Enabled || v.Configured || len(v.Hosts) != 0 {
		t.Fatalf("disabled view: %+v", v)
	}
	if l, _, _, p := k.counts(); l != 0 || p != 0 {
		t.Fatalf("disabled: lists=%d posts=%d", l, p)
	}

	pmdbEnable(m, "someone")
	m.SyncPMDB(ctx)
	if got := k.names(); len(got) != 3 || got[0] != "loki.pulsedmedia.com" || got[1] != "odin.pulsedmedia.com" || got[2] != "thor.pulsedmedia.com" {
		t.Fatalf("submitted %v", got)
	}
	if l, c, n, _ := k.counts(); l != 1 || c != 1 || n != 0 {
		t.Fatalf("requests: lists=%d configs=%d nodes=%d; want 1, 1, 0", l, c, n)
	}
	// The row, field for field: the catalogue refuses a key it does not
	// know, so the set is checked exactly.
	row := k.row("thor.pulsedmedia.com")
	want := map[string]any{"username": "someone", "serverName": "thor.pulsedmedia.com", "processorName": "Intel(R) N100", "coreCount": 4.0,
		"processorMhz": 2100.0, "ramGiB": 32.0, "swapGiB": 0.0, "storageTiB": 60.0, "kvm": false, "plan": "M10G S", "userCount": 17.0}
	if len(row) != len(want)+1 {
		t.Fatalf("row keys: %v", row)
	}
	for key, v := range want {
		if row[key] != v {
			t.Errorf("row[%s] = %v, want %v", key, row[key], v)
		}
	}
	if sys, _ := row["system"].(map[string]any); sys["distro"] != "Debian GNU/Linux 12 (bookworm)" || sys["kernel"] != "6.1.0-37-amd64" || sys["arch"] != "x86_64" || sys["vm"] != "NONE" || len(sys) != 4 {
		t.Errorf("system: %v", row["system"])
	}
	if lk := k.row("loki.pulsedmedia.com"); lk["plan"] != nil || lk["username"] != "someone" {
		t.Errorf("a plan the catalogue does not list must be left out: %v", lk)
	}
	v := m.PMDB()
	if !v.Configured || v.Error != "" || len(v.Hosts) != 3 || v.CheckedAt == "" {
		t.Fatalf("view: %+v", v)
	}
	hosts := pmdbHosts(m)
	if h := hosts["thor.pulsedmedia.com"]; h.Status != "submitted" || !h.Listed || h.Plan != "M10G S" || h.Detail != "" || h.SubmittedAt == "" {
		t.Fatalf("thor: %+v", h)
	}
	if h := hosts["odin.pulsedmedia.com"]; h.Status != "submitted" || h.Listed || len(h.BoxIDs) != 2 {
		t.Fatalf("odin: %+v", h)
	}
	if h := hosts["loki.pulsedmedia.com"]; h.Status != "submitted" || h.Plan != "Not A Plan" || h.Detail != pmdbPlanUnknown {
		t.Fatalf("loki: %+v", h)
	}

	// Later runs post nothing: the receipts stand, whatever the list says.
	m.SyncPMDB(ctx)
	m.syncPMDB(ctx, false)
	if _, _, _, p := k.counts(); p != 3 {
		t.Fatalf("resubmitted: %d posts", p)
	}

	// A new identifier is a new contributor: every host once more.
	pmdbEnable(m, "else")
	m.SyncPMDB(ctx)
	if got := k.names(); len(got) != 6 || k.row("odin.pulsedmedia.com")["username"] != "else" {
		t.Fatalf("after the identifier change: %v", got)
	}
	m.SyncPMDB(ctx)
	if _, _, _, p := k.counts(); p != 6 {
		t.Fatalf("posted again under the same new identifier: %d", p)
	}

	// A 409 is the catalogue refusing the name: an error on the host,
	// retried later, no hold.
	k.set(func(k *pmdbMock) { k.postStatus = http.StatusConflict })
	pmdbAddHost(t, m, "e", "freya.pulsedmedia.com", pmdbFullHW)
	m.SyncPMDB(ctx)
	if h := pmdbHosts(m)["freya.pulsedmedia.com"]; h.Status != "error" || h.Detail != pmdb.ErrNameConflict.Error() {
		t.Fatalf("freya: %+v", h)
	}
	if v := m.PMDB(); v.Error != "" {
		t.Fatalf("a 409 must not hold the run: %+v", v)
	}
}

// Opting out while a run is in flight posts nothing more: a list fetched
// before the opt-out is discarded instead of becoming state, a run
// between two submissions stops before the second, and an acceptance
// that lands as a reset does is still a receipt.
func TestSyncPMDBStopsOnOptOut(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var posts int
	// listArrived is buffered: the fetch can land before the test is at
	// the receive, and the non-blocking send below must not lose it.
	listArrived, release := make(chan struct{}, 1), make(chan struct{})
	var onPost func()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/servers":
			select {
			case listArrived <- struct{}{}:
			default:
			}
			<-release
			_, _ = io.WriteString(w, `[]`)
		case r.URL.Path == "/api/servers/raw":
			mu.Lock()
			posts++
			mu.Unlock()
			if onPost != nil {
				// The opt-out lands as this submission is in flight, and
				// no answer ever reaches the client (the connection is
				// dropped), so the outcome is the same every time: no
				// acceptance, no receipt. An acceptance that does arrive
				// as a reset lands is 3. below.
				onPost()
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					conn.Close()
				}
				return
			}
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	m, st := newPMDBManager(t, srv.URL)
	pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", pmdbFullHW)
	pmdbAddHost(t, m, "b", "thor.pulsedmedia.com", pmdbFullHW)
	pmdbEnable(m, "someone")
	s := m.Settings()

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
	if recs, _ := st.PMDBSubmissions(ctx); len(recs) != 0 {
		t.Fatalf("an unanswered submission left a receipt: %+v", recs)
	}

	// 3. An answer that arrives just as a reset lands (display state gone,
	// generation moved on) is still a receipt. The window cannot be hit
	// on purpose from outside, so the outcome is recorded under a stale
	// generation directly; re-enabling then posts only the other host
	// (the one from 2., which got no answer and so has no receipt).
	onPost = nil
	m.SetSettings(s)
	thor := m.pmdbCandidates()["thor.pulsedmedia.com"]
	m.pmdbRecord(&pmdbRun{gen: 0, username: s.PMDBUsername}, "thor.pulsedmedia.com", thor.hash, "", nil)
	m.SyncPMDB(ctx)
	mu.Lock()
	n = posts
	mu.Unlock()
	if n != 2 {
		t.Fatalf("after re-enable: %d posts; want 2 (the host with a receipt is not posted again)", n)
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
	k := newPMDBMock(t)
	k.set(func(k *pmdbMock) { k.postStatus = http.StatusTooManyRequests })
	m, _ := newPMDBManager(t, k.srv.URL)
	for i, host := range []string{"odin.pulsedmedia.com", "thor.pulsedmedia.com", "loki.pulsedmedia.com"} {
		pmdbAddHost(t, m, string(rune('a'+i)), host, pmdbFullHW)
	}
	pmdbEnable(m, "someone")
	m.SyncPMDB(ctx)
	if _, _, _, p := k.counts(); p != 1 {
		t.Fatalf("%d submissions after a 429; want 1", p)
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

// A hostile catalogue that puts text in a redirect, an error body, the
// list, or a host's row cannot get it into the public view or the log;
// a string of a host's row that passes the text rule reaches the next
// submission and nothing else.
func TestSyncPMDBNeverReflectsCatalogueText(t *testing.T) {
	ctx := context.Background()
	const sentinel = "sentinel-text-of-words"
	var mode atomic.Value
	var mu sync.Mutex
	var posted []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		md := mode.Load().(string)
		switch {
		case r.URL.Path == "/api/servers":
			if md == "listbody" { // the catalogue can echo anything on any response
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"`+sentinel+`"}`)
				return
			}
			_, _ = io.WriteString(w, `[]`)
		case strings.HasPrefix(r.URL.Path, "/api/nodes/"):
			switch md {
			case "node":
				_, _ = io.WriteString(w, `{"server":{},"system":{"arch":"`+sentinel+`\u0007"}}`)
			case "nodeclean":
				_, _ = io.WriteString(w, `{"server":{},"system":{"arch":"`+sentinel+`"}}`)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		case r.URL.Path == "/api/servers/raw":
			var row map[string]any
			_ = json.NewDecoder(r.Body).Decode(&row)
			mu.Lock()
			posted = append(posted, row)
			mu.Unlock()
			switch md {
			case "redirect":
				w.Header().Set("Location", "https://invalid.example/%zz?echo="+sentinel)
				w.WriteHeader(http.StatusFound)
			case "body":
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":"rejected `+sentinel+`"}`)
			default:
				w.WriteHeader(http.StatusCreated)
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	mode.Store("redirect")
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
	hw := pmdbFullHW
	hw.Arch = "" // unknown: the catalogue's row is read for it
	id := pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", hw)
	pmdbEnable(m, "someone")
	for _, md := range []string{"redirect", "body", "listbody", "node", "nodeclean"} {
		mode.Store(md)
		m.resetPMDB()
		m.SyncPMDB(ctx)
		view, _ := json.Marshal(m.PMDB())
		if strings.Contains(string(view), sentinel) {
			t.Fatalf("%s: catalogue text in the public view: %s", md, view)
		}
		if strings.Contains(logs.String(), sentinel) {
			t.Fatalf("%s: catalogue text in the log: %s", md, logs.String())
		}
		h := pmdbHosts(m)["odin.pulsedmedia.com"]
		switch md {
		case "listbody":
			if v := m.PMDB(); v.Error == "" {
				t.Fatalf("listbody: standing: %+v", v)
			}
		case "node":
			if h.Status != "error" || h.Detail != pmdbCannotRepeat {
				t.Fatalf("node: standing: %+v", h)
			}
		case "nodeclean":
			mu.Lock()
			last := posted[len(posted)-1]
			mu.Unlock()
			if sys, _ := last["system"].(map[string]any); sys["arch"] != sentinel || h.Status != "submitted" {
				t.Fatalf("nodeclean: a repeatable value must reach the submission and only it: %v %+v", last, h)
			}
		default:
			if h.Status != "error" || h.Detail == "" {
				t.Fatalf("%s: standing: %+v", md, h)
			}
		}
		// The next mode's run must find the host due again.
		m.mutateState(id, func(s *BoxState) { s.HostSys.Users++ })
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
		switch r.URL.Path {
		case "/api/servers":
			lists++
			if slow {
				mu.Unlock()
				time.Sleep(300 * time.Millisecond)
				mu.Lock()
			}
			w.WriteHeader(listStatus)
			_, _ = io.WriteString(w, `[]`)
		case "/api/servers/raw":
			posts++
			w.WriteHeader(http.StatusTooManyRequests)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	m, _ := newPMDBManager(t, srv.URL)
	pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", pmdbFullHW)
	pmdbAddHost(t, m, "b", "thor.pulsedmedia.com", pmdbFullHW)
	pmdbEnable(m, "someone")
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
	// A new identifier lifts the hold; a slow catalogue under a tiny
	// budget posts nothing and is held as a service failure.
	pmdbEnable(m, "another")
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
	pmdbEnable(m, "third")
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
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	m, _ := newPMDBManager(t, srv.URL)
	id := pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", pmdbFullHW)
	pmdbEnable(m, "someone")
	done := make(chan struct{})
	go func() { m.SyncPMDB(ctx); close(done) }()
	<-listArrived
	if err := m.RemoveBox(ctx, id); err != nil {
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

// A box removed or moved to another host while its catalogue row was
// being read is not submitted from the old facts: the row is checked
// against the boxes again after the lookup, and a host that changed
// meanwhile waits for the next run.
func TestSyncPMDBRevalidatesAfterNodeLookup(t *testing.T) {
	ctx := context.Background()
	var mu sync.Mutex
	var posts int
	nodeArrived, release := make(chan struct{}, 1), make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/servers":
			_, _ = io.WriteString(w, `[]`)
		case strings.HasPrefix(r.URL.Path, "/api/nodes/"):
			select {
			case nodeArrived <- struct{}{}:
			default:
			}
			<-release
			_, _ = io.WriteString(w, `{"server":{"kvm":false},"system":{"arch":"x86_64"}}`)
		case r.URL.Path == "/api/servers/raw":
			mu.Lock()
			posts++
			mu.Unlock()
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	m, st := newPMDBManager(t, srv.URL)
	hw := pmdbFullHW
	hw.Arch = "" // unknown: the catalogue's row is read, which is where the box change lands
	id := pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", hw)
	pmdbEnable(m, "someone")
	// 1. The box is removed while the row is being read.
	done := make(chan struct{})
	go func() { m.SyncPMDB(ctx); close(done) }()
	<-nodeArrived
	if err := m.RemoveBox(ctx, id); err != nil {
		t.Fatal(err)
	}
	release <- struct{}{}
	<-done
	mu.Lock()
	n := posts
	mu.Unlock()
	recs, _ := st.PMDBSubmissions(ctx)
	if n != 0 || len(recs) != 0 || len(m.PMDB().Hosts) != 0 {
		t.Fatalf("removed box submitted: posts=%d receipts=%v hosts=%+v", n, recs, m.PMDB().Hosts)
	}
	// 2. The box turns out to be on another host meanwhile: the old
	// host's row is not posted, and the new host waits for the next run.
	id = pmdbAddHost(t, m, "b", "thor.pulsedmedia.com", hw)
	done = make(chan struct{})
	go func() { m.SyncPMDB(ctx); close(done) }()
	<-nodeArrived
	m.mutateState(id, func(s *BoxState) { s.Hostname = "loki.pulsedmedia.com" })
	release <- struct{}{}
	<-done
	mu.Lock()
	n = posts
	mu.Unlock()
	recs, _ = st.PMDBSubmissions(ctx)
	if n != 0 || len(recs) != 0 {
		t.Fatalf("moved box submitted under the old host: posts=%d receipts=%v", n, recs)
	}
	// 3. A fact changes while the row is being read: the host waits, and
	// the next run posts the new facts.
	done = make(chan struct{})
	go func() { m.SyncPMDB(ctx); close(done) }()
	<-nodeArrived
	m.mutateState(id, func(s *BoxState) { s.HostSys.Users++ })
	release <- struct{}{}
	<-done
	mu.Lock()
	n = posts
	mu.Unlock()
	if h := pmdbHosts(m)["loki.pulsedmedia.com"]; n != 0 || h.Status != "pending" || h.Detail != pmdbChanged {
		t.Fatalf("changed facts posted: posts=%d %+v", n, h)
	}
	go func() { <-nodeArrived; release <- struct{}{} }()
	m.SyncPMDB(ctx)
	mu.Lock()
	n = posts
	mu.Unlock()
	if recs, _ = st.PMDBSubmissions(ctx); n != 1 || len(recs) != 1 || pmdbHosts(m)["loki.pulsedmedia.com"].Status != "submitted" {
		t.Fatalf("next run: posts=%d receipts=%v", n, recs)
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
		switch r.URL.Path {
		case "/api/servers":
			lists++
			_, _ = io.WriteString(w, `[]`)
		case "/api/servers/raw":
			posts++
			if posts == 2 {
				// The page closed while the second submission was in
				// flight; the first was answered. This one gets no answer
				// at all (the connection is dropped), so the cancel is
				// what the client sees, every time.
				cancel()
				conn, _, err := w.(http.Hijacker).Hijack()
				if err == nil {
					conn.Close()
				}
				return
			}
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	m, _ := newPMDBManager(t, srv.URL)
	pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", pmdbFullHW)
	pmdbAddHost(t, m, "b", "thor.pulsedmedia.com", pmdbFullHW)
	pmdbEnable(m, "someone")
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

// An acceptance that arrives just as the caller goes away is still a
// receipt, or the next run would post the host again.
func TestSyncPMDBCallerCancelKeepsReceipt(t *testing.T) {
	ctx := &armedCtx{Context: context.Background()}
	k := newPMDBMock(t)
	k.set(func(k *pmdbMock) {
		k.onPost = func(row map[string]any) {
			if row["serverName"] == "odin.pulsedmedia.com" {
				ctx.armed.Store(true) // the caller is gone by the time this answer is read
			}
		}
	})
	m, st := newPMDBManager(t, k.srv.URL)
	pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", pmdbFullHW)
	pmdbAddHost(t, m, "b", "thor.pulsedmedia.com", pmdbFullHW)
	pmdbEnable(m, "someone")
	m.SyncPMDB(ctx) // odin is accepted and the caller found gone; the run ends there
	if got := k.names(); len(got) != 1 || got[0] != "odin.pulsedmedia.com" {
		t.Fatalf("first run posted %v", got)
	}
	if recs, err := st.PMDBSubmissions(context.Background()); err != nil || recs["odin.pulsedmedia.com"].Username != "someone" {
		t.Fatalf("receipt not stored: %+v %v", recs, err)
	}
	m.SyncPMDB(context.Background())
	if got := k.names(); len(got) != 2 || got[1] != "thor.pulsedmedia.com" {
		t.Fatalf("after the cancelled acceptance, posts were %v; odin must not be posted again", got)
	}
	if h := pmdbHosts(m)["odin.pulsedmedia.com"]; h.Status != "submitted" {
		t.Fatalf("odin: %+v", h)
	}
}

// A caller that goes away during the list fetch is not a catalogue failure
// either: nothing is recorded and no hold is set.
func TestSyncPMDBCallerCancelDuringList(t *testing.T) {
	var lists atomic.Int32
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/servers":
			if lists.Add(1) == 1 {
				cancel() // the page closed mid-fetch; the answer never comes
				if conn, _, err := w.(http.Hijacker).Hijack(); err == nil {
					conn.Close()
				}
				return
			}
			_, _ = io.WriteString(w, `[{"serverName":"odin.pulsedmedia.com"}]`)
		case "/api/servers/raw":
			w.WriteHeader(http.StatusCreated)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	m, _ := newPMDBManager(t, srv.URL)
	pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", pmdbFullHW)
	pmdbEnable(m, "someone")
	m.SyncPMDB(ctx)
	if v := m.PMDB(); v.Error != "" || v.CheckedAt != "" {
		t.Fatalf("caller cancel during the list recorded: %+v", v)
	}
	m.SyncPMDB(context.Background()) // not held
	if n := lists.Load(); n != 2 {
		t.Fatalf("lists=%d; want 2 (no hold after a caller cancel)", n)
	}
	if v := m.PMDB(); v.Error != "" || len(v.Hosts) != 1 || v.Hosts[0].Status != "submitted" || !v.Hosts[0].Listed {
		t.Fatalf("second run: %+v", v)
	}
}

// Submissions are spent against a rolling window: a fleet larger than it
// is worked through as the window opens, no host posted twice, a manual
// check inside the window posts nothing, and a reset does not reopen it.
func TestSyncPMDBRateWindow(t *testing.T) {
	ctx := context.Background()
	k := newPMDBMock(t)
	m, _ := newPMDBManager(t, k.srv.URL)
	const hosts = pmdbWindowMax + 2
	for i := 0; i < hosts; i++ {
		host := fmt.Sprintf("host%03d.pulsedmedia.com", i)
		pmdbAddHost(t, m, host, host, pmdbFullHW)
	}
	pmdbEnable(m, "someone")
	count := func() (n, dup int) {
		seen := map[string]int{}
		for _, name := range k.names() {
			seen[name]++
		}
		for _, c := range seen {
			n++
			if c > 1 {
				dup++
			}
		}
		return
	}
	m.syncPMDB(ctx, false)
	if n, dup := count(); n != pmdbWindowMax || dup != 0 {
		t.Fatalf("first run: %d hosts posted (%d twice); want %d", n, dup, pmdbWindowMax)
	}
	var waiting int
	for _, h := range m.PMDB().Hosts {
		if h.Status == "pending" && h.Detail == pmdbWaitWindow {
			waiting++
		}
	}
	if waiting != 2 {
		t.Fatalf("%d hosts waiting for the window; want 2: %+v", waiting, m.PMDB().Hosts)
	}
	m.SyncPMDB(ctx) // inside the window: nothing more, however the check was asked for
	m.resetPMDB()
	m.SyncPMDB(ctx) // a reset does not buy submissions
	if n, dup := count(); n != pmdbWindowMax || dup != 0 {
		t.Fatalf("inside the window: %d hosts posted (%d twice)", n, dup)
	}
	prev := pmdbWindow
	pmdbWindow = 0
	defer func() { pmdbWindow = prev }()
	m.syncPMDB(ctx, false) // the window has passed: the rest, from the cached list
	if n, dup := count(); n != hosts || dup != 0 {
		t.Fatalf("after the window: %d hosts posted (%d twice); want %d", n, dup, hosts)
	}
	m.syncPMDB(ctx, false)
	m.SyncPMDB(ctx)
	if n, dup := count(); n != hosts || dup != 0 {
		t.Fatalf("later runs: %d hosts posted (%d twice)", n, dup)
	}
}

// Automatic runs are cheap whatever the catalogue sends: once nothing is
// due an inventory wake fetches nothing, a new host within
// pmdbListInterval of the last fetch is posted from that copy (not listed
// again), a host backing off asks for nothing either, and only a check
// the user asks for (or the periodic one) always fetches.
func TestSyncPMDBAutomaticRunsAskLittle(t *testing.T) {
	ctx := context.Background()
	k := newPMDBMock(t)
	k.set(func(k *pmdbMock) {
		k.listBody = `[{"serverName":"odin.pulsedmedia.com"},{"serverName":"thor.pulsedmedia.com"}]`
	})
	m, _ := newPMDBManager(t, k.srv.URL)
	pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", pmdbFullHW)
	pmdbAddHost(t, m, "b", "thor.pulsedmedia.com", pmdbFullHW)
	pmdbEnable(m, "someone")
	counts := func() (int, int) {
		l, _, _, p := k.counts()
		return l, p
	}
	for i := 0; i < 20; i++ {
		m.syncPMDB(ctx, false) // twenty inventory wakes
	}
	if l, p := counts(); l != 1 || p != 2 {
		t.Fatalf("settled fleet: lists=%d posts=%d; want 1 and 2", l, p)
	}
	// A new host within the interval: posted from the copy, no fetch.
	pmdbAddHost(t, m, "c", "freya.pulsedmedia.com", pmdbFullHW)
	m.syncPMDB(ctx, false)
	if l, p := counts(); l != 1 || p != 3 {
		t.Fatalf("new host: lists=%d posts=%d; want 1 and 3", l, p)
	}
	// Past the interval, a host that is due fetches again; one refused
	// with a 4xx backs off and asks for nothing meanwhile.
	pmdbListInterval = 0
	defer func() { pmdbListInterval = 10 * time.Minute }()
	k.set(func(k *pmdbMock) { k.postStatus = http.StatusBadRequest })
	pmdbAddHost(t, m, "d", "loki.pulsedmedia.com", pmdbFullHW)
	m.syncPMDB(ctx, false)
	if l, p := counts(); l != 2 || p != 4 {
		t.Fatalf("due host past the interval: lists=%d posts=%d; want 2 and 4", l, p)
	}
	for i := 0; i < 5; i++ {
		m.syncPMDB(ctx, false)
	}
	if l, p := counts(); l != 2 || p != 4 {
		t.Fatalf("host backing off: lists=%d posts=%d; want 2 and 4 still", l, p)
	}
	// The check the user asks for always fetches (and the backing-off
	// host still waits its hour).
	m.SyncPMDB(ctx)
	m.SyncPMDB(ctx)
	if l, p := counts(); l != 4 || p != 4 {
		t.Fatalf("manual checks: lists=%d posts=%d; want 4 and 4", l, p)
	}
	if v := m.PMDB(); v.CheckedAt == "" {
		t.Fatalf("view: %+v", v)
	}
}

// An opt-out that lands between a run being asked for and the run starting
// is seen: the settings are read under the state lock, after the reset,
// so not even the one list fetch goes out.
func TestSyncPMDBOptOutBeforeBegin(t *testing.T) {
	k := newPMDBMock(t)
	m, _ := newPMDBManager(t, k.srv.URL)
	pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", pmdbFullHW)
	pmdbEnable(m, "someone")
	pmdbBeginHook = func() {
		pmdbBeginHook = nil
		off := m.Settings()
		off.PMDBEnabled = false
		m.SetSettings(off) // the user opts out as the run is about to begin
	}
	defer func() { pmdbBeginHook = nil }()
	m.SyncPMDB(context.Background())
	if l, _, _, _ := k.counts(); l != 0 {
		t.Fatalf("%d list fetches after an opt-out", l)
	}
}

// A host is posted again only on the dashboard's own say-so: its facts
// changed, the refresh interval passed, or the identifier changed. The
// catalogue's list, its numbers, and a reset cannot make it due.
func TestSyncPMDBRefreshPolicy(t *testing.T) {
	ctx := context.Background()
	k := newPMDBMock(t)
	m, _ := newPMDBManager(t, k.srv.URL)
	odin := pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", pmdbFullHW)
	pmdbAddHost(t, m, "b", "thor.pulsedmedia.com", pmdbFullHW)
	pmdbEnable(m, "someone")
	posts := func() int { _, _, _, p := k.counts(); return p }
	m.SyncPMDB(ctx)
	if posts() != 2 {
		t.Fatalf("first run: %d posts", posts())
	}
	m.SyncPMDB(ctx)
	m.resetPMDB()
	m.SyncPMDB(ctx)
	k.set(func(k *pmdbMock) { k.listBody = `[{"serverName":"odin.pulsedmedia.com","coreCount":99,"kvm":true}]` })
	m.SyncPMDB(ctx)
	k.set(func(k *pmdbMock) { k.listBody = `[]` })
	m.SyncPMDB(ctx)
	if posts() != 2 {
		t.Fatalf("unchanged facts were posted again: %d posts", posts())
	}
	// A changed fact: that host, once.
	m.mutateState(odin, func(s *BoxState) { s.HostSys.MemTotal = 64 << 30 })
	m.SyncPMDB(ctx)
	m.SyncPMDB(ctx)
	if got := k.names(); posts() != 3 || got[2] != "odin.pulsedmedia.com" || k.row("odin.pulsedmedia.com")["ramGiB"] != 64.0 {
		t.Fatalf("changed fact: %v", got)
	}
	// The refresh interval: the receipt aged past it posts the host again.
	m.pmdbMu.Lock()
	rec := m.pmdbDone["thor.pulsedmedia.com"]
	rec.SubmittedAt = rec.SubmittedAt.Add(-pmdbRefresh - time.Minute)
	m.pmdbDone["thor.pulsedmedia.com"] = rec
	m.pmdbMu.Unlock()
	m.SyncPMDB(ctx)
	m.SyncPMDB(ctx)
	if got := k.names(); posts() != 4 || got[3] != "thor.pulsedmedia.com" {
		t.Fatalf("refresh: %v", got)
	}
}

// Receipts live in the store: a dashboard restarted on the same database
// shows what it submitted and posts nothing again.
func TestSyncPMDBPersistsAcrossRestart(t *testing.T) {
	ctx := context.Background()
	k := newPMDBMock(t)
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	start := func() *Manager {
		m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
		m.pmdb.URL = k.srv.URL
		if err := m.Start(ctx); err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := start()
	pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", pmdbFullHW, func(s *BoxState) { s.Plan = "M10G S" })
	pmdbAddHost(t, m, "b", "thor.pulsedmedia.com", pmdbFullHW)
	pmdbEnable(m, "someone")
	if err := st.SaveSettings(ctx, m.Settings()); err != nil { // what the settings endpoint does beside SetSettings
		t.Fatal(err)
	}
	m.SyncPMDB(ctx)
	if _, _, _, p := k.counts(); p != 2 {
		t.Fatalf("first process: %d posts", p)
	}
	m.Stop()

	m2 := start()
	defer m2.Stop()
	boxes, _ := st.ListBoxes(ctx)
	for _, b := range boxes {
		host := map[string]string{"a": "odin.pulsedmedia.com", "b": "thor.pulsedmedia.com"}[b.Name]
		m2.mutateState(b.ID, func(s *BoxState) {
			s.Ready, s.Hostname, s.HostSys = true, host, pmdbFullHW
			if b.Name == "a" {
				s.Plan = "M10G S"
			}
		})
	}
	// Before any run, the view already shows the receipts.
	v := m2.PMDB()
	if !v.Enabled || len(v.Hosts) != 2 || v.CheckedAt != "" {
		t.Fatalf("view after restart: %+v", v)
	}
	for _, h := range v.Hosts {
		if h.Status != "submitted" || h.SubmittedAt == "" {
			t.Fatalf("host after restart: %+v", h)
		}
	}
	m2.SyncPMDB(ctx)
	if l, _, _, p := k.counts(); p != 2 || l != 2 {
		t.Fatalf("second process: lists=%d posts=%d; want 2 and 2 (nothing posted again)", l, p)
	}
	for _, h := range m2.PMDB().Hosts {
		if h.Status != "submitted" {
			t.Fatalf("after the run: %+v", h)
		}
	}
}

// A row with an unknown reads the catalogue's row first and repeats what
// it holds, number for number and string for string; a row with nothing
// unknown reads nothing; a row that cannot be read is not posted (the
// service failing holds the run), and a value that cannot be repeated
// skips the host rather than erasing it.
func TestSyncPMDBNeverErasesKnownValues(t *testing.T) {
	ctx := context.Background()
	k := newPMDBMock(t)
	k.set(func(k *pmdbMock) {
		k.nodeStatus = http.StatusOK
		k.nodeBody = `{"server":{"serverName":"odin.pulsedmedia.com","processorMhz":2400,"swapBytes":1048576,"kvm":true},
			"system":{"arch":"x86_64","vm":"KVM","distro":"Other OS","kernel":null},"history":{}}`
	})
	m, _ := newPMDBManager(t, k.srv.URL)
	hw := pmdbFullHW
	hw.CPUMHz, hw.SwapKnown, hw.Virt, hw.Arch = 0, false, "", ""
	pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", hw)
	pmdbEnable(m, "someone")
	m.SyncPMDB(ctx) // the cached list says odin is not listed; the row is read all the same
	if _, _, n, p := k.counts(); n != 1 || p != 1 {
		t.Fatalf("nodes=%d posts=%d; want 1 and 1", n, p)
	}
	row := k.row("odin.pulsedmedia.com")
	sys, _ := row["system"].(map[string]any)
	if row["processorMhz"] != 2400.0 || row["swapGiB"] != 1.0/1024 || row["kvm"] != true || sys["arch"] != "x86_64" || sys["vm"] != "KVM" ||
		sys["distro"] != "Debian GNU/Linux 12 (bookworm)" || sys["kernel"] != "6.1.0-37-amd64" {
		t.Fatalf("filled row: %v", row)
	}
	if h := pmdbHosts(m)["odin.pulsedmedia.com"]; h.Status != "submitted" {
		t.Fatalf("odin: %+v", h)
	}
	// No row at the catalogue: nothing to preserve, posted with the
	// unknowns as null.
	k.set(func(k *pmdbMock) { k.nodeStatus = http.StatusNotFound })
	pmdbAddHost(t, m, "b", "thor.pulsedmedia.com", hw)
	m.SyncPMDB(ctx)
	if row := k.row("thor.pulsedmedia.com"); row == nil || row["processorMhz"] != nil || row["kvm"] != nil || row["swapGiB"] != nil {
		t.Fatalf("unlisted host: %v", row)
	}
	// The row cannot be read: not posted, and the service failing holds
	// the run.
	k.set(func(k *pmdbMock) { k.nodeStatus = http.StatusInternalServerError })
	pmdbAddHost(t, m, "c", "loki.pulsedmedia.com", hw)
	m.SyncPMDB(ctx)
	if h := pmdbHosts(m)["loki.pulsedmedia.com"]; h.Status != "error" || h.Detail != pmdbReadFailed || k.row("loki.pulsedmedia.com") != nil {
		t.Fatalf("unreadable row: %+v %v", h, k.row("loki.pulsedmedia.com"))
	}
	if v := m.PMDB(); !strings.Contains(v.Error, "not answering") {
		t.Fatalf("hold not shown: %+v", v)
	}
	// A value that cannot be repeated: the host is skipped, not posted
	// with the value missing.
	m.resetPMDB()
	k.set(func(k *pmdbMock) {
		k.nodeStatus = http.StatusOK
		k.nodeBody = `{"server":{},"system":{"arch":"x86_64\u0000"}}`
	})
	pmdbAddHost(t, m, "d", "freya.pulsedmedia.com", hw)
	m.SyncPMDB(ctx)
	if h := pmdbHosts(m)["freya.pulsedmedia.com"]; h.Status != "error" || h.Detail != pmdbCannotRepeat || k.row("freya.pulsedmedia.com") != nil {
		t.Fatalf("unrepeatable value: %+v %v", h, k.row("freya.pulsedmedia.com"))
	}
	// A row with nothing unknown reads nothing.
	_, _, before, _ := k.counts()
	pmdbAddHost(t, m, "e", "tyr.pulsedmedia.com", pmdbFullHW)
	m.SyncPMDB(ctx)
	if _, _, after, _ := k.counts(); after != before || k.row("tyr.pulsedmedia.com") == nil {
		t.Fatalf("a complete row asked for the catalogue's: nodes %d -> %d", before, after)
	}
}

// The plan goes only when the catalogue's own list has it; a plan list
// that cannot be read defers the hosts that claim a plan (not the others)
// until it can; a catalogue with submissions off holds the run.
func TestSyncPMDBPlanOnlyWhenInConfig(t *testing.T) {
	ctx := context.Background()
	k := newPMDBMock(t)
	k.set(func(k *pmdbMock) { k.configBody = `{"uploadsEnabled":true,"plans":["M10G S"]}` })
	m, _ := newPMDBManager(t, k.srv.URL)
	pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", pmdbFullHW, func(s *BoxState) { s.Plan = "M10G S" })
	pmdbAddHost(t, m, "b", "thor.pulsedmedia.com", pmdbFullHW, func(s *BoxState) { s.Plan = "Other" })
	pmdbAddHost(t, m, "c", "loki.pulsedmedia.com", pmdbFullHW)
	pmdbEnable(m, "someone")
	m.SyncPMDB(ctx)
	if _, c, _, p := k.counts(); c != 1 || p != 3 {
		t.Fatalf("configs=%d posts=%d; want 1 and 3", c, p)
	}
	if k.row("odin.pulsedmedia.com")["plan"] != "M10G S" {
		t.Fatalf("odin: %v", k.row("odin.pulsedmedia.com"))
	}
	if r := k.row("thor.pulsedmedia.com"); r["plan"] != nil {
		t.Fatalf("thor: a plan the catalogue does not list went out: %v", r)
	}
	if h := pmdbHosts(m)["thor.pulsedmedia.com"]; h.Detail != pmdbPlanUnknown || h.Plan != "Other" {
		t.Fatalf("thor standing: %+v", h)
	}
	if _, has := k.row("loki.pulsedmedia.com")["plan"]; has {
		t.Fatal("loki: no plan claimed, none must go")
	}
	// The plan list cannot be read: the host with a plan waits, the one
	// without goes; once it can, the waiting host goes with its plan.
	k.set(func(k *pmdbMock) { k.configStatus = http.StatusInternalServerError })
	pmdbAddHost(t, m, "d", "freya.pulsedmedia.com", pmdbFullHW, func(s *BoxState) { s.Plan = "M10G S" })
	pmdbAddHost(t, m, "e", "tyr.pulsedmedia.com", pmdbFullHW)
	m.SyncPMDB(ctx)
	if _, _, _, p := k.counts(); p != 4 || k.row("tyr.pulsedmedia.com") == nil || k.row("freya.pulsedmedia.com") != nil {
		t.Fatalf("plan list down: posts=%d tyr=%v freya=%v", p, k.row("tyr.pulsedmedia.com"), k.row("freya.pulsedmedia.com"))
	}
	if h := pmdbHosts(m)["freya.pulsedmedia.com"]; h.Status != "pending" || h.Detail != pmdbWaitPlans {
		t.Fatalf("freya waiting: %+v", h)
	}
	if v := m.PMDB(); v.Error != "" {
		t.Fatalf("a plan-list failure must not hold: %+v", v)
	}
	k.set(func(k *pmdbMock) { k.configStatus = http.StatusOK })
	m.SyncPMDB(ctx)
	if r := k.row("freya.pulsedmedia.com"); r == nil || r["plan"] != "M10G S" {
		t.Fatalf("freya after the plan list came back: %v", r)
	}
	// Submissions switched off at the catalogue: nothing posted, held.
	k.set(func(k *pmdbMock) { k.configBody = `{"uploadsEnabled":false,"plans":["M10G S"]}` })
	pmdbAddHost(t, m, "f", "baldr.pulsedmedia.com", pmdbFullHW, func(s *BoxState) { s.Plan = "M10G S" })
	m.SyncPMDB(ctx)
	l1, _, _, p1 := k.counts()
	m.SyncPMDB(ctx)
	l2, _, _, p2 := k.counts()
	if v := m.PMDB(); k.row("baldr.pulsedmedia.com") != nil || p1 != p2 || l1 != l2 || !strings.Contains(v.Error, "switched off") {
		t.Fatalf("uploads off: posts %d/%d lists %d/%d view=%+v", p1, p2, l1, l2, v)
	}
}

// Strings go out whole or not at all, in the catalogue's own units.
func TestSyncPMDBOutboundText(t *testing.T) {
	ctx := context.Background()
	k := newPMDBMock(t)
	k.set(func(k *pmdbMock) {
		k.nodeStatus = http.StatusOK
		k.nodeBody = `{"server":{},"system":{"distro":"Catalogue Linux"}}`
	})
	m, _ := newPMDBManager(t, k.srv.URL)
	long := pmdbFullHW
	long.CPUModel = strings.Repeat("c", 301)
	pmdbAddHost(t, m, "a", "odin.pulsedmedia.com", long)
	wide := pmdbFullHW
	wide.Distro = strings.Repeat("d", 201) // refused, so unknown: the catalogue's own is repeated
	pmdbAddHost(t, m, "b", "thor.pulsedmedia.com", wide)
	emoji := pmdbFullHW
	emoji.CPUModel = strings.Repeat("\U0001F600", 150) // 300 units: just fits
	pmdbAddHost(t, m, "c", "loki.pulsedmedia.com", emoji)
	emoji.CPUModel += "\U0001F600" // 302 units
	pmdbAddHost(t, m, "d", "freya.pulsedmedia.com", emoji)
	pmdbEnable(m, "someone")
	m.SyncPMDB(ctx)
	hosts := pmdbHosts(m)
	if h := hosts["odin.pulsedmedia.com"]; h.Status != "skipped" || h.Detail != pmdbSkipCPU || k.row("odin.pulsedmedia.com") != nil {
		t.Fatalf("long processor name: %+v %v", h, k.row("odin.pulsedmedia.com"))
	}
	if r := k.row("thor.pulsedmedia.com"); r == nil || r["system"].(map[string]any)["distro"] != "Catalogue Linux" {
		t.Fatalf("over-long distro: %v", r)
	}
	if r := k.row("loki.pulsedmedia.com"); r == nil || r["processorName"] != strings.Repeat("\U0001F600", 150) {
		t.Fatalf("300-unit processor name: %v", r)
	}
	if h := hosts["freya.pulsedmedia.com"]; h.Status != "skipped" || k.row("freya.pulsedmedia.com") != nil {
		t.Fatalf("302-unit processor name: %+v", h)
	}
}

// A box whose hostname is not a Pulsed Media host is shown and never
// posted: the catalogue is their hosts, and a misconfigured hostname would
// make a stray row under the identifier.
func TestSyncPMDBSkipsForeignHostnames(t *testing.T) {
	ctx := context.Background()
	k := newPMDBMock(t)
	m, _ := newPMDBManager(t, k.srv.URL)
	pmdbAddHost(t, m, "a", "box.example.org", pmdbFullHW)
	pmdbEnable(m, "someone")
	m.syncPMDB(ctx, false)
	if l, _, _, p := k.counts(); l != 0 || p != 0 {
		t.Fatalf("an automatic run with nothing due asked: lists=%d posts=%d", l, p)
	}
	m.SyncPMDB(ctx)
	if l, _, _, p := k.counts(); l != 1 || p != 0 {
		t.Fatalf("lists=%d posts=%d; want 1 and 0", l, p)
	}
	if h := pmdbHosts(m)["box.example.org"]; h.Status != "skipped" || h.Detail != pmdbSkipForeign {
		t.Fatalf("foreign host: %+v", h)
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
		switch r.URL.Path {
		case "/api/servers":
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
		case "/api/servers/raw":
			posts++
			w.Header().Set("Location", "https://elsewhere.example/")
			w.WriteHeader(http.StatusFound)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	m, _ := newPMDBManager(t, srv.URL)
	for i, host := range []string{"odin.pulsedmedia.com", "thor.pulsedmedia.com", "loki.pulsedmedia.com"} {
		pmdbAddHost(t, m, string(rune('a'+i)), host, pmdbFullHW)
	}
	pmdbEnable(m, "someone")
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
