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
	"errors"
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
	svcCacheKey   = "pmapi_feed"
	svcCacheAtKey = "pmapi_feed_at"
	svcInterval   = 6 * time.Hour
)

// decorate attaches the matching service to a state. Caller must not hold m.mu.
func (m *Manager) decorate(s *BoxState) {
	m.svcMu.RLock()
	feed := m.svc.feed
	m.svcMu.RUnlock()
	s.Service = nil
	if feed == nil {
		return
	}
	if svc, how := matchService(feed.Services, s.Host, s.User); svc != nil {
		si := toInfo(*svc, feed.Currency)
		si.MatchedBy = how
		s.Service = &si
	}
}

// loadServicesCache restores the last feed from the database at startup.
func (m *Manager) loadServicesCache(ctx context.Context) {
	if m.Settings().PMAPIKey == "" {
		return
	}
	raw, err := m.st.GetValue(ctx, svcCacheKey)
	if err != nil || raw == "" {
		return
	}
	feed, err := pmapi.Parse([]byte(raw))
	if err != nil {
		return
	}
	at, _ := m.st.GetValue(ctx, svcCacheAtKey)
	t, _ := time.Parse(time.RFC3339, at)
	m.svcMu.Lock()
	m.svc = servicesCache{feed: feed, syncedAt: t}
	m.svcMu.Unlock()
}

// SyncServices fetches the customer API feed now, caches it, and re-attaches
// services to every box. It is safe to call concurrently; calls overlap only
// in doing redundant work.
func (m *Manager) SyncServices(ctx context.Context) error {
	key := m.Settings().PMAPIKey
	if key == "" {
		m.svcMu.Lock()
		m.svc = servicesCache{}
		m.svcMu.Unlock()
		_ = m.st.SetValue(ctx, svcCacheKey, "")
		m.reattachServices()
		return errors.New("no Pulsed Media API key configured")
	}
	fctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	feed, raw, err := m.pmapi.Fetch(fctx, key)
	m.svcMu.Lock()
	if err != nil {
		m.svc.err = err.Error()
		m.svcMu.Unlock()
		m.log.Warn("pmapi sync", "err", err)
		m.Publish(Event{Type: "services"})
		return err
	}
	m.svc = servicesCache{feed: feed, syncedAt: time.Now()}
	m.svcMu.Unlock()
	if err := m.st.SetValue(ctx, svcCacheKey, string(raw)); err == nil {
		_ = m.st.SetValue(ctx, svcCacheAtKey, time.Now().UTC().Format(time.RFC3339))
	}
	m.log.Info("pmapi sync", "services", len(feed.Services))
	m.reattachServices()
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
			cur.Service = cp.Service
			cp = *cur
		}
		m.mu.Unlock()
		for _, h := range m.hooks {
			h(m.ctx, &cp)
		}
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
		if strings.EqualFold(s.Status, "active") {
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
