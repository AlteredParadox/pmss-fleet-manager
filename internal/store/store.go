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
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite" // registers the "sqlite" driver with database/sql

	"pmss-fleet-manager/internal/secrets"
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

	// RtEnabled turns rTorrent monitoring on; RtSCGI overrides the SCGI
	// endpoint read from the box's ~/.rtorrent.rc ("unix:/path" or
	// "tcp:127.0.0.1:port"; blank = from the config).
	RtEnabled bool   `json:"rt_enabled"`
	RtSCGI    string `json:"rt_scgi"`

	DockerEnabled bool `json:"docker_enabled"`
	DuEnabled     bool `json:"du_enabled"`
	Enabled       bool `json:"enabled"`
	// AppsWatch lists the detected applications (collect.AppView keys) that
	// raise an alert when they are not running. Edited only through
	// SetAppsWatch, so a box edit never touches it.
	AppsWatch []string `json:"apps_watch"`
	// Revision counts edits of the record. An update must name the
	// revision it was based on and fails with ErrStale otherwise, so two
	// tabs editing one box from the same snapshot cannot each apply their
	// view of the endpoint and leave a password bound to the wrong host.
	Revision int64 `json:"revision"`

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

	// PMDBEnabled switches on the pmdb.fyi hardware catalogue integration:
	// the dashboard checks the public catalogue for each box's host and
	// submits the host's hardware when it is missing. The upload key is
	// handled like PMAPIKey: never sent to the browser, kept on an empty
	// PUT, removed by ClearPMDBUploadKey.
	PMDBEnabled        bool   `json:"pmdb_enabled"`
	PMDBUploadKey      string `json:"pmdb_upload_key,omitempty"`
	HasPMDBUploadKey   bool   `json:"has_pmdb_upload_key"`
	ClearPMDBUploadKey bool   `json:"clear_pmdb_upload_key,omitempty"`
}

// Public returns a copy safe to serialise to the browser.
func (s Settings) Public() Settings {
	s.HasPMAPIKey = s.PMAPIKey != ""
	s.PMAPIKey = ""
	s.ClearPMAPIKey = false
	s.HasPMDBUploadKey = s.PMDBUploadKey != ""
	s.PMDBUploadKey = ""
	s.ClearPMDBUploadKey = false
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

func boolKV(b bool) string {
	if b {
		return "1"
	}
	return "0"
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
	// sealer encrypts the secret columns (see labels below); nil stores
	// them in plain text. Set with WithSealer.
	sealer *secrets.Sealer
	// prev opens credentials sealed under the secret before a change (see
	// WithPreviousSealer); forget drops the ones nothing opens.
	prev      *secrets.Sealer
	forget    bool
	resealed  int
	forgotten int
}

// The additional data each secret column is sealed under: a ciphertext
// copied from one column to another does not open.
const (
	labelPassword      = "boxes.password"
	labelQbtPass       = "boxes.qbt_pass"
	labelPMAPIKey      = "settings.pm_api_key"
	labelPMDBUploadKey = "settings.pmdb_upload_key"
	// Not passwords, but either may carry a token (a private build's
	// download link, a start command with a key in it).
	labelQbtStartCmd    = "boxes.qbt_start_cmd"
	labelQbtDownloadURL = "settings.qbt_download_url"
)

// Option configures Open.
type Option func(*Store)

// WithSealer encrypts the stored credentials with sl (nil: no encryption).
// On Open, plaintext credentials already in the database are sealed in
// place and the file is vacuumed so the plaintext is not left in free
// pages; a sealed credential that does not open with sl makes Open fail.
// Without a sealer, Open fails if the database holds sealed credentials,
// so a dashboard started without its secret never sends ciphertext to a
// box as the password.
func WithSealer(sl *secrets.Sealer) Option { return func(s *Store) { s.sealer = sl } }

// WithPreviousSealer names the secret the credentials were sealed under
// before this start (nil: none): credentials that open with it (and not
// with the current sealer) are re-encrypted under the current one, or
// stored in plain text again when there is none. That is how the secret
// is changed, or encryption switched off, without re-entering anything.
func WithPreviousSealer(sl *secrets.Sealer) Option { return func(s *Store) { s.prev = sl } }

// WithForgetUnreadable makes Open clear every sealed credential that
// neither the current nor the previous sealer opens, instead of failing.
// It is the way out after losing the secret: the dashboard starts with
// those passwords and keys removed, and the edit forms ask for them again.
func WithForgetUnreadable() Option { return func(s *Store) { s.forget = true } }

// ResealedOnOpen reports how many credentials Open re-encrypted: plaintext
// ones sealed for the first time, and ones moved from the previous secret.
func (s *Store) ResealedOnOpen() int { return s.resealed }

// ForgottenOnOpen reports how many unreadable credentials Open cleared.
func (s *Store) ForgottenOnOpen() int { return s.forgotten }

// Open opens (creating if needed) the SQLite database at path.
func Open(path string, opts ...Option) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	// Create the file owner-only BEFORE SQLite does: the -wal and -shm
	// files copy the main file's mode when they are first created, so a
	// chmod after the fact would leave them (and the plaintext secrets in
	// the WAL) readable by other local users until the next clean close.
	// This matters when -data points at a pre-existing 0755 directory.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	// Existing files from an older build or an unclean stop: fix their modes
	// before SQLite reuses them (and again after, for ones created now). A
	// chmod that fails leaves plaintext secrets readable by other local
	// users, so it is fatal rather than logged: a bind-mounted database
	// owned by another uid is exactly the case that must not start.
	if err := restrictModes(path, "", "-wal", "-shm"); err != nil {
		return nil, err
	}
	// The path goes into a SQLite file URI, where ? starts the query, #
	// a fragment and % an escape: a data directory containing one would
	// open a different file from the one created and chmodded above.
	dsn := "file:" + sqliteURIPath.Replace(path) + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{db: db}
	for _, o := range opts {
		o(s)
	}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.resealSecrets(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	if err := restrictModes(path, "-wal", "-shm"); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// sqliteURIPath escapes the characters with a meaning in a SQLite file URI.
var sqliteURIPath = strings.NewReplacer("%", "%25", "?", "%3F", "#", "%23")

// restrictModes makes the database file and its existing sidecars
// owner-only; a missing sidecar is fine, anything else is an error that
// names the file.
func restrictModes(path string, sides ...string) error {
	for _, side := range sides {
		err := os.Chmod(path+side, 0o600)
		if err == nil || errors.Is(err, fs.ErrNotExist) {
			continue
		}
		return fmt.Errorf("cannot make %s owner-only (%w): the database holds box passwords and the API key, fix its ownership or permissions", path+side, err)
	}
	return nil
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
	if err := s.ensureAppsWatchColumn(); err != nil {
		return err
	}
	if err := s.ensureRevisionColumn(); err != nil {
		return err
	}
	return s.ensureRtColumns()
}

// Columns for rTorrent monitoring (1.10.0); constants like the others.
const (
	addRtEnabledColSQL = "ALTER TABLE boxes ADD COLUMN rt_enabled INTEGER NOT NULL DEFAULT 0"
	addRtSCGIColSQL    = "ALTER TABLE boxes ADD COLUMN rt_scgi TEXT NOT NULL DEFAULT ''"
)

// ensureRtColumns adds boxes.rt_enabled and boxes.rt_scgi to a database
// from before they existed.
func (s *Store) ensureRtColumns() error {
	for _, c := range []struct{ name, ddl string }{{"rt_enabled", addRtEnabledColSQL}, {"rt_scgi", addRtSCGIColSQL}} {
		has, err := s.hasBoxesColumn(c.name)
		if err != nil || has {
			if err != nil {
				return err
			}
			continue
		}
		if _, err := s.db.Exec(c.ddl); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

const addRevisionColSQL = "ALTER TABLE boxes ADD COLUMN revision INTEGER NOT NULL DEFAULT 0"

// ensureRevisionColumn adds boxes.revision to a database from before it existed.
func (s *Store) ensureRevisionColumn() error {
	has, err := s.hasBoxesColumn("revision")
	if err != nil || has {
		return err
	}
	if _, err := s.db.Exec(addRevisionColSQL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// Columns added after 1.3.1; SQLite has no ADD COLUMN IF NOT EXISTS, so the
// table is inspected first. Both statements are constants.
const (
	boxesColumnsSQL    = "PRAGMA table_info(boxes)"
	addAppsWatchColSQL = "ALTER TABLE boxes ADD COLUMN apps_watch TEXT NOT NULL DEFAULT ''"
)

// ensureAppsWatchColumn adds boxes.apps_watch to a database from before it existed.
func (s *Store) ensureAppsWatchColumn() error {
	has, err := s.hasBoxesColumn("apps_watch")
	if err != nil {
		return err
	}
	if has {
		return nil
	}
	if _, err := s.db.Exec(addAppsWatchColSQL); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	return nil
}

// hasBoxesColumn reports whether the boxes table has the named column.
func (s *Store) hasBoxesColumn(col string) (bool, error) {
	rows, err := s.db.Query(boxesColumnsSQL)
	if err != nil {
		return false, fmt.Errorf("migrate: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notnull, pk int
		var name, typ string
		var dflt any
		if err := rows.Scan(&cid, &name, &typ, &notnull, &dflt, &pk); err != nil {
			return false, fmt.Errorf("migrate: %w", err)
		}
		if name == col {
			return true, nil
		}
	}
	if err := rows.Err(); err != nil {
		return false, fmt.Errorf("migrate: %w", err)
	}
	return false, nil
}

// SetAppsWatch replaces a box's watch list (see Box.AppsWatch).
func (s *Store) SetAppsWatch(ctx context.Context, id int64, keys []string) error {
	if keys == nil {
		keys = []string{}
	}
	b, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx, `UPDATE boxes SET apps_watch=? WHERE id=?`, string(b), id)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
}

const boxCols = `id, name, host, port, user, auth_method, key_path, password, host_key, type, plan, datacenter, renewal,
	qbt_enabled, qbt_port, qbt_user, qbt_pass, qbt_start_cmd, docker_enabled, du_enabled, enabled, created_at, updated_at, apps_watch, revision, rt_enabled, rt_scgi`

// The box queries are constants: nothing from a request is ever spliced in.
const (
	listBoxesSQL = "SELECT " + boxCols + " FROM boxes ORDER BY name COLLATE NOCASE, id"
	getBoxSQL    = "SELECT " + boxCols + " FROM boxes WHERE id = ?"
)

func (s *Store) scanBox(row interface{ Scan(...any) error }) (*Box, error) {
	var b Box
	var created, updated, watch string
	err := row.Scan(&b.ID, &b.Name, &b.Host, &b.Port, &b.User, &b.AuthMethod, &b.KeyPath, &b.Password, &b.HostKey,
		&b.Type, &b.Plan, &b.Datacenter, &b.Renewal, &b.QbtEnabled, &b.QbtPort, &b.QbtUser, &b.QbtPass, &b.QbtStartCmd,
		&b.DockerEnabled, &b.DuEnabled, &b.Enabled, &created, &updated, &watch, &b.Revision, &b.RtEnabled, &b.RtSCGI)
	if err != nil {
		return nil, err
	}
	if b.Password, err = s.openSecret(labelPassword, b.Password); err != nil {
		return nil, fmt.Errorf("box %d SSH password: %w", b.ID, err)
	}
	if b.QbtPass, err = s.openSecret(labelQbtPass, b.QbtPass); err != nil {
		return nil, fmt.Errorf("box %d WebUI password: %w", b.ID, err)
	}
	if b.QbtStartCmd, err = s.openSecret(labelQbtStartCmd, b.QbtStartCmd); err != nil {
		return nil, fmt.Errorf("box %d qBittorrent start command: %w", b.ID, err)
	}
	b.AppsWatch = []string{}
	if watch != "" {
		_ = json.Unmarshal([]byte(watch), &b.AppsWatch) // an unreadable list is an empty one
	}
	b.CreatedAt, _ = time.Parse(time.RFC3339, created)
	b.HasPassword, b.HasQbtPass = b.Password != "", b.QbtPass != ""
	b.UpdatedAt, _ = time.Parse(time.RFC3339, updated)
	return &b, nil
}

// ListBoxes returns all boxes ordered by name.
func (s *Store) ListBoxes(ctx context.Context) ([]*Box, error) {
	rows, err := s.db.QueryContext(ctx, listBoxesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Box
	for rows.Next() {
		b, err := s.scanBox(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ErrNotFound is returned when a row does not exist.
var ErrNotFound = errors.New("not found")

// ErrStale is returned by UpdateBox when the record changed since the
// revision the edit was based on.
var ErrStale = errors.New("the box was changed by another edit since this form was loaded; reload it and apply your change again")

// GetBox loads one box.
func (s *Store) GetBox(ctx context.Context, id int64) (*Box, error) {
	b, err := s.scanBox(s.db.QueryRowContext(ctx, getBoxSQL, id))
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
	// The Box keeps its plaintext: the caller starts the poller from it.
	pw, qp, sc := s.sealBoxSecrets(b)
	res, err := s.db.ExecContext(ctx, `INSERT INTO boxes (name, host, port, user, auth_method, key_path, password, host_key, type, plan, datacenter, renewal,
		qbt_enabled, qbt_port, qbt_user, qbt_pass, qbt_start_cmd, docker_enabled, du_enabled, enabled, created_at, updated_at, rt_enabled, rt_scgi)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		b.Name, b.Host, b.Port, b.User, b.AuthMethod, b.KeyPath, pw, b.HostKey, b.Type, b.Plan, b.Datacenter, b.Renewal,
		b.QbtEnabled, b.QbtPort, b.QbtUser, qp, sc, b.DockerEnabled, b.DuEnabled, b.Enabled,
		now.Format(time.RFC3339), now.Format(time.RFC3339), b.RtEnabled, b.RtSCGI)
	if err != nil {
		return err
	}
	b.ID, err = res.LastInsertId()
	return err
}

// Clears names the stored secrets an update removes. A blank Password or
// QbtPass on the Box keeps the stored value, so removing one is explicit.
type Clears struct {
	Password bool // the SSH password
	QbtPass  bool // the WebUI password
	// The host-key pin: ForgetHostKey clears it, ReplaceHostKey (the
	// endpoint changed) sets it to the Box's HostKey (possibly none), and
	// neither keeps a pin already in the row, filling an empty one with the
	// Box's HostKey. Decided in the same statement as the rest, so a pin a
	// poll stored between the edit's read and its write survives.
	ForgetHostKey  bool
	ReplaceHostKey bool
}

// UpdateBox writes all mutable fields in one statement, conditioned on
// b.Revision being the row's current revision (compare-and-set) and
// incrementing it. For each secret the outcome is clear, keep (blank field)
// or replace, so an edit and the removal of a password it implies land
// together or not at all. A missing row is ErrNotFound, a changed row
// ErrStale; on success b.Revision is the new revision.
func (s *Store) UpdateBox(ctx context.Context, b *Box, clears Clears) error {
	now := time.Now().UTC()
	b.UpdatedAt = now
	if clears.Password {
		b.Password = ""
	}
	if clears.QbtPass {
		b.QbtPass = ""
	}
	// A blank secret seals to blank, so the CASE below still reads it as
	// "keep the stored value".
	pw, qp, sc := s.sealBoxSecrets(b)
	res, err := s.db.ExecContext(ctx, `UPDATE boxes SET name=?, host=?, port=?, user=?, auth_method=?, key_path=?,
		password=CASE WHEN ? THEN '' WHEN ?='' THEN password ELSE ? END,
		host_key=CASE WHEN ? THEN '' WHEN ? THEN ? WHEN host_key='' THEN ? ELSE host_key END,
		type=?, plan=?, datacenter=?, renewal=?,
		qbt_enabled=?, qbt_port=?, qbt_user=?, qbt_pass=CASE WHEN ? THEN '' WHEN ?='' THEN qbt_pass ELSE ? END, qbt_start_cmd=?,
		docker_enabled=?, du_enabled=?, enabled=?, updated_at=?, rt_enabled=?, rt_scgi=?, revision=revision+1 WHERE id=? AND revision=?`,
		b.Name, b.Host, b.Port, b.User, b.AuthMethod, b.KeyPath, clears.Password, pw, pw,
		clears.ForgetHostKey, clears.ReplaceHostKey, b.HostKey, b.HostKey,
		b.Type, b.Plan, b.Datacenter, b.Renewal,
		b.QbtEnabled, b.QbtPort, b.QbtUser, clears.QbtPass, qp, qp, sc, b.DockerEnabled, b.DuEnabled, b.Enabled,
		now.Format(time.RFC3339), b.RtEnabled, b.RtSCGI, b.ID, b.Revision)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err == nil && n == 0 {
		var exists int
		if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM boxes WHERE id=?`, b.ID).Scan(&exists); err != nil {
			return err
		}
		if exists == 0 {
			return ErrNotFound
		}
		return ErrStale
	}
	b.Revision++
	return nil
}

// openSecret returns the plaintext of a stored value. Without a sealer
// every value is plaintext, whatever it starts with: Open established that
// (the secrets_sealed marker) before anything is read.
func (s *Store) openSecret(label, stored string) (string, error) {
	if s.sealer == nil {
		return stored, nil
	}
	return s.sealer.Open(label, stored)
}

// sealBoxSecrets returns the stored forms of b's SSH and WebUI passwords
// and its qBittorrent start command.
func (s *Store) sealBoxSecrets(b *Box) (pw, qp, sc string) {
	return s.sealer.Seal(labelPassword, b.Password), s.sealer.Seal(labelQbtPass, b.QbtPass),
		s.sealer.Seal(labelQbtStartCmd, b.QbtStartCmd)
}

// SetHostKey pins a key seen while dialling host:port onto the box, only if
// the record still names that endpoint and has no pin: an edit that moved
// the box meanwhile must not end up pinned to the old host's key.
// It reports whether a row was pinned: none when the box was edited to
// another endpoint meanwhile or already holds a pin.
func (s *Store) SetHostKey(ctx context.Context, id int64, host string, port int, key string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `UPDATE boxes SET host_key=? WHERE id=? AND lower(host)=lower(?) AND port=? AND host_key=''`, key, id, host, port)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// DeleteBox removes a box and its alerts and cache.
func (s *Store) DeleteBox(ctx context.Context, id int64) error {
	r, err := s.db.ExecContext(ctx, `DELETE FROM boxes WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, err := r.RowsAffected(); err == nil && n == 0 {
		return ErrNotFound
	}
	return nil
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
	gets("pmdb_upload_key", &out.PMDBUploadKey)
	out.PMDBEnabled = kv["pmdb_enabled"] == "1"
	if err := rows.Err(); err != nil {
		return out, err
	}
	if out.QbtDownloadURL, err = s.openSecret(labelQbtDownloadURL, out.QbtDownloadURL); err != nil {
		return out, fmt.Errorf("qBittorrent download URL: %w", err)
	}
	if out.QbtDownloadURL == "" {
		out.QbtDownloadURL = DefaultQbtDownloadURL
	}
	if out.PMAPIKey, err = s.openSecret(labelPMAPIKey, out.PMAPIKey); err != nil {
		return out, fmt.Errorf("Pulsed Media API key: %w", err)
	}
	if out.PMDBUploadKey, err = s.openSecret(labelPMDBUploadKey, out.PMDBUploadKey); err != nil {
		return out, fmt.Errorf("pmdb upload key: %w", err)
	}
	return out, nil
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
		"qbt_download_url":      s.sealer.Seal(labelQbtDownloadURL, st.QbtDownloadURL),
		"card_min_px":           strconv.Itoa(st.CardMinPx),
		"pm_api_key":            s.sealer.Seal(labelPMAPIKey, st.PMAPIKey),
		"pmdb_upload_key":       s.sealer.Seal(labelPMDBUploadKey, st.PMDBUploadKey),
		"pmdb_enabled":          boolKV(st.PMDBEnabled),
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

// RaiseAlert creates an active alert for (box, key) or refreshes its title and
// severity. An acknowledged alert that escalates (warn to bad) is unacked
// again, so the badge comes back; the same or a lower severity keeps the ack.
// It returns true when a new alert row was created.
func (s *Store) RaiseAlert(ctx context.Context, boxID int64, key, severity, title string) (bool, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	// Update an active row only when something material changes (title,
	// severity, or an escalation un-acknowledging it), so an unchanged
	// alert costs no write per poll and a change is reported to the
	// caller, which announces it. The CASE reads the stored severity: SET
	// expressions see the old row.
	rank := severityRank(severity)
	res, err := s.db.ExecContext(ctx, `UPDATE alerts SET title=?, severity=?,
		acked=CASE WHEN ? > (CASE severity WHEN 'bad' THEN 2 WHEN 'warn' THEN 1 ELSE 0 END) THEN 0 ELSE acked END,
		updated_at=? WHERE box_id=? AND key=? AND active=1
		AND (title<>? OR severity<>? OR (acked=1 AND ? > (CASE severity WHEN 'bad' THEN 2 WHEN 'warn' THEN 1 ELSE 0 END)))`,
		title, severity, rank, now, boxID, key, title, severity, rank)
	if err != nil {
		return false, err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		return true, nil // changed
	}
	var active int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM alerts WHERE box_id=? AND key=? AND active=1`, boxID, key).Scan(&active); err != nil {
		return false, err
	}
	if active > 0 {
		return false, nil // unchanged
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO alerts (box_id, key, severity, title, active, acked, created_at, updated_at) VALUES (?,?,?,?,1,0,?,?)`,
		boxID, key, severity, title, now, now)
	if err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed") {
		return false, nil // raised concurrently by another evaluation
	}
	return err == nil, err
}

// RaiseAlert's result is "changed": a new row, or an active row whose
// title or severity changed (or that an escalation un-acknowledged). An
// unchanged active alert is a no-op and reports false.

// severityRank orders alert severities; it must match the CASE in RaiseAlert.
func severityRank(severity string) int {
	switch severity {
	case "bad":
		return 2
	case "warn":
		return 1
	}
	return 0
}

// ResolveAlert marks the active alert for (box, key) resolved.
func (s *Store) ResolveAlert(ctx context.Context, boxID int64, key string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(ctx, `UPDATE alerts SET active=0, resolved_at=?, updated_at=? WHERE box_id=? AND key=? AND active=1`, now, now, boxID, key)
	return err
}

// ResolveBoxAlerts resolves every active alert of a box except the given
// keys, and reports how many it resolved. Used when a record moves to
// another machine: the old machine's alerts are not the new one's.
func (s *Store) ResolveBoxAlerts(ctx context.Context, boxID int64, except ...string) (int64, error) {
	now := time.Now().UTC().Format(time.RFC3339)
	q := `UPDATE alerts SET active=0, resolved_at=?, updated_at=? WHERE box_id=? AND active=1`
	args := []any{now, now, boxID}
	for _, k := range except {
		q += ` AND key<>?`
		args = append(args, k)
	}
	res, err := s.db.ExecContext(ctx, q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
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

// DeleteCache drops the given cached kinds of a box.
func (s *Store) DeleteCache(ctx context.Context, boxID int64, kinds ...string) error {
	for _, k := range kinds {
		if _, err := s.db.ExecContext(ctx, `DELETE FROM box_cache WHERE box_id=? AND kind=?`, boxID, k); err != nil {
			return err
		}
	}
	return nil
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
