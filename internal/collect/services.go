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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"pmss-fleet-manager/internal/pmapi"
)

// ServiceInfo is the Pulsed Media billing view of a box, from the customer
// API, attached to BoxState when a service matches the box.
type ServiceInfo struct {
	ID               int64        `json:"id"`
	Product          string       `json:"product"`
	Group            string       `json:"group,omitempty"`
	Domain           string       `json:"domain"`
	Status           string       `json:"status"`
	Registered       string       `json:"registered,omitempty"`
	NextDue          string       `json:"next_due,omitempty"`
	BillingCycle     string       `json:"billing_cycle"`
	RecurringAmount  float64      `json:"recurring_amount"`
	Monthly          float64      `json:"monthly"`
	Currency         string       `json:"currency"`
	Username         string       `json:"username,omitempty"`
	LoginURL         string       `json:"login_url,omitempty"`
	PermanentAddress string       `json:"permanent_address,omitempty"`
	Usage            *pmapi.Usage `json:"usage,omitempty"`
	BoxID            int64        `json:"box_id,omitempty"` // matched box, in the services list
	MatchedBy        string       `json:"matched_by,omitempty"`
}

// ServicesView is what GET /api/services returns.
type ServicesView struct {
	Configured  bool          `json:"configured"`
	Services    []ServiceInfo `json:"services"`
	Currency    string        `json:"currency,omitempty"`
	ClientID    int64         `json:"client_id,omitempty"`
	GeneratedAt string        `json:"generated_at,omitempty"`
	SyncedAt    string        `json:"synced_at,omitempty"`
	Error       string        `json:"error,omitempty"`
	MonthlyCost float64       `json:"monthly_cost"`
}

func toInfo(s pmapi.Service, currency string) ServiceInfo {
	si := ServiceInfo{
		ID: s.ID, Product: s.Product, Group: s.Group, Domain: s.Domain, Status: s.Status,
		Registered: s.Registered, NextDue: s.NextDue, BillingCycle: s.BillingCycle,
		RecurringAmount: float64(s.RecurringAmount), Monthly: s.Monthly(), Currency: currency,
	}
	if s.Seedbox != nil {
		si.Username, si.LoginURL, si.PermanentAddress, si.Usage = s.Seedbox.Username, s.Seedbox.LoginURL, s.Seedbox.PermanentAddress, s.Seedbox.Usage
	}
	return si
}

// matchService picks the service for a box. The same user can exist on two
// hosts and one host can carry two of the account's users, so an exact
// host+user match wins, then a host-only match if unique, then a user-only
// match if unique.
func matchService(services []pmapi.Service, host, user string) (*pmapi.Service, string) {
	host, user = strings.ToLower(strings.TrimSpace(host)), strings.TrimSpace(user)
	var hostOnly, userOnly []int
	for i, s := range services {
		dh := strings.ToLower(strings.TrimSpace(s.Domain))
		hm := dh != "" && dh == host
		um := user != "" && s.Username() == user
		if hm && um {
			return &services[i], "host+user"
		}
		if hm {
			hostOnly = append(hostOnly, i)
		}
		if um {
			userOnly = append(userOnly, i)
		}
	}
	if len(hostOnly) == 1 {
		return &services[hostOnly[0]], "host"
	}
	if len(userOnly) == 1 {
		return &services[userOnly[0]], "user"
	}
	return nil, ""
}

type servicesCache struct {
	feed     *pmapi.Feed
	syncedAt time.Time
	err      string
}

const (
	svcCacheKey = "pmapi_cache" // one record: feed, sync time and the key's fingerprint
	svcInterval = 6 * time.Hour
)

// svcCacheRecord is the persisted cache. KeyFP ties the feed to the API key
// that produced it, so a feed is never restored under a different key.
type svcCacheRecord struct {
	KeyFP    string          `json:"key_fp"`
	SyncedAt time.Time       `json:"synced_at"`
	Feed     json.RawMessage `json:"feed"`
}

// keyFingerprint is a short, non-reversible identity for an API key.
func keyFingerprint(key string) string {
	h := sha256.Sum256([]byte(key))
	return hex.EncodeToString(h[:8])
}

// serviceActive reports whether a service is current: only those count
// towards costs and API-derived renewal dates. Suspended counts, since that
// is usually an overdue service, exactly when its renewal matters; Cancelled,
// Terminated, Pending, Fraud and the rest stay visible but do not count.
func serviceActive(status string) bool {
	return strings.EqualFold(status, "active") || strings.EqualFold(status, "suspended")
}

// ServiceActive is serviceActive for other packages (the API totals).
func ServiceActive(status string) bool { return serviceActive(status) }

// decorate attaches the matching service to a state. Caller must not hold m.mu.
func (m *Manager) decorate(s *BoxState) {
	m.svcMu.RLock()
	feed := m.svc.feed
	m.svcMu.RUnlock()
	s.Service = nil
	if feed == nil {
		return
	}
	// An active service wins over a suspended one, and either over a
	// cancelled one on the same host (a cancel-and-reorder); an inactive
	// match is still shown, for history.
	svc, how := matchService(servicesWith(feed.Services, isActiveStatus), s.Host, s.User)
	if svc == nil {
		svc, how = matchService(servicesWith(feed.Services, serviceActive), s.Host, s.User)
	}
	if svc == nil {
		svc, how = matchService(feed.Services, s.Host, s.User)
	}
	if svc != nil {
		si := toInfo(*svc, feed.Currency)
		si.MatchedBy = how
		s.Service = &si
	}
}

func isActiveStatus(status string) bool { return strings.EqualFold(status, "active") }

func servicesWith(all []pmapi.Service, keep func(status string) bool) []pmapi.Service {
	out := make([]pmapi.Service, 0, len(all))
	for _, s := range all {
		if keep(s.Status) {
			out = append(out, s)
		}
	}
	return out
}

// loadServicesCache restores the last feed from the database at startup,
// only if it was fetched with the key that is configured now.
func (m *Manager) loadServicesCache(ctx context.Context) {
	// Pre-1.2.1 cache keys carried no key identity: drop them.
	_ = m.st.SetValue(ctx, "pmapi_feed", "")
	_ = m.st.SetValue(ctx, "pmapi_feed_at", "")
	key := m.Settings().PMAPIKey
	if key == "" {
		return
	}
	raw, err := m.st.GetValue(ctx, svcCacheKey)
	if err != nil || raw == "" {
		return
	}
	var rec svcCacheRecord
	if err := json.Unmarshal([]byte(raw), &rec); err != nil || rec.KeyFP != keyFingerprint(key) {
		return
	}
	feed, err := pmapi.Parse(rec.Feed)
	if err != nil {
		return
	}
	m.svcMu.Lock()
	m.svc = servicesCache{feed: feed, syncedAt: rec.SyncedAt}
	m.svcMu.Unlock()
}

// forgetServices drops the cached feed, in memory and on disk, and detaches
// it from every box. Used when the key is cleared or replaced: a feed
// fetched with one key must never be shown under another, not even after a
// restart.
func (m *Manager) forgetServices(ctx context.Context) {
	m.svcMu.Lock()
	m.svc = servicesCache{}
	m.svcMu.Unlock()
	if err := m.st.SetValue(ctx, svcCacheKey, ""); err != nil {
		// Not fatal: the record carries the old key's fingerprint, so a
		// restart under the new key refuses it anyway.
		m.log.Warn("clear services cache", "err", err)
	}
	m.reattachServices()
}

// SyncServices fetches the customer API feed now, caches it, and re-attaches
// services to every box. Calls are serialised, so the cache always holds the
// most recent fetch; a sync whose key was replaced while it ran is discarded.
func (m *Manager) SyncServices(ctx context.Context) error {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	key := m.Settings().PMAPIKey
	if key == "" {
		m.forgetServices(ctx)
		return errors.New("no Pulsed Media API key configured")
	}
	fctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	feed, raw, err := m.pmapi.Fetch(fctx, key)
	m.svcMu.Lock()
	// Checked under the cache lock: SetSettings swaps the key before it
	// clears the cache (under this lock), so a fetch for a replaced key
	// either sees the new key here and is discarded, or commits before
	// the clear and is wiped by it. Either way the old key's feed is gone.
	if m.Settings().PMAPIKey != key {
		m.svcMu.Unlock()
		return errors.New("api key changed during sync; superseded")
	}
	if err != nil {
		m.svc.err = err.Error()
		m.svcMu.Unlock()
		m.log.Warn("pmapi sync", "err", err)
		m.Publish(Event{Type: "services"})
		return err
	}
	now := time.Now()
	m.svc = servicesCache{feed: feed, syncedAt: now}
	rec, _ := json.Marshal(svcCacheRecord{KeyFP: keyFingerprint(key), SyncedAt: now.UTC(), Feed: raw})
	perr := m.st.SetValue(ctx, svcCacheKey, string(rec))
	if perr != nil {
		// The live data is good and stays; what a restart would restore is
		// not, so the caller (and a manual sync's toast) hears about it.
		m.svc.err = "fetched, but the cache could not be saved: " + perr.Error()
		m.log.Warn("persist services cache", "err", perr)
	}
	m.svcMu.Unlock()
	m.log.Info("pmapi sync", "services", len(feed.Services))
	m.reattachServices()
	if perr != nil {
		return fmt.Errorf("services fetched and applied, but not saved for the next start: %w", perr)
	}
	return nil
}

// reattachServices re-runs matching for every box and re-evaluates alerts
// (the renewal rule depends on the service's next due date).
func (m *Manager) reattachServices() {
	m.mu.RLock()
	ids := make([]int64, 0, len(m.states))
	for id := range m.states {
		ids = append(ids, id)
	}
	m.mu.RUnlock()
	for _, id := range ids {
		m.mu.Lock()
		s, ok := m.states[id]
		if !ok {
			m.mu.Unlock()
			continue
		}
		cp := *s
		m.mu.Unlock()
		m.decorate(&cp)
		m.mu.Lock()
		if cur, ok := m.states[id]; ok {
			// Replace the stored snapshot rather than writing into it: the
			// object may be in a hook's hands on another goroutine.
			ncp := *cur
			ncp.Service = cp.Service
			m.states[id] = &ncp
			cp = ncp
		}
		m.mu.Unlock()
		if cp.Unpolled && cp.Status != "disabled" {
			// Restored or just-edited: its reachability fields were reset and
			// no poll has verified them; evaluating now would raise false
			// "not responding" alerts. The next poll runs the hooks.
			continue
		}
		m.runHooks(cp)
	}
	m.Publish(Event{Type: "services"})
}

// Services returns the cached feed with each service's matched box.
func (m *Manager) Services() ServicesView {
	v := ServicesView{Configured: m.Settings().PMAPIKey != "", Services: []ServiceInfo{}}
	m.svcMu.RLock()
	c := m.svc
	m.svcMu.RUnlock()
	v.Error = c.err
	if !c.syncedAt.IsZero() {
		v.SyncedAt = c.syncedAt.UTC().Format(time.RFC3339)
	}
	if c.feed == nil {
		return v
	}
	v.Currency, v.ClientID, v.GeneratedAt = c.feed.Currency, c.feed.ClientID, c.feed.GeneratedAt
	states := m.States()
	for _, s := range c.feed.Services {
		si := toInfo(s, c.feed.Currency)
		for _, st := range states {
			if st.Service != nil && st.Service.ID == s.ID {
				si.BoxID, si.MatchedBy = st.ID, st.Service.MatchedBy
				break
			}
		}
		if serviceActive(s.Status) {
			v.MonthlyCost += si.Monthly
		}
		v.Services = append(v.Services, si)
	}
	return v
}

// servicesLoop refreshes the feed every svcInterval while a key is set.
func (m *Manager) servicesLoop() {
	defer m.wg.Done()
	// First sync shortly after start if the cache is stale or missing.
	m.svcMu.RLock()
	age := time.Since(m.svc.syncedAt)
	m.svcMu.RUnlock()
	first := time.Minute
	if age < svcInterval {
		first = svcInterval - age
	}
	t := time.NewTimer(first)
	defer t.Stop()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-t.C:
			t.Reset(svcInterval)
		}
		if m.Settings().PMAPIKey == "" {
			continue
		}
		_ = m.SyncServices(m.ctx)
	}
}
