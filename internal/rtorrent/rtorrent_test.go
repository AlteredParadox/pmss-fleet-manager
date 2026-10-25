package rtorrent

import (
	"bufio"
	"bytes"
	"context"
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
			torrentRow(hashB, "Beta", "<i4>2000</i4>", "<int>500</int>", i8(99), i8(0), i8(250), i8(0), i8(0), i8(0), i8(0), i8(0), str("Tracker: [Failure reason]"), str(""), i8(0), i8(0), i8(0), i8(0), i8(500), i8(0)) +
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
		return resp(`<array><data>` + torrentRow(hashA, long) + torrentRow(hashB, bad, i8(1), i8(1), i8(1), i8(1), i8(1), i8(1), i8(1), i8(1), i8(0), i8(1), str(strings.Repeat("m", 500)), str(strings.Repeat("l", 500)), i8(0), i8(0), i8(0), i8(0), i8(0), i8(0)) + `</data></array>`)
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
	f.on(func(m string, p []string) string {
		if m == "load.raw_start" && p[1] != "ZDg6YW5ub3VuY2Vl" { // base64("d8:announcee")
			t.Errorf("raw params %q", p)
		}
		if strings.Contains(p[1], "bad.example") {
			return fault(-501, "Could not create download")
		}
		return resp(i8(0))
	})
	c := New(f.dial)
	res, err := c.Add(context.Background(), AddRequest{
		URLs:  []string{"magnet:?xt=urn:btih:" + hashA, "https://bad.example/x.torrent", "/etc/passwd", "file:///x"},
		Files: [][]byte{[]byte("d8:announcee")},
		Label: `tv,"d.directory.set=/tmp"`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Added != 2 || res.Failed != 3 {
		t.Fatalf("%+v", res)
	}
	methods := f.methods()
	if strings.Join(methods, ",") != "load.start,load.start,load.raw_start" {
		t.Fatalf("methods %v (paths and file: URLs never reach the box)", methods)
	}
	// The label cannot smuggle a second command.
	if got := f.last().params[2]; got != "d.custom1.set=tvd.directory.set/tmp" {
		t.Fatalf("label arg %q", got)
	}
	// Stopped uses the non-starting loaders.
	f.on(func(m string, p []string) string { return resp(i8(0)) })
	if _, err := c.Add(context.Background(), AddRequest{URLs: []string{"magnet:?x"}, Files: [][]byte{{1}}, Stopped: true}); err != nil {
		t.Fatal(err)
	}
	m := f.methods()
	if strings.Join(m[len(m)-2:], ",") != "load.normal,load.raw" {
		t.Fatalf("%v", m)
	}
	// Every URL refused by rTorrent: the fault surfaces.
	f.on(func(m string, p []string) string { return fault(-501, "no") })
	if _, err := c.Add(context.Background(), AddRequest{URLs: []string{"magnet:?x"}}); !isFault(err) {
		t.Fatal(err)
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
