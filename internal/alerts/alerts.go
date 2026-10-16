// Package alerts turns box states into persisted, deduplicated alerts.
package alerts

import (
	"context"
	"fmt"
	"log/slog"
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

// Evaluate is a collect.Hook.
func (e *Evaluator) Evaluate(ctx context.Context, s *collect.BoxState) {
	if s.Status == "disabled" || s.Status == "pending" {
		return
	}
	set := e.settings()
	var active []want
	name := s.Name

	if s.Status == "offline" {
		active = append(active, want{"unreachable", "bad", fmt.Sprintf("%s unreachable — %s", name, s.LastError)})
	}
	probeOK := s.Status == "online" || s.Status == "degraded" && s.Failures == 0
	if probeOK {
		if s.Account.Suspended {
			active = append(active, want{"suspended", "bad", fmt.Sprintf("%s account is suspended by PMSS", name)})
		}
		if s.Disk.OK && s.Disk.QuotaBytes > 0 {
			switch {
			case s.Disk.UsedBytes >= s.Disk.QuotaBytes:
				active = append(active, want{"disk", "bad", fmt.Sprintf("%s disk at %.0f%% — over quota (%s of %s)", name, s.Disk.Pct, hb(s.Disk.UsedBytes), hb(s.Disk.QuotaBytes))})
			case s.Disk.Pct >= float64(set.DiskThresholdPct):
				active = append(active, want{"disk", "warn", fmt.Sprintf("%s disk at %.0f%% of %s quota", name, s.Disk.Pct, hb(s.Disk.QuotaBytes))})
			}
		}
		if s.Traffic.OK && s.Traffic.LimitBytes > 0 {
			switch {
			case s.Traffic.Pct >= 100:
				active = append(active, want{"traffic", "bad", fmt.Sprintf("%s traffic at %.0f%% of %s 30-day limit", name, s.Traffic.Pct, hb(s.Traffic.LimitBytes))})
			case s.Traffic.Pct >= float64(set.TrafficThreshold):
				active = append(active, want{"traffic", "warn", fmt.Sprintf("%s traffic at %.0f%% of %s 30-day limit", name, s.Traffic.Pct, hb(s.Traffic.LimitBytes))})
			}
		}
		if s.Traffic.Throttled {
			active = append(active, want{"throttled", "bad", fmt.Sprintf("%s is being throttled by PMSS for traffic overage", name)})
		}
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
		if s.RAM.OK && s.RAM.LimitBytes > 0 && s.RAM.Pct >= 95 {
			active = append(active, want{"ram", "warn", fmt.Sprintf("%s RAM at %.0f%% of %s slice limit", name, s.RAM.Pct, hb(s.RAM.LimitBytes))})
		}
	}
	if renewal := s.EffectiveRenewal(); renewal != "" {
		if d, err := time.Parse("2006-01-02", renewal); err == nil {
			days := int(time.Until(d.Add(24*time.Hour)).Hours() / 24)
			switch {
			case days < 0:
				active = append(active, want{"renewal", "bad", fmt.Sprintf("%s renewal date %s has passed", name, renewal)})
			case days <= set.RenewalWarnDays:
				active = append(active, want{"renewal", "warn", fmt.Sprintf("%s renews in %dd (%s)", name, days, renewal)})
			}
		}
	}

	changed := false
	wanted := map[string]bool{}
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
	keys, err := e.st.ActiveAlertKeys(ctx, s.ID)
	if err != nil {
		e.log.Warn("list alerts", "err", err)
		return
	}
	for _, k := range keys {
		if wanted[k] {
			continue
		}
		// Only auto-resolve conditions we could actually re-check this poll.
		if !probeOK && k != "unreachable" && k != "renewal" {
			continue
		}
		if err := e.st.ResolveAlert(ctx, s.ID, k); err == nil {
			changed = true
		}
	}
	if changed && e.notify != nil {
		e.notify()
	}
}

func hb(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for n/div >= unit && exp < 5 {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}
