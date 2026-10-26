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
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"pmss-fleet-manager/internal/probe"
	"pmss-fleet-manager/internal/qbt"
	"pmss-fleet-manager/internal/rtorrent"
	"pmss-fleet-manager/internal/sshx"
	"pmss-fleet-manager/internal/store"
)

// fakeSCGI answers XML-RPC over a unix socket by method name, recording
// the methods called.
type fakeSCGI struct {
	ln      net.Listener
	mu      sync.Mutex
	methods []string
	reply   func(method string, body string) string
}

func newFakeSCGI(t *testing.T) *fakeSCGI {
	t.Helper()
	dir, err := os.MkdirTemp("", "scgi")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeSCGI{ln: ln}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

func (f *fakeSCGI) serve(c net.Conn) {
	defer c.Close()
	br := bufio.NewReader(c)
	n, err := br.ReadString(':')
	if err != nil {
		return
	}
	hl, _ := strconv.Atoi(strings.TrimSuffix(n, ":"))
	hdr := make([]byte, hl+1)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return
	}
	cl, _ := strconv.Atoi(strings.Split(string(hdr[:hl]), "\x00")[1])
	body := make([]byte, cl)
	if _, err := io.ReadFull(br, body); err != nil {
		return
	}
	s := string(body)
	method := ""
	if i := strings.Index(s, "<methodName>"); i >= 0 {
		method = s[i+12 : strings.Index(s, "</methodName>")]
	}
	f.mu.Lock()
	f.methods = append(f.methods, method)
	reply := f.reply
	f.mu.Unlock()
	resp := reply(method, s)
	if resp == closeConn {
		return // the peer read the request and went away without answering
	}
	if method == "d.custom1.set" && resp == xmlResp(i8(0)) {
		// Real rTorrent answers a string variable's setter with the string it
		// assigned (fred on PR 122): a reply that did not care gets that.
		if i := strings.LastIndex(s, "<string>"); i >= 0 {
			if j := strings.Index(s[i:], "</string>"); j > 0 {
				resp = xmlResp(s[i : i+j+len("</string>")])
			}
		}
	}
	fmt.Fprintf(c, "Status: 200 OK\r\nContent-Type: text/xml\r\nContent-Length: %d\r\n\r\n%s", len(resp), resp)
}

func (f *fakeSCGI) dialer(string) rtorrent.Dialer {
	return func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", f.ln.Addr().String())
	}
}

func (f *fakeSCGI) called() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.methods...)
}

// closeConn, returned by a reply, closes the connection without an answer.
const closeConn = "\x00close"

func xmlResp(inner string) string {
	return `<?xml version="1.0"?><methodResponse><params><param><value>` + inner + `</value></param></params></methodResponse>`
}

func xmlRow(vals ...string) string {
	var b strings.Builder
	b.WriteString("<value><array><data>")
	for _, v := range vals {
		b.WriteString("<value>" + v + "</value>")
	}
	b.WriteString("</data></array></value>")
	return b.String()
}

func i8(n int64) string  { return fmt.Sprintf("<i8>%d</i8>", n) }
func xs(s string) string { return "<string>" + s + "</string>" }

const rtHashA = "43519D14444904F6015E1A0D92018068FB0A49FA"
const rtHashB = "5B1E0D988FC7A0C9E99BD852071681A59974B39F"

// rtReplies is a fake rTorrent 0.9.8 with two torrents: one downloading,
// one stopped with a stale tracker message (what pm1 showed after a stop).
func rtReplies(method, body string) string {
	switch method {
	case "system.multicall":
		return xmlResp(`<array><data>` + xmlRow(xs("0.9.8")) + xmlRow(xs("0.13.8")) + xmlRow(i8(54197410)) + xmlRow(i8(140945)) + xmlRow(i8(59112)) +
			`<value><array><data><value><struct><member><name>dht</name><value><string>auto</string></value></member><member><name>active</name><value><i8>1</i8></value></member></struct></value></data></array></value></data></array>`)
	case "d.multicall2":
		row := func(hash, name string, size, done, down, up, ratio, state, complete, active, msg, label string) string {
			return xmlRow(xs(hash), xs(name), i8(mustInt(size)), i8(mustInt(done)), i8(mustInt(down)), i8(mustInt(up)), i8(mustInt(ratio)), i8(mustInt(state)), i8(mustInt(complete)), i8(mustInt(active)), i8(0), i8(mustInt(state)), xs(msg), xs(label), i8(0), i8(48), i8(40), i8(1000), i8(2000), i8(0))
		}
		return xmlResp(`<array><data>` +
			row(rtHashA, "ubuntu-24.04.5.1-desktop-amd64.iso", "6250332160", "4868000000", "54197410", "0", "0", "1", "0", "1", "", "iso") +
			row(rtHashB, "ubuntu-26.04.1-desktop-amd64.iso", "6482409472", "6482409472", "0", "0", "2", "0", "1", "0", "Tracker: [Could not parse bencoded data]", "") +
			`</data></array>`)
	case "system.client_version":
		return xmlResp(xs("0.9.8"))
	case "d.base_path":
		return xmlResp(xs("/home/u/data/ubuntu-24.04.5.1-desktop-amd64.iso"))
	case "directory.default":
		return xmlResp(xs("/home/u/data"))
	}
	return xmlResp(i8(0))
}

func mustInt(s string) int64 {
	n, _ := strconv.ParseInt(s, 10, 64)
	return n
}

func rtFixture(t *testing.T, f *fakeSCGI) (*poller, *probe.Snapshot) {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	m := New(st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// The box exists in the store (the removal-notice row hangs off it) and
	// the poller is the box's current one.
	box := &store.Box{ID: 1, Name: "x", Host: "box.example", User: "u", RtEnabled: true}
	if err := st.CreateBox(context.Background(), box); err != nil {
		t.Fatal(err)
	}
	p := newPoller(m, box)
	p.dialRt = f.dialer
	m.mu.Lock()
	m.pollers[p.box.ID] = p
	m.mu.Unlock()
	// The destructive phase's own transport: the same fake SCGI server and a
	// runner that cannot run anything (tests that delete install their own).
	p.dialDetached = func(ctx context.Context, ep string) (scriptRunner, rtorrent.Dialer, func(), error) {
		return &localRunner{home: "/nonexistent"}, f.dialer(ep), func() {}, nil
	}
	snap := &probe.Snapshot{UID: 1001, Home: "/home/u", ProcsKnown: true, Procs: []probe.ProcInfo{{PID: 7500, Comm: "rtorrent main"}},
		Rt: probe.RtInfo{ConfFound: true, SCGI: "unix:/home/u/.rtorrent.socket"}}
	return p, snap
}

func TestPollRt(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, snap := rtFixture(t, f)
	st := BoxState{Rates: RateState{UpBps: 10, DownBps: 20, Source: "qbt"}}
	p.pollRt(context.Background(), &st, snap)
	r := st.Rt
	if r.Error != "" || !r.Reachable || !r.ProcessUp || r.Version != "0.9.8" || r.Library != "0.13.8" || r.ListenPort != 59112 || !r.DHT || r.Endpoint != "unix:/home/u/.rtorrent.socket" {
		t.Fatalf("%+v", r)
	}
	if r.Total != 2 || r.Leeching != 1 || r.Paused != 1 || r.Seeding != 0 || r.Errored != 0 {
		t.Fatalf("counts %+v", r)
	}
	if st.Rates.Source != "clients" || st.Rates.UpBps != 10+140945 || st.Rates.DownBps != 20+54197410 {
		t.Fatalf("rates %+v", st.Rates)
	}
	a := r.Torrents[0] // the active one sorts first
	if a.Hash != strings.ToLower(rtHashA) || a.Client != "rtorrent" || a.Kind != "leeching" || a.State != "downloading" || a.Category != "iso" || a.Seeds != 40 || a.Peers != 48 || a.Progress < 0.77 || a.Progress > 0.78 || a.Uploaded != 1000 {
		t.Fatalf("a = %+v", a)
	}
	b := r.Torrents[1]
	if b.Kind != "paused" || b.State != "closed" || b.Ratio != 0.002 || b.Progress != 1 {
		t.Fatalf("b = %+v", b)
	}
	// Without qBittorrent's rates this poll, rTorrent's stand alone.
	st = BoxState{Rates: RateState{UpBps: 1, Source: "pmss-15min"}}
	p.pollRt(context.Background(), &st, snap)
	if st.Rates.Source != "rtorrent" || st.Rates.UpBps != 140945 {
		t.Fatalf("rates %+v", st.Rates)
	}
}

func TestPollRtGates(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, snap := rtFixture(t, f)
	// Process list read, no rtorrent in it: no dial.
	snap.Procs = nil
	st := BoxState{}
	p.pollRt(context.Background(), &st, snap)
	if st.Rt.Reachable || !strings.Contains(st.Rt.Error, "not running") || len(f.called()) != 0 {
		t.Fatalf("%+v %v", st.Rt, f.called())
	}
	snap.Rt.Disabled = true
	p.pollRt(context.Background(), &st, snap)
	if !strings.Contains(st.Rt.Error, ".rtorrentDisable") || !st.Rt.Disabled {
		t.Fatalf("%+v", st.Rt)
	}
	// Unknown process list: the socket decides.
	snap.Rt.Disabled, snap.ProcsKnown = false, false
	p.pollRt(context.Background(), &st, snap)
	if !st.Rt.Reachable || !st.Rt.ProcessUp || st.Rt.ProcessKnown {
		t.Fatalf("%+v", st.Rt)
	}
	// No endpoint at all.
	snap.Rt.SCGI, snap.Home = "", ""
	p.pollRt(context.Background(), &st, snap)
	if st.Rt.Reachable || !strings.Contains(st.Rt.Error, "no SCGI endpoint") {
		t.Fatalf("%+v", st.Rt)
	}
	// A refused directive is reported as such, with the way out.
	snap.Home = "/home/u"
	snap.Rt = probe.RtInfo{ConfFound: true, ConfError: "unusable SCGI socket in the rTorrent config: scgi_local = ../x"}
	p.pollRt(context.Background(), &st, snap)
	if st.Rt.Reachable || !strings.Contains(st.Rt.Error, "unusable SCGI socket") || !strings.Contains(st.Rt.Error, "under edit") || len(f.called()) != 0 && st.Rt.Endpoint != "" {
		t.Fatalf("%+v", st.Rt)
	}
	snap.Rt = probe.RtInfo{ConfFound: true, SCGI: "unix:/home/u/.rtorrent.socket", DataDir: "/home/u/dl"}
	p.pollRt(context.Background(), &st, snap)
	if st.Rt.DataDir != "/home/u/dl" {
		t.Fatalf("data dir %q", st.Rt.DataDir)
	}
	// A loopback port must be this account's.
	snap.Rt.SCGI = "tcp:127.0.0.1:5000"
	snap.ListenersKnown, snap.Listeners = true, map[int]int{5000: 1002}
	p.pollRt(context.Background(), &st, snap)
	if st.Rt.Reachable || st.Rt.PortOK || !strings.Contains(st.Rt.Error, "uid 1002") {
		t.Fatalf("%+v", st.Rt)
	}
	snap.Listeners[5000] = 1001
	p.pollRt(context.Background(), &st, snap)
	if !st.Rt.Reachable || !st.Rt.PortOK {
		t.Fatalf("%+v", st.Rt)
	}
	// The box setting overrides the config.
	p.box.RtSCGI = "unix:/srv/rpc.sock"
	p.pollRt(context.Background(), &st, snap)
	if st.Rt.Endpoint != "unix:/srv/rpc.sock" {
		t.Fatal(st.Rt.Endpoint)
	}
}

func TestRtEndpointAndValidation(t *testing.T) {
	snap := &probe.Snapshot{Home: "/home/u"}
	if rtEndpoint(rtBox{}, snap) != "unix:/home/u/.rtorrent.socket" {
		t.Fatal("default")
	}
	snap.Rt.SCGI = "tcp:127.0.0.1:5000"
	if rtEndpoint(rtBox{}, snap) != "tcp:127.0.0.1:5000" || rtEndpoint(rtBox{"unix:/x"}, snap) != "unix:/x" {
		t.Fatal("precedence")
	}
	// A directive that was found but refused never falls back to the
	// default socket; the box setting still overrides.
	snap.Rt = probe.RtInfo{ConfFound: true, ConfError: "unusable SCGI socket in the rTorrent config: scgi_local = ../x"}
	if rtEndpoint(rtBox{}, snap) != "" || rtEndpoint(rtBox{"unix:/srv/s"}, snap) != "unix:/srv/s" {
		t.Fatal("refused directive must not select the default")
	}
	if rtDataDir(&probe.Snapshot{Home: "/home/u"}) != "/home/u/data" || rtDataDir(&probe.Snapshot{Home: "/home/u", Rt: probe.RtInfo{DataDir: "/home/u/dl"}}) != "/home/u/dl" || rtDataDir(&probe.Snapshot{}) != "" {
		t.Fatal("data dir")
	}
	for ep, ok := range map[string]bool{
		"": true, "unix:/home/u/.rtorrent.socket": true, "tcp:127.0.0.1:5000": true,
		"unix:relative": false, "unix:/a/../b": false, "unix:/a\n": false, "tcp:0.0.0.0:5000": false, "tcp:127.0.0.1:0": false,
		"tcp:127.0.0.1:70000": false, "http://x": false, "unix:" + strings.Repeat("/a", 300): false,
	} {
		if ValidRtEndpoint(ep) != ok {
			t.Errorf("ValidRtEndpoint(%q) = %v", ep, !ok)
		}
	}
}

// TestRtRemoveDataScript runs the on-box deletion check with a fake HOME:
// only a real path strictly inside a download root that is itself strictly
// inside the home is deleted; symlinks are resolved before the check.
func TestRtRemoveDataScript(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	mk := func(rel string) string {
		p := filepath.Join(home, rel)
		os.MkdirAll(p, 0o755)
		os.WriteFile(filepath.Join(p, "f"), []byte("x"), 0o600)
		return p
	}
	outside := t.TempDir()
	os.WriteFile(filepath.Join(outside, "f"), []byte("x"), 0o600)
	run := func(root, target string) (int, string) {
		cmd := exec.Command("sh")
		cmd.Env = append(os.Environ(), "HOME="+home)
		cmd.Stdin = strings.NewReader(rtRemoveDataVars("delete", root, target) + rtRemoveDataScript)
		out, err := cmd.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		}
		return code, strings.TrimSpace(string(out))
	}
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }

	data := mk("data")
	good := mk("data/Alpha")
	if code, out := run(data, good); code != 0 || exists(good) {
		t.Fatalf("inside: %d %q", code, out)
	}
	// The root itself, the home, a sibling of the root, and a path outside.
	for _, tc := range [][2]string{{data, data}, {home, mk("data/B")}, {data, mk("session")}, {data, outside}, {outside, filepath.Join(outside, "f")}, {"/", mk("data/C")}} {
		code, out := run(tc[0], tc[1])
		if code == 0 || !strings.Contains(out, "refusing") && !strings.Contains(out, "HOME") {
			t.Errorf("%v: code %d %q", tc, code, out)
		}
		if tc[1] != data && tc[1] != home && !exists(tc[1]) {
			t.Errorf("%v: deleted", tc)
		}
	}
	// A symlink inside the root that points outside is resolved and refused.
	link := filepath.Join(data, "link")
	os.Symlink(outside, link)
	if code, out := run(data, link); code == 0 || !strings.Contains(out, "refusing") || !exists(filepath.Join(outside, "f")) {
		t.Fatalf("symlink: %d %q", code, out)
	}
	// A missing target is reported, not treated as success.
	if code, _ := run(data, filepath.Join(data, "gone")); code != 5 {
		t.Fatalf("missing: %d", code)
	}
	// Hostile input: quoting keeps a path with shell metacharacters a path
	// (newlines and NULs are refused before the box is asked).
	odd := mk("data/it's $(touch " + filepath.Join(home, "pwned") + ") x")
	if code, out := run(data, odd); code != 0 || exists(odd) || exists(filepath.Join(home, "pwned")) {
		t.Fatalf("metacharacters: %d %q", code, out)
	}
	if code, out := run(data, "'; touch "+filepath.Join(home, "pwned2")+"; '"); code == 0 || exists(filepath.Join(home, "pwned2")) {
		t.Fatalf("quote break-out: %d %q", code, out)
	}
}

func TestRtClientFollowsTransport(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	p.dialRt = nil
	// Without a transport and without the hook there is nothing to dial.
	if _, err := p.rtClient(context.Background(), nil, "unix:/x"); err == nil {
		t.Fatal("nil transport")
	}
	p.dialRt = f.dialer
	a := &sshx.Client{}
	c1, err := p.rtClient(context.Background(), a, "unix:/x")
	if err != nil {
		t.Fatal(err)
	}
	if c2, _ := p.rtClient(context.Background(), a, "unix:/x"); c2 != c1 {
		t.Fatal("same transport and endpoint: same client")
	}
	// A reconnected transport must not reuse a client whose dialer holds the
	// closed one (Codex P1 on PR 123); so must a changed endpoint.
	if c3, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x"); c3 == c1 {
		t.Fatal("new transport must rebuild the client")
	}
	if c4, _ := p.rtClient(context.Background(), a, "unix:/y"); c4 == c1 {
		t.Fatal("new endpoint must rebuild the client")
	}
	// The teardown paths drop the cache.
	p.rt, p.rtSSH = c1, a
	p.closeConns()
	if p.rt != nil || p.rtSSH != nil {
		t.Fatal("closeConns must drop the rtorrent client")
	}
	// The real dialers are built for a unix path and a loopback port over a
	// transport; a malformed or foreign endpoint is refused.
	p.dialRt = nil
	tr := &sshx.Client{}
	if _, err := p.rtClient(context.Background(), tr, "unix:/home/u/.rtorrent.socket"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.rtClient(context.Background(), tr, "tcp:127.0.0.1:5000"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.rtClient(context.Background(), tr, "tcp:127.0.0.1:x"); err == nil {
		t.Fatal("bad port")
	}
	if _, err := p.rtClient(context.Background(), tr, "http://x"); err == nil {
		t.Fatal("unknown scheme")
	}
}

// readyRt seeds a Ready state with rTorrent's endpoint for p, as a poll
// would, so the action gates pass.
func readyRt(t *testing.T, p *poller, ep string) {
	t.Helper()
	st := BoxState{ID: p.box.ID, Host: p.box.Host, Port: p.box.Port, User: p.box.User, Ready: true, Rt: RtState{Enabled: true, Endpoint: ep, Reachable: true}}
	p.m.mu.Lock()
	p.m.states[p.box.ID] = &st
	p.m.mu.Unlock()
}

func TestRtActionsThroughPoller(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	ctx := context.Background()
	// Not ready yet: nothing reaches the box.
	if err := p.rtTorrentAction(ctx, rtHashA, "stop"); !errors.Is(err, ErrNotReady) {
		t.Fatalf("%v", err)
	}
	p.box.RtEnabled = false
	readyRt(t, p, "unix:/home/u/.rtorrent.socket")
	if err := p.rtTorrentAction(ctx, rtHashA, "stop"); err == nil || !strings.Contains(err.Error(), "not enabled") {
		t.Fatalf("%v", err)
	}
	p.box.RtEnabled = true
	if err := p.rtTorrentAction(ctx, strings.ToLower(rtHashA), "stop"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.called(), ","); got != "d.stop,d.close" {
		t.Fatalf("methods %s", got)
	}
	// With a label the hash must be known up front: the magnet carries it,
	// the file's is computed; the label then goes by hash as a literal
	// XML-RPC parameter, never inside a command string.
	torrent := []byte("d8:announce9:http://x/4:infod4:name1:a12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee")
	res, err := p.rtAddTorrents(ctx, qbt.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashA}, Files: []qbt.TorrentFile{{Name: "a", Data: torrent}}, Category: "(d.stop)", Stopped: true})
	if err != nil || res.Added != 2 || res.Unlabelled != 0 {
		t.Fatalf("%+v %v", res, err)
	}
	m := f.called()
	if strings.Join(m[len(m)-4:], ",") != "load.normal,d.custom1.set,load.raw,d.custom1.set" {
		t.Fatalf("%v", m)
	}
	// No endpoint in the state: refused before any dial.
	readyRt(t, p, "")
	if err := p.rtTorrentAction(ctx, rtHashA, "stop"); err == nil || !strings.Contains(err.Error(), "no SCGI endpoint") {
		t.Fatalf("%v", err)
	}
	// A loopback endpoint needs the poll's ownership verdict first.
	readyRt(t, p, "tcp:127.0.0.1:5000")
	if err := p.rtTorrentAction(ctx, rtHashA, "stop"); err == nil || !strings.Contains(err.Error(), "not been verified") {
		t.Fatalf("%v", err)
	}
	// With the verdict in, the action goes through (the live re-check needs
	// a box and is skipped under the dial hook).
	st := BoxState{ID: p.box.ID, Ready: true, Rt: RtState{Enabled: true, Endpoint: "tcp:127.0.0.1:5000", Reachable: true, PortOK: true}}
	p.m.mu.Lock()
	p.m.states[p.box.ID] = &st
	p.m.mu.Unlock()
	if err := p.rtTorrentAction(ctx, rtHashA, "start"); err != nil {
		t.Fatal(err)
	}
	// remove-data with an unusable path never erases (the state carries a
	// download root, the runner is the SSH client, nil under the hook).
	st = BoxState{ID: p.box.ID, Ready: true, Rt: RtState{Enabled: true, Endpoint: "unix:/home/u/.rtorrent.socket", Reachable: true, DataDir: "/home/u/data"}}
	p.m.mu.Lock()
	p.m.states[p.box.ID] = &st
	p.m.mu.Unlock()
	f.reply = func(m, b string) string {
		if m == "d.base_path" {
			return xmlResp(xs("relative/path"))
		}
		return rtReplies(m, b)
	}
	before := len(f.called())
	err = p.rtTorrentAction(ctx, rtHashA, RtActionRemoveData)
	if err == nil || !strings.Contains(err.Error(), "no SSH connection") && !strings.Contains(err.Error(), "cannot be checked") {
		t.Fatalf("%v", err)
	}
	for _, m := range f.called()[before:] {
		if m == "d.erase" {
			t.Fatal("erase ran on an unusable path")
		}
	}
}

func TestManagerRtGuards(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	m := p.m
	ctx := context.Background()
	m.mu.Lock()
	delete(m.pollers, 1) // not registered yet: unknown box
	m.mu.Unlock()
	if err := m.RtTorrentAction(ctx, 1, rtHashA, "stop"); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	if _, err := m.RtAddTorrents(ctx, 1, qbt.AddRequest{}); !errors.Is(err, store.ErrNotFound) {
		t.Fatal(err)
	}
	m.mu.Lock()
	m.pollers[1] = p
	m.mu.Unlock()
	if err := m.RtTorrentAction(ctx, 1, rtHashA, "pause"); err == nil || !strings.Contains(err.Error(), "unknown rTorrent action") {
		t.Fatal(err)
	}
	if err := m.RtTorrentAction(ctx, 1, rtHashA, "stop"); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
	if _, err := m.RtAddTorrents(ctx, 1, qbt.AddRequest{}); !errors.Is(err, ErrDisabled) {
		t.Fatal(err)
	}
	p.box.Enabled = true
	readyRt(t, p, "unix:/home/u/.rtorrent.socket")
	if err := m.RtTorrentAction(ctx, 1, rtHashA, "start"); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.called(), ","); got != "d.open,d.start" {
		t.Fatal(got)
	}
	res, err := m.RtAddTorrents(ctx, 1, qbt.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashB}})
	if err != nil || res.Added != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestDetectRt(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	detectRtDial = f.dialer
	t.Cleanup(func() { detectRtDial = nil })
	run := func(snap *probe.Snapshot, b *store.Box) (*DetectResult, string) {
		res := &DetectResult{}
		var texts []string
		var add addStep = func(status, format string, args ...any) {
			texts = append(texts, status+": "+fmt.Sprintf(format, args...))
		}
		detectRt(context.Background(), nil, snap, b, res, add)
		return res, strings.Join(texts, " | ")
	}
	// Nothing found: off.
	b := &store.Box{}
	_, out := run(&probe.Snapshot{Home: "/home/u", ProcsKnown: true}, b)
	if b.RtEnabled || !strings.Contains(out, "no rTorrent configuration") {
		t.Fatal(out)
	}
	_, out = run(&probe.Snapshot{Home: "/home/u", ProcsKnown: true, Rt: probe.RtInfo{Disabled: true}}, b)
	if b.RtEnabled || !strings.Contains(out, "switched off") {
		t.Fatal(out)
	}
	// Configured but not running: on, unless stopped on purpose.
	conf := probe.RtInfo{ConfFound: true, SCGI: "unix:/home/u/.rtorrent.socket"}
	_, out = run(&probe.Snapshot{Home: "/home/u", ProcsKnown: true, Rt: conf}, b)
	if !b.RtEnabled || !strings.Contains(out, "warn: rTorrent is configured") {
		t.Fatal(out)
	}
	conf.Disabled = true
	_, out = run(&probe.Snapshot{Home: "/home/u", ProcsKnown: true, Rt: conf}, b)
	if b.RtEnabled || !strings.Contains(out, "stopped on purpose") {
		t.Fatal(out)
	}
	conf.Disabled = false
	// Running: the socket is asked.
	running := &probe.Snapshot{Home: "/home/u", ProcsKnown: true, Procs: []probe.ProcInfo{{Comm: "rtorrent main"}}, Rt: conf}
	res, out := run(running, b)
	if !b.RtEnabled || res.Facts.RtVersion != "0.9.8" || res.Facts.RtEndpoint != "unix:/home/u/.rtorrent.socket" || !strings.Contains(out, "ok: rTorrent 0.9.8 responding") {
		t.Fatalf("%+v %s", res.Facts, out)
	}
	// A loopback port owned by someone else is not dialed.
	tcp := *running
	tcp.Rt = probe.RtInfo{ConfFound: true, SCGI: "tcp:127.0.0.1:5000"}
	tcp.ListenersKnown, tcp.Listeners = true, map[int]int{5000: 1002}
	n := len(f.called())
	_, out = run(&tcp, b)
	if !b.RtEnabled || !strings.Contains(out, "not connecting") || len(f.called()) != n {
		t.Fatal(out)
	}
	// The socket not answering is a warning, still enabled.
	f.reply = func(m, b string) string {
		return `<?xml version="1.0"?><methodResponse><fault><value><struct><member><name>faultCode</name><value><i4>1</i4></value></member><member><name>faultString</name><value><string>x</string></value></member></struct></value></fault></methodResponse>`
	}
	_, out = run(running, b)
	if !b.RtEnabled || !strings.Contains(out, "did not answer") {
		t.Fatal(out)
	}
	// The box setting counts as configuration.
	b2 := &store.Box{RtSCGI: "unix:/srv/s"}
	_, out = run(&probe.Snapshot{Home: "/home/u", ProcsKnown: true}, b2)
	if !b2.RtEnabled || !strings.Contains(out, "unix:/srv/s") {
		t.Fatal(out)
	}
}

func TestRtRemoveDataDeletionOutlivesRPCDeadline(t *testing.T) {
	// The RPC phase is bounded by apiTimeout, the rm by rtRemoveTimeout from
	// the caller's context (Codex on PR 123).
	if rtRemoveTimeout <= apiTimeout {
		t.Fatalf("rtRemoveTimeout %v must exceed apiTimeout %v", rtRemoveTimeout, apiTimeout)
	}
}

func TestRtEndpointNotCarriedAcrossEdit(t *testing.T) {
	// A probed state carries its resolved endpoint; after an edit the
	// record's override (or nothing) is what actions may use until the next
	// poll resolves it again (Codex on PR 123).
	st := BoxState{Ready: true, Rt: RtState{Enabled: true, Endpoint: "unix:/home/u/.rtorrent.socket", Reachable: true}}
	st.adoptIdentity(fromBox(&store.Box{RtEnabled: true, RtSCGI: "tcp:127.0.0.1:5000"}))
	if st.Rt.Endpoint != "tcp:127.0.0.1:5000" {
		t.Fatal(st.Rt.Endpoint)
	}
	st.adoptIdentity(fromBox(&store.Box{RtEnabled: true}))
	if st.Rt.Endpoint != "" {
		t.Fatalf("removed override must not leave the old endpoint: %q", st.Rt.Endpoint)
	}
	// With no endpoint the action gate refuses rather than guessing.
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	readyRt(t, p, "")
	if err := p.rtTorrentAction(context.Background(), rtHashA, "stop"); err == nil || !strings.Contains(err.Error(), "no SCGI endpoint") {
		t.Fatalf("%v", err)
	}
}

func TestClassifyRtDisabledIsNotDegraded(t *testing.T) {
	st := &BoxState{Rt: RtState{Enabled: true, Reachable: false}}
	if s, r := classify(st); s != "degraded" || !strings.Contains(r, "rTorrent") {
		t.Fatal(s, r)
	}
	st.Rt.Disabled = true
	if s, _ := classify(st); s != "online" {
		t.Fatal("stopped on purpose must not degrade the box")
	}
	if !ValidRtAction("stop") || !ValidRtAction("remove-data") || ValidRtAction("pause") {
		t.Fatal("ValidRtAction")
	}
}

// localRunner runs the deletion script with sh against a fake HOME, like
// the box would, recording each run; gate, when set, holds a delete-mode
// run until released.
type localRunner struct {
	home  string
	mu    sync.Mutex
	runs  []string
	gate  chan struct{}
	block bool
}

func (r *localRunner) Run(ctx context.Context, cmd string, stdin io.Reader) ([]byte, []byte, error) {
	script, _ := io.ReadAll(stdin)
	mode := "check"
	if strings.HasPrefix(string(script), "mode='delete'") {
		mode = "delete"
	}
	r.mu.Lock()
	r.runs = append(r.runs, mode)
	block := r.block && mode == "delete"
	r.mu.Unlock()
	if block {
		<-r.gate
	}
	c := exec.CommandContext(ctx, cmd)
	c.Env = append(os.Environ(), "HOME="+r.home)
	c.Stdin = bytes.NewReader(script)
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	return out.Bytes(), errb.Bytes(), err
}

func (r *localRunner) modes() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.runs...)
}

// TestRtRemoveDataBoundary: the boundary is the config's download root,
// never what the peer says; the box check runs before the erase and a
// refusal changes nothing (daybreak blue review of PR 123).
func TestRtRemoveDataBoundary(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	mk := func(rel string) string {
		p := filepath.Join(home, rel)
		os.MkdirAll(p, 0o755)
		os.WriteFile(filepath.Join(p, "f"), []byte("x"), 0o600)
		return p
	}
	data := mk("data")
	keys := mk(".ssh")
	exists := func(p string) bool { _, err := os.Stat(p); return err == nil }
	f := newFakeSCGI(t)
	var base string
	f.reply = func(m, b string) string {
		switch m {
		case "d.base_path":
			return xmlResp(xs(base))
		case "directory.default":
			return xmlResp(xs(keys)) // the peer's idea of its root is never asked for, and would be ignored
		}
		return rtReplies(m, b)
	}
	p, _ := rtFixture(t, f)
	cli := rtorrent.New(f.dialer(""))
	run := &localRunner{home: home}
	p.dialDetached = func(ctx context.Context, ep string) (scriptRunner, rtorrent.Dialer, func(), error) {
		return run, f.dialer(ep), func() {}, nil
	}
	ctx := context.Background()
	erased := func() int {
		n := 0
		for _, m := range f.called() {
			if m == "d.erase" {
				n++
			}
		}
		return n
	}
	// A hostile peer naming a path outside the download root: refused by
	// the box before anything, torrent left in place.
	base = filepath.Join(keys, "f")
	err := p.rtRemoveData(ctx, cli, run, "unix:/x", data, rtHashA, nil)
	if err == nil || !strings.Contains(err.Error(), "left in place") || !strings.Contains(err.Error(), "not inside the download root") || erased() != 0 || !exists(base) {
		t.Fatalf("%v erased %d exists %v", err, erased(), exists(base))
	}
	if got := run.modes(); strings.Join(got, ",") != "check" {
		t.Fatalf("runs %v", got)
	}
	// The root itself, a missing path and no root at all are refused too.
	base = data
	if err := p.rtRemoveData(ctx, cli, run, "unix:/x", data, rtHashA, nil); err == nil || erased() != 0 {
		t.Fatalf("root itself: %v", err)
	}
	base = filepath.Join(data, "gone")
	if err := p.rtRemoveData(ctx, cli, run, "unix:/x", data, rtHashA, nil); err == nil || erased() != 0 || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("missing: %v", err)
	}
	if err := p.rtRemoveData(ctx, cli, run, "unix:/x", "", rtHashA, nil); err == nil || erased() != 0 || !strings.Contains(err.Error(), "download directory is not known") {
		t.Fatalf("no root: %v", err)
	}
	if err := p.rtRemoveData(ctx, cli, nil, "unix:/x", data, rtHashA, nil); err == nil || erased() != 0 {
		t.Fatalf("no runner: %v", err)
	}
	// A torrent that never started has no path: said so, nothing erased.
	base = ""
	if err := p.rtRemoveData(ctx, cli, run, "unix:/x", data, rtHashA, nil); err == nil || !strings.Contains(err.Error(), "no download path") || erased() != 0 {
		t.Fatalf("empty path: %v", err)
	}
	// Inside the root: check, erase, delete, in that order; the tree is gone.
	target := mk("data/Alpha")
	base = target
	run.runs = nil
	if err := p.rtRemoveData(ctx, cli, run, "unix:/x", data, rtHashA, nil); err != nil || erased() != 1 || exists(target) {
		t.Fatalf("%v erased %d exists %v", err, erased(), exists(target))
	}
	if got := run.modes(); strings.Join(got, ",") != "check,delete" {
		t.Fatalf("runs %v", got)
	}
	// A symlink inside the root pointing outside is refused by resolution.
	link := filepath.Join(data, "link")
	os.Symlink(keys, link)
	base = link
	if err := p.rtRemoveData(ctx, cli, run, "unix:/x", data, rtHashA, nil); err == nil || erased() != 1 || !exists(filepath.Join(keys, "f")) {
		t.Fatalf("symlink: %v", err)
	}
	// Metacharacters in a path stay a path.
	odd := mk("data/it's $(touch " + filepath.Join(home, "pwned") + ") x")
	base = odd
	if err := p.rtRemoveData(ctx, cli, run, "unix:/x", data, rtHashA, nil); err != nil || exists(odd) || exists(filepath.Join(home, "pwned")) {
		t.Fatalf("metacharacters: %v", err)
	}
}

// TestRtRemoveDataOutlivesRequest: once the box approved the path, the
// erase and the deletion run under the manager's deadline; a request that
// ends meanwhile is told so, and the deletion still completes before the
// manager's Stop returns.
func TestRtRemoveDataOutlivesRequest(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	data := filepath.Join(home, "data")
	target := filepath.Join(data, "Alpha")
	os.MkdirAll(target, 0o755)
	os.WriteFile(filepath.Join(target, "f"), []byte("x"), 0o600)
	f := newFakeSCGI(t)
	f.reply = func(m, b string) string {
		if m == "d.base_path" {
			return xmlResp(xs(target))
		}
		return rtReplies(m, b)
	}
	p, _ := rtFixture(t, f)
	cli := rtorrent.New(f.dialer(""))
	run := &localRunner{home: home, gate: make(chan struct{}), block: true}
	p.dialDetached = func(ctx context.Context, ep string) (scriptRunner, rtorrent.Dialer, func(), error) {
		return run, f.dialer(ep), func() {}, nil
	}
	rctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.rtRemoveData(rctx, cli, run, "unix:/x", data, rtHashA, nil) }()
	// Wait for the delete run to be held, then end the request.
	for i := 0; i < 200; i++ {
		if m := run.modes(); len(m) == 2 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "continues on the box") {
		t.Fatalf("%v", err)
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatal("deleted before the gate opened")
	}
	close(run.gate)
	p.m.jobWG.Wait() // Stop waits the same way
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("the deletion must complete after the request ended")
	}
}

// TestRtRemoveDataOwnTransport: the destructive phase runs on a transport
// of its own, so closing the poller's connections (what an edit's
// stopAndWait, a reconnect or a probe failure do) between the box check
// and the deletion changes nothing: the erase and the rm complete on the
// detached transport (fred on PR 123).
func TestRtRemoveDataOwnTransport(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	data := filepath.Join(home, "data")
	target := filepath.Join(data, "Alpha")
	os.MkdirAll(target, 0o755)
	os.WriteFile(filepath.Join(target, "f"), []byte("x"), 0o600)
	f := newFakeSCGI(t)
	f.reply = func(m, b string) string {
		if m == "d.base_path" {
			return xmlResp(xs(target))
		}
		return rtReplies(m, b)
	}
	p, _ := rtFixture(t, f)
	pollerCli, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	check := &localRunner{home: home} // the poller's transport: used for the check only
	detached := &localRunner{home: home, gate: make(chan struct{}), block: true}
	var opened, closed int
	p.dialDetached = func(ctx context.Context, ep string) (scriptRunner, rtorrent.Dialer, func(), error) {
		opened++
		return detached, f.dialer(ep), func() { closed++ }, nil
	}
	done := make(chan error, 1)
	go func() { done <- p.rtRemoveData(context.Background(), pollerCli, check, "unix:/x", data, rtHashA, nil) }()
	for i := 0; i < 200 && len(detached.modes()) == 0; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	// An edit lands now: the poller's connections go away.
	p.closeConns()
	close(detached.gate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("deletion must complete on the detached transport")
	}
	if strings.Join(check.modes(), ",") != "check" || strings.Join(detached.modes(), ",") != "delete" || opened != 1 || closed != 1 {
		t.Fatalf("check %v detached %v opened %d closed %d", check.modes(), detached.modes(), opened, closed)
	}
	// A transport that cannot be opened changes nothing. (closeConns retired
	// the poller's client: the current one is resolved again.)
	pollerCli, _ = p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	p.dialDetached = func(ctx context.Context, ep string) (scriptRunner, rtorrent.Dialer, func(), error) {
		return nil, nil, nil, errors.New("ssh: dial failed")
	}
	os.MkdirAll(target, 0o755)
	n := 0
	for _, m := range f.called() {
		if m == "d.erase" {
			n++
		}
	}
	err := p.rtRemoveData(context.Background(), pollerCli, check, "unix:/x", data, rtHashA, nil)
	m := 0
	for _, x := range f.called() {
		if x == "d.erase" {
			m++
		}
	}
	if err == nil || !strings.Contains(err.Error(), "nothing was changed") || m != n {
		t.Fatalf("%v erases %d → %d", err, n, m)
	}
}

// TestStopDrainsDetachedDeletion (daybreak blue re-review of PR 123,
// finding 1): a deletion admitted before Stop keeps its context and
// completes before Stop returns; a job asked for after Stop began is
// refused and changes nothing.
func TestStopDrainsDetachedDeletion(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	data := filepath.Join(home, "data")
	target := filepath.Join(data, "Alpha")
	os.MkdirAll(target, 0o755)
	os.WriteFile(filepath.Join(target, "f"), []byte("x"), 0o600)
	f := newFakeSCGI(t)
	f.reply = func(m, b string) string {
		if m == "d.base_path" {
			return xmlResp(xs(target))
		}
		return rtReplies(m, b)
	}
	p, _ := rtFixture(t, f)
	cli := rtorrent.New(f.dialer(""))
	run := &localRunner{home: home, gate: make(chan struct{}), block: true}
	var seen context.Context
	var seenMu sync.Mutex
	p.dialDetached = func(ctx context.Context, ep string) (scriptRunner, rtorrent.Dialer, func(), error) {
		seenMu.Lock()
		seen = ctx
		seenMu.Unlock()
		return run, f.dialer(ep), func() {}, nil
	}
	done := make(chan error, 1)
	go func() { done <- p.rtRemoveData(context.Background(), cli, run, "unix:/x", data, rtHashA, nil) }()
	for i := 0; i < 300 && len(run.modes()) < 2; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	stopped := make(chan struct{})
	go func() { p.m.Stop(); close(stopped) }()
	select {
	case <-stopped:
		t.Fatal("Stop returned while a deletion was in flight")
	case <-time.After(300 * time.Millisecond):
	}
	seenMu.Lock()
	jobCtx := seen
	seenMu.Unlock()
	if jobCtx == nil || jobCtx.Err() != nil {
		t.Fatalf("the deletion's context was cancelled by Stop: %v", jobCtx)
	}
	close(run.gate)
	<-stopped
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the deletion must complete before Stop returns")
	}
	// After Stop, nothing destructive is admitted.
	os.MkdirAll(target, 0o755)
	err := p.rtRemoveData(context.Background(), cli, run, "unix:/x", data, rtHashA, nil)
	if !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("%v", err)
	}
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatal("a refused job must change nothing")
	}
}

// TestRmGetsFreshBudget (finding 2): the delete runner sees a deadline of
// (about) rtRemoveTimeout even after a slow connection and erase.
func TestRmGetsFreshBudget(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	data := filepath.Join(home, "data")
	target := filepath.Join(data, "Alpha")
	os.MkdirAll(target, 0o755)
	f := newFakeSCGI(t)
	f.reply = func(m, b string) string {
		switch m {
		case "d.base_path":
			return xmlResp(xs(target))
		case "d.erase":
			time.Sleep(500 * time.Millisecond) // a slow erase eats into the job budget, not the rm's
		}
		return rtReplies(m, b)
	}
	p, _ := rtFixture(t, f)
	cli := rtorrent.New(f.dialer(""))
	var remaining time.Duration
	run := &deadlineRunner{inner: &localRunner{home: home}, onDelete: func(ctx context.Context) {
		if dl, ok := ctx.Deadline(); ok {
			remaining = time.Until(dl)
		}
	}}
	p.dialDetached = func(ctx context.Context, ep string) (scriptRunner, rtorrent.Dialer, func(), error) {
		time.Sleep(500 * time.Millisecond) // a slow connection too
		return run, f.dialer(ep), func() {}, nil
	}
	if err := p.rtRemoveData(context.Background(), cli, run.inner, "unix:/x", data, rtHashA, nil); err != nil {
		t.Fatal(err)
	}
	if remaining < rtRemoveTimeout-2*time.Second || remaining > rtRemoveTimeout {
		t.Fatalf("rm budget %v, want about %v", remaining, rtRemoveTimeout)
	}
}

// deadlineRunner records the context the delete run sees.
type deadlineRunner struct {
	inner    *localRunner
	onDelete func(ctx context.Context)
}

func (d *deadlineRunner) Run(ctx context.Context, cmd string, stdin io.Reader) ([]byte, []byte, error) {
	script, _ := io.ReadAll(stdin)
	if strings.HasPrefix(string(script), "mode='delete'") {
		d.onDelete(ctx)
	}
	return d.inner.Run(ctx, cmd, bytes.NewReader(script))
}

// TestRtLeaseSerialisesMutations (finding 3): an add of the same box waits
// while a remove-with-data holds the lease, through its detached phase.
func TestRtLeaseSerialisesMutations(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	data := filepath.Join(home, "data")
	target := filepath.Join(data, "Alpha")
	os.MkdirAll(target, 0o755)
	f := newFakeSCGI(t)
	eraseGate := make(chan struct{})
	eraseEntered := make(chan struct{}, 1)
	f.reply = func(m, b string) string {
		switch m {
		case "d.base_path":
			return xmlResp(xs(target))
		case "d.erase":
			eraseEntered <- struct{}{}
			<-eraseGate
		}
		return rtReplies(m, b)
	}
	p, _ := rtFixture(t, f)
	readyRt(t, p, "unix:/home/u/.rtorrent.socket")
	// The state must carry the download root for remove-data.
	st := BoxState{ID: p.box.ID, Ready: true, Rt: RtState{Enabled: true, Endpoint: "unix:/home/u/.rtorrent.socket", Reachable: true, DataDir: data}}
	p.m.mu.Lock()
	p.m.states[p.box.ID] = &st
	p.m.mu.Unlock()
	run := &localRunner{home: home}
	p.dialDetached = func(ctx context.Context, ep string) (scriptRunner, rtorrent.Dialer, func(), error) {
		return run, f.dialer(ep), func() {}, nil
	}
	// Give the poller's client a transport-free path for the check: the
	// sshc is nil under the dial hook, so hand the check runner in by
	// running rtRemoveData directly with the lease taken as the action does.
	cli, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	release, err := p.m.rtLease(p.box.ID).Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- p.rtRemoveData(context.Background(), cli, run, "unix:/x", data, rtHashA, release) }()
	<-eraseEntered
	// An add now must wait for the removal, not interleave with it.
	addDone := make(chan error, 1)
	go func() {
		_, err := p.rtAddTorrents(context.Background(), qbt.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashA}})
		addDone <- err
	}()
	select {
	case err := <-addDone:
		t.Fatalf("add ran during the removal: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	close(eraseGate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := <-addDone; err != nil {
		t.Fatal(err)
	}
	methods := f.called()
	erase, load := -1, -1
	for i, m := range methods {
		if m == "d.erase" {
			erase = i
		}
		if m == "load.start" {
			load = i
		}
	}
	if erase < 0 || load < 0 || load < erase {
		t.Fatalf("order %v", methods)
	}
	// A waiter gives up with its context.
	release2, _ := p.m.rtLease(p.box.ID).Acquire(context.Background())
	cctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := p.rtTorrentAction(cctx, rtHashA, "stop"); err == nil || !strings.Contains(err.Error(), "another rTorrent change") {
		t.Fatalf("%v", err)
	}
	release2()
}

// TestRefusedJobReleasesLease (finding 5): a remove-with-data refused at
// admission (Stop began) gives the per-box lease back and answers
// ErrShuttingDown whatever the request context did.
func TestRefusedJobReleasesLease(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	data := filepath.Join(home, "data")
	target := filepath.Join(data, "Alpha")
	os.MkdirAll(target, 0o755)
	f := newFakeSCGI(t)
	f.reply = func(m, b string) string {
		if m == "d.base_path" {
			return xmlResp(xs(target))
		}
		return rtReplies(m, b)
	}
	p, _ := rtFixture(t, f)
	cli := rtorrent.New(f.dialer(""))
	run := &localRunner{home: home}
	p.m.jobMu.Lock()
	p.m.jobsClosed = true
	p.m.jobMu.Unlock()
	release, err := p.m.rtLease(p.box.ID).Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.rtRemoveData(context.Background(), cli, run, "unix:/x", data, rtHashA, release); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("%v", err)
	}
	lctx, lcancel := context.WithTimeout(context.Background(), time.Second)
	defer lcancel()
	release2, err := p.m.rtLease(p.box.ID).Acquire(lctx)
	if err != nil {
		t.Fatal("the lease leaked on a refused job")
	}
	release2()
	if _, statErr := os.Stat(target); statErr != nil {
		t.Fatal("a refused job must change nothing")
	}
}

func countCalls(f *fakeSCGI, method string) int {
	n := 0
	for _, m := range f.called() {
		if m == method {
			n++
		}
	}
	return n
}

// waitFor polls cond until it holds or the wait runs out.
func waitFor(t *testing.T, d time.Duration, cond func() bool, why func() string) {
	t.Helper()
	deadline := time.Now().Add(d)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("not reached in %v: %s", d, why())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// failingRunner runs the check like the box would and fails the delete.
type failingRunner struct{ inner *localRunner }

func (r *failingRunner) Run(ctx context.Context, cmd string, stdin io.Reader) ([]byte, []byte, error) {
	script, _ := io.ReadAll(stdin)
	if strings.HasPrefix(string(script), "mode='delete'") {
		r.inner.mu.Lock()
		r.inner.runs = append(r.inner.runs, "delete")
		r.inner.mu.Unlock()
		if r.inner.gate != nil {
			<-r.inner.gate
		}
		return nil, []byte("rm: disk says no"), errors.New("exit 1")
	}
	return r.inner.Run(ctx, cmd, bytes.NewReader(script))
}

// TestRtRemoveDataFailureReportedAfterRequest (Codex on PR 123): a deletion
// that fails after the request ended is not lost: the job logs it, records
// it on the box's rTorrent state (with the torrent's folder and hash) where
// the alert rule and the UI read it, polls carry it, and a dismiss clears it.
func TestRtRemoveDataFailureReportedAfterRequest(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	data := filepath.Join(home, "data")
	target := filepath.Join(data, "Alpha")
	os.MkdirAll(target, 0o755)
	f := newFakeSCGI(t)
	f.reply = func(m, b string) string {
		if m == "d.base_path" {
			return xmlResp(xs(target))
		}
		return rtReplies(m, b)
	}
	p, snap := rtFixture(t, f)
	p.m.mu.Lock()
	p.m.pollers[p.box.ID] = p
	p.m.mu.Unlock()
	readyRt(t, p, "unix:/x")
	cli := rtorrent.New(f.dialer(""))
	check := &localRunner{home: home}
	run := &failingRunner{inner: &localRunner{home: home, gate: make(chan struct{}), block: true}}
	p.dialDetached = func(ctx context.Context, ep string) (scriptRunner, rtorrent.Dialer, func(), error) {
		return run, f.dialer(ep), func() {}, nil
	}
	rctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- p.rtRemoveData(rctx, cli, check, "unix:/x", data, rtHashA, nil) }()
	// The delete is held; the request ends; then the delete fails.
	waitFor(t, 3*time.Second, func() bool { return len(run.inner.modes()) == 1 }, func() string { return fmt.Sprint(run.inner.modes()) })
	cancel()
	err := <-done
	if err == nil || !strings.Contains(err.Error(), "raises an alert") {
		t.Fatalf("%v", err)
	}
	close(run.inner.gate)
	p.m.jobWG.Wait()
	p.m.mu.RLock()
	st := *p.m.states[p.box.ID]
	p.m.mu.RUnlock()
	if !strings.Contains(st.Rt.RemoveFailed, "Alpha (43519d14…)") || !strings.Contains(st.Rt.RemoveFailed, "disk says no") || st.Rt.RemoveFailedAt.IsZero() {
		t.Fatalf("failure not recorded: %q", st.Rt.RemoveFailed)
	}
	// A poll keeps it.
	p.pollRt(context.Background(), &st, snap)
	if st.Rt.RemoveFailed == "" || st.Rt.Error != "" {
		t.Fatalf("poll dropped the record: %+v", st.Rt)
	}
	// A poll whose starting snapshot predates the record publishes with the
	// stored (latest) value, and one that predates a dismiss does not
	// resurrect it (Codex on PR 123).
	stale := BoxState{ID: p.box.ID, Rt: RtState{Enabled: true}}
	p.storeFix(time.Now(), false)(&stale, &st)
	if stale.Rt.RemoveFailed != st.Rt.RemoveFailed || !stale.Rt.RemoveFailedAt.Equal(st.Rt.RemoveFailedAt) {
		t.Fatalf("a poll in flight must carry the stored record: %q", stale.Rt.RemoveFailed)
	}
	dismissed := st
	dismissed.Rt.RemoveFailed, dismissed.Rt.RemoveFailedAt = "", time.Time{}
	p.storeFix(time.Now(), false)(&st, &dismissed)
	if st.Rt.RemoveFailed != "" {
		t.Fatal("a poll in flight must not resurrect a dismissed record")
	}
	// A failure reported for another generation (the record was moved to
	// another machine while the job ran) is logged but not attached, in
	// memory or durably.
	p.reportRtRemove("other-generation", rtHashA, target, errors.New("boom"))
	p.m.mu.RLock()
	gotMsg := p.m.states[p.box.ID].Rt.RemoveFailed
	p.m.mu.RUnlock()
	if msg, _, ok := rowNotice(p.m, p.box); strings.Contains(gotMsg, "boom") || !ok || strings.Contains(msg, "boom") {
		t.Fatalf("another generation's failure must not be attached: %q / %q", gotMsg, msg)
	}
	// A restart: the state cache was written by the poll before the merge
	// (or never, during Stop's drain), yet the notice is restored from its
	// own row, for this generation only (fred on PR 123).
	stale.Rt.RemoveFailed = ""
	if err := p.m.st.SetCache(context.Background(), p.box.ID, "state", stale.Summary()); err != nil {
		t.Fatal(err)
	}
	m2 := New(p.m.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	var cached BoxState
	if _, err := m2.st.GetCache(context.Background(), p.box.ID, "state", &cached); err != nil || cached.Rt.RemoveFailed != "" {
		t.Fatalf("fixture: the state cache should carry no notice: %v %q", err, cached.Rt.RemoveFailed)
	}
	m2.applyRtRemove(p.box, &cached, false, "", time.Time{})
	if !strings.Contains(cached.Rt.RemoveFailed, "disk says no") || cached.Rt.RemoveFailedAt.IsZero() {
		t.Fatalf("the notice must survive a restart: %q", cached.Rt.RemoveFailed)
	}
	moved := *p.box
	moved.Host = "elsewhere.example"
	if _, _, ok := rowNotice(m2, &moved); ok {
		t.Fatal("another generation must not restore the notice")
	}
	// Dismiss, then a restart without any poll in between: the stale state
	// cache (which still carries the notice) does not resurrect it.
	p.m.mu.RLock()
	live := *p.m.states[p.box.ID] // still carries the notice
	p.m.mu.RUnlock()
	if err := p.m.st.SetCache(context.Background(), p.box.ID, "state", live.Summary()); err != nil {
		t.Fatal(err)
	}
	if err := p.m.DismissRtRemoveFailure(p.box.ID, live.Rt.RemoveFailedAt); err != nil {
		t.Fatal(err)
	}
	cached = BoxState{}
	if _, err := m2.st.GetCache(context.Background(), p.box.ID, "state", &cached); err != nil || cached.Rt.RemoveFailed == "" {
		t.Fatalf("fixture: the state cache should still carry the notice: %v", err)
	}
	m2.applyRtRemove(p.box, &cached, false, "", time.Time{})
	if cached.Rt.RemoveFailed != "" || !cached.Rt.RemoveFailedAt.IsZero() {
		t.Fatalf("a dismissed notice came back after a restart: %q", cached.Rt.RemoveFailed)
	}
	p.m.mu.RLock()
	cleared := p.m.states[p.box.ID].Rt.RemoveFailed == "" && p.m.states[p.box.ID].Rt.RemoveFailedAt.IsZero()
	p.m.mu.RUnlock()
	if !cleared {
		t.Fatal("dismiss must clear the record")
	}
	// A dismiss names the notice it saw; a newer notice recorded since is
	// left alone and the dismiss refused (daybreak blue re-review 5 of PR
	// 123): report A, report B over it, dismiss A → stale, B stays;
	// dismiss B → gone.
	p.reportRtRemove(p.generation(), rtHashA, target, errors.New("first failure"))
	p.m.mu.RLock()
	seenA := p.m.states[p.box.ID].Rt.RemoveFailedAt
	p.m.mu.RUnlock()
	time.Sleep(2 * time.Millisecond)
	p.reportRtRemove(p.generation(), rtHashB, filepath.Join(data, "Beta"), errors.New("second failure"))
	p.m.mu.RLock()
	seenB, msgB := p.m.states[p.box.ID].Rt.RemoveFailedAt, p.m.states[p.box.ID].Rt.RemoveFailed
	p.m.mu.RUnlock()
	if !strings.Contains(msgB, "Beta") || !seenB.After(seenA) {
		t.Fatalf("fixture: %q %v %v", msgB, seenA, seenB)
	}
	if err := p.m.DismissRtRemoveFailure(p.box.ID, seenA); !errors.Is(err, ErrStaleNotice) {
		t.Fatalf("dismissing the old notice: %v", err)
	}
	p.m.mu.RLock()
	still := p.m.states[p.box.ID].Rt.RemoveFailed
	p.m.mu.RUnlock()
	if msgR, _, ok := rowNotice(p.m, p.box); still != msgB || !ok || !strings.Contains(msgR, "Beta") {
		t.Fatalf("the newer notice must survive a stale dismiss: %q / %q", still, msgR)
	}
	if err := p.m.DismissRtRemoveFailure(p.box.ID, seenB); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := rowNotice(p.m, p.box); ok {
		t.Fatal("dismissed")
	}
	if err := p.m.DismissRtRemoveFailure(999, seenB); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("%v", err)
	}
}

// TestStartPollerPublishesNoticeUnderGuard (Codex on PR 123): a replacement
// poller reads the durable removal notice and publishes its state under the
// box's per-box guard, so a report or dismiss cannot land between the read
// and the publish and be overwritten by the older value.
func TestStartPollerPublishesNoticeUnderGuard(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	m := p.m
	m.ctx, m.cancel = context.WithCancel(context.Background())
	defer m.cancel()
	box := *p.box
	box.Host, box.Enabled = "box.example", false // disabled: no poll loop starts
	if err := m.st.CreateBox(context.Background(), &box); err != nil {
		t.Fatal(err)
	}
	g := m.noticeGuard(box.ID)
	g.Lock() // a report in progress
	started := make(chan struct{})
	go func() { m.startPoller(&box, nil); close(started) }()
	select {
	case <-started:
		t.Fatal("startPoller published without the guard")
	case <-time.After(150 * time.Millisecond):
	}
	// The report lands (row, then live state is absent yet) before the guard
	// is given up: the publication must carry it.
	if err := m.st.SetCache(context.Background(), box.ID, rtRemoveCacheKind, rtRemoveCache{Generation: boxGeneration(&box), Message: "Alpha: boom", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	g.Unlock()
	<-started
	m.mu.RLock()
	got := m.states[box.ID].Rt.RemoveFailed
	m.mu.RUnlock()
	if got != "Alpha: boom" {
		t.Fatalf("published notice %q", got)
	}
}

// TestStaleDismissWithDirtyNoticeRow (daybreak blue re-review 6 of PR 123,
// finding 4): notice B's row write fails, so the row still holds A while B
// is live; a stale dismiss of A must not clear B, and a dismiss of B, the
// notice the user sees, clears it although the row disagrees.
func TestStaleDismissWithDirtyNoticeRow(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	m := p.m
	readyRt(t, p, "unix:/x")
	p.reportRtRemove(p.generation(), rtHashA, "/home/u/data/Alpha", errors.New("first"))
	m.mu.RLock()
	seenA := m.states[p.box.ID].Rt.RemoveFailedAt
	m.mu.RUnlock()
	m.cacheFault = func(kind string) error {
		if kind == rtRemoveCacheKind {
			return errors.New("disk full")
		}
		return nil
	}
	time.Sleep(2 * time.Millisecond)
	p.reportRtRemove(p.generation(), rtHashB, "/home/u/data/Beta", errors.New("second"))
	m.cacheFault = nil
	m.mu.RLock()
	seenB, msgB := m.states[p.box.ID].Rt.RemoveFailedAt, m.states[p.box.ID].Rt.RemoveFailed
	m.mu.RUnlock()
	if !strings.Contains(msgB, "Beta") || !seenB.After(seenA) {
		t.Fatalf("fixture: %q", msgB)
	}
	if msgRow, _, ok := rowNotice(m, p.box); !ok || !strings.Contains(msgRow, "Alpha") {
		t.Fatalf("fixture: the row should still hold A: %q %v", msgRow, ok)
	}
	if err := m.DismissRtRemoveFailure(p.box.ID, seenA); !errors.Is(err, ErrStaleNotice) {
		t.Fatalf("a stale dismiss over a dirty row: %v", err)
	}
	m.mu.RLock()
	still := m.states[p.box.ID].Rt.RemoveFailed
	m.mu.RUnlock()
	if still != msgB {
		t.Fatalf("B must survive: %q", still)
	}
	if err := m.DismissRtRemoveFailure(p.box.ID, seenB); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	cleared := m.states[p.box.ID].Rt.RemoveFailed == ""
	m.mu.RUnlock()
	if _, _, ok := rowNotice(m, p.box); !cleared || ok {
		t.Fatal("dismissing the live notice clears it and the dirty row")
	}
}

// TestRtClientReplacementRetiresOld: replacing the client, for a new
// endpoint or transport, is a terminal handoff like a teardown. The
// replacement waits for the old client's operation in flight and the old
// pointer is dead: an operation on it fails with ErrRetired, and a caller
// going through the poller resolves the current client and succeeds.
func TestRtClientReplacementRetiresOld(t *testing.T) {
	f := newFakeSCGI(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	f.reply = func(m, b string) string {
		if m == "d.custom1.set" {
			entered <- struct{}{}
			<-gate
		}
		return rtReplies(m, b)
	}
	p, _ := rtFixture(t, f)
	a := &sshx.Client{}
	c1, _ := p.rtClient(context.Background(), a, "unix:/x")
	addDone := make(chan error, 1)
	go func() {
		_, err := c1.Add(context.Background(), rtorrent.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashA}, Label: "tv"})
		addDone <- err
	}()
	<-entered
	replaced := make(chan *rtorrent.Client, 1)
	go func() { c, _ := p.rtClient(context.Background(), a, "unix:/y"); replaced <- c }()
	select {
	case <-replaced:
		t.Fatal("the replacement did not wait for the operation in flight")
	case <-time.After(150 * time.Millisecond):
	}
	close(gate)
	if err := <-addDone; err != nil {
		t.Fatal(err)
	}
	c2 := <-replaced
	if c2 == c1 || !c1.Retired() {
		t.Fatalf("retired %v", c1.Retired())
	}
	if err := c1.Action(context.Background(), rtorrent.ActionStop, rtHashA); !errors.Is(err, rtorrent.ErrRetired) {
		t.Fatalf("old pointer: %v", err)
	}
	readyRt(t, p, "unix:/y")
	if err := p.onRetired(context.Background(), c1, func(c *rtorrent.Client) error { return c.Action(context.Background(), rtorrent.ActionStop, rtHashA) }); err != nil {
		t.Fatalf("retry on the current client: %v", err)
	}
	c3, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/y")
	if c3 == c2 || !c2.Retired() {
		t.Fatal("transport change must retire the old client")
	}
}

// TestRemoveBoxWaitsForMutationLease (daybreak blue re-review of PR 123):
// a box removal takes the box's rTorrent mutation lease first, so it cannot
// unpublish the box under an admitted remove-with-data, whose failure report
// would then have no state to land on; a lease held past the request's
// deadline fails the request and leaves the box; once free, the removal
// proceeds.
func TestRemoveBoxWaitsForMutationLease(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	m := p.m
	m.ctx, m.cancel = context.WithCancel(context.Background())
	defer m.cancel()
	m.mu.Lock()
	m.states[p.box.ID] = &BoxState{ID: p.box.ID}
	m.mu.Unlock()
	release, err := m.rtLease(p.box.ID).Acquire(context.Background()) // a remove-with-data in flight
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if err := m.RemoveBox(ctx, p.box.ID); err == nil || !strings.Contains(err.Error(), "cannot be removed right now") {
		t.Fatalf("removal over a held lease: %v", err)
	}
	if m.poller(p.box.ID) == nil {
		t.Fatal("the box must stay while the lease is held")
	}
	release()
	if err := m.RemoveBox(context.Background(), p.box.ID); err != nil {
		t.Fatal(err)
	}
	if m.poller(p.box.ID) != nil {
		t.Fatal("removed")
	}
}

// TestEditKeepsNewerLiveNotice (Codex on PR 123): a removal notice whose
// row write failed is live but older on disk (or absent there); an edit
// carrying the live state keeps the newer live notice, judged by the box's
// live notice as it is under the guard, not by the edit's earlier snapshot,
// so a dismiss in between is not undone; a restart, with no live state,
// takes the row.
func TestEditKeepsNewerLiveNotice(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	m := p.m
	readyRt(t, p, "unix:/x")
	p.reportRtRemove(p.generation(), rtHashA, "/home/u/data/Alpha", errors.New("first"))
	m.cacheFault = func(kind string) error {
		if kind == rtRemoveCacheKind {
			return errors.New("disk full")
		}
		return nil
	}
	time.Sleep(2 * time.Millisecond)
	p.reportRtRemove(p.generation(), rtHashB, "/home/u/data/Beta", errors.New("second"))
	m.cacheFault = nil
	m.mu.RLock()
	live := *m.states[p.box.ID]
	m.mu.RUnlock()
	if !strings.Contains(live.Rt.RemoveFailed, "Beta") {
		t.Fatalf("fixture: %q", live.Rt.RemoveFailed)
	}
	// An edit: the carried snapshot and the current live notice agree on
	// Beta; the row still says Alpha. Beta stays.
	carried := live
	m.applyRtRemove(p.box, &carried, true, live.Rt.RemoveFailed, live.Rt.RemoveFailedAt)
	if !strings.Contains(carried.Rt.RemoveFailed, "Beta") {
		t.Fatalf("an edit must keep the newer live notice: %q", carried.Rt.RemoveFailed)
	}
	// No row at all: the live notice still stays across an edit.
	if err := m.st.DeleteCache(context.Background(), p.box.ID, rtRemoveCacheKind); err != nil {
		t.Fatal(err)
	}
	carried = live
	m.applyRtRemove(p.box, &carried, true, live.Rt.RemoveFailed, live.Rt.RemoveFailedAt)
	if !strings.Contains(carried.Rt.RemoveFailed, "Beta") {
		t.Fatalf("an edit must keep a live notice that has no row: %q", carried.Rt.RemoveFailed)
	}
	// A dismiss landed after the edit took its snapshot: the current live
	// notice is empty and the row gone; the snapshot's Beta must not come
	// back (Codex on PR 123).
	carried = live
	m.applyRtRemove(p.box, &carried, true, "", time.Time{})
	if carried.Rt.RemoveFailed != "" {
		t.Fatalf("a dismiss between the snapshot and the publish must hold: %q", carried.Rt.RemoveFailed)
	}
	// A restart (no live state): the row decides, here nothing.
	restored := live
	m.applyRtRemove(p.box, &restored, false, "", time.Time{})
	if restored.Rt.RemoveFailed != "" {
		t.Fatalf("a restart takes the row: %q", restored.Rt.RemoveFailed)
	}
}

// rowNotice reads the durable removal notice: present only when readable
// and set.
func rowNotice(m *Manager, b *store.Box) (string, time.Time, bool) {
	msg, at, err := m.restoredRtRemove(b)
	return msg, at, err == nil && msg != ""
}

// Daybreak blue re-review of PR 123: a removal of an unknown box allocates
// no lease and answers not found; an unreadable notice row on a start keeps
// what the state carried or says the record is unreadable, never clears it;
// a notice's time strictly increases per box even when the clock does not.
func TestRemovalNoticeEdges(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	m := p.m
	m.leaseMu.Lock()
	before := len(m.rtLeases)
	m.leaseMu.Unlock()
	for i := int64(1000); i < 1100; i++ {
		if err := m.RemoveBox(context.Background(), i); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%d: %v", i, err)
		}
	}
	m.leaseMu.Lock()
	after := len(m.rtLeases)
	m.leaseMu.Unlock()
	if after != before {
		t.Fatalf("unknown ids grew the lease registry: %d -> %d", before, after)
	}
	// An unreadable row (bad JSON) on a start: not "absent".
	if err := m.st.SetCache(context.Background(), p.box.ID, rtRemoveCacheKind, "not an object"); err != nil {
		t.Fatal(err)
	}
	carried := BoxState{}
	carried.Rt.RemoveFailed, carried.Rt.RemoveFailedAt = "Alpha: earlier", time.Now()
	m.applyRtRemove(p.box, &carried, false, "", time.Time{})
	if carried.Rt.RemoveFailed != "Alpha: earlier" {
		t.Fatalf("an unreadable row must keep what the state carried: %q", carried.Rt.RemoveFailed)
	}
	empty := BoxState{}
	m.applyRtRemove(p.box, &empty, false, "", time.Time{})
	if !strings.Contains(empty.Rt.RemoveFailed, "could not be read") || empty.Rt.RemoveFailedAt.IsZero() {
		t.Fatalf("an unreadable row with nothing carried must say so: %q", empty.Rt.RemoveFailed)
	}
	if err := m.st.DeleteCache(context.Background(), p.box.ID, rtRemoveCacheKind); err != nil {
		t.Fatal(err)
	}
	// Strictly increasing times: a live notice dated in the future (a clock
	// stepped back since) is still followed by a later one.
	readyRt(t, p, "unix:/x")
	future := time.Now().Add(time.Hour).Round(0) // wall time only, as a stepped-back clock leaves it
	m.mu.Lock()
	m.states[p.box.ID].Rt.RemoveFailed, m.states[p.box.ID].Rt.RemoveFailedAt = "old", future
	m.mu.Unlock()
	p.reportRtRemove(p.generation(), rtHashA, "/home/u/data/Alpha", errors.New("x"))
	m.mu.RLock()
	got := m.states[p.box.ID].Rt.RemoveFailedAt
	m.mu.RUnlock()
	if !got.After(future) {
		t.Fatalf("the new notice must sort after the live one: %v vs %v", got, future)
	}
	_, rowAt, ok := rowNotice(m, p.box)
	if !ok || !rowAt.Equal(got) {
		t.Fatalf("row and live share the new time: %v %v %v", ok, rowAt, got)
	}
}

// Daybreak blue re-review of PR 123: a box whose row delete failed is no
// longer live but can be removed again (not a 404 until a restart); a
// removal of an id the store does not know is a 404 from the store too.
func TestRemoveBoxRetryAfterFailedDelete(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	m := p.m
	m.ctx, m.cancel = context.WithCancel(context.Background())
	defer m.cancel()
	// What a removal whose row delete failed leaves: no poller, the row.
	m.mu.Lock()
	delete(m.pollers, p.box.ID)
	m.mu.Unlock()
	if err := m.RemoveBox(context.Background(), p.box.ID); err != nil {
		t.Fatalf("a retry must reach the store: %v", err)
	}
	if _, err := m.st.GetBox(context.Background(), p.box.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the row must be gone: %v", err)
	}
	if err := m.RemoveBox(context.Background(), p.box.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("then not found: %v", err)
	}
	if err := m.st.DeleteBox(context.Background(), 4242); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the store reports a delete of nothing: %v", err)
	}
}

// Daybreak blue re-review of PR 123: a dismiss never erases a row newer
// than the notice dismissed (here the shown notice was the stand-in for an
// unreadable row); the newer row becomes the live notice.
func TestDismissKeepsNewerRow(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	m := p.m
	readyRt(t, p, "unix:/x")
	shown := time.Now()
	m.mu.Lock()
	m.states[p.box.ID].Rt.RemoveFailed, m.states[p.box.ID].Rt.RemoveFailedAt = "stand-in", shown
	m.mu.Unlock()
	newer := shown.Add(time.Second)
	if err := m.st.SetCache(context.Background(), p.box.ID, rtRemoveCacheKind, rtRemoveCache{Generation: boxGeneration(p.box), Message: "Gamma: real failure", At: newer}); err != nil {
		t.Fatal(err)
	}
	if err := m.DismissRtRemoveFailure(p.box.ID, shown); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	live := m.states[p.box.ID].Rt
	m.mu.RUnlock()
	if live.RemoveFailed != "Gamma: real failure" || !live.RemoveFailedAt.Equal(newer) {
		t.Fatalf("the newer row must become the live notice: %+v", live)
	}
	if msg, _, ok := rowNotice(m, p.box); !ok || msg != "Gamma: real failure" {
		t.Fatalf("the newer row must survive: %q %v", msg, ok)
	}
	if err := m.DismissRtRemoveFailure(p.box.ID, newer); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := rowNotice(m, p.box); ok {
		t.Fatal("dismissing the newer notice clears it")
	}
}

// fred on PR 123: the stand-in for an unreadable record observed nothing;
// a durable outcome older than the restart that showed the stand-in is
// surfaced by its dismiss, never deleted unseen.
func TestStandInNeverDeletesOlderRow(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	m := p.m
	readyRt(t, p, "unix:/x")
	older := time.Now().Add(-time.Minute).Round(0)
	if err := m.st.SetCache(context.Background(), p.box.ID, rtRemoveCacheKind, "unreadable"); err != nil {
		t.Fatal(err)
	}
	st := BoxState{}
	m.applyRtRemove(p.box, &st, false, "", time.Time{})
	if !st.Rt.RemoveFailedAt.Equal(rtStandInAt) {
		t.Fatalf("stand-in date: %v", st.Rt.RemoveFailedAt)
	}
	m.mu.Lock()
	m.states[p.box.ID].Rt.RemoveFailed, m.states[p.box.ID].Rt.RemoveFailedAt = st.Rt.RemoveFailed, st.Rt.RemoveFailedAt
	m.mu.Unlock()
	// The read recovers: the real row, older than the restart.
	if err := m.st.SetCache(context.Background(), p.box.ID, rtRemoveCacheKind, rtRemoveCache{Generation: boxGeneration(p.box), Message: "Beta: real", At: older}); err != nil {
		t.Fatal(err)
	}
	if err := m.DismissRtRemoveFailure(p.box.ID, rtStandInAt); err != nil {
		t.Fatal(err)
	}
	m.mu.RLock()
	live := m.states[p.box.ID].Rt
	m.mu.RUnlock()
	if live.RemoveFailed != "Beta: real" {
		t.Fatalf("the recovered row must be shown, not deleted: %+v", live)
	}
	if _, _, ok := rowNotice(m, p.box); !ok {
		t.Fatal("the row must survive the stand-in's dismiss")
	}
}

// Daybreak blue re-review of PR 123: a dismiss whose record cannot be read
// back changes nothing and fails.
func TestDismissFailsClosedOnUnreadableRow(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	m := p.m
	readyRt(t, p, "unix:/x")
	shown := time.Now().Round(0)
	m.mu.Lock()
	m.states[p.box.ID].Rt.RemoveFailed, m.states[p.box.ID].Rt.RemoveFailedAt = "Alpha: failed", shown
	m.mu.Unlock()
	if err := m.st.SetCache(context.Background(), p.box.ID, rtRemoveCacheKind, "unreadable"); err != nil {
		t.Fatal(err)
	}
	if err := m.DismissRtRemoveFailure(p.box.ID, shown); err == nil {
		t.Fatal("a dismiss over an unreadable row must fail")
	}
	m.mu.RLock()
	still := m.states[p.box.ID].Rt.RemoveFailed
	m.mu.RUnlock()
	if still != "Alpha: failed" {
		t.Fatalf("the live notice must stay: %q", still)
	}
}
