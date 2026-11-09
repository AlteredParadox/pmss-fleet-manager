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

// Package pmapi reads the Pulsed Media customer API ("Your own services"):
// https://pulsedmedia.com/remote/pmapi-feed.php authenticated with an
// X-API-Key header. The key is created in the client area (?m=pmapi).
package pmapi

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// FeedURL is the read-only services endpoint.
const FeedURL = "https://pulsedmedia.com/remote/pmapi-feed.php"

// Feed is the whole response.
type Feed struct {
	ClientID    int64     `json:"client_id"`
	GeneratedAt string    `json:"generated_at"`
	Currency    string    `json:"currency"`
	Services    []Service `json:"services"`
}

// Service is one product on the account.
type Service struct {
	ID              int64   `json:"id"`
	Product         string  `json:"product"`
	Group           string  `json:"group"`
	Domain          string  `json:"domain"`
	Status          string  `json:"status"`
	Registered      string  `json:"registered"` // YYYY-MM-DD
	NextDue         string  `json:"next_due"`   // YYYY-MM-DD
	BillingCycle    string  `json:"billing_cycle"`
	RecurringAmount Amount  `json:"recurring_amount"`
	Seedbox         *Access `json:"seedbox,omitempty"`
}

// Access is the seedbox / storage box login block.
type Access struct {
	Username         string `json:"username"`
	LoginURL         string `json:"login_url"`
	PermanentAddress string `json:"permanent_address"`
	Usage            *Usage `json:"usage,omitempty"`
}

// Usage is Pulsed Media's own accounting of the service.
type Usage struct {
	TrackedSince   string   `json:"tracked_since"`
	SentBytes      int64    `json:"sent_bytes"`
	ReceivedBytes  int64    `json:"received_bytes"`
	CPUHours       *float64 `json:"cpu_hours"` // sent rounded to one decimal, or null
	CPUSince       string   `json:"cpu_since"`
	DiskUsedBytes  int64    `json:"disk_used_bytes"`
	DiskQuotaBytes int64    `json:"disk_quota_bytes"`
	Torrents       int      `json:"torrents"`
}

// Amount decodes a money value sent as a string ("11.94", "€11.94",
// "1,234.56", "11,94") or a number. It never fails: one odd amount must not
// reject the whole feed and with it every renewal date, so an unparsable
// value decodes as 0.
type Amount float64

func (a *Amount) UnmarshalJSON(b []byte) error {
	*a = 0
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		*a = Amount(parseAmount(s)) // a JSON string, escapes decoded
		return nil
	}
	var n json.Number
	if err := json.Unmarshal(b, &n); err == nil {
		if f, err := n.Float64(); err == nil && f <= 1e12 && f >= -1e12 {
			*a = Amount(f) // a number, exponent and all; absurd ones as in parseAmount
		}
	}
	return nil
}

func parseAmount(s string) float64 {
	s = strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' || r == '.' || r == ',' || r == '-' {
			return r
		}
		return -1
	}, s)
	// Whichever separator comes last with at most two digits after it is the
	// decimal point: "1,234.56", "1.234,56", "11,94", "1,234".
	lc, ld := strings.LastIndex(s, ","), strings.LastIndex(s, ".")
	if lc < 0 && strings.Count(s, ".") > 1 {
		s = strings.ReplaceAll(s, ".", "") // "1.234.567": dots are thousands separators
	}
	switch {
	case lc > ld && len(s)-lc-1 <= 2 && strings.Count(s, ",") == 1:
		s = strings.ReplaceAll(s[:lc], ".", "") + "." + s[lc+1:]
	case lc > ld:
		s = strings.ReplaceAll(s, ",", "")
	default:
		s = strings.ReplaceAll(s, ",", "")
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(f) || f > 1e12 || f < -1e12 {
		return 0 // unparsable or absurd: no real invoice is anywhere near this
	}
	return f
}

// MonthsPerCycle maps a WHMCS billing cycle name to months; 0 = not recurring.
func MonthsPerCycle(cycle string) int {
	switch strings.ToLower(strings.TrimSpace(cycle)) {
	case "monthly":
		return 1
	case "quarterly":
		return 3
	case "semi-annually", "semiannually", "semi annually":
		return 6
	case "annually", "yearly":
		return 12
	case "biennially":
		return 24
	case "triennially":
		return 36
	}
	return 0
}

// Monthly returns the recurring amount normalised to one month.
func (s Service) Monthly() float64 {
	m := MonthsPerCycle(s.BillingCycle)
	if m == 0 {
		return 0
	}
	return float64(s.RecurringAmount) / float64(m)
}

// Username returns the service's shell/web user, if it has one.
func (s Service) Username() string {
	if s.Seedbox == nil {
		return ""
	}
	return s.Seedbox.Username
}

// ErrAuth means the key was rejected.
var ErrAuth = errors.New("API key rejected")

// Client fetches the feed. URL is overridable for tests (PMSS_PMAPI_URL).
type Client struct {
	URL string
}

// Fetch downloads and parses the feed with the given key.
func (c *Client) Fetch(ctx context.Context, key string) (*Feed, []byte, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil, errors.New("no API key configured")
	}
	// The key travels in a custom header, which Go (unlike Authorization)
	// forwards unchanged across redirects to any host or scheme. The feed
	// has one fixed URL, so a redirect is refused rather than followed.
	hc := &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		return fmt.Errorf("feed redirected to %s://%s; refusing to send the API key there", req.URL.Scheme, req.URL.Host)
	}}
	u := c.URL
	if u == "" {
		u = FeedURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("X-API-Key", key)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "pmss-fleet-manager")
	resp, err := hc.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxFeedBytes+1))
	if err != nil {
		return nil, nil, err
	}
	if len(body) > maxFeedBytes {
		return nil, nil, fmt.Errorf("feed larger than %d MiB; refusing to parse it", maxFeedBytes>>20)
	}
	switch {
	case resp.StatusCode == 401 || resp.StatusCode == 403:
		return nil, body, fmt.Errorf("%w (HTTP %d)", ErrAuth, resp.StatusCode)
	case resp.StatusCode != 200:
		return nil, body, fmt.Errorf("HTTP %d: %s", resp.StatusCode, firstLine(body))
	}
	f, err := Parse(body)
	return f, body, err
}

// maxFeedBytes bounds one feed response, and anything Parse is given (the
// cached copy included).
const maxFeedBytes = 4 << 20

// maxServices bounds the services one feed may list. An account has tens
// of services, not thousands; the byte cap alone admits over a million
// compact objects, each a 136-byte Service plus its strings, and every
// box's decoration walks the whole list (2026-11 audits). The count is
// taken from the raw array before the services are decoded, so an
// oversized feed costs its bytes once and nothing per element.
const maxServices = 1000

// Parse decodes a feed body. A syntactically valid JSON object that is not
// a feed (an error envelope, `{}` from a proxy) must not become an empty
// account that replaces the last good feed and resolves its renewal
// alerts: the services member has to be present (an explicit empty list or
// null is the real "no services"), and an error member is an error. A
// body over maxFeedBytes or listing more than maxServices is refused,
// wherever it came from.
func Parse(body []byte) (*Feed, error) {
	if len(body) > maxFeedBytes {
		return nil, fmt.Errorf("feed larger than %d MiB; refusing to parse it", maxFeedBytes>>20)
	}
	var env feedEnvelope
	if err := json.Unmarshal(body, &env); err != nil {
		return nil, fmt.Errorf("parse feed: %w (%s)", err, firstLine(body))
	}
	if len(env.Error) > 0 && string(env.Error) != "null" {
		return nil, fmt.Errorf("feed reports an error: %s", firstLine(env.Error))
	}
	if env.Services == nil {
		return nil, fmt.Errorf("feed has no services member (%s)", firstLine(body))
	}
	if n, err := countArray(env.Services, maxServices); err != nil {
		return nil, fmt.Errorf("parse feed: services: %w", err)
	} else if n > maxServices {
		return nil, fmt.Errorf("feed lists more than %d services; refusing it", maxServices)
	}
	f := &Feed{ClientID: env.ClientID, GeneratedAt: env.GeneratedAt, Currency: env.Currency}
	if string(env.Services) != "null" {
		if err := json.Unmarshal(env.Services, &f.Services); err != nil {
			return nil, fmt.Errorf("parse feed: services: %w (%s)", err, firstLine(env.Services))
		}
	}
	return f, nil
}

// feedEnvelope is Feed with its services left undecoded. The count is
// taken from, and the services then decoded from, this one value: the
// decoder matches member names without regard to case and keeps the last
// of duplicate names, so a count read off one spelling of the member and
// a decode of the whole body again could settle on different arrays (fred
// on PR 176). Absent and null differ: nil, and the four bytes.
type feedEnvelope struct {
	ClientID    int64           `json:"client_id"`
	GeneratedAt string          `json:"generated_at"`
	Currency    string          `json:"currency"`
	Services    json.RawMessage `json:"services"`
	Error       json.RawMessage `json:"error"`
}

// countArray counts the elements of a JSON array (null counts as empty)
// without decoding them into values, stopping at limit+1.
func countArray(raw json.RawMessage, limit int) (int, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		return 0, err
	}
	if tok == nil {
		return 0, nil
	}
	if d, ok := tok.(json.Delim); !ok || d != '[' {
		return 0, errors.New("not an array")
	}
	n := 0
	for dec.More() {
		var skip json.RawMessage
		if err := dec.Decode(&skip); err != nil {
			return n, err
		}
		if n++; n > limit {
			return n, nil
		}
	}
	return n, nil
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}
