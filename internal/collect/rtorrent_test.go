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
	"sync/atomic"
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
			return xmlRow(xs(hash), xs(name), i8(mustInt(size)), i8(mustInt(done)), i8(mustInt(down)), i8(mustInt(up)), i8(mustInt(ratio)), i8(mustInt(state)), i8(mustInt(complete)), i8(mustInt(active)), i8(0), i8(mustInt(state)), xs(msg), xs(label), i8(0), i8(48), i8(40), i8(1000), i8(2000), i8(0), i8(0))
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
	// The box exists in the store (cache rows hang off it: labels, erase
	// tombstones, removal notices) and the poller is the box's current one
	// (the only one that persists labels).
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

func TestRtPendingLabelsSurviveTransportAndRestart(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	// Labels restored from the cache reach the first client; a client
	// rebuilt for a new transport inherits what is still owed.
	owed := []rtorrent.PendingLabel{{Hash: rtHashA, Label: "tv", Until: time.Now().Add(time.Hour)}}
	p.rtLabels = owed
	c1, err := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	if err != nil || len(c1.Pending()) != 1 || p.rtLabels != nil {
		t.Fatalf("%v %d", err, len(c1.Pending()))
	}
	c2, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	if c2 == c1 || len(c2.Pending()) != 1 {
		t.Fatal("rebuilt client must inherit the owed labels")
	}
	// A reconnect (closeConns, conn()'s teardown, the probe-failure path)
	// drops the client but not what it owes: the next client has it (Codex
	// on PR 123).
	p.closeConns()
	if p.rt != nil || len(p.rtLabels) != 1 {
		t.Fatalf("teardown must keep the owed labels: %+v", p.rtLabels)
	}
	c3, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	if len(c3.Pending()) != 1 || p.rtLabels != nil {
		t.Fatal("client after a reconnect must inherit the owed labels")
	}
	c2 = c3
	// record persists them for this generation; a restarted manager finds
	// them for the same record and not for a moved one. (box_cache rows
	// belong to a stored box, so the record is created first.)
	p.recordRtLabels(context.Background())
	got := p.m.restoredRtLabels(p.box)
	if len(got) != 1 || got[0].Hash != rtHashA || got[0].Label != "tv" {
		t.Fatalf("%+v", got)
	}
	moved := *p.box
	moved.Host = "elsewhere.example"
	if p.m.restoredRtLabels(&moved) != nil {
		t.Fatal("another generation must not get the labels")
	}
	// While the client is down (reconnect pending), the labels saved by the
	// teardown are what gets persisted, not nothing (Codex on PR 123).
	p.closeConns()
	p.m.st.DeleteCache(context.Background(), p.box.ID, rtLabelsCacheKind)
	p.recordRtLabels(context.Background())
	if got := p.m.restoredRtLabels(p.box); len(got) != 1 {
		t.Fatalf("saved labels not persisted while the client is down: %+v", got)
	}
	c2, _ = p.rtClient(context.Background(), &sshx.Client{}, "unix:/x") // takes them back
	// Once settled, the cache holds an empty list, not the old entry.
	c2.Restore(nil)
	p.mu.Lock()
	p.rt = rtorrent.New(f.dialer(""))
	p.mu.Unlock()
	p.recordRtLabels(context.Background())
	if got := p.m.restoredRtLabels(p.box); len(got) != 0 {
		t.Fatalf("settled labels came back: %+v", got)
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

func TestRtLabelsHandedOverOnEdit(t *testing.T) {
	// A labelled magnet added just before an edit: the retiring poller's
	// owed label is written to the cache by UpdateBox so the replacement
	// poller restores it (Codex on PR 123).
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	c, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	c.Restore([]rtorrent.PendingLabel{{Hash: rtHashA, Label: "tv", Until: time.Now().Add(time.Hour)}})
	// What UpdateBox does after stopAndWait: the client is torn down and the
	// owed labels recorded for the unchanged record.
	p.closeConns()
	p.recordRtLabels(nil)
	got := p.m.restoredRtLabels(p.box)
	if len(got) != 1 || got[0].Label != "tv" {
		t.Fatalf("%+v", got)
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
// detached transport (fred on PR 123). The poller's client also forgets
// the label it owed the removed torrent.
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
	pollerCli.Restore([]rtorrent.PendingLabel{{Hash: rtHashA, Label: "tv", Until: time.Now().Add(time.Hour)}})
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
	if len(pollerCli.Pending()) != 0 {
		t.Fatal("the poller's client must forget the removed torrent's label")
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

// TestRtRemoveDataForgetsLabelOnCurrentOwner: the erase is paused, the
// poller is torn down and replaced (its owed labels copied, persisted and
// restored into the replacement), the erase completes; re-adding the hash
// unlabelled on the replacement must not inherit the old label (fred on
// PR 123).
func TestRtRemoveDataForgetsLabelOnCurrentOwner(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	data := filepath.Join(home, "data")
	target := filepath.Join(data, "Alpha")
	os.MkdirAll(target, 0o755)
	os.WriteFile(filepath.Join(target, "f"), []byte("x"), 0o600)
	f := newFakeSCGI(t)
	eraseGate := make(chan struct{})
	eraseEntered := make(chan struct{}, 1)
	var mu sync.Mutex
	sets := 0
	f.reply = func(m, b string) string {
		switch m {
		case "d.base_path":
			return xmlResp(xs(target))
		case "d.erase":
			eraseEntered <- struct{}{}
			<-eraseGate
		case "d.custom1.set":
			mu.Lock()
			sets++
			mu.Unlock()
		case "d.multicall2":
			return xmlResp(`<array><data>` + xmlRow(xs(rtHashA), xs("Alpha"), i8(1000), i8(0), i8(0), i8(0), i8(0), i8(1), i8(0), i8(1), i8(0), i8(1), xs(""), xs(""), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0)) + `</data></array>`)
		}
		return rtReplies(m, b)
	}
	p, _ := rtFixture(t, f)
	p.m.mu.Lock()
	p.m.pollers[p.box.ID] = p
	p.m.mu.Unlock()
	cli, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	cli.Restore([]rtorrent.PendingLabel{{Hash: rtHashA, Label: "old", Until: time.Now().Add(time.Hour)}})
	run := &localRunner{home: home}
	p.dialDetached = func(ctx context.Context, ep string) (scriptRunner, rtorrent.Dialer, func(), error) {
		return run, f.dialer(ep), func() {}, nil
	}
	done := make(chan error, 1)
	go func() { done <- p.rtRemoveData(context.Background(), cli, run, "unix:/x", data, rtHashA, nil) }()
	<-eraseEntered // the detached erase is in flight
	// An edit: the poller is torn down (its owed labels saved), recorded,
	// and replaced by a poller that restores them from the cache.
	p.closeConns()
	p.recordRtLabels(context.Background())
	box := *p.box
	p2 := newPoller(p.m, &box)
	p2.dialRt = f.dialer
	// The erase intent was recorded before the erase went out, so the flush
	// already kept the label out of the cache (daybreak blue re-review 5 of
	// PR 123); the replacement starts without it either way.
	p2.rtLabels = p.m.restoredRtLabels(&box)
	p.m.mu.Lock()
	p.m.pollers[box.ID] = p2
	p.m.mu.Unlock()
	close(eraseGate)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	// Nothing owed anywhere: the replacement's copy, its client, the cache.
	p2.mu.Lock()
	n := len(p2.rtLabels)
	p2.mu.Unlock()
	if n != 0 {
		t.Fatalf("replacement still owes %d", n)
	}
	c2, _ := p2.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	if len(c2.Pending()) != 0 || len(p.m.restoredRtLabels(&box)) != 0 {
		t.Fatalf("client %d cache %d", len(c2.Pending()), len(p.m.restoredRtLabels(&box)))
	}
	// Re-adding the hash unlabelled through the replacement stays unlabelled.
	if _, err := c2.Add(context.Background(), rtorrent.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashA}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Torrents(context.Background()); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if sets != 0 {
		t.Fatalf("the re-added torrent inherited a label: %d setter calls", sets)
	}
}

// TestRtRemoveDataCleanupSerialisedWithEdit: the cleanup after the erase
// waits for the edit lock, so it cannot land between an edit's restore of
// the owed labels and its publish of the replacement poller; and it only
// touches a replacement of the same generation (fred on PR 123).
func TestRtRemoveDataCleanupSerialisedWithEdit(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	data := filepath.Join(home, "data")
	target := filepath.Join(data, "Alpha")
	os.MkdirAll(target, 0o755)
	os.WriteFile(filepath.Join(target, "f"), []byte("x"), 0o600)
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
	m := p.m
	m.mu.Lock()
	m.pollers[p.box.ID] = p
	m.mu.Unlock()
	cli, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	cli.Restore([]rtorrent.PendingLabel{{Hash: rtHashA, Label: "old", Until: time.Now().Add(time.Hour)}})
	run := &localRunner{home: home}
	p.dialDetached = func(ctx context.Context, ep string) (scriptRunner, rtorrent.Dialer, func(), error) {
		return run, f.dialer(ep), func() {}, nil
	}
	done := make(chan error, 1)
	go func() { done <- p.rtRemoveData(context.Background(), cli, run, "unix:/x", data, rtHashA, nil) }()
	<-eraseEntered
	// The edit transaction begins and holds the lock: the retiring poller
	// is stopped and recorded, the replacement built with the restored
	// labels, but not yet published.
	m.boxMu.Lock()
	p.closeConns()
	p.recordRtLabels(context.Background())
	box := *p.box
	p2 := newPoller(m, &box)
	p2.dialRt = f.dialer
	p2.rtLabels = m.restoredRtLabels(&box)
	// The erase completes now; the cleanup must wait for the lock.
	close(eraseGate)
	select {
	case err := <-done:
		t.Fatalf("cleanup ran during the edit transaction: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	// The edit publishes the replacement and releases the lock.
	m.mu.Lock()
	m.pollers[box.ID] = p2
	m.mu.Unlock()
	m.boxMu.Unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	p2.mu.Lock()
	n := len(p2.rtLabels)
	p2.mu.Unlock()
	if n != 0 || len(m.restoredRtLabels(&box)) != 0 {
		t.Fatalf("replacement owes %d, cache %d", n, len(m.restoredRtLabels(&box)))
	}

	// A replacement of another generation (the record moved to another
	// machine) owes the same hash a label of its own: left alone.
	os.MkdirAll(target, 0o755)
	moved := *p.box
	moved.Host = "elsewhere.example"
	p3 := newPoller(m, &moved)
	p3.dialRt = f.dialer
	p3.rtLabels = []rtorrent.PendingLabel{{Hash: rtHashA, Label: "theirs", Until: time.Now().Add(time.Hour)}}
	m.mu.Lock()
	m.pollers[moved.ID] = p3
	m.mu.Unlock()
	f.reply = func(m, b string) string {
		if m == "d.base_path" {
			return xmlResp(xs(target))
		}
		return rtReplies(m, b)
	}
	cli, _ = p.rtClient(context.Background(), &sshx.Client{}, "unix:/x") // the old one was retired by closeConns
	if err := p.rtRemoveData(context.Background(), cli, run, "unix:/x", data, rtHashA, nil); err != nil {
		t.Fatal(err)
	}
	p3.mu.Lock()
	kept := len(p3.rtLabels)
	p3.mu.Unlock()
	if kept != 1 {
		t.Fatal("another generation's owed label must not be cleared")
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

// TestRecordRtLabelsRace (finding 4): recording and forgetting run
// concurrently without a data race, and the cache never holds a retired
// label after a forget that persisted.
func TestRecordRtLabelsRace(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	p.m.mu.Lock()
	p.m.pollers[p.box.ID] = p
	p.m.mu.Unlock()
	var wg sync.WaitGroup
	for round := 0; round < 20; round++ {
		p.mu.Lock()
		p.rt, p.rtSSH = nil, nil
		p.rtLabels = []rtorrent.PendingLabel{{Hash: rtHashA, Label: "a", Until: time.Now().Add(time.Hour)}, {Hash: rtHashB, Label: "b", Until: time.Now().Add(time.Hour)}}
		p.mu.Unlock()
		wg.Add(2)
		go func() { defer wg.Done(); p.recordRtLabels(context.Background()) }()
		go func() { defer wg.Done(); p.forgetRtLabel(context.Background(), rtHashA, true) }()
		wg.Wait()
		// The last writer was the forget (it persists after the record under
		// the same guard, or the record ran after it with the forgotten set).
		for _, l := range p.m.restoredRtLabels(p.box) {
			if l.Hash == rtHashA {
				t.Fatalf("round %d: retired label came back", round)
			}
		}
	}
}

// TestForgetRtLabelSurvivesReconnect (Codex on PR 123): while Forget waits
// for the client's operation token, a reconnect copies the still-pending
// label into the saved set and a replacement client restores it; the
// forget must clear that copy and the new client too.
func TestForgetRtLabelSurvivesReconnect(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	c1, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	c1.Restore([]rtorrent.PendingLabel{{Hash: rtHashA, Label: "old", Until: time.Now().Add(time.Hour)}})
	// Keep c1 busy with a real operation (a listing held inside the fake):
	// the forget on it has to wait.
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	f.reply = func(m, b string) string {
		if m == "d.multicall2" {
			entered <- struct{}{}
			<-gate
		}
		return rtReplies(m, b)
	}
	go func() { c1.Torrents(context.Background()) }()
	<-entered
	done := make(chan struct{})
	go func() { p.forgetRtLabel(context.Background(), rtHashA, false); close(done) }()
	time.Sleep(50 * time.Millisecond) // the forget is waiting on c1
	// A reconnect: the handoff (Retire) waits for c1's operation too, so it
	// runs on its own goroutine; once the listing ends, the label moves
	// into the saved copy and a new client inherits it, and the forget's
	// stability loop clears both.
	closed := make(chan struct{})
	go func() { p.closeConns(); close(closed) }()
	time.Sleep(50 * time.Millisecond)
	close(gate)
	<-closed
	c2, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("forget did not finish")
	}
	p.mu.Lock()
	n := len(p.rtLabels)
	p.mu.Unlock()
	if n != 0 || len(c2.Pending()) != 0 {
		t.Fatalf("label survived the reconnect: saved %d, new client %d", n, len(c2.Pending()))
	}
}

// TestHandoffWaitsForInFlightAdd (daybreak blue re-review of PR 123,
// finding 2): a reconnect during an Add paused between its remote label
// setter and its remember hands the label over; the mirror remove paused
// after its erase hands nothing over.
func TestHandoffWaitsForInFlightAdd(t *testing.T) {
	f := newFakeSCGI(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	f.reply = func(m, b string) string {
		if m == "d.custom1.set" || m == "d.erase" {
			entered <- struct{}{}
			<-gate
		}
		return rtReplies(m, b)
	}
	p, _ := rtFixture(t, f)
	c1, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	addDone := make(chan error, 1)
	go func() {
		_, err := c1.Add(context.Background(), rtorrent.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashA}, Label: "tv"})
		addDone <- err
	}()
	<-entered // the label is set on the box, not yet remembered
	closed := make(chan struct{})
	go func() { p.closeConns(); close(closed) }()
	select {
	case <-closed:
		t.Fatal("the handoff snapshotted during the Add")
	case <-time.After(150 * time.Millisecond):
	}
	close(gate)
	if err := <-addDone; err != nil {
		t.Fatal(err)
	}
	<-closed
	p.mu.Lock()
	saved := append([]rtorrent.PendingLabel(nil), p.rtLabels...)
	p.mu.Unlock()
	if len(saved) != 1 || saved[0].Hash != rtHashA || saved[0].Label != "tv" {
		t.Fatalf("the owed label must survive the handoff: %+v", saved)
	}
	// Mirror: a remove paused after its erase; the handoff must not keep it.
	gate = make(chan struct{})
	c2, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	if len(c2.Pending()) != 1 {
		t.Fatal("fixture: the new client should hold the label")
	}
	rmDone := make(chan error, 1)
	go func() { rmDone <- c2.Action(context.Background(), rtorrent.ActionRemove, rtHashA) }()
	<-entered
	closed = make(chan struct{})
	go func() { p.closeConns(); close(closed) }()
	time.Sleep(100 * time.Millisecond)
	close(gate)
	if err := <-rmDone; err != nil {
		t.Fatal(err)
	}
	<-closed
	p.mu.Lock()
	n := len(p.rtLabels)
	p.mu.Unlock()
	if n != 0 {
		t.Fatalf("a removed torrent's label survived the handoff: %d", n)
	}
}

// TestPostEraseLockIsBounded (finding 1): an edit holding the global lock
// past the job's deadline does not hold the job, Stop, or the rm: the
// bookkeeping is skipped and the deletion still runs with its own budget.
func TestPostEraseLockIsBounded(t *testing.T) {
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
	p, snap := rtFixture(t, f)
	p.m.mu.Lock()
	p.m.pollers[p.box.ID] = p
	p.m.mu.Unlock()
	p.jobTimeout = 500 * time.Millisecond // the whole setup-and-bookkeeping budget
	cli, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	cli.Restore([]rtorrent.PendingLabel{{Hash: rtHashA, Label: "old", Until: time.Now().Add(time.Hour)}})
	var rmDeadline time.Duration
	run := &deadlineRunner{inner: &localRunner{home: home}, onDelete: func(ctx context.Context) {
		if dl, ok := ctx.Deadline(); ok {
			rmDeadline = time.Until(dl)
		}
	}}
	p.dialDetached = func(ctx context.Context, ep string) (scriptRunner, rtorrent.Dialer, func(), error) {
		return run, f.dialer(ep), func() {}, nil
	}
	// An edit holds the lock for longer than the job's deadline; inside it,
	// the poller is torn down and recorded and a replacement built with the
	// restored label (fred on PR 123: the skipped cleanup must still not
	// let that label reach a re-added torrent).
	p.m.boxMu.Lock()
	p.closeConns()
	p.recordRtLabels(context.Background())
	box := *p.box
	p2 := newPoller(p.m, &box)
	p2.dialRt = f.dialer
	p2.rtLabels = p.m.restoredRtLabels(&box)
	if len(p2.rtLabels) != 1 {
		t.Fatalf("fixture: the replacement should restore the label: %+v", p2.rtLabels)
	}
	cli, _ = p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	start := time.Now()
	err := p.rtRemoveData(context.Background(), cli, run.inner, "unix:/x", data, rtHashA, nil)
	took := time.Since(start)
	if err != nil {
		t.Fatalf("the deletion must still run: %v", err)
	}
	// The edit publishes the replacement and ends.
	p.m.mu.Lock()
	p.m.pollers[box.ID] = p2
	p.m.mu.Unlock()
	p.m.boxMu.Unlock()
	if !p.m.rtTombed(box.ID, p2.generation(), rtHashA) {
		t.Fatal("the skipped cleanup must leave an erase tombstone")
	}
	// The replacement's first poll settles it before applying any label:
	// nothing owed, nothing persisted, tombstone lifted.
	var st BoxState
	p2.pollRt(context.Background(), &st, snap)
	if st.Rt.Error != "" {
		t.Fatal(st.Rt.Error)
	}
	c2, _ := p2.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	if len(c2.Pending()) != 0 || len(p.m.restoredRtLabels(&box)) != 0 || len(p.m.rtTombList(box.ID, p2.generation())) != 0 {
		t.Fatalf("pending %d cache %d tombs %d", len(c2.Pending()), len(p.m.restoredRtLabels(&box)), len(p.m.rtTombList(box.ID, p2.generation())))
	}
	// Re-adding the hash unlabelled stays unlabelled.
	before := countCalls(f, "d.custom1.set")
	if _, err := c2.Add(context.Background(), rtorrent.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashA}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Torrents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countCalls(f, "d.custom1.set") != before {
		t.Fatal("the re-added torrent inherited the erased torrent's label")
	}
	if took > 3*time.Second {
		t.Fatalf("the job waited on the lock for %v", took)
	}
	if _, statErr := os.Stat(target); statErr == nil {
		t.Fatal("the files must be gone")
	}
	if rmDeadline < rtRemoveTimeout-2*time.Second {
		t.Fatalf("the rm must get its own budget even after the job deadline passed: %v", rmDeadline)
	}
	// The helper goroutine let the lock go: it can be taken again at once.
	p.m.boxMu.Lock()
	p.m.boxMu.Unlock()
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

// TestLockWithinNeverLeaks (Codex P1 on PR 123): under a storm of deadlines
// racing the lock, every lockWithin either hands the mutex over (and the
// caller unlocks) or leaves it free; the mutex is always lockable after.
func TestLockWithinNeverLeaks(t *testing.T) {
	var mu sync.Mutex
	for i := 0; i < 2000; i++ {
		// Hold the lock briefly on another goroutine so the helper and the
		// deadline race for it.
		mu.Lock()
		rel := make(chan struct{})
		go func() { <-rel; mu.Unlock() }()
		ctx, cancel := context.WithTimeout(context.Background(), time.Duration(i%3)*time.Microsecond)
		go func() { time.Sleep(time.Duration(i%5) * time.Microsecond); close(rel) }()
		if unlock, ok := lockWithin(ctx, &mu); ok {
			unlock()
		}
		cancel()
		// Whatever happened, the mutex must be free (promptly).
		done := make(chan struct{})
		go func() { mu.Lock(); mu.Unlock(); close(done) }()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("iteration %d: the mutex leaked", i)
		}
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

// TestRtClientReplacementRetiresOld (daybreak blue re-review 4 of PR 123,
// finding 1): replacing the client, for a new endpoint or transport, is the
// same terminal handoff as a teardown. The replacement waits for the old
// client's operation in flight, takes what it owes, and the old pointer is
// dead: an operation on it fails with ErrRetired, and a caller going
// through the poller resolves the current client and succeeds.
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
	// An endpoint change while the Add is in flight: the replacement waits.
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
	if c2 == c1 || !c1.Retired() || len(c2.Pending()) != 1 || c2.Pending()[0].Label != "tv" {
		t.Fatalf("retired %v pending %+v", c1.Retired(), c2.Pending())
	}
	if err := c1.Action(context.Background(), rtorrent.ActionStop, rtHashA); !errors.Is(err, rtorrent.ErrRetired) {
		t.Fatalf("old pointer: %v", err)
	}
	// A caller holding the old pointer goes through the poller: the current
	// client answers.
	readyRt(t, p, "unix:/y")
	if err := p.onRetired(context.Background(), c1, func(c *rtorrent.Client) error { return c.Action(context.Background(), rtorrent.ActionStop, rtHashA) }); err != nil {
		t.Fatalf("retry on the current client: %v", err)
	}
	// A transport change retires the same way.
	c3, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/y")
	if c3 == c2 || !c2.Retired() || len(c3.Pending()) != 1 {
		t.Fatal("transport change must retire and hand over")
	}
}

// TestRtRetireTimeoutAdoptsLate (daybreak blue re-review 4 of PR 123,
// finding 1): when the teardown cannot wait for the old client's operation,
// nothing is read from it (a snapshot then could be wrong either way); the
// client is retired, its final owed set is collected once the operation
// ends and adopted by the current client, minus hashes erased meanwhile.
func TestRtRetireTimeoutAdoptsLate(t *testing.T) {
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
	p.m.mu.Lock()
	p.m.pollers[p.box.ID] = p // the late set goes to the box's current poller
	p.m.mu.Unlock()
	c1, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	c1.Restore([]rtorrent.PendingLabel{{Hash: rtHashB, Label: "gone", Until: time.Now().Add(time.Hour)}})
	addDone := make(chan error, 1)
	go func() {
		_, err := c1.Add(context.Background(), rtorrent.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashA}, Label: "tv"})
		addDone <- err
	}()
	<-entered
	sctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	p.mu.Lock()
	p.dropRtLocked(sctx)
	unknown := p.rtLabels == nil
	p.mu.Unlock()
	cancel()
	if !unknown || !c1.Retired() {
		t.Fatalf("a timed-out handoff must read nothing from the client (saved %+v) and retire it (%v)", p.rtLabels, c1.Retired())
	}
	// Nothing is persisted from an unknown set either.
	p.recordRtLabels(context.Background())
	if _, err := p.m.st.GetCache(context.Background(), p.box.ID, rtLabelsCacheKind, &rtLabelsCache{}); err == nil {
		t.Fatal("an unknown owed set must not be written")
	}
	// The successor comes up (over the hook's nil transport, as rtReady
	// resolves it) and removes hashB through the poller: forgotten,
	// tombstoned, and then fully settled, before the predecessor's
	// operation ends (fred on PR 123: the barrier must outlive the
	// settlement).
	readyRt(t, p, "unix:/x")
	c2, _ := p.rtClient(context.Background(), nil, "unix:/x")
	if err := p.rtTorrentAction(context.Background(), rtHashB, rtorrent.ActionRemove); err != nil {
		t.Fatal(err)
	}
	p.settleRtTombs(context.Background(), false)
	if n := len(p.m.rtTombList(p.box.ID, p.generation())); n != 0 {
		t.Fatalf("fixture: the tombstone should be settled, %d open", n)
	}
	close(gate)
	if err := <-addDone; err != nil {
		t.Fatal(err)
	}
	// The late set arrives: hashA adopted and persisted, hashB refused.
	waitFor(t, 3*time.Second, func() bool {
		got := c2.Pending()
		saved := p.m.restoredRtLabels(p.box)
		return len(got) == 1 && got[0].Hash == rtHashA && got[0].Label == "tv" && len(saved) == 1 && saved[0].Hash == rtHashA
	}, func() string { return fmt.Sprintf("pending %+v cache %+v", c2.Pending(), p.m.restoredRtLabels(p.box)) })
	// Re-adding hashB unlabelled stays unlabelled.
	before := countCalls(f, "d.custom1.set")
	if _, err := c2.Add(context.Background(), rtorrent.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashB}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Torrents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countCalls(f, "d.custom1.set") != before {
		t.Fatal("the erased torrent's label was applied to its unlabelled re-add")
	}
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

// TestRtLateAdoptionGoesToCurrentPoller (Codex on PR 123): a late
// retirement snapshot is handed to the box's current poller of the same
// generation, not to the retired one that spawned the wait, and is never
// written over a replacement's own labels; a replacement of another
// generation gets nothing.
func TestRtLateAdoptionGoesToCurrentPoller(t *testing.T) {
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
	p.m.mu.Lock()
	p.m.pollers[p.box.ID] = p
	p.m.mu.Unlock()
	c1, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	addDone := make(chan error, 1)
	go func() {
		_, err := c1.Add(context.Background(), rtorrent.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashA}, Label: "tv"})
		addDone <- err
	}()
	<-entered
	// An edit: the probe is cancelled, the teardown cannot wait, and a
	// replacement poller with a label of its own is published.
	sctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	p.mu.Lock()
	p.dropRtLocked(sctx)
	p.mu.Unlock()
	cancel()
	box := *p.box
	p2 := newPoller(p.m, &box)
	p2.dialRt = f.dialer
	p2.rtLabels = []rtorrent.PendingLabel{{Hash: rtHashB, Label: "mine", Until: time.Now().Add(time.Hour)}}
	p.m.mu.Lock()
	p.m.pollers[box.ID] = p2
	p.m.mu.Unlock()
	close(gate)
	if err := <-addDone; err != nil {
		t.Fatal(err)
	}
	// Both the replacement's copy and the cache: the write follows the
	// publication, so the wait covers both (fred on PR 123).
	same := func(ls []rtorrent.PendingLabel) bool {
		got := map[string]string{}
		for _, l := range ls {
			got[l.Hash] = l.Label
		}
		return len(got) == 2 && got[rtHashA] == "tv" && got[rtHashB] == "mine"
	}
	waitFor(t, 3*time.Second, func() bool {
		p2.mu.Lock()
		mem := same(p2.rtLabels)
		p2.mu.Unlock()
		return mem && same(p.m.restoredRtLabels(&box))
	}, func() string {
		p2.mu.Lock()
		defer p2.mu.Unlock()
		return fmt.Sprintf("replacement owes %+v, cache %+v", p2.rtLabels, p.m.restoredRtLabels(&box))
	})
	p.mu.Lock()
	stale := len(p.rtLabels)
	p.mu.Unlock()
	if stale != 0 {
		t.Fatalf("the retired poller must not take the labels: %d", stale)
	}
	// Another generation: the late set is dropped, not written.
	gate = make(chan struct{})
	c3, _ := p2.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	go func() {
		_, err := c3.Add(context.Background(), rtorrent.AddRequest{URLs: []string{"magnet:?xt=urn:btih:43519D14444904F6015E1A0D92018068FB0A49FB"}, Label: "x"})
		addDone <- err
	}()
	<-entered
	sctx, cancel = context.WithTimeout(context.Background(), 50*time.Millisecond)
	p2.mu.Lock()
	p2.dropRtLocked(sctx)
	p2.mu.Unlock()
	cancel()
	moved := box
	moved.Host = "elsewhere.example"
	p3 := newPoller(p.m, &moved)
	p.m.mu.Lock()
	p.m.pollers[moved.ID] = p3
	p.m.mu.Unlock()
	close(gate)
	if err := <-addDone; err != nil {
		t.Fatal(err)
	}
	p.m.lateWG.Wait()
	p3.mu.Lock()
	n := len(p3.rtLabels)
	p3.mu.Unlock()
	if n != 0 || p.m.restoredRtLabels(&moved) != nil {
		t.Fatalf("another generation took the labels: %d, cache %+v", n, p.m.restoredRtLabels(&moved))
	}
}

// TestRtEmptySnapshotIsAuthoritative (daybreak blue re-review 4 of PR 123,
// finding 2): a retired client that owes nothing yields a known-empty set,
// and that is what gets persisted, not the stale cached entry.
func TestRtEmptySnapshotIsAuthoritative(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	c, _ := p.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	c.Restore([]rtorrent.PendingLabel{{Hash: rtHashA, Label: "tv", Until: time.Now().Add(time.Hour)}})
	p.recordRtLabels(context.Background())
	if len(p.m.restoredRtLabels(p.box)) != 1 {
		t.Fatal("fixture")
	}
	if err := c.Forget(context.Background(), rtHashA); err != nil {
		t.Fatal(err)
	}
	p.closeConns() // retires: an empty, known snapshot
	p.mu.Lock()
	saved := p.rtLabels
	p.mu.Unlock()
	if saved == nil || len(saved) != 0 {
		t.Fatalf("known-empty must be kept as a non-nil empty slice: %#v", saved)
	}
	p.recordRtLabels(context.Background())
	var cached rtLabelsCache
	if _, err := p.m.st.GetCache(context.Background(), p.box.ID, rtLabelsCacheKind, &cached); err != nil || len(cached.Labels) != 0 {
		t.Fatalf("the empty set must be written: %v %+v", err, cached)
	}
	if len(p.m.restoredRtLabels(p.box)) != 0 {
		t.Fatal("a restart must not restore the settled label")
	}
}

// TestRtPlainRemovePersistsAtOnce (daybreak blue re-review 4 of PR 123,
// finding 2): a labelled add is persisted, a plain remove through the
// poller forgets and persists at once (no poll in between), and a restart
// restores nothing; the tombstone it leaves is lifted by the next poll.
func TestRtPlainRemovePersistsAtOnce(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, snap := rtFixture(t, f)
	readyRt(t, p, "unix:/x")
	if _, err := p.rtAddTorrents(context.Background(), qbt.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashA}, Category: "tv"}); err != nil {
		t.Fatal(err)
	}
	if got := p.m.restoredRtLabels(p.box); len(got) != 1 {
		t.Fatalf("the add must persist its owed label: %+v", got)
	}
	if err := p.rtTorrentAction(context.Background(), rtHashA, rtorrent.ActionRemove); err != nil {
		t.Fatal(err)
	}
	if got := p.m.restoredRtLabels(p.box); len(got) != 0 {
		t.Fatalf("the remove must persist at once: %+v", got)
	}
	// Like a restart: a new poller for the same record restores nothing.
	p2 := newPoller(p.m, p.box)
	p2.rtLabels = p.m.restoredRtLabels(p.box)
	if len(p2.rtLabels) != 0 {
		t.Fatalf("restored %+v", p2.rtLabels)
	}
	// The remove leaves a tombstone until a poll confirms the forget.
	if !p.m.rtTombed(p.box.ID, p.generation(), rtHashA) {
		t.Fatal("a remove must leave an erase tombstone")
	}
	var st BoxState
	p.pollRt(context.Background(), &st, snap)
	if st.Rt.Error != "" || len(p.m.rtTombList(p.box.ID, p.generation())) != 0 {
		t.Fatalf("the poll must settle the tombstone: %q %v", st.Rt.Error, p.m.rtTombList(p.box.ID, p.generation()))
	}
	// A tombstone without a client (the box offline, or Stop's final pass)
	// settles into the saved copy and the cache.
	p.closeConns()
	p.mu.Lock()
	p.rtLabels = []rtorrent.PendingLabel{{Hash: rtHashB, Label: "x", Until: time.Now().Add(time.Hour)}}
	p.mu.Unlock()
	p.m.rtTombstone(p.box.ID, p.generation(), rtHashB)
	p.settleRtTombs(context.Background(), true)
	p.mu.Lock()
	n := len(p.rtLabels)
	p.mu.Unlock()
	if n != 0 || len(p.m.restoredRtLabels(p.box)) != 0 || p.m.rtTombed(p.box.ID, p.generation(), rtHashB) {
		t.Fatalf("saved %d cache %d tombed %v", n, len(p.m.restoredRtLabels(p.box)), p.m.rtTombed(p.box.ID, p.generation(), rtHashB))
	}
	// Tombstones are bounded: the cap evicts the oldest, another
	// generation's and expired ones are purged on listing.
	for i := 0; i < rtTombMax+5; i++ {
		p.m.rtTombstone(p.box.ID, "g", fmt.Sprintf("%040X", i))
	}
	if n := len(p.m.rtTombList(p.box.ID, "g")); n != rtTombMax {
		t.Fatalf("cap: %d", n)
	}
	if n := len(p.m.rtTombList(p.box.ID, "other")); n != 0 || len(p.m.rtTombList(p.box.ID, "g")) != 0 {
		t.Fatal("another generation's listing purges the rest")
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
	if msg, _, ok := p.m.restoredRtRemove(p.box); strings.Contains(gotMsg, "boom") || !ok || strings.Contains(msg, "boom") {
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
	m2.applyRtRemove(p.box, &cached)
	if !strings.Contains(cached.Rt.RemoveFailed, "disk says no") || cached.Rt.RemoveFailedAt.IsZero() {
		t.Fatalf("the notice must survive a restart: %q", cached.Rt.RemoveFailed)
	}
	moved := *p.box
	moved.Host = "elsewhere.example"
	if _, _, ok := m2.restoredRtRemove(&moved); ok {
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
	m2.applyRtRemove(p.box, &cached)
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
	if msgR, _, ok := p.m.restoredRtRemove(p.box); still != msgB || !ok || !strings.Contains(msgR, "Beta") {
		t.Fatalf("the newer notice must survive a stale dismiss: %q / %q", still, msgR)
	}
	if err := p.m.DismissRtRemoveFailure(p.box.ID, seenB); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := p.m.restoredRtRemove(p.box); ok {
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
	g := m.labelGuard(box.ID)
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

// TestOldAddCannotOverwriteSuccessorLabels (daybreak blue re-review 5 of PR
// 123, finding 1): an add's persistence runs on a poller an edit has
// replaced meanwhile; the successor has since spent the label and persisted
// an empty set; the old writer must not put its snapshot over that.
func TestOldAddCannotOverwriteSuccessorLabels(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	readyRt(t, p, "unix:/x")
	entered, release := make(chan struct{}), make(chan struct{})
	p.beforeRecord = func() { close(entered); <-release }
	addDone := make(chan error, 1)
	go func() {
		_, err := p.rtAddTorrents(context.Background(), qbt.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashA}, Category: "tv"})
		addDone <- err
	}()
	<-entered // the label is remembered on the client; the write has not run
	// The edit: the old poller is torn down and flushed (still current, so
	// the flush passes), the successor restores the label and is published.
	p.closeConns()
	p.recordRtLabels(context.Background())
	if got := p.m.restoredRtLabels(p.box); len(got) != 1 {
		t.Fatalf("fixture: the edit's flush should persist the label: %+v", got)
	}
	box := *p.box
	p2 := newPoller(p.m, &box)
	p2.dialRt = f.dialer
	p2.rtLabels = p.m.restoredRtLabels(&box)
	p.m.mu.Lock()
	p.m.pollers[box.ID] = p2
	p.m.mu.Unlock()
	// The successor spends the label and persists the empty set.
	c2, _ := p2.rtClient(context.Background(), nil, "unix:/x")
	if err := c2.Forget(context.Background(), rtHashA); err != nil {
		t.Fatal(err)
	}
	p2.recordRtLabels(context.Background())
	if got := p.m.restoredRtLabels(&box); len(got) != 0 {
		t.Fatalf("fixture: %+v", got)
	}
	// The old add resumes and writes: nothing must change.
	close(release)
	if err := <-addDone; err != nil {
		t.Fatal(err)
	}
	if got := p.m.restoredRtLabels(&box); len(got) != 0 {
		t.Fatalf("the retired poller overwrote the successor's snapshot: %+v", got)
	}
	m2 := New(p.m.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if got := m2.restoredRtLabels(&box); len(got) != 0 {
		t.Fatalf("after a restart: %+v", got)
	}
}

// TestRtEraseIntentIsDurable (daybreak blue re-review 5 of PR 123, finding
// 2): the erase is on record before the command is sent, so a lost answer
// (the peer read the request and closed) or a process gone before the
// forget still leaves a restart knowing the hash was erased: the restored
// labels skip it, a fresh poller's first poll settles it, and an
// unlabelled re-add gets no label.
func TestRtEraseIntentIsDurable(t *testing.T) {
	f := newFakeSCGI(t)
	var lost atomic.Bool
	lost.Store(true)
	f.reply = func(m, b string) string {
		if m == "d.erase" && lost.Load() {
			return closeConn
		}
		return rtReplies(m, b)
	}
	p, snap := rtFixture(t, f)
	readyRt(t, p, "unix:/x")
	cli, _ := p.rtClient(context.Background(), nil, "unix:/x")
	cli.Restore([]rtorrent.PendingLabel{{Hash: rtHashA, Label: "old", Until: time.Now().Add(time.Hour)}})
	p.recordRtLabels(context.Background())
	if err := p.rtTorrentAction(context.Background(), rtHashA, rtorrent.ActionRemove); err == nil {
		t.Fatal("a lost answer must surface as an error")
	}
	if !p.m.rtTombed(p.box.ID, p.generation(), rtHashA) {
		t.Fatal("the erase intent must be on record although the answer was lost")
	}
	// The old label row is still there (nothing persisted after the error),
	// as after a hard exit; a restart restores the tombstone and skips it.
	if got := p.m.restoredRtLabels(p.box); len(got) != 1 {
		t.Fatalf("fixture: %+v", got)
	}
	m2 := New(p.m.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m2.restoreRtTombs(p.box)
	if !m2.rtTombed(p.box.ID, boxGeneration(p.box), rtHashA) {
		t.Fatal("the tombstone must survive a restart")
	}
	kept := 0 // what startPoller does with the restored labels
	for _, l := range m2.restoredRtLabels(p.box) {
		if !m2.rtTombed(p.box.ID, boxGeneration(p.box), l.Hash) {
			kept++
		}
	}
	if kept != 0 {
		t.Fatal("a restored label for an erased hash must be skipped")
	}
	// A fresh poller on the restarted manager settles it on its first poll
	// and an unlabelled re-add stays unlabelled.
	p2 := newPoller(m2, p.box)
	p2.dialRt = f.dialer
	m2.mu.Lock()
	m2.pollers[p.box.ID] = p2
	m2.mu.Unlock()
	p2.rtLabels = []rtorrent.PendingLabel{} // as startPoller leaves it after the skip
	lost.Store(false)
	var st BoxState
	p2.pollRt(context.Background(), &st, snap)
	if st.Rt.Error != "" || len(m2.rtTombList(p.box.ID, p2.generation())) != 0 {
		t.Fatalf("settle: %q %v", st.Rt.Error, m2.rtTombList(p.box.ID, p2.generation()))
	}
	c2, _ := p2.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	before := countCalls(f, "d.custom1.set")
	if _, err := c2.Add(context.Background(), rtorrent.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashA}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Torrents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countCalls(f, "d.custom1.set") != before {
		t.Fatal("the erased torrent's label reached its unlabelled re-add")
	}
	// Another generation restores nothing.
	moved := *p.box
	moved.Host = "elsewhere.example"
	m3 := New(p.m.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m3.restoreRtTombs(&moved)
	if m3.rtTombed(moved.ID, boxGeneration(&moved), rtHashA) {
		t.Fatal("another generation's tombstone restored")
	}
}

// TestSettleWaitsForDurableLabelRow (fred on PR 123): a tombstone is
// settled durably only after the cleaned label row is durable. A settle
// whose label write fails leaves the tombstone open; and a restart from
// the only reachable between-writes state (row cleaned, tombstone still
// open) finds the hash tombstoned, restores no label for it, and an
// unlabelled re-add gets none.
func TestSettleWaitsForDurableLabelRow(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, snap := rtFixture(t, f)
	m := p.m
	// The ordinary case: both rows end up consistent.
	cli, _ := p.rtClient(context.Background(), nil, "unix:/x")
	cli.Restore([]rtorrent.PendingLabel{{Hash: rtHashA, Label: "old", Until: time.Now().Add(time.Hour)}})
	p.recordRtLabels(context.Background())
	if err := m.rtTombstone(p.box.ID, p.generation(), rtHashA); err != nil {
		t.Fatal(err)
	}
	p.settleRtTombs(context.Background(), true)
	var tombs rtTombsCache
	if _, err := m.st.GetCache(context.Background(), p.box.ID, rtTombsCacheKind, &tombs); err != nil || len(tombs.Tombs) != 1 || !tombs.Tombs[0].Settled {
		t.Fatalf("settled row: %v %+v", err, tombs)
	}
	if got := m.restoredRtLabels(p.box); len(got) != 0 {
		t.Fatalf("label row not cleaned: %+v", got)
	}
	// A poller whose label row cannot be written (no box row: the store
	// refuses the cache write) must not settle: the tombstone stays open.
	orphan := newPoller(m, &store.Box{ID: 2, Name: "y", Host: "y.example", User: "u", RtEnabled: true})
	orphan.dialRt = f.dialer
	m.mu.Lock()
	m.pollers[2] = orphan
	m.mu.Unlock()
	orphan.rtLabels = []rtorrent.PendingLabel{{Hash: rtHashB, Label: "x", Until: time.Now().Add(time.Hour)}}
	m.rtTombstone(2, orphan.generation(), rtHashB) // the durable write fails too; the memory record stands
	orphan.settleRtTombs(context.Background(), true)
	if n := len(m.rtTombList(2, orphan.generation())); n != 1 {
		t.Fatalf("a settle whose label write failed must leave the tombstone open: %d open", n)
	}
	// Restart from the reachable between-writes state: the cleaned label
	// row is durable, the tombstone is still open on disk.
	if err := m.st.SetCache(context.Background(), p.box.ID, rtLabelsCacheKind, rtLabelsCache{Generation: boxGeneration(p.box), Labels: []rtorrent.PendingLabel{}}); err != nil {
		t.Fatal(err)
	}
	if err := m.st.SetCache(context.Background(), p.box.ID, rtTombsCacheKind, rtTombsCache{Tombs: []rtTombRec{{Hash: rtHashA, Generation: boxGeneration(p.box), At: time.Now()}}}); err != nil {
		t.Fatal(err)
	}
	m2 := New(m.st, slog.New(slog.NewTextHandler(io.Discard, nil)))
	m2.restoreRtTombs(p.box)
	if !m2.rtTombed(p.box.ID, boxGeneration(p.box), rtHashA) || len(m2.restoredRtLabels(p.box)) != 0 {
		t.Fatal("restart: the hash must be tombstoned and owe nothing")
	}
	p2 := newPoller(m2, p.box)
	p2.dialRt = f.dialer
	m2.mu.Lock()
	m2.pollers[p.box.ID] = p2
	m2.mu.Unlock()
	p2.rtLabels = []rtorrent.PendingLabel{}
	var st BoxState
	p2.pollRt(context.Background(), &st, snap)
	if st.Rt.Error != "" || len(m2.rtTombList(p.box.ID, p2.generation())) != 0 {
		t.Fatalf("first poll must settle: %q %v", st.Rt.Error, m2.rtTombList(p.box.ID, p2.generation()))
	}
	c2, _ := p2.rtClient(context.Background(), &sshx.Client{}, "unix:/x")
	before := countCalls(f, "d.custom1.set")
	if _, err := c2.Add(context.Background(), rtorrent.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashA}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Torrents(context.Background()); err != nil {
		t.Fatal(err)
	}
	if countCalls(f, "d.custom1.set") != before {
		t.Fatal("the erased torrent's label reached its unlabelled re-add")
	}
}
