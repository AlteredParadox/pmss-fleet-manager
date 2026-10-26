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

package rtorrent

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha1"
	"encoding/base32"
	"encoding/base64"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeRT is an SCGI server on a unix socket answering by method name.
type fakeRT struct {
	t      *testing.T
	ln     net.Listener
	mu     sync.Mutex
	calls  []call
	handle func(method string, params []string) string // returns the XML body; "" = hang
	raw    func(conn net.Conn)                         // overrides everything when set
}

type call struct {
	method string
	params []string // the text of each <value>, typed element stripped
}

// on installs the handler for the next calls (the server goroutines read
// it under the same lock).
func (f *fakeRT) on(h func(method string, params []string) string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.handle, f.raw = h, nil
}

func (f *fakeRT) onRaw(h func(conn net.Conn)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.raw = h
}

func newFake(t *testing.T) *fakeRT {
	t.Helper()
	dir, err := os.MkdirTemp("", "rt")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	ln, err := net.Listen("unix", filepath.Join(dir, "s"))
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeRT{t: t, ln: ln}
	t.Cleanup(func() { ln.Close() })
	go f.serve()
	return f
}

func (f *fakeRT) dial(ctx context.Context) (net.Conn, error) {
	var d net.Dialer
	return d.DialContext(ctx, "unix", f.ln.Addr().String())
}

func (f *fakeRT) serve() {
	for {
		conn, err := f.ln.Accept()
		if err != nil {
			return
		}
		go f.one(conn)
	}
}

func (f *fakeRT) one(conn net.Conn) {
	defer conn.Close()
	f.mu.Lock()
	raw, handle := f.raw, f.handle
	f.mu.Unlock()
	br := bufio.NewReader(conn)
	if raw != nil {
		// Read the request first: a close before the client wrote would
		// be a broken pipe on its side, not the response under test.
		n, err := br.ReadString(':')
		if err == nil {
			hl, _ := strconv.Atoi(strings.TrimSuffix(n, ":"))
			hdr := make([]byte, hl+1)
			if _, err := io.ReadFull(br, hdr); err == nil {
				cl, _ := strconv.Atoi(strings.Split(string(hdr[:hl]), "\x00")[1])
				io.ReadFull(br, make([]byte, cl))
			}
		}
		raw(conn)
		return
	}
	n, err := br.ReadString(':')
	if err != nil {
		return
	}
	hl, _ := strconv.Atoi(strings.TrimSuffix(n, ":"))
	hdr := make([]byte, hl+1)
	if _, err := io.ReadFull(br, hdr); err != nil {
		return
	}
	fields := strings.Split(string(hdr[:hl]), "\x00")
	if fields[0] != "CONTENT_LENGTH" || hdr[hl] != ',' {
		f.t.Errorf("bad scgi header %q", hdr)
		return
	}
	cl, _ := strconv.Atoi(fields[1])
	body := make([]byte, cl)
	if _, err := io.ReadFull(br, body); err != nil {
		return
	}
	method, params := parseCall(body)
	f.mu.Lock()
	f.calls = append(f.calls, call{method, params})
	f.mu.Unlock()
	resp := handle(method, params)
	if method == "d.custom1.set" && resp == `<?xml version="1.0" encoding="UTF-8"?><methodResponse><params><param><value><i8>0</i8></value></param></params></methodResponse>` && len(params) == 2 {
		// Real rTorrent answers a string variable's setter with the string it
		// assigned (fred on PR 122); a handler that did not care gets that.
		resp = `<?xml version="1.0" encoding="UTF-8"?><methodResponse><params><param><value><string>` + params[1] + `</string></value></param></params></methodResponse>`
	}
	if resp == "" {
		time.Sleep(2 * time.Second)
		return
	}
	fmt.Fprintf(conn, "Status: 200 OK\r\nContent-Type: text/xml\r\nContent-Length: %d\r\n\r\n%s", len(resp), resp)
}

// parseCall pulls the method name and the top-level param texts out of a
// methodCall, enough for assertions.
func parseCall(body []byte) (string, []string) {
	d := xml.NewDecoder(bytes.NewReader(body))
	var method string
	var params []string
	depth, inName := 0, false
	var buf strings.Builder
	for {
		tok, err := d.Token()
		if err != nil {
			break
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "methodName" {
				inName = true
			}
			if t.Name.Local == "param" {
				depth = 1
				buf.Reset()
			}
		case xml.CharData:
			if inName {
				method += string(t)
			} else if depth > 0 {
				buf.Write(t)
			}
		case xml.EndElement:
			if t.Name.Local == "methodName" {
				inName = false
			}
			if t.Name.Local == "param" {
				params = append(params, buf.String())
				depth = 0
			}
		}
	}
	return method, params
}

func (f *fakeRT) last() call {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[len(f.calls)-1]
}

func (f *fakeRT) methods() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var m []string
	for _, c := range f.calls {
		m = append(m, c.method)
	}
	return m
}

func resp(inner string) string {
	return `<?xml version="1.0" encoding="UTF-8"?><methodResponse><params><param><value>` + inner + `</value></param></params></methodResponse>`
}

func fault(code int, msg string) string {
	return fmt.Sprintf(`<?xml version="1.0"?><methodResponse><fault><value><struct><member><name>faultCode</name><value><i4>%d</i4></value></member><member><name>faultString</name><value><string>%s</string></value></member></struct></value></fault></methodResponse>`, code, msg)
}

func row(vals ...string) string {
	var b strings.Builder
	b.WriteString("<value><array><data>")
	for _, v := range vals {
		b.WriteString("<value>" + v + "</value>")
	}
	b.WriteString("</data></array></value>")
	return b.String()
}

func i8(n int64) string   { return fmt.Sprintf("<i8>%d</i8>", n) }
func str(s string) string { return "<string>" + s + "</string>" }

const hashA = "0123456789ABCDEF0123456789ABCDEF01234567"
const hashB = "89abcdef0123456789abcdef0123456789abcdef"

func torrentRow(hash, name string, extra ...string) string {
	// d.hash, d.name, size, completed, down, up, ratio, state, complete, active, hashing, open, message, label, finished, peers, seeds, uptotal, downtotal, private
	vals := []string{str(hash), str(name), i8(1000), i8(1000), i8(0), i8(2048), i8(1500), i8(1), i8(1), i8(1), i8(0), i8(1), str(""), str("tv"), i8(1700000000), i8(3), i8(2), i8(4000), i8(1000), i8(1)}
	copy(vals[2:], extra)
	return row(vals...)
}

func TestFramingAndVersion(t *testing.T) {
	f := newFake(t)
	f.on(func(m string, p []string) string {
		if m != "system.client_version" || len(p) != 1 || p[0] != "" {
			t.Errorf("call %s %q", m, p)
		}
		return resp(str("0.9.8"))
	})
	c := New(f.dial)
	v, err := c.Version(context.Background())
	if err != nil || v != "0.9.8" {
		t.Fatalf("version %q %v", v, err)
	}
}

func TestStatsMulticall(t *testing.T) {
	f := newFake(t)
	f.on(func(m string, p []string) string {
		if m != "system.multicall" {
			t.Errorf("method %s", m)
		}
		return resp(`<array><data>` +
			row(str("0.16.25")) + row(str("0.16.1")) + row(i8(123)) + row(i8(456)) + row(i8(50123)) +
			`<value><array><data><value><struct><member><name>dht</name><value><string>auto</string></value></member><member><name>active</name><value><i8>1</i8></value></member></struct></value></data></array></value>` +
			`</data></array>`)
	})
	st, err := New(f.dial).Stats(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if st.Version != "0.16.25" || st.Library != "0.16.1" || st.DownBPS != 123 || st.UpBPS != 456 || st.ListenPort != 50123 || !st.DHT {
		t.Fatalf("%+v", st)
	}
	// The mode string is not the state: "off" (and "auto" before any node
	// answered) with active=0 is DHT off (Codex on PR 122).
	for _, mode := range []string{"off", "auto", "disable", "on"} {
		f.on(func(m string, p []string) string {
			return resp(`<array><data>` + row(str("0.9.8")) + row(str("0.13.8")) + row(i8(1)) + row(i8(2)) + row(i8(3)) +
				`<value><array><data><value><struct><member><name>dht</name><value><string>` + mode + `</string></value></member><member><name>active</name><value><i8>0</i8></value></member></struct></value></data></array></value></data></array>`)
		})
		st, err := New(f.dial).Stats(context.Background())
		if err != nil || st.DHT {
			t.Fatalf("mode %s active=0: %+v %v", mode, st, err)
		}
	}
	// Every member carries the empty target.
	if got := f.last().params[0]; strings.Count(got, "methodName") != 6 {
		t.Fatalf("multicall params %q", got)
	}
}

func TestStatsFaultMemberIsZero(t *testing.T) {
	f := newFake(t)
	f.on(func(m string, p []string) string {
		return resp(`<array><data>` + row(str("0.9.8")) + row(str("0.13.8")) + row(i8(1)) + row(i8(2)) +
			`<value><struct><member><name>faultCode</name><value><i4>-506</i4></value></member><member><name>faultString</name><value><string>Method 'network.listen.port' not defined</string></value></member></struct></value>` +
			`<value><array><data><value><struct><member><name>dht</name><value><string>off</string></value></member><member><name>active</name><value><i8>0</i8></value></member></struct></value></data></array></value>` + `</data></array>`)
	})
	st, err := New(f.dial).Stats(context.Background())
	if err != nil || st.ListenPort != 0 || st.DHT {
		t.Fatalf("%+v %v", st, err)
	}
}

func TestTorrentsDecodeAndMapping(t *testing.T) {
	f := newFake(t)
	f.on(func(m string, p []string) string {
		if m != "d.multicall2" || p[0] != "" || p[1] != "main" || len(p) != 2+len(torrentFields) {
			t.Errorf("call %s %q", m, p)
		}
		return resp(`<array><data>` +
			torrentRow(hashA, "Alpha") +
			// lower-case hash, mixed int element names, stopped, with a tracker message
			torrentRow(hashB, "Beta", "<i4>2000</i4>", "<int>500</int>", i8(99), i8(0), i8(250), i8(0), i8(0), i8(0), i8(0), i8(0), str("Tracker: [Failure reason]"), str(""), i8(0), i8(0), i8(0), i8(0), i8(500), i8(0), i8(0)) +
			// not a torrent row: ignored, not fatal
			row(str("garbage"), str("x"), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), str(""), str(""), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0)) +
			`</data></array>`)
	})
	ts, err := New(f.dial).Torrents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 {
		t.Fatalf("rows %d", len(ts))
	}
	a, b := ts[0], ts[1]
	if a.Hash != hashA || a.Name != "Alpha" || a.Size != 1000 || a.UpRate != 2048 || a.Ratio != 1500 || !a.Complete || !a.Active || a.Label != "tv" || a.Finished != 1700000000 || a.Peers != 3 || a.Seeds != 2 || !a.Private {
		t.Fatalf("a = %+v", a)
	}
	if a.Kind() != "seeding" || a.State() != "seeding" {
		t.Fatalf("a kind %s state %s", a.Kind(), a.State())
	}
	if b.Hash != strings.ToUpper(hashB) || b.Size != 2000 || b.Completed != 500 || b.DownRate != 99 || b.Started || b.Open {
		t.Fatalf("b = %+v", b)
	}
	if b.Kind() != "error" || b.State() != "closed" {
		t.Fatalf("b kind %s state %s", b.Kind(), b.State())
	}
	// Hashing wins over everything; paused when started but inactive; leeching otherwise.
	h := Torrent{Hashing: 1, Message: "x"}
	if h.Kind() != "checking" {
		t.Fatal(h.Kind())
	}
	p := Torrent{Started: true, Open: true}
	if p.Kind() != "paused" || p.State() != "paused" {
		t.Fatal(p.Kind(), p.State())
	}
	l := Torrent{Started: true, Open: true, Active: true}
	if l.Kind() != "leeching" || l.State() != "downloading" {
		t.Fatal(l.Kind(), l.State())
	}
	// "Tried all trackers" is informational in rTorrent, not an error.
	ok := Torrent{Started: true, Open: true, Active: true, Complete: true, Message: "Tracker: [Tried all trackers.]"}
	if ok.Kind() != "seeding" {
		t.Fatal(ok.Kind())
	}
}

func TestTorrentsBadShapeIsAnError(t *testing.T) {
	f := newFake(t)
	f.on(func(m string, p []string) string {
		return resp(`<array><data>` + row(str(hashA), str("short")) + `</data></array>`)
	})
	if _, err := New(f.dial).Torrents(context.Background()); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("err %v", err)
	}
	f.on(func(m string, p []string) string { return resp(str("not a list")) })
	if _, err := New(f.dial).Torrents(context.Background()); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("err %v", err)
	}
}

func TestRetainedStringsClippedAndUTF8(t *testing.T) {
	f := newFake(t)
	long := strings.Repeat("ü", 1000)
	bad := "caf\xe9 \xff\xfe ok" // latin-1 bytes in a name
	f.on(func(m string, p []string) string {
		return resp(`<array><data>` + torrentRow(hashA, long) + torrentRow(hashB, bad, i8(1), i8(1), i8(1), i8(1), i8(1), i8(1), i8(1), i8(1), i8(0), i8(1), str(strings.Repeat("m", 500)), str(strings.Repeat("l", 500)), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0)) + `</data></array>`)
	})
	ts, err := New(f.dial).Torrents(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(ts[0].Name)); n != maxName {
		t.Fatalf("name runes %d", n)
	}
	if ts[1].Name != "caf� �� ok" {
		t.Fatalf("utf8 repair %q", ts[1].Name)
	}
	if len(ts[1].Message) != maxMessage || len(ts[1].Label) != maxLabel {
		t.Fatalf("message %d label %d", len(ts[1].Message), len(ts[1].Label))
	}
}

func TestUTF8RuneSplitAcrossReads(t *testing.T) {
	src := strings.Repeat("a", 4095) + "é" + "z" // the 2-byte rune straddles the 4096 buffer
	out, err := io.ReadAll(newUTF8Reader(strings.NewReader(src)))
	if err != nil || string(out) != src {
		t.Fatalf("split rune mangled: %v %d", err, len(out))
	}
	out, _ = io.ReadAll(newUTF8Reader(strings.NewReader("ok\xc3"))) // truncated at EOF
	if string(out) != "ok�" {
		t.Fatalf("%q", out)
	}
}

func TestFaultAndMalformed(t *testing.T) {
	f := newFake(t)
	f.on(func(m string, p []string) string { return fault(-501, "Could not find info-hash.") })
	err := New(f.dial).Action(context.Background(), ActionStop, hashA)
	var fe *Fault
	if !errors.As(err, &fe) || fe.Code != -501 || !strings.Contains(fe.String, "info-hash") {
		t.Fatalf("fault %v", err)
	}
	for _, body := range []string{
		`<html>nope</html>`,
		`<?xml version="1.0"?><methodResponse><params></params></methodResponse>`,
		`<?xml version="1.0"?><methodResponse><params><param><value><i8>x</i8></value></param></params></methodResponse>`,
		`<?xml version="1.0"?><methodResponse><params><param><value><string>a</string></value></param><param><value><string>b</string></value></param></params></methodResponse>`,
		`<?xml version="1.0"?><!DOCTYPE x [<!ENTITY e "boom">]><methodResponse><params><param><value><string>&e;</string></value></param></params></methodResponse>`,
		`<?xml version="1.0"?><methodResponse><params><param><value><string>unterminated`,
	} {
		f.on(func(m string, p []string) string { return body })
		if _, err := New(f.dial).Version(context.Background()); !errors.Is(err, ErrMalformed) {
			t.Errorf("%q: %v", body[:40], err)
		}
	}
}

func TestNotSCGI(t *testing.T) {
	f := newFake(t)
	f.onRaw(func(conn net.Conn) { io.WriteString(conn, "<?xml version=\"1.0\"?><methodResponse/>") })
	if _, err := New(f.dial).Version(context.Background()); !errors.Is(err, ErrSCGI) {
		t.Fatalf("%v", err)
	}
	f.onRaw(func(conn net.Conn) {
		io.WriteString(conn, "Status: 200 OK\r\n"+strings.Repeat("X-Pad: y\r\n", 40)+"\r\n")
	})
	if _, err := New(f.dial).Version(context.Background()); !errors.Is(err, ErrSCGI) {
		t.Fatalf("%v", err)
	}
	f.onRaw(func(conn net.Conn) {
		io.WriteString(conn, "Status: 200 OK\r\nX: "+strings.Repeat("y", 9000)+"\r\n\r\n")
	})
	if _, err := New(f.dial).Version(context.Background()); !errors.Is(err, ErrSCGI) {
		t.Fatalf("%v", err)
	}
}

func TestCaps(t *testing.T) {
	f := newFake(t)
	// Too many rows: an error, not a truncated list.
	f.on(func(m string, p []string) string {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><methodResponse><params><param><value><array><data>`)
		for i := 0; i <= 50; i++ {
			b.WriteString(torrentRow(hashA, "n"))
		}
		b.WriteString(`</data></array></value></param></params></methodResponse>`)
		return b.String()
	})
	capped := New(f.dial)
	capped.maxRows = 50
	if _, err := capped.Torrents(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("rows: %v", err)
	}
	// Nesting past maxDepth.
	deep := strings.Repeat("<array><data><value>", maxDepth+2) + "<i8>1</i8>" + strings.Repeat("</value></data></array>", maxDepth+2)
	f.on(func(m string, p []string) string { return resp(deep) })
	if _, err := New(f.dial).Version(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("depth: %v", err)
	}
	// Bytes past maxResponse: the stream is cut and the decode fails closed.
	f.onRaw(func(conn net.Conn) {
		fmt.Fprintf(conn, "Status: 200 OK\r\n\r\n<?xml version=\"1.0\"?><methodResponse><params><param><value><string>")
		chunk := bytes.Repeat([]byte("a"), 1<<20)
		for i := 0; i < 3; i++ {
			if _, err := conn.Write(chunk); err != nil {
				return
			}
		}
		io.WriteString(conn, "</string></value></param></params></methodResponse>")
	})
	c := New(f.dial)
	c.maxBytes = 1 << 20
	_, err := c.Version(context.Background())
	if err == nil || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("oversize: %v", err)
	}
}

func TestActions(t *testing.T) {
	f := newFake(t)
	f.on(func(m string, p []string) string {
		if len(p) != 1 || p[0] != hashA {
			t.Errorf("%s params %q", m, p)
		}
		return resp(i8(0))
	})
	c := New(f.dial)
	ctx := context.Background()
	if err := c.Action(ctx, ActionStop, strings.ToLower(hashA)); err != nil {
		t.Fatal(err)
	}
	if err := c.Action(ctx, ActionStart, hashA); err != nil {
		t.Fatal(err)
	}
	if err := c.Action(ctx, ActionRemove, hashA); err != nil {
		t.Fatal(err)
	}
	want := []string{"d.stop", "d.close", "d.open", "d.start", "d.erase"}
	if got := f.methods(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("methods %v", got)
	}
	if err := c.Action(ctx, "remove-data", hashA); !errors.Is(err, ErrBadAction) {
		t.Fatal(err)
	}
	if !ValidAction(ActionStop) || !ValidAction(ActionStart) || !ValidAction(ActionRemove) || ValidAction("remove-data") || ValidAction("") {
		t.Fatal("ValidAction")
	}
	if err := c.Action(ctx, ActionStop, "../x"); !errors.Is(err, ErrBadHash) {
		t.Fatal(err)
	}
	if len(f.methods()) != len(want) {
		t.Fatal("refused actions must not reach the box")
	}
}

func TestAdd(t *testing.T) {
	f := newFake(t)
	torrent := "d8:announce9:http://x/4:infod4:name1:a12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee"
	want, err := InfoHash([]byte(torrent))
	if err != nil {
		t.Fatal(err)
	}
	var sets []call
	f.on(func(m string, p []string) string {
		switch m {
		case "load.raw_start":
			if p[1] != base64.StdEncoding.EncodeToString([]byte(torrent)) {
				t.Errorf("raw params %q", p)
			}
		case "d.custom1.set":
			sets = append(sets, call{m, p})
		}
		if strings.Contains(p[1], "bad.example") {
			return fault(-501, "Could not create download")
		}
		return resp(i8(0))
	})
	c := New(f.dial)
	// Without a label: http(s) links are fine, paths and file: URLs never
	// reach the box, a refused link counts as failed.
	res, err := c.Add(context.Background(), AddRequest{
		URLs:  []string{"magnet:?xt=urn:btih:" + hashA, "https://bad.example/x.torrent", "https://ok.example/y.torrent", "/etc/passwd", "file:///x"},
		Files: [][]byte{[]byte(torrent)},
	})
	if err != nil || res.Added != 3 || res.Failed != 3 {
		t.Fatalf("%+v %v", res, err)
	}
	if got := strings.Join(f.methods(), ","); got != "load.start,load.start,load.start,load.raw_start" {
		t.Fatalf("methods %v", got)
	}
	if len(sets) != 0 {
		t.Fatal("no label, no d.custom1.set")
	}
	// With a label: loaded first, then d.custom1.set by hash as a literal
	// XML-RPC parameter; the label text is never part of a command string,
	// so "(d.stop)" and "$system.shutdown.normal=" are just text (fred on
	// PR 122, confirmed live).
	label := `$system.shutdown.normal= (d.stop) "x",y\z`
	sets = nil
	res, err = c.Add(context.Background(), AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + strings.ToLower(hashB)}, Files: [][]byte{[]byte(torrent), []byte("not a torrent")}, Label: label, Stopped: true})
	if err != nil || res.Added != 2 || res.Failed != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(sets) != 2 || sets[0].params[0] != strings.ToUpper(hashB) || sets[0].params[1] != label || sets[1].params[0] != want || sets[1].params[1] != label {
		t.Fatalf("sets %+v", sets)
	}
	m := f.methods()
	if strings.Join(m[len(m)-4:], ",") != "load.normal,d.custom1.set,load.raw,d.custom1.set" {
		t.Fatalf("%v", m)
	}
	for _, c := range f.calls {
		for _, p := range c.params {
			if c.method != "d.custom1.set" && strings.Contains(p, "custom1") {
				t.Fatalf("label leaked into a command string: %q", p)
			}
		}
	}
	// A label on an http(s) link cannot be set by hash: the link loads
	// without it and is counted, the magnet beside it gets the label.
	sets = nil
	res, err = c.Add(context.Background(), AddRequest{URLs: []string{"https://ok.example/y.torrent", "magnet:?xt=urn:btih:" + hashA}, Label: "tv"})
	if err != nil || res.Added != 2 || res.Unlabelled != 1 || len(sets) != 1 || sets[0].params[0] != hashA {
		t.Fatalf("%+v %v %+v", res, err, sets)
	}
	// Every URL refused by rTorrent: the fault surfaces.
	f.on(func(m string, p []string) string { return fault(-501, "no") })
	if _, err := c.Add(context.Background(), AddRequest{URLs: []string{"magnet:?x"}}); !isFault(err) {
		t.Fatal(err)
	}
	// A fault on the label set is counted, the torrent stays loaded.
	f.on(func(m string, p []string) string {
		if m == "d.custom1.set" {
			return fault(-501, "Could not find info-hash.")
		}
		return resp(i8(0))
	})
	res, err = c.Add(context.Background(), AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + hashA}, Label: "tv"})
	if err != nil || res.Added != 1 || res.Unlabelled != 1 {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestInfoHashAndMagnet(t *testing.T) {
	// Built the way a client would: the hash is the SHA-1 of the raw info value.
	info := "d4:name1:a12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaae"
	sum := sha1.Sum([]byte(info))
	want := strings.ToUpper(hex.EncodeToString(sum[:]))
	for _, tor := range []string{
		"d8:announce9:http://x/4:info" + info + "e",
		"d4:info" + info + "8:announce9:http://x/e", // key order does not matter to the scan
		"d7:comment3:abc13:announce-listll9:http://x/ee4:info" + info + "7:privatei1ee",
	} {
		h, err := InfoHash([]byte(tor))
		if err != nil || h != want {
			t.Errorf("%q: %s %v", tor[:20], h, err)
		}
	}
	for _, bad := range []string{"", "l4:infoe", "d4:info", "d4:infoi1", "d8:announce9:http://x/e", "d4:info" + info[:len(info)-1], "d3:keye", "d4:infod1:ke" + "e", "not bencode", "d99999:xe"} {
		if _, err := InfoHash([]byte(bad)); !errors.Is(err, ErrNoInfoHash) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	// Deep nesting is bounded.
	deep := "d4:info" + strings.Repeat("l", 100) + strings.Repeat("e", 100) + "e"
	if _, err := InfoHash([]byte(deep)); !errors.Is(err, ErrNoInfoHash) {
		t.Fatal("depth")
	}
	if h, err := MagnetHash("magnet:?xt=urn:btih:" + strings.ToLower(hashA) + "&dn=x"); err != nil || h != hashA {
		t.Fatal(h, err)
	}
	raw, _ := hex.DecodeString(hashA)
	b32 := base32.StdEncoding.EncodeToString(raw)
	if h, err := MagnetHash("magnet:?dn=x&xt=urn:btih:" + strings.ToLower(b32)); err != nil || h != hashA {
		t.Fatal(h, err)
	}
	for _, bad := range []string{"https://x/y.torrent", "magnet:?dn=x", "magnet:?xt=urn:btmh:1220" + hashA + hashA[:24], "magnet:?xt=urn:btih:zz", "magnet:?xt=urn:btih:" + hashA[:39], "magnet:?%zz"} {
		if _, err := MagnetHash(bad); !errors.Is(err, ErrNoInfoHash) {
			t.Errorf("%q: %v", bad, err)
		}
	}
}

func TestTimeoutAndSerialisation(t *testing.T) {
	f := newFake(t)
	f.on(func(m string, p []string) string { return "" }) // hang
	c := New(f.dial)
	c.timeout = 150 * time.Millisecond
	start := time.Now()
	_, err := c.Version(context.Background())
	if !errors.Is(err, context.DeadlineExceeded) || time.Since(start) > time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
	// A second caller waits for the operation token rather than opening
	// another channel.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	<-c.ops // hold the token
	_, err = c.Version(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	c.ops <- struct{}{}
}

func TestEncodeCall(t *testing.T) {
	b, err := encodeCall("load.raw_start", "", []byte{0, 1}, "d.custom1.set=a<b")
	if err != nil {
		t.Fatal(err)
	}
	want := `<?xml version="1.0" encoding="UTF-8"?><methodCall><methodName>load.raw_start</methodName><params><param><value><string></string></value></param><param><value><base64>AAE=</base64></value></param><param><value><string>d.custom1.set=a&lt;b</string></value></param></params></methodCall>`
	if string(b) != want {
		t.Fatalf("%s", b)
	}
	if _, err := encodeCall("x", 1.5); err == nil {
		t.Fatal("floats are not sent: tinyxml2 refuses them")
	}
	if !ValidHash(hashA) || !ValidHash(hashB) || ValidHash(hashA[:39]) || ValidHash(hashA[:39]+"g") {
		t.Fatal("ValidHash")
	}
}

func TestSCGIRequestFraming(t *testing.T) {
	got := scgiRequest([]byte("<x/>"))
	want := "24:CONTENT_LENGTH\x004\x00SCGI\x001\x00,<x/>"
	if string(got) != want {
		t.Fatalf("%q", got)
	}
}

func TestPathLookups(t *testing.T) {
	f := newFake(t)
	f.on(func(m string, p []string) string {
		switch m {
		case "d.base_path":
			if p[0] != hashA {
				t.Errorf("params %q", p)
			}
			return resp(str("/home/u/data/Alpha"))
		case "directory.default":
			return resp(str("/home/u/data"))
		}
		return fault(-506, "no")
	})
	c := New(f.dial)
	bp, err := c.BasePath(context.Background(), strings.ToLower(hashA))
	if err != nil || bp != "/home/u/data/Alpha" {
		t.Fatal(bp, err)
	}
	if _, err := c.BasePath(context.Background(), "nope"); !errors.Is(err, ErrBadHash) {
		t.Fatal(err)
	}
}

func TestDecodeKinds(t *testing.T) {
	f := newFake(t)
	cases := []struct {
		body string
		want func(Value) bool
	}{
		{resp(`<double>1.5</double>`), func(v Value) bool { return v.Kind == KindFloat && v.Float == 1.5 }},
		{resp(`<boolean>1</boolean>`), func(v Value) bool { return v.Kind == KindBool && v.Bool }},
		{resp(`<boolean>false</boolean>`), func(v Value) bool { return v.Kind == KindBool && !v.Bool }},
		{resp("<base64>AA\n EC</base64>"), func(v Value) bool { return v.Kind == KindBytes && string(v.Bytes) == "\x00\x01\x02" }},
		{resp(`<nil/>`), func(v Value) bool { return v.Kind == KindNil }},
		{resp(`<ex:i8>-7</ex:i8>`), func(v Value) bool { return v.Kind == KindInt && v.Int == -7 }},
		{resp(`untyped text`), func(v Value) bool { return v.Kind == KindString && v.Str == "untyped text" }},
		{resp(``), func(v Value) bool { return v.Kind == KindString && v.Str == "" }},
		{resp(" \n <i4>3</i4> \n "), func(v Value) bool { return v.Kind == KindInt && v.Int == 3 }},
		{resp(`<struct> <member><name>a</name><value><i8>1</i8></value></member> <member><name>b</name><value><struct><member><name>c</name><value>x</value></member></struct></value></member></struct>`),
			func(v Value) bool {
				return v.Kind == KindStruct && v.Struct["a"].Int == 1 && v.Struct["b"].Struct["c"].Str == "x"
			}},
		{resp(`<array><data> <value><i8>1</i8></value> <value><string>s</string></value> </data></array>`),
			func(v Value) bool { return v.Kind == KindArray && len(v.Array) == 2 && v.Array[1].Str == "s" }},
		{`<?xml version="1.0"?><!-- c --><?pi x?><methodResponse><params><param><value><i8>9</i8></value></param></params></methodResponse>`,
			func(v Value) bool { return v.Int == 9 }},
	}
	c := New(f.dial)
	for _, tc := range cases {
		f.on(func(m string, p []string) string { return tc.body })
		v, err := c.call(context.Background(), 8, 4096, "x", "")
		if err != nil || !tc.want(v) {
			t.Errorf("%s: %+v %v", tc.body[:min(60, len(tc.body))], v, err)
		}
	}
	for _, body := range []string{
		resp(`<boolean>yes</boolean>`), resp(`<double>NaN</double>`), resp(`<double>x</double>`), resp(`<base64>!!</base64>`),
		resp(`<unknown>1</unknown>`), resp(`text<i8>1</i8>`), resp(`<array><data>junk</data></array>`), resp(`<array><data><i8>1</i8></data></array>`),
		resp(`<array><i8>1</i8></array>`), resp(`<struct><value/></struct>`), resp(`<struct><member><value/></member></struct>`),
		resp(`<struct>junk</struct>`), resp(`<i8>1</i8><i8>2</i8>`), resp(`<string><b>x</b></string>`),
		`<?xml version="1.0" encoding="ISO-8859-1"?><methodResponse><params><param><value>x</value></param></params></methodResponse>`,
		`<?xml version="1.0"?><methodResponse><fault><value><string>not a struct</string></value></fault></methodResponse>`,
		`<?xml version="1.0"?><methodResponse><other/></methodResponse>`,
		`<?xml version="1.0"?><methodResponse>text<params/></methodResponse>`,
	} {
		f.on(func(m string, p []string) string { return body })
		if _, err := c.call(context.Background(), 8, 4096, "x", ""); !errors.Is(err, ErrMalformed) {
			t.Errorf("%s: %v", body[:min(70, len(body))], err)
		}
	}
	// A struct keeps at most 256 members; the rest are read and dropped.
	var b strings.Builder
	b.WriteString("<struct>")
	for i := 0; i < 300; i++ {
		fmt.Fprintf(&b, "<member><name>k%d</name><value><i8>%d</i8></value></member>", i, i)
	}
	b.WriteString("</struct>")
	f.on(func(m string, p []string) string { return resp(b.String()) })
	v, err := c.call(context.Background(), 8, 4096, "x", "")
	if err != nil || len(v.Struct) != 256 {
		t.Fatalf("members %d %v", len(v.Struct), err)
	}
	if (&Fault{Code: 1, String: "x"}).Error() != "rtorrent fault 1: x" {
		t.Fatal("fault text")
	}
}

func TestOneHugeTokenIsBounded(t *testing.T) {
	// encoding/xml hands a run of character data over as one token; the
	// retained string must be the clip, not a slice of the whole token.
	f := newFake(t)
	huge := strings.Repeat("n", 2<<20)
	f.on(func(m string, p []string) string { return resp(str(huge)) })
	v, err := New(f.dial).Version(context.Background())
	if err != nil || len(v) != maxVersion {
		t.Fatalf("%d %v", len(v), err)
	}
	if got := clip(huge, 10); len(got) != 10 || cap([]byte(got)) > 64 {
		t.Fatalf("clip must not pin the source: len %d cap %d", len(got), cap([]byte(got)))
	}
}

func TestStalledBodyIsADeadline(t *testing.T) {
	// Headers and the start of the body arrive, then nothing: the client's
	// own deadline must surface as such, whichever of the context timer and
	// the socket deadline fires first (Codex on PR 122).
	f := newFake(t)
	f.onRaw(func(conn net.Conn) {
		io.WriteString(conn, "Status: 200 OK\r\n\r\n<?xml version=\"1.0\"?><methodResponse><params><param><value><string>")
		time.Sleep(2 * time.Second)
	})
	c := New(f.dial)
	c.timeout = 120 * time.Millisecond
	for i := 0; i < 5; i++ {
		if _, err := c.Version(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("run %d: %v", i, err)
		}
	}
	// A dial that never completes is a deadline too.
	slow := New(func(ctx context.Context) (net.Conn, error) { <-ctx.Done(); return nil, ctx.Err() })
	slow.timeout = 50 * time.Millisecond
	if _, err := slow.Version(context.Background()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
}

func TestEncodeNested(t *testing.T) {
	b, err := encodeCall("system.multicall", []any{map[string]any{"methodName": "a", "params": []any{"", int64(2), true, false}}})
	if err != nil {
		t.Fatal(err)
	}
	want := `<params><param><value><array><data><value><struct><member><name>methodName</name><value><string>a</string></value></member><member><name>params</name><value><array><data><value><string></string></value><value><i8>2</i8></value><value><boolean>1</boolean></value><value><boolean>0</boolean></value></data></array></value></member></struct></value></data></array></value></param></params>`
	if !strings.Contains(string(b), want) {
		t.Fatalf("%s", b)
	}
	if _, err := encodeCall("x", []any{1.5}); err == nil {
		t.Fatal("nested float")
	}
	if _, err := encodeCall("x", map[string]any{"methodName": 1.5}); err == nil {
		t.Fatal("struct float")
	}
}

func TestRowValuesClamped(t *testing.T) {
	f := newFake(t)
	neg := "<i8>-5</i8>"
	f.on(func(m string, p []string) string {
		return resp(`<array><data>` + torrentRow(hashA, "n", neg, neg, neg, neg, neg, i8(0), i8(0), i8(0), neg, i8(0), str(""), str(""), neg, neg, neg, neg, neg, i8(0), i8(0)) + `</data></array>`)
	})
	ts, err := New(f.dial).Torrents(context.Background())
	if err != nil || len(ts) != 1 {
		t.Fatal(err)
	}
	r := ts[0]
	if r.Size != 0 || r.Completed != 0 || r.DownRate != 0 || r.UpRate != 0 || r.Ratio != 0 || r.Hashing != 0 || r.Finished != 0 || r.Peers != 0 || r.Seeds != 0 || r.UpTotal != 0 || r.DownTotal != 0 {
		t.Fatalf("%+v", r)
	}
	if r.State() != "closed" {
		t.Fatal(r.State())
	}
	if (Torrent{Open: true}).State() != "stopped" || (Torrent{Hashing: 2}).State() != "checking" {
		t.Fatal("state")
	}
	f.on(func(m string, p []string) string { return resp(i8(1)) })
	c := New(f.dial)
	if _, err := c.BasePath(context.Background(), hashA); !errors.Is(err, ErrUnexpected) {
		t.Fatal(err)
	}
	if _, err := c.Version(context.Background()); !errors.Is(err, ErrUnexpected) {
		t.Fatal(err)
	}
	if _, err := c.Stats(context.Background()); !errors.Is(err, ErrUnexpected) {
		t.Fatal(err)
	}
}

func TestDialErrorIsReported(t *testing.T) {
	// A missing socket or a refused SSH channel is the dialer's error, not
	// a cancellation (Codex on PR 122).
	sentinel := errors.New("dial unix /home/u/.rtorrent.socket: no such file or directory")
	c := New(func(ctx context.Context) (net.Conn, error) { return nil, sentinel })
	if _, err := c.Version(context.Background()); !errors.Is(err, sentinel) {
		t.Fatalf("%v", err)
	}
}

// unknownMethodFault: a fault means "this build lacks the method" only
// when it says so for that method (used for the optional stats members).
func TestUnknownMethodFault(t *testing.T) {
	for msg, want := range map[string]bool{
		"Method 'network.listen.port' not defined": true, `Command "network.listen.port" does not exist.`: true, "Unsupported command: network.listen.port": true,
		"network.listen.port: unknown": true, "Could not find info-hash.": false, "Method 'd.hash' not defined": false,
	} {
		if got := unknownMethodFault(&Fault{Code: -501, String: msg}, "network.listen.port"); got != want {
			t.Errorf("%q: %v", msg, got)
		}
	}
	if !unknownMethodFault(&Fault{Code: -506, String: "network.listen.port"}, "network.listen.port") || unknownMethodFault(errors.New("x"), "network.listen.port") {
		t.Fatal("code -506 with the name counts; a non-fault does not")
	}
}

// Daybreak blue review of PR 122, finding 1: a wide inner array or a huge
// number of small values must fail fast, not after the whole response was
// retained.
func TestNestedAllocationBounded(t *testing.T) {
	f := newFake(t)
	wide := func(n int) string {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><methodResponse><params><param><value><array><data><value><array><data>`)
		for i := 0; i < n; i++ {
			b.WriteString(`<value><i8>1</i8></value>`)
		}
		b.WriteString(`</data></array></value></data></array></value></param></params></methodResponse>`)
		return b.String()
	}
	f.on(func(m string, p []string) string { return wide(maxInner + 1) })
	if _, err := New(f.dial).Torrents(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("wide inner array: %v", err)
	}
	// The retained-value budget applies across all nesting: 60 rows of 20
	// values pass a budget of 2000 but not one of 1000.
	rows := func(n int) string {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><methodResponse><params><param><value><array><data>`)
		for i := 0; i < n; i++ {
			b.WriteString(torrentRow(hashA, "n"))
		}
		b.WriteString(`</data></array></value></param></params></methodResponse>`)
		return b.String()
	}
	f.on(func(m string, p []string) string { return rows(60) })
	c := New(f.dial)
	c.maxValues = 1000
	if _, err := c.Torrents(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("budget: %v", err)
	}
	c.maxValues = 2000
	if _, err := c.Torrents(context.Background()); err != nil {
		t.Fatalf("within budget: %v", err)
	}
	// Struct members count too.
	var b strings.Builder
	b.WriteString("<struct>")
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&b, "<member><name>k%d</name><value><i8>%d</i8></value></member>", i, i)
	}
	b.WriteString("</struct>")
	f.on(func(m string, p []string) string { return resp(b.String()) })
	if _, err := c.call(context.Background(), 8, 100, "x", ""); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("struct budget: %v", err)
	}
}

// Finding 3: a value of the wrong kind in a successful row or multicall
// member is not rTorrent's answer; in particular a wrong-kind column
// never reads as a known false.
func TestColumnKindsChecked(t *testing.T) {
	f := newFake(t)
	wrong := func(col int) string {
		vals := []string{str(hashA), str("n"), i8(1), i8(1), i8(0), i8(0), i8(0), i8(1), i8(1), i8(1), i8(0), i8(1), str(""), str(""), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0)}
		if columnKinds[col] == KindString {
			vals[col] = i8(7)
		} else {
			vals[col] = str("7")
		}
		return resp(`<array><data>` + row(vals...) + `</data></array>`)
	}
	c := New(f.dial)
	for col := range torrentFields {
		f.on(func(m string, p []string) string { return wrong(col) })
		rows, err := c.Torrents(context.Background())
		if !errors.Is(err, ErrUnexpected) {
			t.Errorf("column %d (%v): %v %+v", col, torrentFields[col], err, rows)
		}
	}
	// Stats: a member of the wrong kind fails; a per-member fault is zero.
	bad := func(i int, v string) string {
		parts := []string{row(str("0.9.8")), row(str("0.13.8")), row(i8(1)), row(i8(2)), row(i8(3)), `<value><array><data><value><struct><member><name>active</name><value><i8>1</i8></value></member></struct></value></data></array></value>`}
		parts[i] = v
		return resp(`<array><data>` + strings.Join(parts, "") + `</data></array>`)
	}
	for i, v := range map[int]string{0: row(i8(1)), 2: row(str("x")), 4: row(str("5000")), 5: row(str("s")), 1: `<value><string>flat</string></value>`} {
		f.on(func(m string, p []string) string { return bad(i, v) })
		if _, err := c.Stats(context.Background()); !errors.Is(err, ErrUnexpected) {
			t.Errorf("stats member %d: %v", i, err)
		}
	}
	f.on(func(m string, p []string) string {
		return bad(5, `<value><array><data><value><struct><member><name>active</name><value><string>yes</string></value></member></struct></value></data></array></value>`)
	})
	if _, err := c.Stats(context.Background()); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("dht active of the wrong kind: %v", err)
	}
	// A fault with an untyped code or string is malformed, not a Fault.
	for _, body := range []string{
		`<?xml version="1.0"?><methodResponse><fault><value><struct><member><name>faultCode</name><value><string>-1</string></value></member><member><name>faultString</name><value><string>x</string></value></member></struct></value></fault></methodResponse>`,
		`<?xml version="1.0"?><methodResponse><fault><value><struct><member><name>faultCode</name><value><i4>-1</i4></value></member><member><name>faultString</name><value><i4>2</i4></value></member></struct></value></fault></methodResponse>`,
	} {
		f.on(func(m string, p []string) string { return body })
		if _, err := c.Version(context.Background()); !errors.Is(err, ErrMalformed) {
			t.Errorf("%v", err)
		}
	}
}

// Finding 5: the envelope must be complete and followed by nothing.
func TestEnvelopeComplete(t *testing.T) {
	f := newFake(t)
	c := New(f.dial)
	for _, body := range []string{
		resp(str("ok")) + `<extra/>`,
		resp(str("ok")) + `garbage`,
		`<?xml version="1.0"?><methodResponse><fault><value><struct><member><name>faultCode</name><value><i4>-506</i4></value></member><member><name>faultString</name><value><string>Method 'd.is_meta' not defined</string></value></member></struct></value>`,
		`<?xml version="1.0"?><methodResponse><fault><value><struct><member><name>faultCode</name><value><i4>-506</i4></value></member><member><name>faultString</name><value><string>x</string></value></member></struct></value></fault>`,
	} {
		f.on(func(m string, p []string) string { return body })
		_, err := c.Version(context.Background())
		if !errors.Is(err, ErrMalformed) {
			t.Errorf("%.60s: %v", body, err)
		}
	}
	// Trailing whitespace and a comment are fine; a namespaced i8 decodes.
	f.on(func(m string, p []string) string { return resp(str("ok")) + "\n<!-- bye -->\n" })
	if v, err := c.Version(context.Background()); err != nil || v != "ok" {
		t.Fatal(v, err)
	}
	f.on(func(m string, p []string) string {
		return `<?xml version="1.0"?><methodResponse xmlns:ex="http://ws.apache.org/xmlrpc/namespaces/extensions"><params><param><value><ex:i8>42</ex:i8></value></param></params></methodResponse>`
	})
	if v, err := c.call(context.Background(), 8, 4096, "x", ""); err != nil || v.Int != 42 {
		t.Fatalf("namespaced i8: %+v %v", v, err)
	}
}

// Daybreak blue re-review of PR 122, finding 1: every call states the shape
// of its answer, so a wide root array fails fast on scalar, stats and
// command calls too, and a command's answer must be rTorrent's integer.
func TestRootArrayBounded(t *testing.T) {
	f := newFake(t)
	wide := func(n int) string {
		var b strings.Builder
		b.WriteString(`<?xml version="1.0"?><methodResponse><params><param><value><array><data>`)
		for i := 0; i < n; i++ {
			b.WriteString(`<value><i8>1</i8></value>`)
		}
		b.WriteString(`</data></array></value></param></params></methodResponse>`)
		return b.String()
	}
	c := New(f.dial)
	ctx := context.Background()
	f.on(func(m string, p []string) string { return wide(scalarRows + 1) })
	if _, err := c.Version(ctx); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("version: %v", err)
	}
	if _, err := c.BasePath(ctx, hashA); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("base path: %v", err)
	}
	if err := c.Action(ctx, ActionStop, hashA); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("action: %v", err)
	}
	if _, err := c.Add(ctx, AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + hashA}}); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("add: %v", err)
	}
	f.on(func(m string, p []string) string { return wide(statsRows + 1) })
	if _, err := c.Stats(ctx); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("stats: %v", err)
	}
	// A small root array passes the cap and then fails the shape check, as
	// before; a command answering a string instead of its status is refused.
	f.on(func(m string, p []string) string { return wide(2) })
	if _, err := c.Version(ctx); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("small array: %v", err)
	}
	f.on(func(m string, p []string) string { return resp(str("ok")) })
	if err := c.Action(ctx, ActionStop, hashA); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("command answer kind: %v", err)
	}
	// The value budget of a scalar call is small too.
	var b strings.Builder
	b.WriteString("<struct>")
	for i := 0; i < scalarValues+10; i++ {
		fmt.Fprintf(&b, "<member><name>k%d</name><value><i8>%d</i8></value></member>", i, i)
	}
	b.WriteString("</struct>")
	f.on(func(m string, p []string) string { return resp(b.String()) })
	if _, err := c.Version(ctx); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("scalar budget: %v", err)
	}
}

// Finding 3: a multicall fault reads as "missing" only when it is a typed
// unknown-method fault on an optional member.
func TestStatsFaultsStrict(t *testing.T) {
	f := newFake(t)
	c := New(f.dial)
	ctx := context.Background()
	fault := func(code, msg string) string {
		return `<value><struct><member><name>faultCode</name><value><i4>` + code + `</i4></value></member><member><name>faultString</name><value><string>` + msg + `</string></value></member></struct></value>`
	}
	members := func(i int, v string) string {
		parts := []string{row(str("0.9.8")), row(str("0.13.8")), row(i8(1)), row(i8(2)), row(i8(3)), `<value><array><data><value><struct><member><name>active</name><value><i8>1</i8></value></member></struct></value></data></array></value>`}
		parts[i] = v
		return resp(`<array><data>` + strings.Join(parts, "") + `</data></array>`)
	}
	// Optional members with an unknown-method fault: zero, no error.
	f.on(func(m string, p []string) string {
		return members(4, fault("-506", "Method 'network.listen.port' not defined"))
	})
	if st, err := c.Stats(ctx); err != nil || st.ListenPort != 0 || !st.DHT {
		t.Fatalf("%+v %v", st, err)
	}
	f.on(func(m string, p []string) string {
		return members(5, fault("-506", "Method 'dht.statistics' not defined"))
	})
	if st, err := c.Stats(ctx); err != nil || st.DHT {
		t.Fatalf("%+v %v", st, err)
	}
	// A fault on a mandatory member surfaces as the fault.
	for i := 0; i < 4; i++ {
		f.on(func(m string, p []string) string { return members(i, fault("-506", "Method not defined")) })
		if _, err := c.Stats(ctx); !isFault(err) {
			t.Errorf("mandatory member %d: %v", i, err)
		}
	}
	// An optional member's fault that is not "unknown method" surfaces too.
	f.on(func(m string, p []string) string { return members(4, fault("-501", "Could not find info-hash.")) })
	if _, err := c.Stats(ctx); !isFault(err) {
		t.Fatalf("non-unknown optional fault: %v", err)
	}
	// Malformed faults are not faults.
	for _, bad := range []string{
		`<value><struct><member><name>faultCode</name><value><string>-506</string></value></member><member><name>faultString</name><value><string>x</string></value></member></struct></value>`,
		`<value><struct><member><name>faultCode</name><value><i4>-506</i4></value></member></struct></value>`,
		`<value><struct><member><name>faultCode</name><value><i4>-506</i4></value></member><member><name>faultString</name><value><i4>1</i4></value></member></struct></value>`,
	} {
		f.on(func(m string, p []string) string { return members(4, bad) })
		if _, err := c.Stats(ctx); !errors.Is(err, ErrUnexpected) {
			t.Errorf("malformed fault: %v", err)
		}
	}
}

// Daybreak blue re-review of PR 122, finding 1: a header line without a
// newline is refused at maxHeaderLine, promptly, with the connection still
// open, instead of being read toward the response cap.
func TestHeaderLineBounded(t *testing.T) {
	f := newFake(t)
	f.onRaw(func(conn net.Conn) {
		io.WriteString(conn, "Status: 200 OK\r\nX-Pad: "+strings.Repeat("y", maxHeaderLine+100)) // no newline, no close
		time.Sleep(3 * time.Second)
	})
	start := time.Now()
	_, err := New(f.dial).Version(context.Background())
	if !errors.Is(err, ErrSCGI) || time.Since(start) > time.Second {
		t.Fatalf("%v after %v", err, time.Since(start))
	}
	// A line of exactly the limit passes; body bytes after the blank line
	// reach the decoder intact (the buffer is sized to one header line).
	f.onRaw(func(conn net.Conn) {
		io.WriteString(conn, "Status: 200 OK\r\nX-Pad: "+strings.Repeat("y", maxHeaderLine-len("X-Pad: ")-1)+"\r\n\r\n"+resp(str("ok")))
	})
	if v, err := New(f.dial).Version(context.Background()); err != nil || v != "ok" {
		t.Fatalf("%q %v", v, err)
	}
}

// Finding 2: a command's status must be 0.
func TestCommandStatusMustBeZero(t *testing.T) {
	f := newFake(t)
	f.on(func(m string, p []string) string { return resp(i8(1)) })
	c := New(f.dial)
	if err := c.Action(context.Background(), ActionStop, hashA); !errors.Is(err, ErrUnexpected) {
		t.Fatalf("action: %v", err)
	}
	if got := f.methods(); len(got) != 1 {
		t.Fatalf("the second command must not run after a bad status: %v", got)
	}
	res, err := c.Add(context.Background(), AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + hashA}, Label: "tv"})
	if !errors.Is(err, ErrUnexpected) || res.Added != 0 {
		t.Fatalf("add: %+v %v", res, err)
	}
}

// fred and the daybreak blue re-review on PR 122: d.custom1.set answers with
// the assigned string on real rTorrent. A labelled add succeeds on exactly
// that answer, and the reconcile too; a status integer, another string or
// another kind is not rTorrent's answer.
func TestLabelSetterContract(t *testing.T) {
	f := newFake(t)
	torrent := "d8:announce9:http://x/4:infod4:name1:a12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee"
	mode := "echo"
	f.on(func(m string, p []string) string {
		if m == "d.custom1.set" {
			switch mode {
			case "echo":
				return resp(str(p[1]))
			case "zero":
				return resp("<i4>0</i4>")
			case "other":
				return resp(str("something else"))
			}
		}
		return resp(i8(0))
	})
	c := New(f.dial)
	ctx := context.Background()
	res, err := c.Add(ctx, AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + hashA}, Files: [][]byte{[]byte(torrent)}, Label: "tv"})
	if err != nil || res.Added != 2 || res.Unlabelled != 0 {
		t.Fatalf("real contract: %+v %v", res, err)
	}
	// Wrong answers are refused by the setter; the add itself still
	// succeeded (the torrent is loaded) and reports the label unconfirmed.
	for _, m := range []string{"zero", "other"} {
		mode = m
		f.on(func(m2 string, p []string) string {
			if m2 == "d.custom1.set" {
				if mode == "zero" {
					return resp("<i4>0</i4>") // the status an action answers with, which this setter never does
				}
				return resp(str("something else"))
			}
			return resp(i8(0))
		})
		d := New(f.dial)
		if err := d.setLabel(ctx, hashA, "tv"); !errors.Is(err, ErrUnexpected) {
			t.Fatalf("%s: setter contract: %v", m, err)
		}
		if res, err := d.Add(ctx, AddRequest{Files: [][]byte{[]byte(torrent)}, Label: "tv"}); err != nil || res.Added != 1 || res.Unlabelled != 1 {
			t.Fatalf("%s: added with the label unconfirmed, not failed: %+v %v", m, res, err)
		}
	}
}

// Daybreak blue re-review 5 of PR 122: the response cap is exact. A valid
// document padded to exactly the cap passes; one byte more, even trailing
// whitespace after a valid document, is refused.
func TestResponseCapIsExact(t *testing.T) {
	f := newFake(t)
	doc := resp(str("0.9.8"))
	hdr := "Status: 200 OK\r\nContent-Type: text/xml\r\n\r\n"
	pad := func(total int) string {
		body := doc + strings.Repeat(" ", total-len(hdr)-len(doc))
		return hdr + body
	}
	cap_ := int64(len(hdr) + len(doc) + 64)
	var payload string
	f.onRaw(func(conn net.Conn) { io.WriteString(conn, payload) })
	c := New(f.dial)
	c.maxBytes = cap_
	payload = pad(int(cap_))
	if v, err := c.Version(context.Background()); err != nil || v != "0.9.8" {
		t.Fatalf("exactly the cap: %v %q", err, v)
	}
	payload = pad(int(cap_) + 1)
	if _, err := c.Version(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("one byte over the cap: %v", err)
	}
	payload = pad(int(cap_) + 500)
	if _, err := c.Version(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("whitespace well past the cap: %v", err)
	}
	// A fault is held to the same cap (fred on PR 122): at the cap it is the
	// fault, one byte over it is ErrTooLarge, and an oversized unknown-method
	// fault is refused as a whole.
	fault := `<?xml version="1.0" encoding="UTF-8"?><methodResponse><fault><value><struct><member><name>faultCode</name><value><i4>-506</i4></value></member><member><name>faultString</name><value><string>Method 'd.is_meta' not defined</string></value></member></struct></value></fault></methodResponse>`
	padF := func(total int) string { return hdr + fault + strings.Repeat(" ", total-len(hdr)-len(fault)) }
	capF := int64(len(hdr) + len(fault) + 64)
	c.maxBytes = capF
	payload = padF(int(capF))
	var fe *Fault
	if _, err := c.Version(context.Background()); !errors.As(err, &fe) || fe.Code != -506 {
		t.Fatalf("fault at the cap: %v", err)
	}
	payload = padF(int(capF) + 1)
	if _, err := c.Version(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("fault one byte over the cap: %v", err)
	}
	if _, err := c.Torrents(context.Background()); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("an oversized fault on the listing: %v", err)
	}
}

// Retire waits for the operation in flight to complete its remote and local
// steps, is terminal (a queued or later operation fails with ErrRetired),
// honours its context, and still retires when it times out.
func TestRetire(t *testing.T) {
	f := newFake(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	f.on(func(m string, p []string) string {
		if m == "d.custom1.set" {
			entered <- struct{}{}
			<-gate
		}
		return resp(i8(0))
	})
	c := New(f.dial)
	ctx := context.Background()
	addDone := make(chan error, 1)
	go func() {
		_, err := c.Add(ctx, AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + hashA}, Label: "tv"})
		addDone <- err
	}()
	<-entered
	retired := make(chan error, 1)
	go func() { retired <- c.Retire(ctx) }()
	select {
	case <-retired:
		t.Fatal("Retire returned during the Add")
	case <-time.After(100 * time.Millisecond):
	}
	queued := make(chan error, 1)
	go func() { queued <- c.Action(ctx, ActionStop, hashA) }()
	time.Sleep(30 * time.Millisecond)
	close(gate)
	if err := <-addDone; err != nil {
		t.Fatal(err)
	}
	if err := <-retired; err != nil {
		t.Fatal(err)
	}
	if err := <-queued; !errors.Is(err, ErrRetired) {
		t.Fatalf("queued operation after retire: %v", err)
	}
	if !c.Retired() {
		t.Fatal("Retired")
	}
	for _, err := range []error{
		func() error { _, e := c.Version(ctx); return e }(),
		func() error { _, e := c.Add(ctx, AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + hashB}}); return e }(),
		func() error { _, e := c.Torrents(ctx); return e }(),
		c.Retire(ctx),
	} {
		if err != nil && !errors.Is(err, ErrRetired) {
			t.Fatalf("%v", err)
		}
	}
	// A timed-out Retire still retires: the operation in flight completes,
	// nothing new starts.
	c2 := New(f.dial)
	gate = make(chan struct{})
	go func() {
		_, err := c2.Add(ctx, AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + hashB}, Label: "x"})
		addDone <- err
	}()
	<-entered
	cctx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err := c2.Retire(cctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal(err)
	}
	if !c2.Retired() {
		t.Fatal("a timed-out Retire still retires the client")
	}
	if _, err := c2.Version(ctx); !errors.Is(err, ErrRetired) {
		t.Fatalf("new operation after a timed-out Retire: %v", err)
	}
	close(gate)
	if err := <-addDone; err != nil {
		t.Fatalf("the operation in flight completes: %v", err)
	}
}

// fred on PR 122: a torrent whose load was acknowledged but whose label
// setter lost its answer is reported as added with the label unconfirmed
// (Unlabelled), never as a failed add.
func TestLostLabelAnswerIsAddedUnlabelled(t *testing.T) {
	f := newFake(t)
	torrent := "d8:announce9:http://x/4:infod4:name1:a12:piece lengthi16384e6:pieces20:aaaaaaaaaaaaaaaaaaaaee"
	f.on(func(m string, p []string) string {
		if m == "d.custom1.set" {
			return "" // the peer never answers: the call times out
		}
		return resp(i8(0))
	})
	c := New(f.dial)
	c.timeout = 150 * time.Millisecond
	res, err := c.Add(context.Background(), AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + hashA}, Files: [][]byte{[]byte(torrent)}, Label: "tv"})
	if err != nil || res.Added != 2 || res.Failed != 0 || res.Unlabelled != 2 {
		t.Fatalf("a lost setter answer is a label warning, not a failed add: %+v %v", res, err)
	}
	// The caller can log why, without the label itself.
	if len(res.LabelErrors) != 2 || !strings.Contains(res.LabelErrors[0], "…: ") || strings.Contains(strings.Join(res.LabelErrors, " "), "tv") {
		t.Fatalf("label errors for the caller's log, without the label: %q", res.LabelErrors)
	}
	// A genuine load failure is still a failure.
	f.on(func(m string, p []string) string {
		if strings.HasPrefix(m, "load.") {
			return `<?xml version="1.0"?><methodResponse><fault><value><struct><member><name>faultCode</name><value><i4>-501</i4></value></member><member><name>faultString</name><value><string>Could not create download</string></value></member></struct></value></fault></methodResponse>`
		}
		return resp(str(p[len(p)-1]))
	})
	res, err = c.Add(context.Background(), AddRequest{Files: [][]byte{[]byte(torrent)}, Label: "tv"})
	if err == nil || res.Added != 0 || res.Failed != 1 {
		t.Fatalf("a refused load stays a failure: %+v %v", res, err)
	}
}

// Finding 2 (daybreak blue review of PR 122): operations are serialised as
// a whole. A Stop whose first call is in flight holds off a Start entirely,
// so the box never sees d.stop, d.open, d.close, d.start; two Adds run one
// after the other, each load followed by its own label set.
func TestOperationsDoNotInterleave(t *testing.T) {
	f := newFake(t)
	gate := make(chan struct{})
	entered := make(chan struct{}, 1)
	f.on(func(m string, p []string) string {
		if m == "d.stop" {
			entered <- struct{}{}
			<-gate
		}
		return resp(i8(0))
	})
	c := New(f.dial)
	ctx := context.Background()
	stopDone := make(chan error, 1)
	go func() { stopDone <- c.Action(ctx, ActionStop, hashA) }()
	<-entered // Stop holds the operation and is blocked inside d.stop
	startDone := make(chan error, 1)
	go func() { startDone <- c.Action(ctx, ActionStart, hashA) }()
	time.Sleep(50 * time.Millisecond) // Start must be waiting, not calling
	if got := f.methods(); len(got) != 1 {
		t.Fatalf("Start ran while Stop was in flight: %v", got)
	}
	close(gate)
	if err := <-stopDone; err != nil {
		t.Fatal(err)
	}
	if err := <-startDone; err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(f.methods(), ","); got != "d.stop,d.close,d.open,d.start" {
		t.Fatalf("order %s", got)
	}
	// Two labelled Adds: whole operations in sequence, so each load is
	// followed by its own label set before the other add starts.
	gate2 := make(chan struct{})
	entered2 := make(chan struct{}, 2)
	var mu sync.Mutex
	var seq []string
	f.on(func(m string, p []string) string {
		mu.Lock()
		seq = append(seq, m+":"+p[len(p)-1])
		mu.Unlock()
		if m == "load.start" {
			entered2 <- struct{}{}
			<-gate2
			return resp(i8(0))
		}
		if m == "d.custom1.set" {
			return resp(str(p[1]))
		}
		return resp(i8(0))
	})
	done := make(chan struct{}, 2)
	go func() {
		c.Add(ctx, AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + hashB}, Label: "A"})
		done <- struct{}{}
	}()
	<-entered2
	go func() {
		c.Add(ctx, AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + hashA}, Label: "B"})
		done <- struct{}{}
	}()
	time.Sleep(30 * time.Millisecond)
	close(gate2)
	<-done
	<-done
	mu.Lock()
	got := seq
	mu.Unlock()
	if len(got) != 4 || !strings.HasPrefix(got[0], "load.start:") || got[1] != "d.custom1.set:A" || !strings.HasPrefix(got[2], "load.start:") || got[3] != "d.custom1.set:B" {
		t.Fatalf("adds interleaved: %v", got)
	}
}

// Daybreak blue re-review of PR 122: every unlabelled outcome carries a
// fixed reason for the log, an http(s) link included; the peer's fault text
// is not repeated; past the bound the rest is counted.
func TestLabelErrorReasons(t *testing.T) {
	f := newFake(t)
	f.on(func(m string, p []string) string {
		if m == "d.custom1.set" {
			return fault(-501, "secret-peer-text")
		}
		return resp(i8(0))
	})
	c := New(f.dial)
	urls := []string{"https://example.invalid/x.torrent"}
	for i := 0; i < 10; i++ {
		urls = append(urls, fmt.Sprintf("magnet:?xt=urn:btih:%040X", i+1))
	}
	res, err := c.Add(context.Background(), AddRequest{URLs: urls, Label: "tv"})
	if err != nil || res.Added != 11 || res.Unlabelled != 11 {
		t.Fatalf("%+v %v", res, err)
	}
	all := strings.Join(res.LabelErrors, "\n")
	if len(res.LabelErrors) != maxLabelErrors || res.LabelErrorsOmitted != 11-maxLabelErrors ||
		!strings.Contains(res.LabelErrors[0], "link: not attempted") || !strings.Contains(all, "fault -501") ||
		strings.Contains(all, "secret-peer-text") || strings.Contains(all, "example.invalid") || strings.Contains(all, "tv") {
		t.Fatalf("reasons %q omitted %d", res.LabelErrors, res.LabelErrorsOmitted)
	}
}
