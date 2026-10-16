// Package pmapi reads the Pulsed Media customer API ("Your own services"):
// https://pulsedmedia.com/remote/pmapi-feed.php authenticated with an
// X-API-Key header. The key is created in the client area (?m=pmapi).
package pmapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	TrackedSince   string `json:"tracked_since"`
	SentBytes      int64  `json:"sent_bytes"`
	ReceivedBytes  int64  `json:"received_bytes"`
	CPUHours       int64  `json:"cpu_hours"`
	CPUSince       string `json:"cpu_since"`
	DiskUsedBytes  int64  `json:"disk_used_bytes"`
	DiskQuotaBytes int64  `json:"disk_quota_bytes"`
	Torrents       int    `json:"torrents"`
}

// Amount decodes a money value sent as a string ("11.94") or a number.
type Amount float64

func (a *Amount) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		*a = 0
		return nil
	}
	f, err := strconv.ParseFloat(strings.ReplaceAll(s, ",", ""), 64)
	if err != nil {
		return fmt.Errorf("amount %q: %w", s, err)
	}
	*a = Amount(f)
	return nil
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

// Client fetches the feed. HTTP is injectable for tests.
type Client struct {
	HTTP *http.Client
	URL  string
}

// Fetch downloads and parses the feed with the given key.
func (c *Client) Fetch(ctx context.Context, key string) (*Feed, []byte, error) {
	key = strings.TrimSpace(key)
	if key == "" {
		return nil, nil, errors.New("no API key configured")
	}
	hc, u := c.HTTP, c.URL
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
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
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, nil, err
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

// Parse decodes a feed body.
func Parse(body []byte) (*Feed, error) {
	var f Feed
	if err := json.Unmarshal(body, &f); err != nil {
		return nil, fmt.Errorf("parse feed: %w (%s)", err, firstLine(body))
	}
	return &f, nil
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
