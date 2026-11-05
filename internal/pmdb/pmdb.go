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

// Package pmdb talks to pmdb.fyi, a community catalogue of Pulsed Media
// host hardware (https://github.com/schmintendo/pmdb). The catalogue is
// public to read (GET /api/servers lists every row, GET /api/config the
// plan names it accepts, GET /api/nodes/<host> one host's row) and public
// to write: POST /api/servers/raw takes one row per host hostname under a
// contributor-chosen identifier, replacing the row that is there.
//
// Everything the catalogue sends back is untrusted: it is a third-party
// service that could be compromised or replaced. So: a response is
// bounded in bytes, rows and header size, and the bytes counted are the
// entity's as the transport delivers them before any content decoding (no
// transparent decompression: a compressed answer is refused, not decoded;
// HTTP/1.1 chunk framing is bounded by Go's parser and the request
// deadline); the list is streamed row by row with a hard row cap; a
// submission's answer is its status code and its body is never read at
// all; transport failures are fixed messages, because Go's own text
// carries the URL and any Location the server sent; redirects are
// refused; every request has its own HTTP/1.1 connection, so bytes a
// server sends after the answer it declared are never found on an idle
// one (which net/http would quote into the standard logger) and no HTTP/2
// frame traffic sits outside the byte caps and deadlines.
//
// What the catalogue says is used for two things and nothing else: which
// hosts are listed, and what its row for a host holds, so a submission
// of ours does not turn a value another contributor supplied into
// unknown (the catalogue keeps one row per host and replaces it whole).
// Numbers and booleans from it may travel back to it for that; a string
// from it travels back only when it passes the catalogue's own text rule
// (CleanText), and is otherwise reported as present but not repeatable.
// No text from any response is ever kept in state, logged or shown: an
// error is a fixed message or a status code.
package pmdb

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
	"unicode/utf8"
)

// BaseURL is the public catalogue.
const BaseURL = "https://pmdb.fyi"

// The catalogue's text limits, in UTF-16 code units (it counts with
// JavaScript's String.length).
const (
	MaxUsername      = 64
	MaxPlan          = 100
	MaxProcessorName = 300
	MaxDistro        = 200
	MaxKernel        = 200
	MaxArch          = 64
	MaxVM            = 64
)

// System is the operating-system part of a row. An empty string is
// omitted, which the catalogue stores as unknown.
type System struct {
	Distro string `json:"distro,omitempty"`
	Kernel string `json:"kernel,omitempty"`
	Arch   string `json:"arch,omitempty"`
	VM     string `json:"vm,omitempty"` // as YABS reports it: "KVM", "NONE", ...
}

// Row is one host as the catalogue's raw submission takes it. Numbers are
// JSON numbers in the unit the field name says; a nil optional hardware
// value is sent as null, which the catalogue stores as unknown, replacing
// whatever it held. Plan and UserCount are different: omitted, the
// catalogue keeps what it has for this identifier. The field set is
// exactly what the catalogue accepts; an unknown key makes it refuse the
// whole row.
type Row struct {
	Username      string   `json:"username"` // the contributor's identifier; never published by the catalogue's current API
	ServerName    string   `json:"serverName"`
	ProcessorName string   `json:"processorName"`
	CoreCount     int      `json:"coreCount"`
	ProcessorMHz  *float64 `json:"processorMhz"`
	RAMGiB        float64  `json:"ramGiB"`
	SwapGiB       *float64 `json:"swapGiB"`
	StorageTiB    float64  `json:"storageTiB"`
	KVM           *bool    `json:"kvm"`                 // nil: unknown
	Plan          *string  `json:"plan,omitempty"`      // one of Config.Plans, or omitted
	UserCount     *int     `json:"userCount,omitempty"` // accounts on the host, or omitted
	System        *System  `json:"system,omitempty"`
}

// Config is what GET /api/config says: whether submissions are open and
// the plan names a row may carry. The names are compared with, never
// shown or logged.
type Config struct {
	UploadsEnabled bool
	Plans          []string
}

// TextState says what the catalogue holds for one string field of a row.
type TextState int

const (
	// TextAbsent: null or missing; there is nothing to preserve.
	TextAbsent TextState = iota
	// TextOK: a value this dashboard can repeat (it passes CleanText).
	TextOK
	// TextRejected: a value is there but cannot be repeated. It is not the
	// same as absent: a submission that sends nothing for it would erase
	// it.
	TextRejected
)

// Text is one string field of the catalogue's row.
type Text struct {
	State TextState
	Value string // set when State is TextOK
}

// NodeRow is what the catalogue holds for one host, the fields a
// submission of ours could turn into unknown: the optional hardware
// numbers and the system strings. A number of the wrong type counts as
// absent (nothing numeric to preserve); a string of the wrong type counts
// as present but not repeatable.
type NodeRow struct {
	ProcessorMHz *float64
	SwapBytes    *int64
	KVM          *bool
	System       map[string]Text // keyed distro, kernel, arch, vm
}

// listed is one catalogue row as GET /api/servers returns it; only the
// name is needed to tell a host is listed, and a row whose name is not a
// string is skipped rather than failing the list.
type listed struct {
	ServerName json.RawMessage `json:"serverName"`
}

var (
	// ErrNameConflict is a 409: the catalogue holds this host under a
	// legacy short name and refuses the full one until its maintainers
	// rename the entry.
	ErrNameConflict = errors.New("the catalogue holds this host under a legacy short name; its maintainers must rename it")
	// ErrNotListed is a 404 for a host's row: the catalogue has none.
	ErrNotListed = errors.New("the catalogue has no row for this host")
	// ErrNoIdentifier: a submission was asked for without an identifier,
	// which the catalogue requires. Nothing is sent.
	ErrNoIdentifier = errors.New("no submission identifier")
	// ErrRateLimited is a 429: too many requests from this address.
	ErrRateLimited = errors.New("rate limit reached")
	// ErrRedirect: the catalogue answered with a redirect, which is never
	// followed (the catalogue has one fixed URL).
	ErrRedirect = errors.New("the catalogue redirected; refusing to follow")
	// ErrTimeout: no complete answer within the deadline.
	ErrTimeout = errors.New("the catalogue did not answer in time")
	// ErrUnreachable: the request failed below HTTP (DNS, TCP, TLS, a
	// broken response, oversized headers).
	ErrUnreachable = errors.New("the catalogue could not be reached")
	// ErrTooLarge: the response exceeded its byte or row cap.
	ErrTooLarge = errors.New("the catalogue's response is larger than allowed; refusing to read it")
	// ErrNotAList: the list body is not a JSON array.
	ErrNotAList = errors.New("the catalogue list is not a JSON array")
	// ErrMalformed: a config or node answer is not the JSON object it
	// should be.
	ErrMalformed = errors.New("the catalogue's answer is not the document it should be")
	// ErrEncoded: the response is compressed or otherwise encoded, which
	// is never asked for and never decoded (the byte caps count wire
	// bytes; a gzip stream of any length can decode to two).
	ErrEncoded = errors.New("the catalogue's response is encoded; refusing to decode it")

	errRedirect = errors.New("redirect")
)

// HTTPError is an unexpected status. It carries the code and nothing else:
// the body of any response from the catalogue is untrusted text, in any
// encoding, so none of it is kept (see the package comment).
type HTTPError struct{ Code int }

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d from the catalogue", e.Code) }

// Client is one catalogue endpoint. URL overrides BaseURL (for tests).
type Client struct {
	URL string

	once sync.Once
	hc   *http.Client
}

// Limits. The whole table is a few hundred rows; a real response is a few
// hundred KiB at most. The byte caps are entity bytes before content
// decoding; the request deadline is the bound on everything else (a slow
// trickle, chunk framing overhead, which Go's parser also bounds).
const (
	maxListBytes      = 4 << 20   // the list body
	maxListRows       = 10000     // elements of the list array, decoded one at a time
	maxConfigBytes    = 256 << 10 // the config body: the catalogue allows 1000 plan names of 100 characters
	maxPlans          = 1000      // the catalogue's own cap on configured plans
	maxNodeBytes      = 256 << 10 // one host's row with one benchmark run (the page size asked for)
	maxHeaderBytes    = 64 << 10  // response headers (Go's default allows ~10 MiB)
	requestTimeout    = 30 * time.Second
	responseHeaderCap = requestTimeout
)

func (c *Client) base() string {
	if c.URL != "" {
		return strings.TrimRight(c.URL, "/")
	}
	return BaseURL
}

func noRedirect(*http.Request, []*http.Request) error { return errRedirect }

// client is the one HTTP client, with its own transport: HTTP/1.1 only,
// header size capped, redirects refused, one request deadline, no
// transparent decompression and no connection reuse.
func (c *Client) client() *http.Client {
	c.once.Do(func() {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxResponseHeaderBytes = maxHeaderBytes
		tr.ResponseHeaderTimeout = responseHeaderCap
		// HTTP/1.1 only. Over HTTP/2 the body limiter counts DATA after
		// framing (control frames, padding and empty frames cost bytes
		// and CPU it never sees), and the stream's acknowledgement and
		// clean-up writes have no deadline of their own: a peer that
		// stops reading can hold Body.Close past every timeout, and with
		// it the serialised run and the shutdown that waits for it. A
		// few requests a day do not need HTTP/2.
		tr.Protocols = new(http.Protocols)
		tr.Protocols.SetHTTP1(true)
		// Protocols alone is not enough: the cloned DefaultTransport
		// carries a TLS configuration that already advertises h2 in its
		// ALPN list, and an h2 server would select it while this client
		// speaks HTTP/1.1 (no request would ever arrive). Own TLS
		// configuration, HTTP/1.1 the only protocol offered.
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{}
		} else {
			tr.TLSClientConfig = tr.TLSClientConfig.Clone()
		}
		tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
		// The body limits must count what arrives, not what a decoder
		// makes of it: with compression on, Go would add Accept-Encoding
		// gzip and unpack the answer underneath the limiter, so a stream
		// of empty gzip members of any size would decode to "[]" and
		// pass. Off, and a Content-Encoding is refused in do.
		tr.DisableCompression = true
		// One connection per request. On a kept-alive HTTP/1.1 connection
		// Go reads ahead after the declared body; bytes the server sends
		// beyond it are "unsolicited" and quoted into the standard logger
		// (log.Printf in net/http's read loop), below anything this
		// package can intercept. A connection that is closed after its
		// answer is never idle, so never peeked at.
		tr.DisableKeepAlives = true
		c.hc = &http.Client{Transport: tr, Timeout: requestTimeout, CheckRedirect: noRedirect}
	})
	return c.hc
}

// do sends one request and returns the status and, for a 2xx when limit
// is positive, the body bounded to limit bytes; with limit 0 no body is
// read at all and the connection is dropped at the headers, so the server
// decides nothing by what it sends after them. A transport failure comes
// back as one of the fixed errors and nothing else: Go's own error text
// carries the request URL and, for a refused or malformed redirect, the
// Location the server sent, which a hostile catalogue can fill with
// anything it knows, the upload key included. That text is dropped here,
// before it can reach a log, a state or the page.
func (c *Client) do(ctx context.Context, req *http.Request, limit int64) (int, []byte, error) {
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "pmss-fleet-manager")
	// Close is on the request itself, not only on the transport: the
	// transport setting adds the Connection: close header, which a hostile
	// server can ignore and answer as if the connection were reusable, in
	// which case an early Body.Close would drain up to 256 KiB of the
	// answer for up to 50 ms to keep the connection. With the request
	// marked to close, the connection is never a candidate for reuse and
	// nothing after the status is read.
	req.Close = true
	resp, err := c.client().Do(req)
	if err != nil {
		return 0, nil, classify(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return resp.StatusCode, nil, nil // nothing in an error answer is read; the code is the whole message
	}
	if limit <= 0 {
		return resp.StatusCode, nil, nil // the status is the whole answer
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		return resp.StatusCode, nil, ErrEncoded // not asked for; never decoded (see client)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return resp.StatusCode, nil, classify(ctx, err)
	}
	if int64(len(body)) > limit {
		return resp.StatusCode, nil, ErrTooLarge
	}
	return resp.StatusCode, body, nil
}

// classify maps a transport error to a fixed one (see do).
func classify(ctx context.Context, err error) error {
	var ne interface{ Timeout() bool }
	switch {
	case errors.Is(err, errRedirect), strings.Contains(err.Error(), "Location header"):
		// The second form is a 3xx whose Location Go could not parse (or
		// that was missing): it fails before CheckRedirect is asked, with
		// the raw header in the text. Still a redirect, still refused.
		return ErrRedirect
	case ctx.Err() != nil:
		return fmt.Errorf("%w: %w", ErrUnreachable, ctx.Err()) // the caller's cancel or deadline, both ours
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return ErrTimeout
	}
	return ErrUnreachable
}

// List returns the hostnames in the catalogue, normalised the way the
// catalogue keys them (see Normalize). The array is streamed element by
// element, at most maxListRows of them, so a body made of millions of
// tiny elements costs the bytes it is and no more.
func (c *Client) List(ctx context.Context) (map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/api/servers", nil)
	if err != nil {
		return nil, err
	}
	code, body, err := c.do(ctx, req, maxListBytes)
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusOK:
		return parseList(body)
	case http.StatusTooManyRequests:
		return nil, fmt.Errorf("%w (HTTP %d)", ErrRateLimited, code)
	}
	return nil, &HTTPError{Code: code}
}

func parseList(body []byte) (map[string]bool, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	// null is not an empty catalogue: treating it as one would submit every
	// host, so only a real array counts.
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return nil, ErrNotAList
	}
	out := map[string]bool{}
	for n := 0; dec.More(); n++ {
		if n >= maxListRows {
			return nil, ErrTooLarge
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return nil, ErrNotAList
		}
		// A row that is not an object with a string name is skipped, not
		// fatal: one odd row must not hide every other host.
		var r listed
		var name string
		if json.Unmarshal(raw, &r) != nil || json.Unmarshal(r.ServerName, &name) != nil {
			continue
		}
		if h, ok := Normalize(name); ok {
			out[h] = true
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
		return nil, ErrNotAList
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrNotAList // trailing bytes after the array: not the document it claims to be
	}
	return out, nil
}

// GetConfig returns the catalogue's configuration: whether it takes
// submissions and the plan names it accepts. A plan name that is not a
// string, is empty or fails the catalogue's own text rule is left out; more
// names than the catalogue itself allows is refused as oversized.
func (c *Client) GetConfig(ctx context.Context) (Config, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/api/config", nil)
	if err != nil {
		return Config{}, err
	}
	code, body, err := c.do(ctx, req, maxConfigBytes)
	if err != nil {
		return Config{}, err
	}
	switch code {
	case http.StatusOK:
		return parseConfig(body)
	case http.StatusTooManyRequests:
		return Config{}, fmt.Errorf("%w (HTTP %d)", ErrRateLimited, code)
	}
	return Config{}, &HTTPError{Code: code}
}

func parseConfig(body []byte) (Config, error) {
	var doc struct {
		UploadsEnabled json.RawMessage   `json:"uploadsEnabled"`
		Plans          []json.RawMessage `json:"plans"`
	}
	if !objectBody(body) || json.Unmarshal(body, &doc) != nil {
		return Config{}, ErrMalformed
	}
	if len(doc.Plans) > maxPlans {
		return Config{}, ErrTooLarge
	}
	cfg := Config{UploadsEnabled: true} // the catalogue says so unless it says otherwise
	if b := boolField(doc.UploadsEnabled); b != nil {
		cfg.UploadsEnabled = *b
	}
	for _, raw := range doc.Plans {
		var s string
		if json.Unmarshal(raw, &s) != nil {
			continue
		}
		if p, ok := CleanText(s, MaxPlan); ok {
			cfg.Plans = append(cfg.Plans, p)
		}
	}
	return cfg, nil
}

// Node returns what the catalogue holds for one host (see NodeRow). The
// page of benchmark history that comes with it is asked for at its
// smallest, and the whole answer is bounded.
func (c *Client) Node(ctx context.Context, host string) (NodeRow, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/api/nodes/"+url.PathEscape(host)+"?limit=1", nil)
	if err != nil {
		return NodeRow{}, err
	}
	code, body, err := c.do(ctx, req, maxNodeBytes)
	if err != nil {
		return NodeRow{}, err
	}
	switch code {
	case http.StatusOK:
		return parseNode(body)
	case http.StatusNotFound:
		return NodeRow{}, ErrNotListed
	case http.StatusTooManyRequests:
		return NodeRow{}, fmt.Errorf("%w (HTTP %d)", ErrRateLimited, code)
	}
	return NodeRow{}, &HTTPError{Code: code}
}

// systemKeys are the strings of a row and their limits.
var systemKeys = map[string]int{"distro": MaxDistro, "kernel": MaxKernel, "arch": MaxArch, "vm": MaxVM}

func parseNode(body []byte) (NodeRow, error) {
	var doc struct {
		Server map[string]json.RawMessage `json:"server"`
		System map[string]json.RawMessage `json:"system"`
	}
	if !objectBody(body) || json.Unmarshal(body, &doc) != nil || doc.Server == nil {
		return NodeRow{}, ErrMalformed
	}
	n := NodeRow{System: map[string]Text{}}
	// The clock as the catalogue stores it: a positive MHz figure. The
	// bounds are what a real processor can be, not what a hostile row says.
	if f := numberField(doc.Server["processorMhz"]); f != nil && *f > 0 && *f < 100000 {
		n.ProcessorMHz = f
	}
	if f := numberField(doc.Server["swapBytes"]); f != nil && *f >= 0 && *f == math.Trunc(*f) && *f <= math.MaxInt64 {
		b := int64(*f)
		n.SwapBytes = &b
	}
	n.KVM = boolField(doc.Server["kvm"])
	for key, max := range systemKeys {
		raw, has := doc.System[key]
		if !has || isNull(raw) {
			n.System[key] = Text{State: TextAbsent}
			continue
		}
		var s string
		if json.Unmarshal(raw, &s) != nil {
			n.System[key] = Text{State: TextRejected}
			continue
		}
		if v, ok := CleanText(s, max); ok {
			n.System[key] = Text{State: TextOK, Value: v}
		} else {
			n.System[key] = Text{State: TextRejected}
		}
	}
	return n, nil
}

// objectBody reports whether the body is a JSON object and nothing else:
// a null, an array or trailing bytes are not the document it should be.
func objectBody(body []byte) bool {
	dec := json.NewDecoder(bytes.NewReader(body))
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return false
	}
	var raw json.RawMessage
	dec = json.NewDecoder(bytes.NewReader(body))
	if dec.Decode(&raw) != nil {
		return false
	}
	_, err := dec.Token()
	return errors.Is(err, io.EOF)
}

// isNull reports a JSON null (which Unmarshal into a number or boolean
// accepts silently, leaving the zero value: a null must stay unknown, not
// become 0 or false and travel back to the catalogue as a fact).
func isNull(raw json.RawMessage) bool {
	return len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

// numberField decodes a finite JSON number, or nil for anything else.
func numberField(raw json.RawMessage) *float64 {
	if isNull(raw) {
		return nil
	}
	var f float64
	if json.Unmarshal(raw, &f) != nil || math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return &f
}

// boolField decodes a JSON boolean, or nil for anything else.
func boolField(raw json.RawMessage) *bool {
	if isNull(raw) {
		return nil
	}
	var b bool
	if json.Unmarshal(raw, &b) != nil {
		return nil
	}
	return &b
}

// Submit posts one host's row, replacing the catalogue's row for that
// hostname (a new host is created). The answer is its status code: the
// body is never read, so an accepted submission is settled the moment the
// headers arrive, whatever the server does with the body after them.
func (c *Client) Submit(ctx context.Context, row Row) error {
	if row.Username == "" {
		return ErrNoIdentifier
	}
	body, err := json.Marshal(row)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/api/servers/raw", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	code, _, err := c.do(ctx, req, 0)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusCreated, http.StatusOK:
		return nil
	case http.StatusConflict:
		return ErrNameConflict
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w (HTTP %d)", ErrRateLimited, code)
	}
	return &HTTPError{Code: code}
}

// CleanText applies the catalogue's text rule to one string: trimmed and
// not empty, valid UTF-8, no control character, and at most maxUnits
// UTF-16 code units (the catalogue counts with JavaScript's String.length,
// so a character outside the basic plane costs two). A string is accepted
// whole or refused; it is never cut, because a cut string with a mark on
// it would be a different value, and a cut processor name a different
// processor.
func CleanText(s string, maxUnits int) (string, bool) {
	if !utf8.ValidString(s) {
		return "", false
	}
	s = strings.TrimSpace(s)
	if s == "" {
		return "", false
	}
	units := 0
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return "", false
		}
		n := utf16.RuneLen(r)
		if n < 0 {
			return "", false
		}
		units += n
		if units > maxUnits {
			return "", false
		}
	}
	return s, true
}

// Normalize keys a hostname the way the catalogue does: lower-case, no
// trailing dot, a bare name given the catalogue's domain, every label a
// letter or digit run with inner dashes. A name that is not DNS-shaped is
// refused rather than sent to be rejected.
func Normalize(h string) (string, bool) {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
	if h != "" && !strings.Contains(h, ".") {
		h += ".pulsedmedia.com"
	}
	if h == "" || len(h) > 253 {
		return "", false
	}
	for _, label := range strings.Split(h, ".") {
		if label == "" || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return "", false
		}
		for _, r := range label {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '-' {
				return "", false
			}
		}
	}
	return h, true
}
