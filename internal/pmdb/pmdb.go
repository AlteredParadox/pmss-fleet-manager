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
// public to read (GET /api/servers lists every row) and takes one row per
// host hostname on POST /api/servers/raw with a shared upload key.
//
// Everything the catalogue sends back is untrusted: it is a third-party
// service that could be compromised or replaced, and it knows the shared
// upload key. So: a response is bounded in bytes, rows and header size,
// and the bytes counted are the ones on the wire (no transparent
// decompression: a compressed answer is refused, not decoded); the list is
// read for the one fact needed (which DNS-shaped hostnames exist),
// streamed row by row with a hard row cap; a submission's answer is its
// status code and its body is never read at all; no text from any
// response is ever kept, logged or shown (an error is a fixed message or a
// status code); transport failures are fixed messages too, because Go's
// own text carries the URL and any Location the server sent; redirects are
// refused; every request has its own connection, so bytes a server sends
// after the answer it declared are never found on an idle one (which
// net/http would quote into the standard logger). The only thing the
// catalogue's answer decides is whether a host is submitted, once.
package pmdb

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// BaseURL is the public catalogue.
const BaseURL = "https://pmdb.fyi"

// Row is one host as the catalogue's raw submission takes it. Numbers are
// JSON numbers in the unit the field name says; a nil optional is sent as
// null, which the catalogue stores as unknown.
type Row struct {
	ServerName    string   `json:"serverName"`
	ProcessorName string   `json:"processorName"`
	CoreCount     int      `json:"coreCount"`
	ProcessorMHz  *float64 `json:"processorMhz"`
	RAMGiB        float64  `json:"ramGiB"`
	SwapGiB       *float64 `json:"swapGiB"`
	StorageTiB    float64  `json:"storageTiB"`
	QSV           bool     `json:"qsv"`
}

// listed is one catalogue row as GET /api/servers returns it; only the
// name is needed to tell a host is already there, and a row whose name is
// not a string is skipped rather than failing the list.
type listed struct {
	ServerName json.RawMessage `json:"serverName"`
}

var (
	// ErrExists is a 409: the host is in the catalogue already.
	ErrExists = errors.New("host is already in the catalogue")
	// ErrAuth is a 401: the upload key was refused.
	ErrAuth = errors.New("upload key refused")
	// ErrRateLimited is a 429: too many requests from this address.
	ErrRateLimited = errors.New("rate limit reached")
	// ErrRedirect: the catalogue answered with a redirect, which is never
	// followed (the key travels in an Authorization header, and the
	// catalogue has one fixed URL).
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
	// ErrEncoded: the response is compressed or otherwise encoded, which
	// is never asked for and never decoded (the byte caps count wire
	// bytes; a gzip stream of any length can decode to two).
	ErrEncoded = errors.New("the catalogue's response is encoded; refusing to decode it")

	errRedirect = errors.New("redirect")
)

// HTTPError is an unexpected status. It carries the code and nothing else:
// the body of any response from the catalogue is untrusted text that could
// hold the upload key the catalogue knows, in any encoding, so none of it
// is kept (see the package comment).
type HTTPError struct{ Code int }

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d from the catalogue", e.Code) }

// Client is one catalogue endpoint. URL overrides BaseURL (for tests).
type Client struct {
	URL string

	once sync.Once
	hc   *http.Client
}

// Limits. The whole table is a few hundred rows; a real response is a few
// hundred KiB at most. The byte caps are wire bytes of the body as the
// transport frames it; the request deadline is the bound on everything
// else (a slow trickle, chunk framing overhead).
const (
	maxListBytes      = 4 << 20  // the list body
	maxListRows       = 10000    // elements of the list array, decoded one at a time
	maxHeaderBytes    = 64 << 10 // response headers (Go's default allows ~10 MiB)
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

// client is the one HTTP client, with its own transport: header size
// capped, redirects refused, one request deadline, no transparent
// decompression and no connection reuse.
func (c *Client) client() *http.Client {
	c.once.Do(func() {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxResponseHeaderBytes = maxHeaderBytes
		tr.ResponseHeaderTimeout = responseHeaderCap
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
// catalogue keys them (see Normalize). No key is needed. The array is
// streamed element by element, at most maxListRows of them, so a body
// made of millions of tiny elements costs the bytes it is and no more.
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

// Submit adds one host. A host already listed is ErrExists; the catalogue
// is never asked to replace a row. The answer is its status code: the body
// is never read, so an accepted submission is settled the moment the
// headers arrive, whatever the server does with the body after them.
func (c *Client) Submit(ctx context.Context, key string, row Row) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("no upload key")
	}
	body, err := json.Marshal(row)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.base()+"/api/servers/raw", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	code, _, err := c.do(ctx, req, 0)
	if err != nil {
		return err
	}
	switch code {
	case http.StatusCreated, http.StatusOK:
		return nil
	case http.StatusConflict:
		return ErrExists
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w (HTTP %d)", ErrAuth, code)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w (HTTP %d)", ErrRateLimited, code)
	}
	return &HTTPError{Code: code}
}

// Normalize keys a hostname the way the catalogue does: lower-case, no
// trailing dot, every label a letter or digit run with inner dashes. A name
// that is not DNS-shaped is refused rather than sent to be rejected.
func Normalize(h string) (string, bool) {
	h = strings.TrimSuffix(strings.ToLower(strings.TrimSpace(h)), ".")
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
