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

package collect

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"time"

	"pmss-fleet-manager/internal/pmplans"
)

// TierInfo is a box's CPU and I/O priority as shown: each weight typed in
// on the record (source "manual") or read from the plan catalogue for the
// box's plan name (source "catalogue"), with the catalogue's tier label.
// Lookup says how the catalogue was consulted, for the page's wording when
// a value is missing.
type TierInfo struct {
	IOWeight  int     `json:"io_weight,omitempty"`
	IOTier    string  `json:"io_tier,omitempty"`
	IOSource  string  `json:"io_source,omitempty"`
	CPUWeight int     `json:"cpu_weight,omitempty"`
	CPUTier   string  `json:"cpu_tier,omitempty"`
	CPUSource string  `json:"cpu_source,omitempty"`
	Cores     float64 `json:"cores,omitempty"`   // the catalogue's core count for the plan
	Product   string  `json:"product,omitempty"` // the catalogue listing the values came from
	// Lookup: matched, unknown (no listing carries the plan name),
	// ambiguous (listings disagree), no-name (nothing to look up) or
	// no-catalogue (the page has not been read yet).
	Lookup string `json:"lookup,omitempty"`
}

// Sources of a priority value.
const (
	tierManual    = "manual"
	tierCatalogue = "catalogue"
)

// Lookup outcomes of TierInfo beyond the catalogue's own.
const (
	lookupNoName      = "no-name"
	lookupNoCatalogue = "no-catalogue"
)

// PlansView is what GET /api/plans returns.
type PlansView struct {
	// Configured: a customer API key is set, so the page is read on its
	// own whenever a matched product is missing from the catalogue.
	Configured bool           `json:"configured"`
	URL        string         `json:"url"`
	FetchedAt  string         `json:"fetched_at,omitempty"`
	Error      string         `json:"error,omitempty"`
	Plans      []pmplans.Plan `json:"plans"`
}

type plansCache struct {
	cat *pmplans.Catalogue
	err string
	// The automatic read (plansWanted): autoTried holds the normalised
	// product names a successful read was made for, so a name the page
	// does not sell is asked for once and not after every services sync;
	// autoAt is when the last automatic read started, for the hourly cap.
	autoTried map[string]bool
	autoAt    time.Time
}

const (
	plansCacheKey = "pmplans_cache"
	// plansAutoInterval spaces automatic fetches even when a new unknown
	// name keeps appearing.
	plansAutoInterval = time.Hour
)

// loadPlansCache restores the last catalogue read at startup.
func (m *Manager) loadPlansCache(ctx context.Context) {
	raw, err := m.st.GetValue(ctx, plansCacheKey)
	if err != nil || raw == "" {
		return
	}
	var cat pmplans.Catalogue
	if err := json.Unmarshal([]byte(raw), &cat); err != nil || len(cat.Plans) == 0 {
		return
	}
	m.plansMu.Lock()
	m.plans.cat = &cat
	m.plansMu.Unlock()
}

// SyncPlans reads the campaign page now, keeps the catalogue (in memory
// and for the next start) and re-resolves every box's tiers. A failed read
// keeps the last catalogue and records the error.
func (m *Manager) SyncPlans(ctx context.Context) error {
	m.plansSyncMu.Lock()
	defer m.plansSyncMu.Unlock()
	fctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	cat, err := m.pmplans.Fetch(fctx)
	m.plansMu.Lock()
	if err != nil {
		m.plans.err = err.Error()
		m.plansMu.Unlock()
		m.log.Warn("plan catalogue", "err", err)
		m.Publish(Event{Type: "plans"})
		return err
	}
	m.plans.cat, m.plans.err = cat, ""
	m.plansMu.Unlock()
	rec, _ := json.Marshal(cat)
	perr := m.st.SetValue(ctx, plansCacheKey, string(rec))
	if perr != nil {
		m.log.Warn("persist plan catalogue", "err", perr)
	}
	m.log.Info("plan catalogue", "plans", len(cat.Plans))
	m.reattachServices() // decorate resolves every box's tiers
	m.Publish(Event{Type: "plans"})
	if perr != nil {
		return errors.New("plans fetched and applied, but not saved for the next start: " + perr.Error())
	}
	return nil
}

// Plans returns the catalogue as last read.
func (m *Manager) Plans() PlansView {
	v := PlansView{Configured: m.Settings().PMAPIKey != "", URL: pmplans.PageURL, Plans: []pmplans.Plan{}}
	if m.pmplans.URL != "" {
		v.URL = m.pmplans.URL
	}
	m.plansMu.RLock()
	c := m.plans
	m.plansMu.RUnlock()
	v.Error = c.err
	if c.cat != nil {
		v.FetchedAt = c.cat.FetchedAt.UTC().Format(time.RFC3339)
		v.Plans = append(v.Plans, c.cat.Plans...)
	}
	return v
}

// plansWanted reads the page on its own when a customer API key is set and
// a box's matched product is missing from the catalogue. A name is asked
// for once (a product the page does not sell, an older plan, is not asked
// for after every services sync; a failed read leaves it to be asked for
// again), and automatic reads are at least plansAutoInterval apart whatever
// is added or edited meanwhile. Runs after a services sync and after a box
// is added or edited, in the background.
func (m *Manager) plansWanted() {
	if m.Settings().PMAPIKey == "" || m.ctx == nil || m.ctx.Err() != nil {
		return
	}
	missing := map[string]bool{}
	for _, s := range m.States() {
		if s.Service == nil || s.Service.Product == "" || s.Tiers == nil {
			continue
		}
		if s.Tiers.Lookup == pmplans.LookupUnknown || s.Tiers.Lookup == lookupNoCatalogue {
			missing[pmplans.Normalize(s.Service.Product)] = true
		}
	}
	m.plansMu.Lock()
	var names []string
	for n := range missing {
		if !m.plans.autoTried[n] {
			names = append(names, n)
		}
	}
	due := len(names) > 0 && time.Since(m.plans.autoAt) >= plansAutoInterval
	if due {
		m.plans.autoAt = time.Now()
	}
	m.plansMu.Unlock()
	if !due {
		return
	}
	sort.Strings(names)
	m.wg.Add(1)
	go func() {
		defer m.wg.Done()
		if err := m.SyncPlans(m.ctx); err != nil {
			return // asked for again at the next occasion, after the interval
		}
		m.plansMu.Lock()
		if m.plans.autoTried == nil {
			m.plans.autoTried = map[string]bool{}
		}
		for _, n := range names {
			m.plans.autoTried[n] = true
		}
		m.plansMu.Unlock()
	}()
}

// tiersFor resolves a box's priorities: typed-in weights first, then the
// catalogue's listing for the box's plan name (the typed plan, else the
// matched service's product), filling whatever was not typed. Called by
// decorate under m.mu (lock order: mu before plansMu).
func (m *Manager) tiersFor(s *BoxState) *TierInfo {
	m.plansMu.RLock()
	cat := m.plans.cat
	m.plansMu.RUnlock()
	t := &TierInfo{}
	if s.IOWeight > 0 {
		t.IOWeight, t.IOTier, t.IOSource = s.IOWeight, pmplans.Tier(s.IOWeight), tierManual
	}
	if s.CPUWeight > 0 {
		t.CPUWeight, t.CPUTier, t.CPUSource = s.CPUWeight, pmplans.Tier(s.CPUWeight), tierManual
	}
	var names []string
	if p := strings.TrimSpace(s.Plan); p != "" {
		names = append(names, p)
	}
	if s.Service != nil && strings.TrimSpace(s.Service.Product) != "" {
		names = append(names, s.Service.Product)
	}
	switch {
	case len(names) == 0:
		t.Lookup = lookupNoName
	case cat == nil:
		t.Lookup = lookupNoCatalogue
	default:
		t.fillFromCatalogue(cat, names)
	}
	return t
}

// fillFromCatalogue takes the first name the catalogue lists (unambiguously)
// and fills the weights not typed in from that listing.
func (t *TierInfo) fillFromCatalogue(cat *pmplans.Catalogue, names []string) {
	t.Lookup = pmplans.LookupUnknown
	for _, n := range names {
		p, st := cat.Lookup(n)
		if st == pmplans.LookupAmbiguous {
			t.Lookup = st // a later name may still match
		}
		if p == nil {
			continue
		}
		t.Lookup, t.Product, t.Cores = pmplans.LookupMatched, p.Name, p.Cores
		if t.IOSource == "" {
			t.IOWeight, t.IOTier, t.IOSource = p.IOWeight, p.IOTier, tierCatalogue
		}
		if t.CPUSource == "" {
			t.CPUWeight, t.CPUTier, t.CPUSource = p.CPUWeight, p.CPUTier, tierCatalogue
		}
		return
	}
}
