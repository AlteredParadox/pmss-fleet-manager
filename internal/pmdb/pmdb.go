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
// service that could be compromised or replaced. A response is bounded in
// size, read only for the one fact needed (which hostnames exist, as
// DNS-shaped names), an error body never reaches the page (the UI sees
// the status code; the body goes to the log stripped to printable ASCII
// and clipped), and redirects are refused. The only thing the catalogue's
// answer decides is whether a host is submitted, once.
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
	// ErrRateLimited is a 429: too many uploads from this address.
	ErrRateLimited = errors.New("upload rate limit reached")
)

// HTTPError is an unexpected status. Error() names only the code, which is
// what the page shows; Body is the catalogue's message made safe for a log
// line (printable ASCII, clipped) and is never shown to a person in the
// dashboard, where it could carry whatever a compromised service wanted
// read.
type HTTPError struct {
	Code int
	Body string
}

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d from the catalogue", e.Code) }

func httpErr(code int, body []byte) error { return &HTTPError{Code: code, Body: safeText(body)} }

// Client is one catalogue endpoint. URL overrides BaseURL (for tests).
type Client struct {
	URL string
}

// maxBodyBytes bounds one response: the whole table is a few hundred rows.
const maxBodyBytes = 4 << 20

func (c *Client) base() string {
	if c.URL != "" {
		return strings.TrimRight(c.URL, "/")
	}
	return BaseURL
}

// The key travels in an Authorization header, which Go drops on a
// cross-host redirect but keeps on a same-host one, and the catalogue has
// one fixed URL: refuse every redirect rather than reason about it.
func noRedirect(req *http.Request, _ []*http.Request) error {
	return fmt.Errorf("pmdb redirected to %s://%s; refusing to follow", req.URL.Scheme, req.URL.Host)
}

func (c *Client) do(ctx context.Context, req *http.Request) (int, []byte, error) {
	hc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: noRedirect}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "pmss-fleet-manager")
	resp, err := hc.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBodyBytes+1))
	if err != nil {
		return resp.StatusCode, nil, err
	}
	if len(body) > maxBodyBytes {
		return resp.StatusCode, nil, fmt.Errorf("response larger than %d MiB; refusing to parse it", maxBodyBytes>>20)
	}
	return resp.StatusCode, body, nil
}

// List returns the hostnames in the catalogue, normalised the way the
// catalogue keys them (see Normalize). No key is needed.
func (c *Client) List(ctx context.Context) (map[string]bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.base()+"/api/servers", nil)
	if err != nil {
		return nil, err
	}
	code, body, err := c.do(ctx, req)
	if err != nil {
		return nil, err
	}
	if code != http.StatusOK {
		return nil, httpErr(code, body)
	}
	var rows []json.RawMessage
	if err := json.Unmarshal(body, &rows); err != nil || rows == nil {
		// null is not an empty catalogue: treating it as one would submit
		// every host, so only a real array counts.
		return nil, errors.New("the catalogue list is not a JSON array")
	}
	out := make(map[string]bool, len(rows))
	for _, raw := range rows {
		// A row that is not an object with a string name is skipped, not
		// fatal: one odd row must not hide every other host.
		var r listed
		var name string
		if json.Unmarshal(raw, &r) != nil || json.Unmarshal(r.ServerName, &name) != nil {
			continue
		}
		if n, ok := Normalize(name); ok {
			out[n] = true
		}
	}
	return out, nil
}

// Submit adds one host. A host already listed is ErrExists; the catalogue
// is never asked to replace a row.
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
	code, resp, err := c.do(ctx, req)
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
	return httpErr(code, resp)
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

// safeText makes a response body fit for one log line: the catalogue's
// error member if the body is its {"error": "..."} envelope, else the body
// itself, reduced to printable ASCII (no control characters, no escape
// sequences, nothing a terminal or log viewer would interpret) and clipped.
func safeText(b []byte) string {
	var env struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(b, &env) == nil && env.Error != "" {
		b = []byte(env.Error)
	}
	var sb strings.Builder
	for _, c := range b {
		if sb.Len() >= 200 {
			sb.WriteString("…")
			break
		}
		if c >= 0x20 && c < 0x7f {
			sb.WriteByte(c)
		} else if c == '\n' || c == '\t' {
			sb.WriteByte(' ')
		}
	}
	return strings.TrimSpace(sb.String())
}
