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
			`<value><array><data><value><struct><member><name>dht</name><value><string>auto</string></value></member></struct></value></data></array></value></data></array>`)
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
	p := newPoller(m, &store.Box{ID: 1, Name: "x", User: "u", RtEnabled: true})
	p.dialRt = f.dialer
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
		cmd.Stdin = strings.NewReader("root=" + shellQuote(root) + "\ntarget=" + shellQuote(target) + "\n" + rtRemoveDataScript)
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

func TestRtRemoveDataFlow(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	ctx := context.Background()
	cli := rtorrent.New(f.dialer(""))
	// The SSH side cannot run here (no box): check the order up to it with
	// a client whose base path is unusable, so nothing is erased.
	f.reply = func(m, b string) string {
		if m == "d.base_path" {
			return xmlResp(xs("bad\npath"))
		}
		return rtReplies(m, b)
	}
	err := rtRemoveData(ctx, cli, nil, rtHashA)
	if err == nil || !strings.Contains(err.Error(), "cannot be checked") {
		t.Fatalf("%v", err)
	}
	for _, m := range f.called() {
		if m == "d.erase" {
			t.Fatal("erase must not run when the path is unusable")
		}
	}
}

func TestRtClientFollowsTransport(t *testing.T) {
	f := newFakeSCGI(t)
	f.reply = rtReplies
	p, _ := rtFixture(t, f)
	p.dialRt = nil
	// Without a transport and without the hook there is nothing to dial.
	if _, err := p.rtClient(nil, "unix:/x"); err == nil {
		t.Fatal("nil transport")
	}
	p.dialRt = f.dialer
	a := &sshx.Client{}
	c1, err := p.rtClient(a, "unix:/x")
	if err != nil {
		t.Fatal(err)
	}
	if c2, _ := p.rtClient(a, "unix:/x"); c2 != c1 {
		t.Fatal("same transport and endpoint: same client")
	}
	// A reconnected transport must not reuse a client whose dialer holds the
	// closed one (Codex P1 on PR 123); so must a changed endpoint.
	if c3, _ := p.rtClient(&sshx.Client{}, "unix:/x"); c3 == c1 {
		t.Fatal("new transport must rebuild the client")
	}
	if c4, _ := p.rtClient(a, "unix:/y"); c4 == c1 {
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
	if _, err := p.rtClient(tr, "unix:/home/u/.rtorrent.socket"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.rtClient(tr, "tcp:127.0.0.1:5000"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.rtClient(tr, "tcp:127.0.0.1:x"); err == nil {
		t.Fatal("bad port")
	}
	if _, err := p.rtClient(tr, "http://x"); err == nil {
		t.Fatal("unknown scheme")
	}
}

func TestRtRemoveDataSSHFailureIsReported(t *testing.T) {
	// Paths fine, erase accepted, but the box cannot be reached for the rm:
	// the torrent is gone from rTorrent and the caller is told the files
	// were not deleted (a disconnected transport answers every Run with an
	// error).
	f := newFakeSCGI(t)
	f.reply = rtReplies
	cli := rtorrent.New(f.dialer(""))
	err := rtRemoveData(context.Background(), cli, &sshx.Client{}, rtHashA)
	if err == nil || !strings.Contains(err.Error(), "files were not deleted") {
		t.Fatalf("%v", err)
	}
	erased := false
	for _, m := range f.called() {
		erased = erased || m == "d.erase"
	}
	if !erased {
		t.Fatal("erase should have run before the rm attempt")
	}
	// A fault on the erase stops everything.
	f.reply = func(m, b string) string {
		if m == "d.erase" {
			return `<?xml version="1.0"?><methodResponse><fault><value><struct><member><name>faultCode</name><value><i4>-501</i4></value></member><member><name>faultString</name><value><string>Could not find info-hash.</string></value></member></struct></value></fault></methodResponse>`
		}
		return rtReplies(m, b)
	}
	var fe *rtorrent.Fault
	if err := rtRemoveData(context.Background(), cli, &sshx.Client{}, rtHashA); !errors.As(err, &fe) {
		t.Fatalf("%v", err)
	}
	// A fault on either path lookup stops everything before the erase.
	for _, failing := range []string{"d.base_path", "directory.default"} {
		f.reply = func(m, b string) string {
			if m == failing {
				return `<?xml version="1.0"?><methodResponse><fault><value><struct><member><name>faultCode</name><value><i4>-506</i4></value></member><member><name>faultString</name><value><string>no</string></value></member></struct></value></fault></methodResponse>`
			}
			return rtReplies(m, b)
		}
		n := len(f.called())
		if err := rtRemoveData(context.Background(), cli, &sshx.Client{}, rtHashA); !errors.As(err, &fe) {
			t.Fatalf("%s: %v", failing, err)
		}
		for _, m := range f.called()[n:] {
			if m == "d.erase" {
				t.Fatalf("%s failed but erase ran", failing)
			}
		}
	}
}

// readyRt seeds a Ready state with rTorrent's endpoint for p, as a poll
// would, so the action gates pass.
func readyRt(t *testing.T, p *poller, ep string) {
	t.Helper()
	st := BoxState{ID: p.box.ID, Ready: true, Rt: RtState{Enabled: true, Endpoint: ep, Reachable: true}}
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
	res, err := p.rtAddTorrents(ctx, qbt.AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + rtHashA}, Files: []qbt.TorrentFile{{Name: "a", Data: []byte("d8:announcee")}}, Category: "(d.stop)", Stopped: true})
	if err != nil || res.Added != 2 {
		t.Fatalf("%+v %v", res, err)
	}
	m := f.called()
	if strings.Join(m[len(m)-2:], ",") != "load.normal,load.raw" {
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
	// remove-data with an unusable path never erases.
	readyRt(t, p, "unix:/home/u/.rtorrent.socket")
	f.reply = func(m, b string) string {
		if m == "d.base_path" {
			return xmlResp(xs("relative/path"))
		}
		return rtReplies(m, b)
	}
	before := len(f.called())
	err = p.rtTorrentAction(ctx, rtHashA, RtActionRemoveData)
	if err == nil || !strings.Contains(err.Error(), "cannot be checked") {
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
