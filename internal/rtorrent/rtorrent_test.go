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
	vals := []string{str(hash), str(name), i8(1000), i8(1000), i8(0), i8(2048), i8(1500), i8(1), i8(1), i8(1), i8(0), i8(1), str(""), str("tv"), i8(1700000000), i8(3), i8(2), i8(4000), i8(1000), i8(1), i8(0)}
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
			row(str("x")) + `</data></array>`)
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
			row(str("garbage"), str("x"), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), str(""), str(""), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0)) +
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
	// A second caller waits for the token rather than opening another channel.
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	<-c.calls // hold the token
	_, err = c.Version(ctx)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("%v", err)
	}
	c.calls <- struct{}{}
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
	dd, err := c.DefaultDirectory(context.Background())
	if err != nil || dd != "/home/u/data" {
		t.Fatal(dd, err)
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
		v, err := c.call(context.Background(), 0, "x", "")
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
		if _, err := c.call(context.Background(), 0, "x", ""); !errors.Is(err, ErrMalformed) {
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
	v, err := c.call(context.Background(), 0, "x", "")
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
	if r.State() != "closed" || r.String() != hashA+` "n"` {
		t.Fatal(r.State(), r.String())
	}
	if (Torrent{Open: true}).State() != "stopped" || (Torrent{Hashing: 2}).State() != "checking" {
		t.Fatal("state")
	}
	f.on(func(m string, p []string) string { return resp(i8(1)) })
	c := New(f.dial)
	if _, err := c.BasePath(context.Background(), hashA); !errors.Is(err, ErrUnexpected) {
		t.Fatal(err)
	}
	if _, err := c.DefaultDirectory(context.Background()); !errors.Is(err, ErrUnexpected) {
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

// TestMagnetLabelSurvivesMetadata: on a magnet, Add labels the metadata
// placeholder; when rTorrent rebuilds the torrent from the metadata the
// label is gone and Torrents sets it again, by hash, as a literal call
// (fred and Codex on PR 122). Pending entries settle when seen set, and
// expire.
func TestMagnetLabelSurvivesMetadata(t *testing.T) {
	f := newFake(t)
	var label string // what the box holds for hashA's custom1
	var sets int
	phase := "placeholder"
	f.on(func(m string, p []string) string {
		switch m {
		case "d.custom1.set":
			sets++
			label = p[1]
			return resp(i8(0))
		case "d.multicall2":
			// libtorrent 0.13.8 gives the metadata placeholder a one-byte
			// file, so size tells nothing; d.is_meta does (Codex on PR 122).
			lbl, size, meta := label, int64(1), int64(1)
			if phase == "rebuilt" {
				lbl = "" // the fresh download does not carry custom1
				phase = "rebuilt-listed"
			}
			if phase != "placeholder" {
				size, meta = 1000, 0
			}
			return resp(`<array><data>` + torrentRow(hashA, "Alpha", i8(size), i8(0), i8(0), i8(0), i8(0), i8(1), i8(0), i8(1), i8(0), i8(1), str(""), str(lbl), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), i8(meta)) + `</data></array>`)
		}
		return resp(i8(0))
	})
	c := New(f.dial)
	now := time.Now()
	c.now = func() time.Time { return now }
	res, err := c.Add(context.Background(), AddRequest{URLs: []string{"magnet:?xt=urn:btih:" + hashA}, Label: "tv"})
	if err != nil || label != "tv" || sets != 1 || res.Deferred != 1 {
		t.Fatalf("add: %v label %q sets %d %+v", err, label, sets, res)
	}
	// Still the placeholder (is_meta, one-byte file): the label shows,
	// nothing to do, and the entry must survive, since the rebuild is still
	// to come.
	rows, err := c.Torrents(context.Background())
	if err != nil || rows[0].Label != "tv" || !rows[0].Meta || rows[0].Size != 1 || sets != 1 {
		t.Fatalf("placeholder listing: %v %+v sets %d", err, rows, sets)
	}
	c.pendMu.Lock()
	_, kept := c.pending[hashA]
	c.pendMu.Unlock()
	if !kept {
		t.Fatal("a label seen on the placeholder must not settle the entry")
	}
	// Metadata arrived: rTorrent rebuilt the torrent without custom1.
	phase = "rebuilt"
	rows, err = c.Torrents(context.Background())
	if err != nil || rows[0].Label != "tv" || label != "tv" || sets != 2 {
		t.Fatalf("after rebuild: %v %+v box %q sets %d", err, rows, label, sets)
	}
	// The entry was spent on the rebuilt torrent: a label the user clears in
	// ruTorrent afterwards is not put back.
	c.pendMu.Lock()
	_, kept = c.pending[hashA]
	c.pendMu.Unlock()
	if kept {
		t.Fatal("entry must be spent once the rebuilt torrent was seen")
	}
	label = ""
	if _, err := c.Torrents(context.Background()); err != nil || sets != 2 || label != "" {
		t.Fatalf("cleared label put back: %v sets %d %q", err, sets, label)
	}
	// A rebuilt torrent seen with a different label is left alone too.
	c.remember(hashA, "tv")
	label = "films"
	if _, err := c.Torrents(context.Background()); err != nil || sets != 2 || label != "films" {
		t.Fatalf("other label overwritten: %v sets %d %q", err, sets, label)
	}
	// A setter that fails keeps the entry for the next poll (Codex on PR 122).
	c.remember(hashA, "tv")
	label = ""
	failSet := true
	f.on(func(m string, p []string) string {
		switch m {
		case "d.custom1.set":
			if failSet {
				return fault(-501, "busy")
			}
			sets++
			label = p[1]
			return resp(i8(0))
		case "d.multicall2":
			return resp(`<array><data>` + torrentRow(hashA, "Alpha", i8(1000), i8(0), i8(0), i8(0), i8(0), i8(1), i8(0), i8(1), i8(0), i8(1), str(""), str(label), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0)) + `</data></array>`)
		}
		return resp(i8(0))
	})
	if _, err := c.Torrents(context.Background()); err != nil || label != "" || len(c.Pending()) != 1 {
		t.Fatalf("failed setter must keep the entry: %v %q pending %d", err, label, len(c.Pending()))
	}
	failSet = false
	if _, err := c.Torrents(context.Background()); err != nil || label != "tv" || sets != 3 || len(c.Pending()) != 0 {
		t.Fatalf("retry: %v %q sets %d pending %d", err, label, sets, len(c.Pending()))
	}
	// Expired entries are dropped without a call.
	base := sets
	c.remember(hashB, "x")
	now = now.Add(pendingTTL + time.Minute)
	phase = "rebuilt"
	label = ""
	if _, err := c.Torrents(context.Background()); err != nil || sets != base {
		t.Fatalf("expired: %v sets %d (base %d)", err, sets, base)
	}
	c.pendMu.Lock()
	n := len(c.pending)
	c.pendMu.Unlock()
	if n != 0 {
		t.Fatalf("pending %d after expiry", n)
	}
	// The map is bounded.
	for i := 0; i < pendingMax+5; i++ {
		c.remember(fmt.Sprintf("%040X", i), "l")
	}
	c.pendMu.Lock()
	n = len(c.pending)
	c.pendMu.Unlock()
	if n != pendingMax {
		t.Fatalf("pending %d, cap %d", n, pendingMax)
	}
}

func TestPendingLabelsPersist(t *testing.T) {
	c := New(func(ctx context.Context) (net.Conn, error) { return nil, errors.New("unused") })
	now := time.Now()
	c.now = func() time.Time { return now }
	c.remember(hashA, "tv")
	saved := c.Pending()
	if len(saved) != 1 || saved[0].Hash != hashA || saved[0].Label != "tv" || !saved[0].Until.After(now) {
		t.Fatalf("%+v", saved)
	}
	// A new client (a restarted dashboard) takes them back; junk and expired
	// entries are dropped, hashes normalised, labels clipped.
	d := New(c.dial)
	d.now = c.now
	d.Restore(append(saved, PendingLabel{Hash: "nope", Label: "x", Until: now.Add(time.Hour)},
		PendingLabel{Hash: hashB, Label: "", Until: now.Add(time.Hour)},
		PendingLabel{Hash: hashB, Label: "old", Until: now.Add(-time.Minute)},
		PendingLabel{Hash: strings.ToLower(hashB), Label: strings.Repeat("l", 500), Until: now.Add(time.Hour)}))
	got := d.Pending()
	if len(got) != 2 {
		t.Fatalf("%+v", got)
	}
	for _, p := range got {
		if p.Hash == strings.ToUpper(hashB) && len(p.Label) != maxLabel {
			t.Fatalf("label not clipped: %d", len(p.Label))
		}
	}
}

// TestListWithoutIsMeta: a build that does not know d.is_meta refuses the
// whole listing with an unknown-method fault naming it; the client asks
// again without the getter and remembers. The placeholder can then not be
// told from the real torrent (it grows to the advertised metadata size
// before it is replaced, fred on PR 122), so a pending label is kept until
// the label is seen missing, which is the rebuild, and set then.
func TestListWithoutIsMeta(t *testing.T) {
	f := newFake(t)
	var lists, sets int
	label := "tv"    // what the box holds for hashA's custom1
	size := int64(1) // the placeholder's file: one byte, then the metadata size
	f.on(func(m string, p []string) string {
		switch m {
		case "d.custom1.set":
			sets++
			label = p[1]
			return resp(i8(0))
		case "d.multicall2":
			lists++
			for _, x := range p {
				if x == "d.is_meta=" {
					return fault(-506, "Method 'd.is_meta' not defined")
				}
			}
			return resp(`<array><data>` + row(str(hashA), str("Alpha"), i8(size), i8(0), i8(0), i8(0), i8(0), i8(1), i8(0), i8(1), i8(0), i8(1), str(""), str(label), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0)) + `</data></array>`)
		}
		return resp(i8(0))
	})
	c := New(f.dial)
	c.remember(hashA, "tv")
	rows, err := c.Torrents(context.Background())
	if err != nil || len(rows) != 1 || lists != 2 || rows[0].MetaKnown {
		t.Fatalf("%v rows %d lists %d %+v", err, len(rows), lists, rows)
	}
	// Remembered: the next listings go without the getter at once.
	size = 16384 // the placeholder grew to the metadata size, still a placeholder
	if _, err := c.Torrents(context.Background()); err != nil || lists != 3 || len(c.Pending()) != 1 {
		t.Fatalf("%v lists %d pending %d: a labelled placeholder of any size must not spend the entry", err, lists, len(c.Pending()))
	}
	// The rebuild: the real torrent arrives without custom1 and gets it.
	label = ""
	rows, err = c.Torrents(context.Background())
	if err != nil || sets != 1 || label != "tv" || rows[0].Label != "tv" || len(c.Pending()) != 0 {
		t.Fatalf("rebuild: %v sets %d label %q pending %d", err, sets, label, len(c.Pending()))
	}
	// A fault that is not about an unknown d.is_meta surfaces unchanged and
	// does not switch the getter off.
	g := newFake(t)
	g.on(func(m string, p []string) string { return fault(-501, "Could not find info-hash.") })
	d := New(g.dial)
	if _, err := d.Torrents(context.Background()); !isFault(err) || d.noMeta.Load() {
		t.Fatalf("%v noMeta %v", err, d.noMeta.Load())
	}
	g.on(func(m string, p []string) string { return fault(-506, "Method 'd.hash' not defined") })
	if _, err := d.Torrents(context.Background()); !isFault(err) || d.noMeta.Load() {
		t.Fatal("another missing method is not d.is_meta's fault")
	}
	for msg, want := range map[string]bool{
		"Method 'd.is_meta' not defined": true, `Command "d.is_meta" does not exist.`: true, "Unsupported command: d.is_meta": true,
		"d.is_meta: unknown": true, "Could not find info-hash.": false, "Method 'd.hash' not defined": false,
	} {
		if got := unknownMethodFault(&Fault{Code: -501, String: msg}, "d.is_meta"); got != want {
			t.Errorf("%q: %v", msg, got)
		}
	}
	if !unknownMethodFault(&Fault{Code: -506, String: "d.is_meta"}, "d.is_meta") || unknownMethodFault(errors.New("x"), "d.is_meta") {
		t.Fatal("code -506 with the name counts; a non-fault does not")
	}
}
