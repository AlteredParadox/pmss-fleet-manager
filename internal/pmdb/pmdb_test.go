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

package pmdb

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestNormalize(t *testing.T) {
	for in, want := range map[string]string{
		"Le4-0-105-238louhi.PulsedMedia.com.": "le4-0-105-238louhi.pulsedmedia.com",
		" odin.pulsedmedia.com ":              "odin.pulsedmedia.com",
		"odin":                                "odin",
		"":                                    "",
		"-bad.example":                        "",
		"bad-.example":                        "",
		"under_score.example":                 "",
		"a..b":                                "",
		"sp ace.example":                      "",
	} {
		got, ok := Normalize(in)
		if got != want || ok != (want != "") {
			t.Errorf("Normalize(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
}

func TestListAndSubmit(t *testing.T) {
	var gotAuth, gotCT string
	var gotRow map[string]any
	status := http.StatusCreated
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/servers":
			if r.Method != http.MethodGet {
				t.Errorf("list method %s", r.Method)
			}
			_, _ = io.WriteString(w, `[{"serverName":"Odin.pulsedmedia.com","coreCount":16},{"serverName":"thor.pulsedmedia.com"}]`)
		case "/api/servers/raw":
			gotAuth, gotCT = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &gotRow); err != nil {
				t.Errorf("body: %v", err)
			}
			w.WriteHeader(status)
			if status >= 400 {
				_, _ = io.WriteString(w, `{"error":"This server already exists."}`)
			}
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL + "/"}
	ctx := context.Background()

	listed, err := c.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !listed["odin.pulsedmedia.com"] || !listed["thor.pulsedmedia.com"] || len(listed) != 2 {
		t.Fatalf("listed: %v", listed)
	}

	mhz := 2100.0
	row := Row{ServerName: "loki.pulsedmedia.com", ProcessorName: "Intel(R) Core(TM) i7-12700", CoreCount: 20,
		ProcessorMHz: &mhz, RAMGiB: 62.5, StorageTiB: 13.64, QSV: true}
	if err := c.Submit(ctx, " key-words-that-pass ", row); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer key-words-that-pass" || gotCT != "application/json" {
		t.Fatalf("headers: %q %q", gotAuth, gotCT)
	}
	// Every field the catalogue requires goes as a JSON number or bool;
	// unknown optionals go as null, never as 0, and nothing extra goes.
	want := map[string]any{"serverName": "loki.pulsedmedia.com", "processorName": "Intel(R) Core(TM) i7-12700", "coreCount": 20.0,
		"processorMhz": 2100.0, "ramGiB": 62.5, "swapGiB": nil, "storageTiB": 13.64, "qsv": true}
	if len(gotRow) != len(want) {
		t.Fatalf("row keys: %v", gotRow)
	}
	for k, v := range want {
		if gotRow[k] != v {
			t.Errorf("row[%s] = %v, want %v", k, gotRow[k], v)
		}
	}
	if _, has := gotRow["replace"]; has {
		t.Error("replace must never be sent")
	}

	status = http.StatusConflict
	if err := c.Submit(ctx, "a-passphrase-of-words", row); !errors.Is(err, ErrExists) {
		t.Fatalf("409: %v", err)
	}
	status = http.StatusUnauthorized
	if err := c.Submit(ctx, "a-passphrase-of-words", row); !errors.Is(err, ErrAuth) {
		t.Fatalf("401: %v", err)
	}
	status = http.StatusTooManyRequests
	if err := c.Submit(ctx, "a-passphrase-of-words", row); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429: %v", err)
	}
	status = http.StatusBadRequest
	err = c.Submit(ctx, "a-passphrase-of-words", row)
	var he *HTTPError
	if !errors.As(err, &he) || he.Code != 400 || err.Error() != "HTTP 400 from the catalogue" {
		t.Fatalf("400: %v %+v", err, he)
	}
	if err := c.Submit(ctx, "", row); err == nil {
		t.Fatal("empty key must not be sent")
	}
}

// A redirect is refused, and nothing the server put in it (a Location
// echoing the key it just saw, well-formed or not) survives into the
// error: the error is the fixed ErrRedirect. Likewise an error body echoing
// the key is not kept in any form.
func TestRedirectRefusedAndKeyNeverReflected(t *testing.T) {
	const key = "sentinel-passphrase-of-words"
	var location, body string
	status := http.StatusFound
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if location != "" {
			w.Header().Set("Location", location)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL}
	ctx := context.Background()
	for _, loc := range []string{
		"https://elsewhere.example/api/servers",
		"https://invalid.example/%zz?upload_key=" + key,
		"https://evil.example/?k=" + key,
		"/relative?key=" + key,
		"http://[::1",
	} {
		location, body, status = loc, "", http.StatusFound
		_, err := c.List(ctx)
		if !errors.Is(err, ErrRedirect) || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "example") {
			t.Fatalf("list redirect %q: %v", loc, err)
		}
		err = c.Submit(ctx, key, Row{ServerName: "x.example"})
		if !errors.Is(err, ErrRedirect) || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "example") {
			t.Fatalf("submit redirect %q: %v", loc, err)
		}
	}
	// An error body echoing the key, plain and percent-encoded, is not
	// kept in any form, whichever request it answers.
	location, status = "", http.StatusBadRequest
	body = `{"error":"bad key ` + key + ` and encoded ` + strings.ReplaceAll(key, "-", "%2D") + `"}`
	err := c.Submit(ctx, key, Row{ServerName: "x.example"})
	var he *HTTPError
	if !errors.As(err, &he) || he.Code != 400 || strings.Contains(err.Error(), key) {
		t.Fatalf("submission: %v", err)
	}
	if _, err = c.List(ctx); !errors.As(err, &he) || he.Code != 400 || strings.Contains(err.Error(), key) {
		t.Fatalf("list: %v", err)
	}
}

// A transport failure is one of the fixed errors, never Go's own text
// (which names the URL and whatever the server sent).
func TestTransportErrorsAreFixed(t *testing.T) {
	c := &Client{URL: "http://127.0.0.1:1"} // nothing listens there
	_, err := c.List(context.Background())
	if !errors.Is(err, ErrUnreachable) || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("unreachable: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.List(ctx); !errors.Is(err, ErrUnreachable) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled: %v", err)
	}
}

// What comes back from the catalogue is untrusted: junk rows, non-string
// names, invalid hostnames and a non-array body must not break the list or
// let anything through unvalidated, an oversized body is refused, and an
// error message never reaches the page as it was sent.
func TestHostileResponses(t *testing.T) {
	var body string
	listStatus := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/servers/raw" {
			w.WriteHeader(500)
		} else {
			w.WriteHeader(listStatus)
		}
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL}
	ctx := context.Background()

	body = `[{"serverName":"Good.pulsedmedia.com"},{"serverName":42},{"serverName":null},"string",17,null,{"x":1},
		{"serverName":"bad host.example"},{"serverName":"<script>alert(1)</script>"},{"serverName":"` + strings.Repeat("a", 300) + `"},
		{"serverName":"ok2.example.","coreCount":{"deep":[[[[1]]]]}}]`
	got, err := c.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || !got["good.pulsedmedia.com"] || !got["ok2.example"] {
		t.Fatalf("list: %v", got)
	}
	for _, b := range []string{`{"servers":[]}`, `"x"`, `null`, `[`, ``, `<html>`, `[1,2`, `[{"serverName":"a.example"}]x`} {
		body = b
		if _, err := c.List(ctx); !errors.Is(err, ErrNotAList) {
			t.Errorf("list %q: %v", b, err)
		}
	}
	body = "[" + strings.Repeat(`{"serverName":"a.example"},`, maxListBytes/26) + `{"serverName":"a.example"}]`
	if _, err := c.List(ctx); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized list: %v", err)
	}
	// A flood of tiny elements well under the byte cap must cost no more
	// than the bytes it is: the row cap ends it.
	body = "[" + strings.Repeat("0,", maxListRows*3) + "0]"
	if _, err := c.List(ctx); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("row flood: %v", err)
	}
	body = "[" + strings.Repeat("0,", maxListRows-2) + `{"serverName":"last.example"}]`
	if got, err := c.List(ctx); err != nil || len(got) != 1 || !got["last.example"] {
		t.Fatalf("just under the row cap: %v %v", got, err)
	}

	// No text from an error body is kept, for the list or a submission:
	// the error is the status code and nothing else.
	listStatus = http.StatusTooManyRequests
	body = `{"error":"slow down"}`
	if _, err := c.List(ctx); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("list 429: %v", err)
	}
	listStatus = http.StatusInternalServerError
	body = `{"error":"\u001b[31mvisit evil.example\nand paste your key\u0007<b>now</b>"}`
	_, err = c.List(ctx)
	var he *HTTPError
	if !errors.As(err, &he) || he.Code != 500 || err.Error() != "HTTP 500 from the catalogue" {
		t.Fatalf("hostile list error: %v", err)
	}
	if err := c.Submit(ctx, "a-passphrase-of-words", Row{ServerName: "x.example"}); !errors.As(err, &he) || he.Code != 500 {
		t.Fatalf("hostile submit error: %v", err)
	}
	// A submission's answer is never read at all: a huge error answer
	// still yields its code and nothing else.
	body = strings.Repeat("A", 1<<20)
	if err := c.Submit(ctx, "a-passphrase-of-words", Row{ServerName: "x.example"}); !errors.As(err, &he) || he.Code != 500 {
		t.Fatalf("oversized error answer: %v", err)
	}
	body = strings.Repeat("A", maxListBytes+1)
	if _, err := c.List(ctx); !errors.As(err, &he) || he.Code != 500 {
		t.Fatalf("oversized list error answer: %v", err)
	}
	listStatus, body = http.StatusOK, strings.Repeat("A", maxListBytes+1)
	if _, err := c.List(ctx); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized accepted list: %v", err)
	}
}

// An accepted submission is settled by its status: the body, oversized,
// endless or compressed, is never read, so the server cannot turn an
// acceptance it already gave into a failure that gets the host posted
// again.
func TestAcceptedAnswerBodyIsNotRead(t *testing.T) {
	done := make(chan struct{})
	defer close(done)
	var mode atomic.Value // the client appends its own path, so the mode travels beside the server, not in the URL
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served.Add(1)
		switch mode.Load() {
		case "huge":
			w.WriteHeader(http.StatusCreated)
			_, _ = io.WriteString(w, strings.Repeat("A", 1<<20))
		case "endless":
			w.WriteHeader(http.StatusCreated)
			w.(http.Flusher).Flush()
			junk := []byte(strings.Repeat("A", 4096))
			for {
				if _, err := w.Write(junk); err != nil {
					return // the client hung up, as it should
				}
				select {
				case <-done:
					return
				case <-r.Context().Done():
					return
				default:
				}
			}
		case "gzip":
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write(gzipped(t, []byte(`{"ok":true}`)))
		default:
			t.Errorf("unexpected mode %v", mode.Load())
		}
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL}
	for i, m := range []string{"huge", "endless", "gzip"} {
		mode.Store(m)
		start := time.Now()
		if err := c.Submit(context.Background(), "a-passphrase-of-words", Row{ServerName: "x.example"}); err != nil {
			t.Fatalf("%s accepted answer: %v", m, err)
		}
		if d := time.Since(start); d > 10*time.Second {
			t.Fatalf("%s accepted answer took %v: the body was waited for", m, d)
		}
		if n := served.Load(); int(n) != i+1 {
			t.Fatalf("%s: %d requests served", m, n)
		}
	}
}

func gzipped(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// A compressed list is refused, not decoded: compression is never asked
// for (Accept-Encoding: identity, no transparent gzip in the transport),
// and a gzip stream can be built from any number of empty members so that
// a wire body well past the cap decodes to "[]". The cap counts wire bytes.
func TestCompressedListRefused(t *testing.T) {
	empty := gzipped(t, nil)
	var wire bytes.Buffer
	for wire.Len() <= maxListBytes {
		wire.Write(empty)
	}
	wire.Write(gzipped(t, []byte(`[]`)))
	var sawIdentity atomic.Bool
	var mode atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawIdentity.Store(r.Header.Get("Accept-Encoding") == "identity")
		w.Header().Set("Content-Encoding", "gzip")
		switch mode.Load() {
		case "small":
			_, _ = w.Write(gzipped(t, []byte(`[{"serverName":"odin.pulsedmedia.com"}]`)))
		case "huge":
			_, _ = w.Write(wire.Bytes())
		default:
			t.Errorf("unexpected mode %v", mode.Load())
		}
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL}
	for _, m := range []string{"huge", "small"} {
		mode.Store(m)
		if _, err := c.List(context.Background()); !errors.Is(err, ErrEncoded) {
			t.Fatalf("%s compressed list: %v", m, err)
		}
		if !sawIdentity.Load() {
			t.Fatal("the request did not ask for an identity encoding")
		}
	}
}

// The catalogue is spoken to over HTTP/1.1 only: over HTTP/2 the body caps
// would count DATA after framing and the stream's own acknowledgement and
// clean-up writes have no deadline, so a peer that stops reading could
// hold a run past every timeout. A server offering h2 gets HTTP/1.1.
func TestHTTP1Only(t *testing.T) {
	var proto atomic.Value
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proto.Store(r.Proto)
		_, _ = io.WriteString(w, `[]`)
	}))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	defer srv.Close()
	c := &Client{URL: srv.URL}
	// Trust the test server's certificate; everything else about the
	// transport is the production one.
	pool := srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	c.client().Transport.(*http.Transport).TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if _, err := c.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if p := proto.Load(); p != "HTTP/1.1" {
		t.Fatalf("negotiated %v; want HTTP/1.1", p)
	}
}

// Bytes a server sends after the body it declared never reach the standard
// logger: on a kept-alive HTTP/1.1 connection Go's transport would find
// them on the idle connection and log.Printf them, quoted, below anything
// this package can intercept. Every request gets its own connection and
// says so (Connection: close). A raw listener is needed because an
// http.Server never frames a response that way.
func TestTrailingBytesNeverReachTheStandardLogger(t *testing.T) {
	const sentinel = "sentinel-passphrase-of-words"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	var notClosing atomic.Int32
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				req, err := http.ReadRequest(bufio.NewReader(conn))
				if err != nil {
					return
				}
				if !req.Close {
					notClosing.Add(1)
				}
				_, _ = io.WriteString(conn, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: 2\r\n\r\n[]"+sentinel)
				time.Sleep(50 * time.Millisecond) // leave the extra bytes readable before closing
			}()
		}
	}()
	var logged bytes.Buffer
	prev := log.Writer()
	log.SetOutput(&logged)
	defer log.SetOutput(prev)
	c := &Client{URL: "http://" + ln.Addr().String()}
	for i := 0; i < 10; i++ {
		got, err := c.List(context.Background())
		if err != nil || len(got) != 0 {
			t.Fatalf("list %d: %v %v", i, got, err)
		}
	}
	time.Sleep(100 * time.Millisecond) // a read loop that was going to peek has by now
	if n := notClosing.Load(); n != 0 {
		t.Fatalf("%d requests did not ask for the connection to close", n)
	}
	if out := logged.String(); strings.Contains(out, sentinel) || strings.Contains(out, "Unsolicited") {
		t.Fatalf("response bytes reached the standard logger: %q", out)
	}
}

// Response headers are capped well below Go's default.
func TestOversizedHeadersRefused(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Junk", strings.Repeat("j", maxHeaderBytes+1))
		_, _ = io.WriteString(w, `[]`)
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL}
	if _, err := c.List(context.Background()); !errors.Is(err, ErrUnreachable) {
		t.Fatalf("oversized headers: %v", err)
	}
}
