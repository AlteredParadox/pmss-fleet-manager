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

// Package store persists boxes, settings, alerts and per-box caches in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Box is one managed seedbox or storage box.
type Box struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	AuthMethod string `json:"auth_method"` // key | agent | password | "" (auto)
	KeyPath    string `json:"key_path"`
	Password   string `json:"-"`
	HostKey    string `json:"host_key"`
	// HasPassword/HasQbtPass tell the edit form whether a secret is saved,
	// since the secrets themselves never leave the server.
	HasPassword bool   `json:"has_password"`
	HasQbtPass  bool   `json:"has_qbt_pass"`
	Type        string `json:"type"` // seedbox | storage
	Plan        string `json:"plan"`
	Datacenter  string `json:"datacenter"`
	Renewal     string `json:"renewal"` // YYYY-MM-DD or ""

	QbtEnabled  bool   `json:"qbt_enabled"`
	QbtPort     int    `json:"qbt_port"`
	QbtUser     string `json:"qbt_user"`
	QbtPass     string `json:"-"`
	QbtStartCmd string `json:"qbt_start_cmd"`

	DockerEnabled bool `json:"docker_enabled"`
	DuEnabled     bool `json:"du_enabled"`
	Enabled       bool `json:"enabled"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Settings are the global dashboard preferences.
type Settings struct {
	PollIntervalSec  int    `json:"poll_interval_sec"`
	DiskThresholdPct int    `json:"disk_threshold_pct"`
	TrafficThreshold int    `json:"traffic_threshold_pct"`
	RenewalWarnDays  int    `json:"renewal_warn_days"`
	DuIntervalHours  int    `json:"du_interval_hours"`
	Theme            string `json:"theme"`
	FleetView        string `json:"fleet_view"`
	ClientAreaURL    string `json:"client_area_url"`
	QbtDownloadURL   string `json:"qbt_download_url"`
	CardMinPx        int    `json:"card_min_px"` // fleet card minimum width

	// PMAPIKey is the Pulsed Media customer API key. It is never sent to the
	// browser: Public() blanks it and sets HasPMAPIKey instead. On PUT an
	// empty key keeps the stored one; ClearPMAPIKey removes it.
	PMAPIKey      string `json:"pm_api_key,omitempty"`
	HasPMAPIKey   bool   `json:"has_pm_api_key"`
	ClearPMAPIKey bool   `json:"clear_pm_api_key,omitempty"`
}

// Public returns a copy safe to serialise to the browser.
func (s Settings) Public() Settings {
	s.HasPMAPIKey = s.PMAPIKey != ""
	s.PMAPIKey = ""
	s.ClearPMAPIKey = false
	return s
}

// DefaultQbtDownloadURL is the userdocs static build for x86_64.
const DefaultQbtDownloadURL = "https://github.com/userdocs/qbittorrent-nox-static/releases/latest/download/x86_64-qbittorrent-nox"

// DefaultSettings are used when a key has never been written.
func DefaultSettings() Settings {
	return Settings{
		PollIntervalSec:  30,
		DiskThresholdPct: 90,
		TrafficThreshold: 90,
		RenewalWarnDays:  7,
		DuIntervalHours:  6,
		Theme:            "dark",
		FleetView:        "cards",
		ClientAreaURL:    "https://pulsedmedia.com/clients/",
		QbtDownloadURL:   DefaultQbtDownloadURL,
		CardMinPx:        340,
	}
}

// Alert is a persisted alert. Key dedups by box and rule.
type Alert struct {
	ID         int64      `json:"id"`
	BoxID      int64      `json:"box_id"`
	Key        string     `json:"key"`
	Severity   string     `json:"severity"` // bad | warn | ok
	Title      string     `json:"title"`
	Active     bool       `json:"active"`
	Acked      bool       `json:"acked"`
	CreatedAt  time.Time  `json:"created_at"`
	UpdatedAt  time.Time  `json:"updated_at"`
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
}

// Store wraps the database.
type Store struct {
	db *sql.DB
}

// Open opens (creating if needed) the SQLite database at path.
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	dsn := "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	_ = os.Chmod(path, 0o600)
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS boxes (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL, host TEXT NOT NULL, port INTEGER NOT NULL DEFAULT 22,
			user TEXT NOT NULL, auth_method TEXT NOT NULL DEFAULT '', key_path TEXT NOT NULL DEFAULT '',
			password TEXT NOT NULL DEFAULT '', host_key TEXT NOT NULL DEFAULT '',
			type TEXT NOT NULL DEFAULT 'seedbox', plan TEXT NOT NULL DEFAULT '',
			datacenter TEXT NOT NULL DEFAULT '', renewal TEXT NOT NULL DEFAULT '',
			qbt_enabled INTEGER NOT NULL DEFAULT 0, qbt_port INTEGER NOT NULL DEFAULT 0,
			qbt_user TEXT NOT NULL DEFAULT '', qbt_pass TEXT NOT NULL DEFAULT '',
			qbt_start_cmd TEXT NOT NULL DEFAULT '',
			docker_enabled INTEGER NOT NULL DEFAULT 0, du_enabled INTEGER NOT NULL DEFAULT 1,
			enabled INTEGER NOT NULL DEFAULT 1,
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS settings (key TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		// Poll samples were recorded from the first version but nothing ever read them.
		`DROP TABLE IF EXISTS samples`,
		`CREATE TABLE IF NOT EXISTS alerts (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			box_id INTEGER NOT NULL REFERENCES boxes(id) ON DELETE CASCADE,
			key TEXT NOT NULL, severity TEXT NOT NULL, title TEXT NOT NULL,
			active INTEGER NOT NULL DEFAULT 1, acked INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL, updated_at TEXT NOT NULL, resolved_at TEXT)`,
		`CREATE INDEX IF NOT EXISTS alerts_box_key ON alerts(box_id, key, active)`,
		// Two evaluations of the same box can race (a poll hook and a
		// services sync); the partial unique index makes the second insert
		// fail instead of duplicating the alert. Dedupe before creating it.
		`DELETE FROM alerts WHERE active=1 AND id NOT IN (SELECT MIN(id) FROM alerts WHERE active=1 GROUP BY box_id, key)`,
		`CREATE UNIQUE INDEX IF NOT EXISTS alerts_active_unique ON alerts(box_id, key) WHERE active=1`,
		`CREATE TABLE IF NOT EXISTS box_cache (
			box_id INTEGER NOT NULL REFERENCES boxes(id) ON DELETE CASCADE,
			kind TEXT NOT NULL, json TEXT NOT NULL, at TEXT NOT NULL,
			PRIMARY KEY (box_id, kind))`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

const boxCols = `id, name, host, port, user, auth_method, key_path, password, host_key, type, plan, datacenter, renewal,
	qbt_enabled, qbt_port, qbt_user, qbt_pass, qbt_start_cmd, docker_enabled, du_enabled, enabled, created_at, updated_at`

func scanBox(row interface{ Scan(...any) error }) (*Box, error) {
	var b Box
	var created, updated string
	err := row.Scan(&b.ID, &b.Name, &b.Host, &b.Port, &b.User, &b.AuthMethod, &b.KeyPath, &b.Password, &b.HostKey,
		&b.Type, &b.Plan, &b.Datacenter, &b.Renewal, &b.QbtEnabled, &b.QbtPort, &b.QbtUser, &b.QbtPass, &b.QbtStartCmd,
		&b.DockerEnabled, &b.DuEnabled, &b.Enabled, &created, &updated)
	if err != nil {
		return nil, err
	}
	b.CreatedAt, _ = time.Parse(time.RFC3339, created)
	b.HasPassword, b.HasQbtPass = b.Password != "", b.QbtPass != ""
	b.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return &b, nil
}

// ListBoxes returns all boxes ordered by name.
func (s *Store) ListBoxes(ctx context.Context) ([]*Box, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+boxCols+` FROM boxes ORDER BY name COLLATE NOCASE, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Box
	for rows.Next() {
		b, err := scanBox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// GetBox loads one box.
func (s *Store) GetBox(ctx context.Context, id int64) (*Box, error) {
	b, err := scanBox(s.db.QueryRowContext(ctx, `SELECT `+boxCols+` FROM boxes WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	return b, err
}

// CreateBox inserts a box and sets its ID.
func (s *Store) CreateBox(ctx context.Context, b *Box) error {
	now := time.Now().UTC()
	b.CreatedAt, b.UpdatedAt = now, now
	if b.Port == 0 {
		b.Port = 22
	}
	if b.Type == "" {
		b.Type = "seedbox"
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO boxes (name, host, port, user, auth_method, key_path, password, host_key, type, plan, datacenter, renewal,
		qbt_enabled, qbt_port, qbt_user, qbt_pass, qbt_start_cmd, docker_enabled, du_enabled, enabled, created_at, updated_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		b.Name, b.Host, b.Port, b.User, b.AuthMethod, b.KeyPath, b.Password, b.HostKey, b.Type, b.Plan, b.Datacenter, b.Renewal,
		b.QbtEnabled, b.QbtPort, b.QbtUser, b.QbtPass, b.QbtStartCmd, b.DockerEnabled, b.DuEnabled, b.Enabled,
		now.Format(time.RFC3339), now.Format(time.RFC3339))
	if err != nil {
		return err
	}
	b.ID, err = res.LastInsertId()
	return err
}

// UpdateBox writes all mutable fields. Empty Password/QbtPass keep the stored secret.
func (s *Store) UpdateBox(ctx context.Context, b *Box) error {
	now := time.Now().UTC()
	b.UpdatedAt = now
	_, err := s.db.ExecContext(ctx, `UPDATE boxes SET name=?, host=?, port=?, user=?, auth_method=?, key_path=?,
		password=CASE WHEN ?='' THEN password ELSE ? END, host_key=?, type=?, plan=?, datacenter=?, renewal=?,
		qbt_enabled=?, qbt_port=?, qbt_user=?, qbt_pass=CASE WHEN ?='' THEN qbt_pass ELSE ? END, qbt_start_cmd=?,
		docker_enabled=?, du_enabled=?, enabled=?, updated_at=? WHERE id=?`,
		b.Name, b.Host, b.Port, b.User, b.AuthMethod, b.KeyPath, b.Password, b.Password, b.HostKey, b.Type, b.Plan, b.Datacenter, b.Renewal,
		b.QbtEnabled, b.QbtPort, b.QbtUser, b.QbtPass, b.QbtPass, b.QbtStartCmd, b.DockerEnabled, b.DuEnabled, b.Enabled,
		now.Format(time.RFC3339), b.ID)
	return err
}

// ClearSecrets blanks the stored SSH password and/or WebUI password. The
// normal update keeps a secret when the field is blank, so this is the only
// way to remove one.
func (s *Store) ClearSecrets(ctx context.Context, id int64, ssh, qbt bool) error {
	if ssh {
		if _, err := s.db.ExecContext(ctx, `UPDATE boxes SET password='' WHERE id=?`, id); err != nil {
			return err
		}
	}
	if qbt {
		if _, err := s.db.ExecContext(ctx, `UPDATE boxes SET qbt_pass='' WHERE id=?`, id); err != nil {
			return err
		}
	}
	return nil
}

// SetHostKey pins the host key after a trust-on-first-use connect.
// SetHostKey pins a key seen while dialling host:port onto the box, only if
// the record still names that endpoint and has no pin: an edit that moved
// the box meanwhile must not end up pinned to the old host's key.
func (s *Store) SetHostKey(ctx context.Context, id int64, host string, port int, key string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE boxes SET host_key=? WHERE id=? AND lower(host)=lower(?) AND port=? AND host_key=''`, key, id, host, port)
	return err
}

// DeleteBox removes a box and its alerts and cache.
func (s *Store) DeleteBox(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM boxes WHERE id=?`, id)
	return err
}

// GetSettings returns settings merged over defaults.
func (s *Store) GetSettings(ctx context.Context) (Settings, error) {
	out := DefaultSettings()
	rows, err := s.db.QueryContext(ctx, `SELECT key, value FROM settings`)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	kv := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return out, err
		}
		kv[k] = v
	}
	geti := func(k string, dst *int) {
		if v, ok := kv[k]; ok {
			if n, err := strconv.Atoi(v); err == nil {
				*dst = n
			}
		}
	}
	gets := func(k string, dst *string) {
		if v, ok := kv[k]; ok {
			*dst = v
		}
	}
	geti("poll_interval_sec", &out.PollIntervalSec)
	geti("disk_threshold_pct", &out.DiskThresholdPct)
	geti("traffic_threshold_pct", &out.TrafficThreshold)
	geti("renewal_warn_days", &out.RenewalWarnDays)
	geti("du_interval_hours", &out.DuIntervalHours)
	geti("card_min_px", &out.CardMinPx)
	gets("theme", &out.Theme)
	gets("fleet_view", &out.FleetView)
	gets("client_area_url", &out.ClientAreaURL)
	gets("qbt_download_url", &out.QbtDownloadURL)
	gets("pm_api_key", &out.PMAPIKey)
	if out.QbtDownloadURL == "" {
		out.QbtDownloadURL = DefaultQbtDownloadURL
	}
	return out, rows.Err()
}

// SaveSettings writes every setting.
func (s *Store) SaveSettings(ctx context.Context, st Settings) error {
	kv := map[string]string{
		"poll_interval_sec":     strconv.Itoa(st.PollIntervalSec),
		"disk_threshold_pct":    strconv.Itoa(st.DiskThresholdPct),
		"traffic_threshold_pct": strconv.Itoa(st.TrafficThreshold),
		"renewal_warn_days":     strconv.Itoa(st.RenewalWarnDays),
		"du_interval_hours":     strconv.Itoa(st.DuIntervalHours),
		"theme":                 st.Theme,
		"fleet_view":            st.FleetView,
		"client_area_url":       st.ClientAreaURL,
		"qbt_download_url":      st.QbtDownloadURL,
		"card_min_px":           strconv.Itoa(st.CardMinPx),
		"pm_api_key":            st.PMAPIKey,
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	for k, v := range kv {
		if _, err := tx.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, k, v); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}

// GetValue reads one free-form value from the settings table ("" if unset).
func (s *Store) GetValue(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return v, err
}

// SetValue writes one free-form value to the settings table.
func (s *Store) SetValue(ctx context.Context, key, value string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO settings(key, value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// RaiseAlert creates an active alert for (box, key) or refreshes its title. It
// returns true when a new alert row was created.
func (s *Store) RaiseAlert(ctx context.Context, boxID int64, key, severity, title string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := s.db.ExecContext(ctx, `UPDATE alerts SET title=?, severity=?, updated_at=? WHERE box_id=? AND key=? AND active=1`, title, severity, now, boxID, key)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return false, nil
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO alerts (box_id, key, severity, title, active, acked, created_at, updated_at) VALUES (?,?,?,?,1,0,?,?)`,
		boxID, key, severity, title, now, now)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return false, nil // raised concurrently by another evaluation
	}
	return err == nil, err
}

// ResolveAlert marks the active alert for (box, key) resolved.
func (s *Store) ResolveAlert(ctx context.Context, boxID int64, key string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `UPDATE alerts SET active=0, resolved_at=?, updated_at=? WHERE box_id=? AND key=? AND active=1`, now, now, boxID, key)
	return err
}

// ListAlerts returns alerts, newest first, active before resolved.
func (s *Store) ListAlerts(ctx context.Context, limit int) ([]Alert, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, box_id, key, severity, title, active, acked, created_at, updated_at, resolved_at
		FROM alerts ORDER BY active DESC, updated_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var a Alert
		var created, updated string
		var resolved sql.NullString
		if err := rows.Scan(&a.ID, &a.BoxID, &a.Key, &a.Severity, &a.Title, &a.Active, &a.Acked, &created, &updated, &resolved); err != nil {
			return nil, err
		}
		a.CreatedAt, _ = time.Parse(time.RFC3339, created)
		a.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
		if resolved.Valid {
			t, _ := time.Parse(time.RFC3339, resolved.String)
			a.ResolvedAt = &t
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountUnacked returns how many active, unacknowledged alerts exist.
func (s *Store) CountUnacked(ctx context.Context) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alerts WHERE active=1 AND acked=0`).Scan(&n)
	return n, err
}

// AckAlert acknowledges one alert.
func (s *Store) AckAlert(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `UPDATE alerts SET acked=1, updated_at=? WHERE id=?`, time.Now().UTC().Format(time.RFC3339), id)
	return err
}

// AckAll acknowledges every active alert.
func (s *Store) AckAll(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `UPDATE alerts SET acked=1, updated_at=? WHERE active=1 AND acked=0`, time.Now().UTC().Format(time.RFC3339))
	return err
}

// PruneAlerts removes resolved alerts older than keep.
func (s *Store) PruneAlerts(ctx context.Context, keep time.Duration) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM alerts WHERE active=0 AND updated_at < ?`, time.Now().Add(-keep).UTC().Format(time.RFC3339))
	return err
}

// SetCache stores an arbitrary JSON blob for a box under kind.
func (s *Store) SetCache(ctx context.Context, boxID int64, kind string, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO box_cache (box_id, kind, json, at) VALUES (?,?,?,?)
		ON CONFLICT(box_id, kind) DO UPDATE SET json=excluded.json, at=excluded.at`, boxID, kind, string(b), time.Now().UTC().Format(time.RFC3339))
	return err
}

// GetCache loads a cached blob into v and returns when it was written.
func (s *Store) GetCache(ctx context.Context, boxID int64, kind string, v any) (time.Time, error) {
	var js, at string
	err := s.db.QueryRowContext(ctx, `SELECT json, at FROM box_cache WHERE box_id=? AND kind=?`, boxID, kind).Scan(&js, &at)
	if errors.Is(err, sql.ErrNoRows) {
		return time.Time{}, ErrNotFound
	}
	if err != nil {
		return time.Time{}, err
	}
	t, _ := time.Parse(time.RFC3339, at)
	return t, json.Unmarshal([]byte(js), v)
}

// ActiveAlertKeys lists the keys of active alerts for a box.
func (s *Store) ActiveAlertKeys(ctx context.Context, boxID int64) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT key FROM alerts WHERE box_id=? AND active=1`, boxID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteAlert removes one alert row.
func (s *Store) DeleteAlert(ctx context.Context, id int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM alerts WHERE id=?`, id)
	return err
}

// ClearResolved deletes every resolved alert.
func (s *Store) ClearResolved(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM alerts WHERE active=0`)
	return err
}
