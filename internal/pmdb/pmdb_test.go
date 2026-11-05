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
		"odin":                                "odin.pulsedmedia.com", // a bare name is a catalogue host
		"Odin.":                               "odin.pulsedmedia.com",
		"odin.example":                        "odin.example", // another domain is kept as it is
		strings.Repeat("a", 240):              "",             // over the length cap once the domain is on
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
			_, _ = io.WriteString(w, `[{"serverName":"Odin.pulsedmedia.com","coreCount":16},{"serverName":"thor"}]`)
		case "/api/servers/raw":
			gotAuth, gotCT = r.Header.Get("Authorization"), r.Header.Get("Content-Type")
			body, _ := io.ReadAll(r.Body)
			gotRow = nil // decoded afresh: Unmarshal into a map keeps earlier keys
			if err := json.Unmarshal(body, &gotRow); err != nil {
				t.Errorf("body: %v", err)
			}
			w.WriteHeader(status)
			if status >= 400 {
				_, _ = io.WriteString(w, `{"error":"This node already has data under the short hostname."}`)
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

	mhz, kvm, plan, users := 2100.0, false, "M10G S", 17
	row := Row{Username: "someone", ServerName: "loki.pulsedmedia.com", ProcessorName: "Intel(R) Core(TM) i7-12700", CoreCount: 20,
		ProcessorMHz: &mhz, RAMGiB: 62.5, StorageTiB: 13.64, KVM: &kvm, Plan: &plan, UserCount: &users,
		System: &System{Distro: "Debian GNU/Linux 12 (bookworm)", Kernel: "6.1.0-37-amd64", Arch: "x86_64", VM: "NONE"}}
	if err := c.Submit(ctx, row); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" || gotCT != "application/json" {
		t.Fatalf("headers: %q %q (no credential travels; the catalogue has none)", gotAuth, gotCT)
	}
	// Every field the catalogue requires goes as a JSON number or bool;
	// unknown optionals go as null, never as 0, and nothing extra goes:
	// the catalogue refuses a row with a key it does not know, so the key
	// set is checked exactly.
	want := map[string]any{"username": "someone", "serverName": "loki.pulsedmedia.com", "processorName": "Intel(R) Core(TM) i7-12700", "coreCount": 20.0,
		"processorMhz": 2100.0, "ramGiB": 62.5, "swapGiB": nil, "storageTiB": 13.64, "kvm": false, "plan": "M10G S", "userCount": 17.0,
		"system": map[string]any{"distro": "Debian GNU/Linux 12 (bookworm)", "kernel": "6.1.0-37-amd64", "arch": "x86_64", "vm": "NONE"}}
	if len(gotRow) != len(want) {
		t.Fatalf("row keys: %v", gotRow)
	}
	for k, v := range want {
		if k == "system" {
			sys, _ := gotRow[k].(map[string]any)
			for sk, sv := range v.(map[string]any) {
				if sys[sk] != sv {
					t.Errorf("system[%s] = %v, want %v", sk, sys[sk], sv)
				}
			}
			if len(sys) != 4 {
				t.Errorf("system keys: %v", sys)
			}
			continue
		}
		if gotRow[k] != v {
			t.Errorf("row[%s] = %v, want %v", k, gotRow[k], v)
		}
	}
	for _, k := range []string{"replace", "qsv", "benchmarks"} {
		if _, has := gotRow[k]; has {
			t.Errorf("%s must never be sent", k)
		}
	}
	// The optional plan, count and system are left out, not sent as null:
	// left out, the catalogue keeps what it has.
	lean := Row{Username: "someone", ServerName: "loki.pulsedmedia.com", ProcessorName: "Intel(R) N100", CoreCount: 4, RAMGiB: 32, StorageTiB: 60}
	if err := c.Submit(ctx, lean); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"plan", "userCount", "system"} {
		if _, has := gotRow[k]; has {
			t.Errorf("%s sent when unknown: %v", k, gotRow[k])
		}
	}
	if gotRow["kvm"] != nil || gotRow["processorMhz"] != nil || gotRow["swapGiB"] != nil || len(gotRow) != 9 {
		t.Errorf("unknown hardware must go as null: %v", gotRow)
	}

	status = http.StatusOK // the catalogue replaced its row: accepted all the same
	if err := c.Submit(ctx, row); err != nil {
		t.Fatalf("200: %v", err)
	}
	status = http.StatusConflict
	if err := c.Submit(ctx, row); !errors.Is(err, ErrNameConflict) {
		t.Fatalf("409: %v", err)
	}
	status = http.StatusUnauthorized // not something the catalogue answers any more; an unexpected status like any other
	err = c.Submit(ctx, row)
	var he *HTTPError
	if !errors.As(err, &he) || he.Code != 401 {
		t.Fatalf("401: %v", err)
	}
	status = http.StatusTooManyRequests
	if err := c.Submit(ctx, row); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429: %v", err)
	}
	status = http.StatusBadRequest
	err = c.Submit(ctx, row)
	if !errors.As(err, &he) || he.Code != 400 || err.Error() != "HTTP 400 from the catalogue" {
		t.Fatalf("400: %v %+v", err, he)
	}
	gotRow = nil
	if err := c.Submit(ctx, Row{ServerName: "loki.pulsedmedia.com"}); !errors.Is(err, ErrNoIdentifier) || gotRow != nil {
		t.Fatalf("a row without an identifier must not be sent: %v %v", err, gotRow)
	}
}

func TestConfig(t *testing.T) {
	var body string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/config" || r.Method != http.MethodGet {
			t.Errorf("%s %s", r.Method, r.URL.Path)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL}
	ctx := context.Background()
	body = `{"uploadsEnabled":true,"plans":["M10G S"," Dragon-R Mushu ","",42,null,"bad\u0007plan","` + strings.Repeat("p", 101) + `"]}`
	cfg, err := c.GetConfig(ctx)
	if err != nil || !cfg.UploadsEnabled || len(cfg.Plans) != 2 || cfg.Plans[0] != "M10G S" || cfg.Plans[1] != "Dragon-R Mushu" {
		t.Fatalf("config: %+v %v", cfg, err)
	}
	body = `{"plans":["A"]}` // no word on uploads: open, as the catalogue's own config says
	if cfg, err = c.GetConfig(ctx); err != nil || !cfg.UploadsEnabled {
		t.Fatalf("default uploads: %+v %v", cfg, err)
	}
	body = `{"uploadsEnabled":false,"plans":[]}`
	if cfg, err = c.GetConfig(ctx); err != nil || cfg.UploadsEnabled || len(cfg.Plans) != 0 {
		t.Fatalf("closed: %+v %v", cfg, err)
	}
	for _, b := range []string{`[]`, `null`, `"x"`, `{"plans":1}`, `{}x`, ``} {
		body = b
		if _, err := c.GetConfig(ctx); !errors.Is(err, ErrMalformed) {
			t.Errorf("config %q: %v", b, err)
		}
	}
	body = `{"plans":[` + strings.Repeat(`"p",`, maxPlans) + `"p"]}`
	if _, err := c.GetConfig(ctx); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("too many plans: %v", err)
	}
	body = `{"plans":["` + strings.Repeat("p", maxConfigBytes) + `"]}`
	if _, err := c.GetConfig(ctx); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized config: %v", err)
	}
	status, body = http.StatusTooManyRequests, `{"error":"slow down"}`
	if _, err := c.GetConfig(ctx); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429: %v", err)
	}
	status = http.StatusInternalServerError
	var he *HTTPError
	if _, err := c.GetConfig(ctx); !errors.As(err, &he) || he.Code != 500 {
		t.Fatalf("500: %v", err)
	}
}

// The catalogue's row for one host: typed numbers, and strings reported as
// absent, repeatable or present-but-not. The history page is asked for at
// its smallest and the answer is bounded whatever size the catalogue makes
// it.
func TestNode(t *testing.T) {
	var body, gotQuery, gotPath string
	status := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.RawQuery
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL}
	ctx := context.Background()
	body = `{"server":{"serverName":"odin.pulsedmedia.com","processorMhz":2100,"swapBytes":1048576,"kvm":false,"ramBytes":1},
		"system":{"distro":" Debian GNU/Linux 12 (bookworm) ","kernel":null,"vm":"bad\u001bvalue"},"userCount":3,"history":{"speedTests":[]}}`
	n, err := c.Node(ctx, "odin.pulsedmedia.com")
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/nodes/odin.pulsedmedia.com" || gotQuery != "limit=1" {
		t.Fatalf("request: %s?%s", gotPath, gotQuery)
	}
	if n.ProcessorMHz == nil || *n.ProcessorMHz != 2100 || n.SwapBytes == nil || *n.SwapBytes != 1048576 || n.KVM == nil || *n.KVM {
		t.Fatalf("numbers: %+v", n)
	}
	if n.System["distro"] != (Text{State: TextOK, Value: "Debian GNU/Linux 12 (bookworm)"}) || n.System["kernel"].State != TextAbsent ||
		n.System["arch"].State != TextAbsent || n.System["vm"].State != TextRejected {
		t.Fatalf("strings: %+v", n.System)
	}
	// Wrong types: a number that is not one is absent (nothing numeric to
	// keep), a string that is not one is present and not repeatable.
	body = `{"server":{"processorMhz":"fast","swapBytes":1.5,"kvm":"yes"},"system":{"distro":7,"kernel":"` + strings.Repeat("k", 201) + `"}}`
	if n, err = c.Node(ctx, "odin.pulsedmedia.com"); err != nil || n.ProcessorMHz != nil || n.SwapBytes != nil || n.KVM != nil ||
		n.System["distro"].State != TextRejected || n.System["kernel"].State != TextRejected {
		t.Fatalf("wrong types: %+v %v", n, err)
	}
	body = `{"server":{"processorMhz":-5,"swapBytes":0,"kvm":true},"system":{}}`
	if n, err = c.Node(ctx, "odin.pulsedmedia.com"); err != nil || n.ProcessorMHz != nil || n.SwapBytes == nil || *n.SwapBytes != 0 || n.KVM == nil || !*n.KVM {
		t.Fatalf("bounds: %+v %v", n, err)
	}
	// null is unknown, never 0 or false: a decoder that let it through
	// would post "no swap, not KVM" for a host whose row says neither.
	body = `{"server":{"processorMhz":null,"swapBytes":null,"kvm":null},"system":{}}`
	if n, err = c.Node(ctx, "odin.pulsedmedia.com"); err != nil || n.ProcessorMHz != nil || n.SwapBytes != nil || n.KVM != nil {
		t.Fatalf("nulls: %+v %v", n, err)
	}
	body = `{"uploadsEnabled":null,"plans":["A"]}`
	if cfg, err := c.GetConfig(ctx); err != nil || !cfg.UploadsEnabled {
		t.Fatalf("null uploadsEnabled: %+v %v", cfg, err)
	}
	for _, b := range []string{`[]`, `null`, `{"system":{}}`, `{"server":1}`, `{"server":{}}x`} {
		body = b
		if _, err := c.Node(ctx, "odin.pulsedmedia.com"); !errors.Is(err, ErrMalformed) {
			t.Errorf("node %q: %v", b, err)
		}
	}
	body = `{"server":{},"history":{"pad":"` + strings.Repeat("h", maxNodeBytes) + `"}}`
	if _, err := c.Node(ctx, "odin.pulsedmedia.com"); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized node: %v", err)
	}
	status, body = http.StatusNotFound, `{"error":"Node not found."}`
	if _, err := c.Node(ctx, "odin.pulsedmedia.com"); !errors.Is(err, ErrNotListed) {
		t.Fatalf("404: %v", err)
	}
	status = http.StatusTooManyRequests
	if _, err := c.Node(ctx, "odin.pulsedmedia.com"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("429: %v", err)
	}
	status = http.StatusBadGateway
	var he *HTTPError
	if _, err := c.Node(ctx, "odin.pulsedmedia.com"); !errors.As(err, &he) || he.Code != 502 {
		t.Fatalf("502: %v", err)
	}
	// The hostname travels escaped: a name with a slash in it cannot reach
	// another route.
	status, body = http.StatusOK, `{"server":{}}`
	if _, err := c.Node(ctx, "a/../config"); err != nil || gotPath == "/api/config" || !strings.Contains(gotPath, "/api/nodes/") {
		t.Fatalf("path: %s %v", gotPath, err)
	}
}

// The catalogue's text rule, in its units: a string is taken whole or
// refused, never cut.
func TestCleanText(t *testing.T) {
	for _, c := range []struct {
		in   string
		max  int
		want string
		ok   bool
	}{
		{" Intel(R) N100 ", 300, "Intel(R) N100", true},
		{strings.Repeat("a", 64), 64, strings.Repeat("a", 64), true},
		{strings.Repeat("a", 65), 64, "", false},
		{"tab\there", 64, "", false},
		{"ctrl\x7f", 64, "", false},
		{"   ", 64, "", false},
		{"", 64, "", false},
		{"\xff\xfe", 64, "", false},
		{"Väinämöinen", 11, "Väinämöinen", true},                                       // one unit per character
		{strings.Repeat("\U0001F600", 32), 64, strings.Repeat("\U0001F600", 32), true}, // an emoji is two units
		{strings.Repeat("\U0001F600", 32) + "x", 64, "", false},
		{"ok\u200bzero width", 64, "ok\u200bzero width", true}, // not a control character to the catalogue either
	} {
		got, ok := CleanText(c.in, c.max)
		if got != c.want || ok != c.ok {
			t.Errorf("CleanText(%q, %d) = %q, %v; want %q, %v", c.in, c.max, got, ok, c.want, c.ok)
		}
	}
}

// A redirect is refused, and nothing the server put in it (a Location
// echoing something it saw, the identifier say, well-formed or not)
// survives into the error: the error is the fixed ErrRedirect. Likewise an
// error body echoing it is not kept in any form.
func TestRedirectRefusedAndTextNeverReflected(t *testing.T) {
	const key = "sentinel-identifier-of-words"
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
		err = c.Submit(ctx, Row{Username: key, ServerName: "x.example"})
		if !errors.Is(err, ErrRedirect) || strings.Contains(err.Error(), key) || strings.Contains(err.Error(), "example") {
			t.Fatalf("submit redirect %q: %v", loc, err)
		}
	}
	// An error body echoing the key, plain and percent-encoded, is not
	// kept in any form, whichever request it answers.
	location, status = "", http.StatusBadRequest
	body = `{"error":"bad key ` + key + ` and encoded ` + strings.ReplaceAll(key, "-", "%2D") + `"}`
	err := c.Submit(ctx, Row{Username: key, ServerName: "x.example"})
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
	if err := c.Submit(ctx, Row{Username: "t", ServerName: "x.example"}); !errors.As(err, &he) || he.Code != 500 {
		t.Fatalf("hostile submit error: %v", err)
	}
	// A submission's answer is never read at all: a huge error answer
	// still yields its code and nothing else.
	body = strings.Repeat("A", 1<<20)
	if err := c.Submit(ctx, Row{Username: "t", ServerName: "x.example"}); !errors.As(err, &he) || he.Code != 500 {
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
		if err := c.Submit(context.Background(), Row{Username: "t", ServerName: "x.example"}); err != nil {
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

// A server that ignores Connection: close and answers as if the connection
// were reusable still gets nothing read after a submission's status: with
// the request itself marked to close, the transport never drains the
// unread answer to keep the connection (it would otherwise read up to
// 256 KiB for up to 50 ms), it just closes. Observed from the server as
// the time until the client hangs up: a drain holds the connection for
// its full 50 ms since no body ever comes; the least of five tries is
// well under that.
func TestStatusOnlyAnswerIsNotDrained(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	closedAfter := make(chan time.Duration, 8)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer conn.Close()
				br := bufio.NewReader(conn)
				req, err := http.ReadRequest(br)
				if err != nil {
					return
				}
				_, _ = io.Copy(io.Discard, req.Body) // the submission; the next byte on br is the client's hang-up
				// A keep-alive-looking answer with a body that never comes.
				_, _ = io.WriteString(conn, "HTTP/1.1 201 Created\r\nContent-Type: application/json\r\nContent-Length: 100000\r\n\r\n")
				start := time.Now()
				_ = conn.SetReadDeadline(time.Now().Add(5 * time.Second))
				_, _ = br.ReadByte() // returns when the client hangs up
				closedAfter <- time.Since(start)
			}()
		}
	}()
	c := &Client{URL: "http://" + ln.Addr().String()}
	least := time.Hour
	for i := 0; i < 5; i++ {
		if err := c.Submit(context.Background(), Row{Username: "t", ServerName: "x.example"}); err != nil {
			t.Fatalf("try %d: %v", i, err)
		}
		select {
		case d := <-closedAfter:
			if d < least {
				least = d
			}
		case <-time.After(5 * time.Second):
			t.Fatal("the server never saw the client hang up")
		}
	}
	if least >= 40*time.Millisecond {
		t.Fatalf("the client held every connection for at least %v after the status: the answer was being drained", least)
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
	// Trust the test server's certificate and nothing else: the TLS
	// configuration under test is the production one, including the
	// protocols it advertises (a cloned DefaultTransport carries h2 in
	// its ALPN list, which an h2 server would pick while the client
	// speaks HTTP/1.1 to it: no request would ever arrive).
	tr := c.client().Transport.(*http.Transport)
	if tr.TLSClientConfig == nil {
		t.Fatal("the transport has no TLS configuration of its own")
	}
	tr.TLSClientConfig.RootCAs = srv.Client().Transport.(*http.Transport).TLSClientConfig.RootCAs
	for _, p := range tr.TLSClientConfig.NextProtos {
		if p != "http/1.1" {
			t.Fatalf("ALPN advertises %q", p)
		}
	}
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
	const sentinel = "sentinel-identifier-of-words"
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
