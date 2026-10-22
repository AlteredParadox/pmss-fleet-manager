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

// Package alerts turns box states into persisted, deduplicated alerts.
package alerts

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"pmss-fleet-manager/internal/collect"
	"pmss-fleet-manager/internal/store"
)

// Evaluator applies the rules after every poll.
type Evaluator struct {
	st       *store.Store
	settings func() store.Settings
	log      *slog.Logger
	notify   func()
}

// New builds an Evaluator. notify is called whenever alerts changed.
func New(st *store.Store, settings func() store.Settings, log *slog.Logger, notify func()) *Evaluator {
	return &Evaluator{st: st, settings: settings, log: log, notify: notify}
}

type want struct {
	key, sev, title string
}

// recheckable reports whether this poll could re-evaluate the rule behind an
// alert key. A source that was unavailable on an otherwise good poll (quota
// command hung, dockerd blip, no traffic file yet) must not resolve an alert
// it could not check, or an acknowledged alert comes back unacknowledged on
// the next good poll. A source the user switched off is different: nothing
// will ever re-check it, so its alerts resolve.
func recheckable(s *collect.BoxState, k string) bool {
	switch {
	case k == "disk":
		return s.Disk.OK
	case k == "traffic":
		return s.Traffic.OK
	case k == "ram":
		return s.RAM.OK
	case k == "suspended":
		return s.Account.OK
	case strings.HasPrefix(k, "container:"):
		// Monitoring switched off for the box is a deliberate "nothing to
		// check", like a disabled box: the alert must not be stranded.
		return s.Docker.Reachable || !s.Docker.Enabled
	case k == "qbt-conn":
		return s.Qbt.Reachable || !s.Qbt.Enabled
	case strings.HasPrefix(k, "app:"):
		// Unwatching is a deliberate "nothing to check" too.
		return s.Apps.OK || unwatchedApp(s, k)
	}
	return true
}

// unwatchedApp reports whether k is an app alert whose app is no longer
// on the watch list.
func unwatchedApp(s *collect.BoxState, k string) bool {
	key, ok := strings.CutPrefix(k, "app:")
	return ok && !slices.Contains(s.Apps.Watched, key)
}

// Evaluate is a collect.Hook.
func (e *Evaluator) Evaluate(ctx context.Context, s *collect.BoxState) {
	if s.Status == "pending" {
		return
	}
	set := e.settings()
	var active []want
	if s.Status == "disabled" {
		e.resolveDisabled(ctx, s)
		return
	}
	name := s.Name

	if s.Status == "offline" {
		active = append(active, want{"unreachable", "bad", fmt.Sprintf("%s unreachable — %s", name, s.LastError)})
	}
	probeOK := s.Status == "online" || s.Status == "degraded" && s.Failures == 0
	if probeOK {
		active = append(active, wantProbeRules(s, set)...)
	}
	active = append(active, wantRenewal(s, set)...)
	e.reconcile(ctx, s, active, probeOK)
}

// resolveDisabled handles a disabled box.
func (e *Evaluator) resolveDisabled(ctx context.Context, s *collect.BoxState) {
	// Nothing is being checked any more: resolve whatever was open so
	// a disabled box does not keep an "unreachable" alert forever.
	keys, err := e.st.ActiveAlertKeys(ctx, s.ID)
	if err != nil {
		return
	}
	changed := false
	for _, k := range keys {
		if err := e.st.ResolveAlert(ctx, s.ID, k); err == nil {
			changed = true
		}
	}
	if changed && e.notify != nil {
		e.notify()
	}
}

// wantProbeRules collects the alerts that need a successful probe, in the
// order the rules have always run: suspension, disk, traffic, qBittorrent,
// docker and containers, memory.
func wantProbeRules(s *collect.BoxState, set store.Settings) []want {
	var active []want
	name := s.Name
	if s.Account.Suspended {
		active = append(active, want{"suspended", "bad", fmt.Sprintf("%s account is suspended by PMSS", name)})
	}
	active = append(active, wantDisk(s, set)...)
	active = append(active, wantTraffic(s, set)...)
	active = append(active, wantQbt(s)...)
	active = append(active, wantDocker(s)...)
	active = append(active, wantApps(s)...)
	active = append(active, wantRAM(s)...)
	return active
}

// wantApps raises an alert for every watched application that is not
// running, when the process list could be read this poll.
func wantApps(s *collect.BoxState) []want {
	var active []want
	if !s.Apps.OK {
		return nil
	}
	for _, a := range s.Apps.Apps {
		if a.Watched && !a.Running {
			active = append(active, want{"app:" + a.Key, "warn", fmt.Sprintf("%s — %s is not running", s.Name, a.Name)})
		}
	}
	return active
}

// wantDisk raises the disk alert against the quota.
func wantDisk(s *collect.BoxState, set store.Settings) []want {
	var active []want
	name := s.Name
	if s.Disk.OK && s.Disk.QuotaBytes > 0 {
		switch {
		case s.Disk.UsedBytes >= s.Disk.QuotaBytes:
			active = append(active, want{"disk", "bad", fmt.Sprintf("%s disk at %.0f%% — over quota (%s of %s)", name, s.Disk.Pct, collect.HumanBytes(s.Disk.UsedBytes), collect.HumanBytes(s.Disk.QuotaBytes))})
		case s.Disk.Pct >= float64(set.DiskThresholdPct):
			active = append(active, want{"disk", "warn", fmt.Sprintf("%s disk at %.0f%% of %s quota", name, s.Disk.Pct, collect.HumanBytes(s.Disk.QuotaBytes))})
		}
	}
	return active
}

// wantTraffic raises the 30-day traffic alert and the throttling alert.
func wantTraffic(s *collect.BoxState, set store.Settings) []want {
	var active []want
	name := s.Name
	if s.Traffic.OK && s.Traffic.LimitBytes > 0 {
		switch {
		case s.Traffic.Pct >= 100:
			active = append(active, want{"traffic", "bad", fmt.Sprintf("%s traffic at %.0f%% of %s 30-day limit", name, s.Traffic.Pct, collect.HumanBytes(s.Traffic.LimitBytes))})
		case s.Traffic.Pct >= float64(set.TrafficThreshold):
			active = append(active, want{"traffic", "warn", fmt.Sprintf("%s traffic at %.0f%% of %s 30-day limit", name, s.Traffic.Pct, collect.HumanBytes(s.Traffic.LimitBytes))})
		}
	}
	if s.Traffic.Throttled {
		active = append(active, want{"throttled", "bad", fmt.Sprintf("%s is being throttled by PMSS for traffic overage", name)})
	}
	return active
}

// wantQbt raises the alert for a monitored qBittorrent that does not answer.
func wantQbt(s *collect.BoxState) []want {
	var active []want
	name := s.Name
	if s.Qbt.Enabled && !s.Qbt.Reachable {
		what := "qBittorrent Web API not responding"
		if !s.Qbt.ProcessUp {
			what = "qbittorrent-nox is not running"
		}
		msg := fmt.Sprintf("%s — %s", name, what)
		if s.Qbt.Error != "" {
			msg += " (" + s.Qbt.Error + ")"
		}
		active = append(active, want{"qbt", "warn", msg})
	}
	active = append(active, wantQbtConnection(s, time.Now())...)
	return active
}

// firewalledGrace is how long a client may report no incoming connectivity
// before it is an alert: after a start libtorrent says "firewalled" until
// the first peer dials in, which on a quiet box takes minutes.
const firewalledGrace = 10 * time.Minute

// wantQbtConnection raises the alert for a reachable client that has had no
// incoming connectivity for longer than the grace period: a wrong or
// unforwarded listen port is the usual cause, and it silently halves what
// the box can seed to.
func wantQbtConnection(s *collect.BoxState, now time.Time) []want {
	if !s.Qbt.Enabled || !s.Qbt.Reachable || (s.Qbt.Connection != "firewalled" && s.Qbt.Connection != "disconnected") {
		return nil
	}
	since, err := time.Parse(time.RFC3339, s.Qbt.FirewalledSince)
	if err != nil || now.Sub(since) < firewalledGrace {
		return nil
	}
	port := "its listen port"
	if s.Qbt.ListenPort > 0 {
		port = fmt.Sprintf("listen port %d", s.Qbt.ListenPort)
	}
	what := "no peer has reached " + port + " (firewalled)"
	if s.Qbt.Connection == "disconnected" {
		what = "libtorrent reports no network connection"
	}
	return []want{{"qbt-conn", "warn", fmt.Sprintf("%s — qBittorrent: %s for %s; check the port in qBittorrent against the one Pulsed Media assigned", s.Name, what, humanSince(now.Sub(since)))}}
}

// humanSince renders a duration as "12m" or "3h 05m".
func humanSince(d time.Duration) string {
	d = d.Round(time.Minute)
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	return fmt.Sprintf("%dh %02dm", int(d.Hours()), int(d.Minutes())%60)
}

// wantDocker raises the alerts for an unreachable rootless docker and for
// containers that stopped with a non-zero exit.
func wantDocker(s *collect.BoxState) []want {
	var active []want
	name := s.Name
	if s.Docker.Enabled && !s.Docker.Reachable {
		active = append(active, want{"docker", "warn", fmt.Sprintf("%s — rootless docker not responding (%s)", name, s.Docker.Error)})
	}
	for _, c := range s.Docker.Containers {
		if c.State == "exited" || c.State == "dead" {
			if strings.HasPrefix(c.Status, "Exited (0)") {
				continue // clean exit, probably on purpose
			}
			active = append(active, want{"container:" + c.Name, "warn", fmt.Sprintf("%s — container %s %s", name, c.Name, strings.ToLower(c.Status))})
		}
	}
	return active
}

// wantRAM raises the slice memory alert and the OOM-kill alert.
func wantRAM(s *collect.BoxState) []want {
	var active []want
	name := s.Name
	// Pct is process memory (not page cache) against the slice limit.
	if s.RAM.OK && s.RAM.LimitBytes > 0 && s.RAM.Pct >= 95 {
		active = append(active, want{"ram", "warn", fmt.Sprintf("%s process memory at %.0f%% of the %s slice limit", name, s.RAM.Pct, collect.HumanBytes(s.RAM.LimitBytes))})
	}
	// The definitive signal that memory ran out: the kernel killed something.
	if t, err := time.Parse(time.RFC3339, s.RAM.LastOOM); err == nil && time.Since(t) < 24*time.Hour {
		active = append(active, want{"oom", "bad", fmt.Sprintf("%s: the kernel OOM-killed a process in the slice (noticed %s; %d kills in total)", name, t.Local().Format("Jan 2 15:04"), s.RAM.OOMKills)})
	}
	return active
}

// wantRenewal raises the renewal alert from the effective renewal date; it
// does not need a probe.
func wantRenewal(s *collect.BoxState, set store.Settings) []want {
	var active []want
	name := s.Name
	if renewal := s.EffectiveRenewal(); renewal != "" {
		if days, ok := daysUntilDate(renewal, time.Now()); ok {
			switch {
			case days < 0:
				active = append(active, want{"renewal", "bad", fmt.Sprintf("%s renewal date %s has passed", name, renewal)})
			case days <= set.RenewalWarnDays:
				active = append(active, want{"renewal", "warn", fmt.Sprintf("%s renews in %dd (%s)", name, days, renewal)})
			}
		}
	}
	return active
}

// daysUntilDate counts calendar days from now's date to a YYYY-MM-DD date,
// in now's location: 0 on the day itself, -1 the day after. A renewal date
// is a civil date, not an instant, so the count changes at local midnight
// (not UTC midnight) and DST days do not shift it; the browser does the
// same arithmetic in daysUntil.
func daysUntilDate(ymd string, now time.Time) (int, bool) {
	d, err := time.ParseInLocation("2006-01-02", ymd, now.Location())
	if err != nil {
		return 0, false
	}
	y, m, dd := now.Date()
	today := time.Date(y, m, dd, 0, 0, 0, 0, time.UTC)
	due := time.Date(d.Year(), d.Month(), d.Day(), 0, 0, 0, 0, time.UTC)
	return int(due.Sub(today).Hours() / 24), true
}

// reconcile persists the wanted alerts, resolves the open ones this poll no
// longer wants and could re-check, and notifies when anything changed.
func (e *Evaluator) reconcile(ctx context.Context, s *collect.BoxState, active []want, probeOK bool) {
	wanted, changed := e.raise(ctx, s, active)
	keys, err := e.st.ActiveAlertKeys(ctx, s.ID)
	if err != nil {
		e.log.Warn("list alerts", "err", err)
		return
	}
	if e.resolveStale(ctx, s, keys, wanted, probeOK) {
		changed = true
	}
	if changed && e.notify != nil {
		e.notify()
	}
}

// raise persists every wanted alert and returns the set of wanted keys and
// whether any alert was newly created.
func (e *Evaluator) raise(ctx context.Context, s *collect.BoxState, active []want) (wanted map[string]bool, changed bool) {
	name := s.Name
	wanted = map[string]bool{}
	for _, w := range active {
		wanted[w.key] = true
		created, err := e.st.RaiseAlert(ctx, s.ID, w.key, w.sev, w.title)
		if err != nil {
			e.log.Warn("raise alert", "err", err)
			continue
		}
		if created {
			changed = true
			e.log.Info("alert", "box", name, "key", w.key, "title", w.title)
		}
	}
	return wanted, changed
}

// resolveStale resolves the active alerts that are no longer wanted and
// whose rule this poll could re-check; it reports whether any was resolved.
func (e *Evaluator) resolveStale(ctx context.Context, s *collect.BoxState, keys []string, wanted map[string]bool, probeOK bool) (changed bool) {
	for _, k := range keys {
		if wanted[k] {
			continue
		}
		// Only auto-resolve conditions we could actually re-check this poll,
		// plus the ones that need no probe: the box's reachability, its
		// renewal date, and an app alert the user unwatched (the watch
		// list is edited here, not read from the box).
		if !probeOK && k != "unreachable" && k != "renewal" && !unwatchedApp(s, k) {
			continue
		}
		if !recheckable(s, k) {
			continue
		}
		if err := e.st.ResolveAlert(ctx, s.ID, k); err == nil {
			changed = true
		}
	}
	return changed
}
