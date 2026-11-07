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

// Package pmplans reads the CPU and I/O priority of each Pulsed Media plan
// from the public campaign page (https://pulsedmedia.com/eternal-vainamoinen.php),
// the one place outside the client area that prints them. Every plan card
// there carries a line "I/O: Standard (100)" and one "4 CPU cores ·
// Performance (500)": a tier label and the systemd weight behind it (the
// wiki's "Seedbox and Storage Box Resource Tiers" page explains the ladder).
//
// The page is public and the request carries nothing about the account; its
// content is untrusted all the same: the body is bounded, the markup is read
// for the card lines only, every string kept is cleaned and clipped, and the
// weights are checked against the ladder's range.
package pmplans

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// PageURL is the campaign page.
const PageURL = "https://pulsedmedia.com/eternal-vainamoinen.php"

// Plan is one listing on the page.
type Plan struct {
	Name string `json:"name"`
	// PID is the WHMCS product id from the card's order link, 0 when the
	// card has none (a sold-out listing has no link).
	PID int `json:"pid,omitempty"`
	// Cores is the CPU core count printed on the CPU line (fractional on
	// some specials).
	Cores     float64 `json:"cores,omitempty"`
	CPUTier   string  `json:"cpu_tier"`
	CPUWeight int     `json:"cpu_weight"`
	IOTier    string  `json:"io_tier"`
	IOWeight  int     `json:"io_weight"`
}

// Catalogue is what one read of the page yields.
type Catalogue struct {
	FetchedAt time.Time `json:"fetched_at"`
	Plans     []Plan    `json:"plans"`
	// Labels is the page's own product-id to product-name map (the names
	// WHMCS uses, which is what the customer API reports), by pid. A card
	// whose title differs from the WHMCS name ("🏆 Purnu Trophy" against
	// "Purnu Trophy") is found through it.
	Labels map[int]string `json:"labels,omitempty"`
}

// Lookup outcomes.
const (
	LookupMatched   = "matched"
	LookupUnknown   = "unknown"   // no listing carries the name
	LookupAmbiguous = "ambiguous" // listings with the name disagree
)

// Lookup finds the plan sold under a product name. The customer API's
// names are the page's card titles dressed up: a campaign prefix ("Eternal
// Väinämöinen - Seedbox 1TB R5 10G") and marketing after the plan ("Dragon-R
// Trophy 100 Years GiveAway", "Arkku Trophy Storage Box 6TB R5 10G -
// GiveAway 8000€", "Dragon-R DDoS Recovery Special 1TB"). So the name is
// compared word by word (Words): a listing matches when its words open the
// name, and the longest such listing wins (the exact one when there is one;
// "SSD Seedbox 1TB 10G Special" over "SSD Seedbox 1TB 10G"). A card's
// title and, through its order link, the page's own WHMCS name for its
// product id both count. When the listings that match best disagree on the
// priorities (the page sells two "SSD Seedbox 250GiB 10G" with different
// ones) the name is ambiguous and nothing is returned.
func (c *Catalogue) Lookup(name string) (*Plan, string) {
	if c == nil {
		return nil, LookupUnknown
	}
	key := Words(name)
	if len(key) == 0 {
		return nil, LookupUnknown
	}
	var found *Plan
	best, ambiguous := 0, false
	for i := range c.Plans {
		p := &c.Plans[i]
		n := prefixLen(Words(p.Name), key)
		if p.PID != 0 {
			if m := prefixLen(Words(c.Labels[p.PID]), key); m > n {
				n = m
			}
		}
		switch {
		case n == 0 || n < best:
			continue
		case n > best:
			found, best, ambiguous = p, n, false // a longer listing supersedes
		case found.CPUWeight != p.CPUWeight || found.IOWeight != p.IOWeight:
			ambiguous = true // two listings of this length disagree
		}
	}
	if best == 0 {
		return nil, LookupUnknown
	}
	if ambiguous {
		return nil, LookupAmbiguous
	}
	p := *found
	return &p, LookupMatched
}

// prefixLen is len(listing) when the listing's words open the name, else 0.
func prefixLen(listing, name []string) int {
	if len(listing) == 0 || len(listing) > len(name) {
		return 0
	}
	for i, w := range listing {
		if name[i] != w {
			return 0
		}
	}
	return len(listing)
}

// campaignWords open the API's product names and the page's group name;
// they are not part of any plan name.
var campaignWords = map[string]bool{"the": true, "eternal": true, "väinämöinen": true, "vainamoinen": true}

// Words folds a product name for comparison: its runs of letters and digits,
// lower case, with the campaign's own words dropped from the front, so
// "🏆 Purnu Trophy", "Purnu  Trophy", "purnu-trophy" and "Eternal
// Väinämöinen - Purnu Trophy" are all ["purnu", "trophy"].
func Words(s string) []string {
	var out []string
	var b strings.Builder
	flush := func() {
		if b.Len() == 0 {
			return
		}
		w := b.String()
		b.Reset()
		if len(out) == 0 && campaignWords[w] {
			return
		}
		out = append(out, w)
	}
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			flush()
		}
	}
	flush()
	return out
}

// Normalize is a product name's Words joined, one string per name for use
// as a key.
func Normalize(s string) string { return strings.Join(Words(s), " ") }

// Tier is the label the ladder gives a weight (the wiki's table): Archive
// below 100, Standard to 499, Performance to 999, High from 1000.
func Tier(weight int) string {
	switch {
	case weight <= 0:
		return ""
	case weight < 100:
		return "Archive"
	case weight < 500:
		return "Standard"
	case weight < 1000:
		return "Performance"
	default:
		return "High"
	}
}

// MaxWeight is the top of systemd's weight scale, the page's unit.
const MaxWeight = 10000

// Limits on what is read from the page: a card is a few hundred bytes and
// there are a few dozen; the page as a whole is well under half a MiB.
const (
	maxPageBytes   = 2 << 20
	maxCards       = 200
	maxNameLen     = 100
	maxLabels      = 1000
	maxHeaderBytes = 64 << 10
	requestTimeout = 30 * time.Second
)

// Fixed errors: nothing a server sends reaches a log or the page.
var (
	ErrTooLarge    = errors.New("the plan page is larger than expected; refusing to read it")
	ErrEncoded     = errors.New("the plan page came compressed, which was not asked for")
	ErrRedirect    = errors.New("the plan page redirected; refusing to follow")
	ErrTimeout     = errors.New("the plan page did not answer in time")
	ErrUnreachable = errors.New("the plan page could not be fetched")
	ErrNoPlans     = errors.New("no plan cards found on the page; its layout may have changed")
	errRedirect    = errors.New("redirect")
)

// HTTPError is a non-2xx answer.
type HTTPError struct{ Code int }

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP %d from the plan page", e.Code) }

// Client fetches the page. URL overrides PageURL (for tests).
type Client struct {
	URL string

	once sync.Once
	hc   *http.Client
}

func noRedirect(*http.Request, []*http.Request) error { return errRedirect }

// client is the one HTTP client: HTTP/1.1 only, headers capped, redirects
// refused, no transparent decompression (the byte limit must count what
// arrives) and one connection per request (the pmdb client's reasons).
func (c *Client) client() *http.Client {
	c.once.Do(func() {
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.MaxResponseHeaderBytes = maxHeaderBytes
		tr.ResponseHeaderTimeout = requestTimeout
		tr.Protocols = new(http.Protocols)
		tr.Protocols.SetHTTP1(true)
		if tr.TLSClientConfig == nil {
			tr.TLSClientConfig = &tls.Config{}
		} else {
			tr.TLSClientConfig = tr.TLSClientConfig.Clone()
		}
		tr.TLSClientConfig.NextProtos = []string{"http/1.1"}
		tr.DisableCompression = true
		tr.DisableKeepAlives = true
		c.hc = &http.Client{Transport: tr, Timeout: requestTimeout, CheckRedirect: noRedirect}
	})
	return c.hc
}

// Fetch reads and parses the page.
func (c *Client) Fetch(ctx context.Context) (*Catalogue, error) {
	u := c.URL
	if u == "" {
		u = PageURL
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, ErrUnreachable
	}
	req.Header.Set("Accept", "text/html")
	req.Header.Set("Accept-Encoding", "identity")
	req.Header.Set("User-Agent", "pmss-fleet-manager")
	req.Close = true
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, classify(ctx, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, &HTTPError{Code: resp.StatusCode}
	}
	if ce := resp.Header.Get("Content-Encoding"); ce != "" && !strings.EqualFold(ce, "identity") {
		return nil, ErrEncoded
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxPageBytes+1))
	if err != nil {
		return nil, classify(ctx, err)
	}
	if len(body) > maxPageBytes {
		return nil, ErrTooLarge
	}
	cat, err := Parse(body)
	if err != nil {
		return nil, err
	}
	cat.FetchedAt = time.Now().UTC()
	return cat, nil
}

// classify maps a transport error to a fixed one: Go's text carries the
// URL and, for a redirect, whatever Location the server sent.
func classify(ctx context.Context, err error) error {
	var ne interface{ Timeout() bool }
	switch {
	case errors.Is(err, errRedirect), strings.Contains(err.Error(), "Location header"):
		return ErrRedirect
	case ctx.Err() != nil:
		return fmt.Errorf("%w: %w", ErrUnreachable, ctx.Err())
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &ne) && ne.Timeout():
		return ErrTimeout
	}
	return ErrUnreachable
}

// The card markup, as the page prints it (see testdata/eternal.html). Each
// card is a <ul class="pricing-table ..."> whose title item holds the name,
// linked to the cart with the product id while the plan is on sale, and
// whose bullet items carry the I/O and CPU lines. The expressions are
// anchored on the fixed label text, so styling changes around it do not
// matter; a change to the lines themselves makes the parser find nothing,
// which is reported, never guessed around.
var (
	reCard   = regexp.MustCompile(`(?s)<ul class="pricing-table[^"]*">(.*?)</ul>`)
	reTitle  = regexp.MustCompile(`(?s)<li class="title">\s*(?:<a href="([^"]*)"[^>]*>)?([^<]*)`)
	rePID    = regexp.MustCompile(`[?&]pid=(\d{1,9})(?:&|$)`)
	reIO     = regexp.MustCompile(`<strong>I/O:</strong>\s*([A-Za-z]+)\s*\((\d{1,6})\)`)
	reCPU    = regexp.MustCompile(`([0-9]+(?:\.[0-9]+)?)\s*<strong>CPU cores?</strong>\s*(?:&middot;|·|&#183;|&#xb7;)\s*([A-Za-z]+)\s*\((\d{1,6})\)`)
	reLabels = regexp.MustCompile(`DLABELS\s*=\s*(\{[^}]*\})`)
)

// Parse reads the plan cards out of the page.
func Parse(body []byte) (*Catalogue, error) {
	cat := &Catalogue{Plans: []Plan{}}
	cards := reCard.FindAllSubmatch(body, maxCards)
	for _, c := range cards {
		p, ok := parseCard(c[1])
		if ok {
			cat.Plans = append(cat.Plans, p)
		}
	}
	if len(cat.Plans) == 0 {
		return nil, ErrNoPlans
	}
	if m := reLabels.FindSubmatch(body); m != nil {
		cat.Labels = parseLabels(m[1])
	}
	return cat, nil
}

// parseCard reads one card; a card without a readable title, I/O line and
// CPU line is skipped (a card for something that is not a box).
func parseCard(c []byte) (Plan, bool) {
	var p Plan
	t := reTitle.FindSubmatch(c)
	if t == nil {
		return p, false
	}
	p.Name = cleanName(string(t[2]))
	if p.Name == "" {
		return p, false
	}
	if len(t[1]) > 0 {
		if m := rePID.FindSubmatch(t[1]); m != nil {
			p.PID, _ = strconv.Atoi(string(m[1]))
		}
	}
	iom := reIO.FindSubmatch(c)
	cpu := reCPU.FindSubmatch(c)
	if iom == nil || cpu == nil {
		return p, false
	}
	var ok bool
	if p.IOTier, p.IOWeight, ok = tierOf(string(iom[1]), string(iom[2])); !ok {
		return p, false
	}
	if p.CPUTier, p.CPUWeight, ok = tierOf(string(cpu[2]), string(cpu[3])); !ok {
		return p, false
	}
	if f, err := strconv.ParseFloat(string(cpu[1]), 64); err == nil && f > 0 && f <= 1000 {
		p.Cores = f
	}
	return p, true
}

// tierOf checks a printed tier against the ladder: the weight must be on
// systemd's scale and the label must be one the ladder knows; the label the
// ladder gives the weight is what is kept, so a relabelled page cannot show
// a word the dashboard does not explain.
func tierOf(label, weight string) (string, int, bool) {
	w, err := strconv.Atoi(weight)
	if err != nil || w < 1 || w > MaxWeight {
		return "", 0, false
	}
	switch strings.ToLower(label) {
	case "archive", "standard", "performance", "high", "glacial", "low", "medium":
		return Tier(w), w, true
	}
	return "", 0, false
}

// cleanName unescapes a title and keeps letters, digits, punctuation and
// single spaces: the trophy emoji goes, as does anything a page could use
// to confuse the display, and the result is clipped.
func cleanName(s string) string {
	s = html.UnescapeString(s)
	var b strings.Builder
	space := true
	for _, r := range s {
		switch {
		case unicode.IsSpace(r):
			if !space {
				b.WriteByte(' ')
				space = true
			}
		case unicode.IsLetter(r) || unicode.IsDigit(r) || unicode.IsPunct(r):
			b.WriteRune(r)
			space = false
		}
	}
	out := b.String()
	for len(out) > maxNameLen {
		_, size := utf8.DecodeLastRuneInString(out)
		out = out[:len(out)-size] // whole runes only
	}
	return strings.TrimSpace(out)
}

// parseLabels reads the page's product-id map, a JSON object literal of
// pid to name, keeping cleaned names under numeric keys only.
func parseLabels(raw []byte) map[int]string {
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil || len(m) > maxLabels {
		return nil
	}
	out := map[int]string{}
	for k, v := range m {
		pid, err := strconv.Atoi(k)
		if err != nil || pid <= 0 {
			continue
		}
		if name := cleanName(v); name != "" {
			out[pid] = name
		}
	}
	return out
}
