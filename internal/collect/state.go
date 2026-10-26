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
	"math"
	"reflect"
	"strconv"
	"strings"
	"time"

	"pmss-fleet-manager/internal/store"
)

// BoxState is everything the UI knows about one box. It is rebuilt after
// every poll and served as JSON.
type BoxState struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	Host       string `json:"host"`
	Port       int    `json:"port"`
	User       string `json:"user"`
	Type       string `json:"type"`
	Plan       string `json:"plan"`
	Datacenter string `json:"datacenter"`
	Renewal    string `json:"renewal"`
	Enabled    bool   `json:"enabled"`

	// Service is the Pulsed Media billing record matched from the customer
	// API, when an API key is configured and a service matches this box.
	Service *ServiceInfo `json:"service,omitempty"`

	// Unpolled marks a state restored from cache or carried across an
	// edit: its reachability fields were reset and nothing has verified
	// them yet, so alert rules must not run on it until a poll lands.
	Unpolled bool `json:"-"`

	// Ready is set by a complete probe of the box's current generation
	// (endpoint and pinned host key, see generation) and is never restored
	// from the cache: every field below that names something on the box (the
	// UID, container IDs, the log list) was read from that machine. Actions
	// that use those fields refuse until it is set (ErrNotReady), so a record
	// moved to another host cannot act on the previous host's data. A failed
	// poll leaves it set: the facts are still that machine's.
	Ready bool `json:"ready"`

	// BoxRev is the box record's revision (store.Box.Revision). Torrent
	// actions carry it back, so a button drawn before an edit cannot act on
	// whatever the record points at after it (ErrStaleBox).
	BoxRev int64 `json:"box_rev"`

	// Rev is the fleet-wide revision this state was stored at (Manager
	// .commit); a client drops a state event whose rev is not above the
	// one it holds for the box.
	Rev uint64 `json:"rev"`

	Status       string    `json:"status"` // online | degraded | offline | disabled | pending
	StatusReason string    `json:"status_reason,omitempty"`
	LastPoll     time.Time `json:"last_poll"`
	LastOK       time.Time `json:"last_ok"`
	Failures     int       `json:"failures"`
	NextPoll     time.Time `json:"next_poll"` // when the next retry is due after a failure (zero when none)
	Backoff      bool      `json:"backoff"`   // retries are spaced out (see retryInterval)
	LastError    string    `json:"last_error,omitempty"`
	PollMs       int64     `json:"poll_ms"`
	HostKeyFP    string    `json:"host_key_fp,omitempty"`

	Hostname string `json:"hostname,omitempty"`
	UID      int    `json:"uid,omitempty"`

	Disk    DiskState    `json:"disk"`
	Traffic TrafficState `json:"traffic"`
	Rates   RateState    `json:"rates"`
	CPU     CPUState     `json:"cpu"`
	RAM     RAMState     `json:"ram"`
	HostSys HostState    `json:"host_sys"`
	Account AccountState `json:"account"`
	Qbt     QbtState     `json:"qbt"`
	Rt      RtState      `json:"rtorrent"`
	Docker  DockerState  `json:"docker"`
	Dirs    DirsState    `json:"dirs"`
	Apps    AppsState    `json:"apps"`
	Logs    LogsState    `json:"logs"`
	Live    LiveState    `json:"live"`
}

// LogFileView is one log file found under the home.
type LogFileView struct {
	Path  string    `json:"path"` // relative to the home
	Size  int64     `json:"size"`
	MTime time.Time `json:"mtime"`
}

// LogsState lists the log files the probe found; the fleet summary drops
// the list. Reading one is on demand (Manager.ReadLog).
type LogsState struct {
	OK    bool          `json:"ok"`
	At    time.Time     `json:"at"` // when the listing was read (inventory polls only; a refresh re-reads it)
	Files []LogFileView `json:"files,omitempty"`
}

// has reports whether path is one of the listed files, which is what
// ReadLog allows: the box's own listing, not an arbitrary path.
func (l LogsState) has(path string) bool {
	for _, f := range l.Files {
		if f.Path == path {
			return true
		}
	}
	return false
}

// DiskState is the user quota view.
type DiskState struct {
	OK         bool    `json:"ok"`
	UsedBytes  int64   `json:"used_bytes"`
	QuotaBytes int64   `json:"quota_bytes"` // soft limit = the plan's quota
	HardBytes  int64   `json:"hard_bytes"`
	Pct        float64 `json:"pct"`
	FilesUsed  int64   `json:"files_used"`
	FilesSoft  int64   `json:"files_soft"`
	Source     string  `json:"source,omitempty"`
	AgeSec     int64   `json:"age_sec,omitempty"`
}

// DayPoint is one day's traffic.
type DayPoint struct {
	Date   string  `json:"date"` // YYYY-MM-DD
	OutMiB float64 `json:"out_mib"`
	InMiB  float64 `json:"in_mib"`
}

// TrafficState is the rolling 30-day traffic view.
type TrafficState struct {
	OK             bool       `json:"ok"`
	OutMonthBytes  int64      `json:"out_month_bytes"`
	InMonthBytes   int64      `json:"in_month_bytes"`
	OutDayBytes    int64      `json:"out_day_bytes"`
	InDayBytes     int64      `json:"in_day_bytes"`
	LimitBytes     int64      `json:"limit_bytes"` // effective; 0 = unlimited
	BaseLimitBytes int64      `json:"base_limit_bytes"`
	BonusBytes     int64      `json:"bonus_bytes"`
	Pct            float64    `json:"pct"`
	Throttled      bool       `json:"throttled"`
	CapMbit        float64    `json:"cap_mbit,omitempty"`
	Daily          []DayPoint `json:"daily"`
}

// RateState is the current bandwidth.
type RateState struct {
	UpBps   float64 `json:"up_bps"`
	DownBps float64 `json:"down_bps"`
	Source  string  `json:"source,omitempty"` // qbt | rtorrent | clients (both) | pmss-15min | ""
}

// CPUState is the slice's CPU accounting.
type CPUState struct {
	OK        bool    `json:"ok"`
	Pct       float64 `json:"pct"`      // of the user's quota if set, else of all host CPUs
	HostPct   float64 `json:"host_pct"` // the same usage as a share of every host core; 0 when the core count is unknown
	QuotaCPUs float64 `json:"quota_cpus"`
	HostCPUs  int     `json:"host_cpus"`
}

// RAMState is the slice's memory accounting.
type RAMState struct {
	OK         bool    `json:"ok"`
	UsedBytes  int64   `json:"used_bytes"`  // charged to the slice, page cache included
	RSSBytes   int64   `json:"rss_bytes"`   // process memory; what pressure is measured on
	CacheBytes int64   `json:"cache_bytes"` // reclaimable page cache (torrent data the kernel keeps around)
	LimitBytes int64   `json:"limit_bytes"`
	Pct        float64 `json:"pct"` // process memory vs limit (charged usage when the split is unknown)
	Pids       int64   `json:"pids"`
	OOMKills   int64   `json:"oom_kills"`          // cumulative kernel OOM kills in the slice
	OOMKnown   bool    `json:"oom_known"`          // the counter was reported, so a rise is meaningful
	LastOOM    string  `json:"last_oom,omitempty"` // RFC3339, when a rise of the counter was first seen
}

// HostState is shared-host information.
type HostState struct {
	UptimeSec    float64 `json:"uptime_sec"`
	Load1        float64 `json:"load1"`
	Load5        float64 `json:"load5"`
	Load15       float64 `json:"load15"`
	CPUOK        bool    `json:"cpu_ok"`         // two /proc/stat readings framed this poll
	CPUBusyPct   float64 `json:"cpu_busy_pct"`   // every tenant's cpu time: all but idle and iowait
	CPUIOWaitPct float64 `json:"cpu_iowait_pct"` // cpus waiting on the disks, outside busy
	CPUStealPct  float64 `json:"cpu_steal_pct"`  // time the hypervisor took, inside busy
	MemTotal     int64   `json:"mem_total"`
	MemAvail     int64   `json:"mem_avail"`
	SwapTotal    int64   `json:"swap_total"`
	SwapKnown    bool    `json:"swap_known"`
	NCPU         int     `json:"ncpu"`
	CPUMHz       int     `json:"cpu_mhz,omitempty"`
	Kernel       string  `json:"kernel,omitempty"`
	Debian       string  `json:"debian,omitempty"`
	PMSSVersion  string  `json:"pmss_version,omitempty"`
	HomeFSType   string  `json:"home_fs_type,omitempty"`
	HomeSize     int64   `json:"home_size"`
	HomeUsed     int64   `json:"home_used"`
	HomeAvail    int64   `json:"home_avail"`
	CPUModel     string  `json:"cpu_model,omitempty"`
	CPUShort     string  `json:"cpu_short,omitempty"`
	CPUFlags     string  `json:"cpu_flags,omitempty"`
	GPUPresent   bool    `json:"gpu_present"`
	GPUDevice    string  `json:"gpu_device,omitempty"`
	GPUVendor    string  `json:"gpu_vendor,omitempty"`
	GPUAccess    bool    `json:"gpu_access"`
	HWEncode     string  `json:"hw_encode,omitempty"` // short human label for tags: "QSV", "VAAPI", "NVENC", "GPU no access", ""
}

// AccountState mirrors the PMSS account config.
type AccountState struct {
	OK            bool    `json:"ok"`
	QuotaGiB      float64 `json:"quota_gib"`
	RAMMiB        float64 `json:"ram_mib"`
	DockerEnabled bool    `json:"docker_enabled"`
	Suspended     bool    `json:"suspended"`
}

// TorrentView is one torrent for the UI.
type TorrentView struct {
	Hash     string  `json:"hash"`   // addresses the torrent in per-torrent actions
	Client   string  `json:"client"` // qbt | rtorrent: which client on the box holds it
	Name     string  `json:"name"`
	Size     int64   `json:"size"`
	Progress float64 `json:"progress"`
	UpBps    int64   `json:"up_bps"`
	DownBps  int64   `json:"down_bps"`
	Ratio    float64 `json:"ratio"`
	State    string  `json:"state"`
	Kind     string  `json:"kind"` // seeding | leeching | paused | checking | error | unknown
	AddedOn  int64   `json:"-"`    // sort key only
	Category string  `json:"category,omitempty"`
	// Lifetime bytes and swarm: connected seeds/peers, and the swarm-wide
	// counts when the tracker reported them (0 when unknown).
	Uploaded   int64 `json:"uploaded"`
	Downloaded int64 `json:"downloaded"`
	Seeds      int   `json:"seeds"`
	SeedsSwarm int   `json:"seeds_swarm"`
	Peers      int   `json:"peers"`
	PeersSwarm int   `json:"peers_swarm"`
}

// QbtState is the torrent client view.
type QbtState struct {
	Enabled       bool   `json:"enabled"`
	Installed     bool   `json:"installed"`
	ProcessUp     bool   `json:"process_up"`
	Reachable     bool   `json:"reachable"`
	Version       string `json:"version,omitempty"`
	Port          int    `json:"port"`
	Error         string `json:"error,omitempty"`
	LocalBypass   bool   `json:"local_bypass"`  // qBittorrent.conf bypasses auth for localhost
	SubnetBypass  bool   `json:"subnet_bypass"` // whitelist bypass enabled
	SubnetList    string `json:"subnet_list,omitempty"`
	ConfUser      string `json:"conf_user,omitempty"`
	HasCreds      bool   `json:"has_creds"`
	AuthHold      bool   `json:"auth_hold"`                // login attempts suspended after a failure
	PortOK        bool   `json:"port_ok"`                  // WebUI port verified as owned by this account
	BinaryVersion string `json:"binary_version,omitempty"` // ~/bin/qbittorrent-nox --version
	LatestVersion string `json:"latest_version,omitempty"` // newest userdocs release
	UpdateAvail   bool   `json:"update_available"`         // binary on disk is older than latest
	RestartNeeded bool   `json:"restart_needed"`           // running version differs from binary on disk
	StartHelper   bool   `json:"start_helper"`             // ~/bin/qbt-start present
	// StartHelperStale: the helper keys on $USER, empty under cron, so each
	// cron run starts another copy; "reapply" autostart rewrites it.
	StartHelperStale bool    `json:"start_helper_stale"`
	CronInstalled    bool    `json:"cron_installed"`
	PMSSManaged      bool    `json:"pmss_managed"` // enabled in the PMSS panel, which runs and configures it
	Total            int     `json:"total"`
	Seeding          int     `json:"seeding"`
	Leeching         int     `json:"leeching"`
	Paused           int     `json:"paused"`
	Errored          int     `json:"errored"`
	GlobalRatio      float64 `json:"global_ratio"`
	// Connection is the client's incoming connectivity as libtorrent sees
	// it: connected | firewalled | disconnected | "" (not read). ListenPort
	// is the port peers must reach. FirewalledSince (RFC3339) is when the
	// client was first seen not connected; a fresh client reports
	// firewalled until the first peer dials in, so alerts wait on it.
	Connection      string        `json:"connection,omitempty"`
	ListenPort      int           `json:"listen_port,omitempty"`
	FirewalledSince string        `json:"firewalled_since,omitempty"`
	Discovery       Discovery     `json:"discovery"`
	Torrents        []TorrentView `json:"torrents,omitempty"`
}

// RtState is the rTorrent view. rTorrent on a PMSS box is the panel's own
// client: PMSS's watchdog starts it and the tenant stops it with the
// ~/.rtorrentDisable marker, so there is no install, start or stop here,
// only monitoring and torrent actions over its SCGI socket.
type RtState struct {
	Enabled   bool `json:"enabled"`
	ProcessUp bool `json:"process_up"`
	// ProcessKnown is false when the process list could not be read; the
	// socket then decides whether the client is up.
	ProcessKnown bool   `json:"process_known"`
	Reachable    bool   `json:"reachable"`
	Version      string `json:"version,omitempty"` // system.client_version
	Library      string `json:"library,omitempty"` // system.library_version
	// Endpoint is the SCGI endpoint in use ("unix:/home/u/.rtorrent.socket"
	// or "tcp:127.0.0.1:5000"); ConfFound whether the config named one.
	Endpoint  string `json:"endpoint,omitempty"`
	ConfFound bool   `json:"conf_found"`
	Disabled  bool   `json:"disabled"` // ~/.rtorrentDisable: stopped on purpose
	Error     string `json:"error,omitempty"`
	// DataDir is the download root remove-with-data may delete under, read
	// from the box's own rTorrent config over SSH (never from the client).
	DataDir string `json:"data_dir,omitempty"`
	// RemoveFailed is the outcome of the last remove-with-data that failed
	// on the box after its request had ended (the browser gone, the request
	// timed out): nobody was left to see the error, so the detached job
	// records it here, logs it and raises an alert (Codex on PR 123). Kept
	// until dismissed (DismissRtRemoveFailure): a later success for another
	// torrent says nothing about these files.
	RemoveFailed   string    `json:"remove_failed,omitempty"`
	RemoveFailedAt time.Time `json:"remove_failed_at,omitempty"`
	// PortOK is set when the endpoint is a loopback port verified as this
	// account's (a unix socket inside the home needs no such check).
	PortOK     bool `json:"port_ok"`
	ListenPort int  `json:"listen_port,omitempty"`
	DHT        bool `json:"dht"` // DHT running now
	// PeX is peer exchange in the running client (PeXKnown: the build
	// answered protocol.pex). ConfDHT and ConfPeX are what the config files
	// give a restart, "on", "off" or "" (not set); they differ from the live
	// values until rTorrent is restarted after a change that could not be
	// applied live. See SetRtDiscovery.
	PeX      bool          `json:"pex"`
	PeXKnown bool          `json:"pex_known"`
	ConfDHT  string        `json:"conf_dht,omitempty"`
	ConfPeX  string        `json:"conf_pex,omitempty"`
	Total    int           `json:"total"`
	Seeding  int           `json:"seeding"`
	Leeching int           `json:"leeching"`
	Paused   int           `json:"paused"`
	Errored  int           `json:"errored"`
	Torrents []TorrentView `json:"torrents,omitempty"`
}

// Discovery is the client's peer-discovery configuration. Known is false
// until app/preferences has been read this poll, so the UI can tell "all
// off" from "not read".
type Discovery struct {
	Known bool `json:"known"`
	DHT   bool `json:"dht"`
	PeX   bool `json:"pex"`
	LSD   bool `json:"lsd"`
}

// ContainerView is one container for the UI.
type ContainerView struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	Image    string   `json:"image"`
	State    string   `json:"state"`
	Status   string   `json:"status"`
	Ports    []string `json:"ports"`
	CPUPct   float64  `json:"cpu_pct"`
	MemUsage int64    `json:"mem_usage"`
	HasStats bool     `json:"has_stats"`
}

// DockerState is the rootless docker view.
type DockerState struct {
	Enabled    bool            `json:"enabled"`
	SocketUp   bool            `json:"socket_up"`
	Reachable  bool            `json:"reachable"`
	Version    string          `json:"version,omitempty"`
	Error      string          `json:"error,omitempty"`
	Running    int             `json:"running"`
	Stopped    int             `json:"stopped"`
	Containers []ContainerView `json:"containers,omitempty"`
}

// DirEntry is one measured directory.
type DirEntry struct {
	Name  string `json:"name"`
	Bytes int64  `json:"bytes"`
}

// DirsState is the cached du result.
type DirsState struct {
	Enabled bool `json:"enabled"`
	// Gen is the generation the measurement was taken under (see
	// generation); a cached result from another is not this box's.
	Gen     string     `json:"gen,omitempty"`
	At      time.Time  `json:"at"`
	Running bool       `json:"running"`
	Error   string     `json:"error,omitempty"`
	Entries []DirEntry `json:"entries,omitempty"`
}

// EffectiveRenewal is the manual renewal date, else the next-due date of the
// matched service if that service is active. A cancelled service's due date
// is history, not a renewal to alert on.
func (s BoxState) EffectiveRenewal() string {
	if s.Renewal != "" || s.Service == nil || !serviceActive(s.Service.Status) {
		return s.Renewal
	}
	return s.Service.NextDue
}

// adoptIdentity copies the fields that come from the box record (not from a
// poll) out of from, so measured data can be kept across an edit or restart
// while name, host, plan and feature switches reflect the current record.
func (s *BoxState) adoptIdentity(from BoxState) {
	s.Name, s.Host, s.Port, s.User, s.Type = from.Name, from.Host, from.Port, from.User, from.Type
	s.Plan, s.Datacenter, s.Renewal, s.Enabled, s.HostKeyFP = from.Plan, from.Datacenter, from.Renewal, from.Enabled, from.HostKeyFP
	s.Qbt.Enabled, s.Rt.Enabled, s.Docker.Enabled, s.Dirs.Enabled = from.Qbt.Enabled, from.Rt.Enabled, from.Docker.Enabled, from.Dirs.Enabled
	// The SCGI endpoint is the record's override, or nothing until the next
	// poll resolves it from the box: a carried endpoint could be a setting
	// the edit just changed or removed, and actions would reach the old one
	// (Codex on PR 123). The poller polls at once after an edit.
	s.Rt.Endpoint = from.Rt.Endpoint
	s.Apps.Watched = from.Apps.Watched
	s.Apps.markWatched()
}

// resetVolatile clears what only a fresh poll by the current poller may
// assert: port ownership (a credential-send gate), reachability, the auth
// hold flag, the du-running flag and readiness, and marks the state
// Unpolled. Called when a state is restored from cache or carried across
// an edit.
func (s *BoxState) resetVolatile() {
	s.Qbt.PortOK, s.Qbt.Reachable, s.Qbt.AuthHold = false, false, false
	s.Rt.PortOK, s.Rt.Reachable = false, false
	s.Docker.Reachable = false
	s.Dirs.Running = false
	s.Unpolled = true
	s.Ready = false
}

// has reports whether cid is one of the containers the last poll listed,
// which is what a container action or log read may name.
func (d DockerState) has(cid string) bool {
	for _, c := range d.Containers {
		if c.ID == cid {
			return true
		}
	}
	return false
}

// generation identifies the machine behind a box record: its SSH endpoint
// (host, port, account) and the pinned host key's fingerprint. Everything
// measured on the box belongs to the generation it was measured under; an
// edit that moves the record, or forgets the pin so the next connect may
// reach a reinstalled machine, starts a new one, and nothing from the old
// generation (live state, cached state, cached du) is used for the new.
func generation(host string, port int, user, hostKeyFP string) string {
	return strings.ToLower(host) + ":" + strconv.Itoa(port) + "/" + user + "@" + hostKeyFP
}

func boxGeneration(b *store.Box) string {
	return generation(b.Host, b.Port, b.User, fingerprintOrEmpty(b.HostKey))
}

// generation is the state's own generation (host, port, user and host-key
// fingerprint are copied from the record and the pin that served it).
func (s BoxState) generation() string {
	return generation(s.Host, s.Port, s.User, s.HostKeyFP)
}

// sameGeneration reports whether a measurement taken under gen may serve
// box b: the same endpoint and key, or the same endpoint before any key
// was pinned (the first poll pins, then records).
func sameGeneration(gen string, b *store.Box) bool {
	if gen == boxGeneration(b) {
		return true
	}
	endpoint, fp, ok := strings.Cut(gen, "@")
	return ok && fp == "" && endpoint == AuthEndpoint(b)
}

// sanitizeFloats walks a value and replaces every NaN or infinite float64
// with 0. It is the last line of defence before a state is stored or
// serialised: encoding/json refuses those values and one bad number on one
// box would otherwise take the whole fleet response down. Field-level
// sanitising upstream remains, but a missed path is no longer fatal.
func sanitizeFloats(v reflect.Value) {
	switch v.Kind() {
	case reflect.Float64, reflect.Float32:
		if v.CanSet() {
			f := v.Float()
			if math.IsNaN(f) || math.IsInf(f, 0) {
				v.SetFloat(0)
			}
		}
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			sanitizeFloats(v.Field(i))
		}
	case reflect.Ptr:
		if !v.IsNil() {
			sanitizeFloats(v.Elem())
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			sanitizeFloats(v.Index(i))
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			e := v.MapIndex(k)
			if e.Kind() == reflect.Float64 {
				if f := e.Float(); math.IsNaN(f) || math.IsInf(f, 0) {
					v.SetMapIndex(k, reflect.ValueOf(0.0))
				}
			} else if e.Kind() == reflect.Struct || e.Kind() == reflect.Ptr {
				cp := reflect.New(e.Type()).Elem()
				cp.Set(e)
				sanitizeFloats(cp)
				v.SetMapIndex(k, cp)
			}
		}
	}
}

// Sanitize makes the state safe to serialise (see sanitizeFloats).
func (s *BoxState) Sanitize() { sanitizeFloats(reflect.ValueOf(s).Elem()) }

// Summary strips the large lists for the fleet view and SSE events.
func (s BoxState) Summary() BoxState {
	c := s
	c.Qbt.Torrents = nil
	c.Rt.Torrents = nil
	c.Docker.Containers = nil
	c.Dirs.Entries = nil
	c.Traffic.Daily = nil
	c.Apps.Processes = nil
	c.Logs.Files = nil
	return c
}

func fromBox(b *store.Box) BoxState {
	return BoxState{
		ID: b.ID, Name: b.Name, Host: b.Host, Port: b.Port, User: b.User, Type: b.Type,
		Plan: b.Plan, Datacenter: b.Datacenter, Renewal: b.Renewal, Enabled: b.Enabled, BoxRev: b.Revision,
		Status:    "pending",
		Qbt:       QbtState{Enabled: b.QbtEnabled, Port: b.QbtPort},
		Rt:        RtState{Enabled: b.RtEnabled, Endpoint: b.RtSCGI},
		Docker:    DockerState{Enabled: b.DockerEnabled},
		Dirs:      DirsState{Enabled: b.DuEnabled},
		Apps:      AppsState{Watched: append([]string{}, b.AppsWatch...)},
		HostKeyFP: fingerprintOrEmpty(b.HostKey),
	}
}
