# pmss-fleet-manager architecture

Single Go binary. One central instance polls every Pulsed Media box over SSH as the
tenant user (no root, nothing installed on the boxes) and serves a web dashboard.

```
browser  <-- HTTP/JSON + SSE -->  pmss-fleet-manager  <-- SSH -->  box (tenant shell)
                                     |  sqlite                 |-- probe.sh  (quota, traffic, cgroup, /proc)
                                     |                         |-- 127.0.0.1:<port>  qBittorrent Web API
                                     |                         `-- /run/user/<uid>/docker.sock  Docker API
```

## Data sources on a PMSS box (tenant-readable)

| Need | Source | Notes |
|---|---|---|
| disk used / quota / inodes | `quota -v -w` (live) or `~/.quota` (1 min cron) | KiB blocks; hard limit is 1.25x soft |
| traffic, rolling 30 days | `~/.trafficData` (egress), `~/.trafficDataIngress` (ingress) | PHP `serialize()`: `{raw:{month,week,day,hour,15min} MiB, daily:{Y/m/d: MiB}}`, refreshed :25/:55 |
| traffic limit | `~/.trafficLimit` + `~/.bonusTraffic` (GiB) | effective = limit + min(bonus, limit); 0 = unlimited |
| throttled | `~/.throttle` exists | set hourly by PMSS when over limit |
| ram / cpu / pids | cgroup slice `user-<uid>.slice` (v1 controllers, v2 fallback): usage, `memory.stat` rss/cache split, OOM-kill counter | pressure is process memory, not the charged figure (page cache); CPU% from usage delta between polls |
| account config | `~/.config/pmss-user.json` | quota GiB, ramMiB, dockerEnabled, trafficCapMbit |
| host | `/proc/uptime`, `/proc/loadavg`, `/proc/stat` (busy / iowait / steal over the poll, as PMSS's live-stats panel counts them), `/proc/meminfo`, `/proc/cpuinfo`, `/dev/dri/renderD*`, `/etc/debian_version`, `/etc/seedbox/config/version`, `df /home` | shared host, informational |
| WebUI connection owner | after every new tunnel connection to the port, `EstablishedScript` lists the established sockets on that port (`/proc/net/tcp{,6}` state 01, else `ss -Htnp state established`) and every one must be owned by this uid (`probe.EstablishedOwner`), before any byte is written; a verified connection's peer is fixed so pooling is safe; where the box shows no established sockets at all (socket-table privacy hosts) the listener check alone applies | closes the check-then-connect gap of the listener check: a co-tenant who grabs the port between check and connect accepts a connection that is then refused; pooled connections are dropped when the client process changes |
| WebUI port owner | `/proc/net/tcp` and `/proc/net/tcp6` listening rows (uid per socket); `ss -tlnp` where PMSS has made those tables root-only (own sockets carry the uid, any other listener is foreign) | the credential gate: WebUI logins only when the port is bound by this account alone; re-read live before every action that sends credentials (client stop, pause/resume all, per-torrent stop/start/remove, add, DHT/PeX/LSD toggles) |
| client install state | `~/bin/qbittorrent-nox --version` (only when the binary's `stat -L '%s %Y %Z %i'` changed since the last answer, passed back as `PMSS_QBT_SEEN`: the call writes a start/exit block into the user's qBittorrent log), `~/bin/qbt-start`, `crontab -l` | drives "update available", "restart to apply" and the autostart hint |
| torrents | qBittorrent Web API over SSH tunnel (`sync/maindata` incrementally: the first call fetches everything, later ones send the last response id and merge the delta; the view resets whenever the client process may have changed; at most 20 000 torrents per box, decoded as a stream, more is an error), `app/version` | user-installed `~/bin/qbittorrent-nox`; port from `~/.config/qBittorrent/qBittorrent.conf` or box setting. Writes on request: `torrents/stop|start|pause|resume|delete|add`, `app/shutdown` |
| DHT / PeX / LSD, listen port | `app/preferences` every poll; `app/setPreferences` on toggle | only the three booleans are written |
| incoming connectivity | `server_state.connection_status` from `sync/maindata` | connected / firewalled / disconnected; the time it was first seen not connected is carried in state, and the alert waits ten minutes because a fresh client says firewalled until a peer dials in |
| containers | Docker Engine API on `/run/user/<uid>/docker.sock` over SSH | rootless dockerd |
| log files | `find` over a fixed list of directories under the home (`*.log`, `*.log.*`, `*.txt`, 40 per directory) plus a few single files, as size/mtime/relative path; on demand `tail -n 300 \| tail -c 256K` of one listed path, the path quoted into the script body (never a command line); container output via the Engine API `/containers/{id}/logs` with the stream framing removed | read-only; the API only reads a path the last probe listed, and the script re-checks it on the box (relative, no `..`, regular file) |
| own processes / applications | `ps -u <uid>` (pid, ppid, rss, cumulative CPU seconds, age, comm; args separately, clipped), `ss -tlnp` for port -> own pid | hidepid=2 means only the account's processes are visible anyway; the probe's own shell and children are dropped; `internal/collect/apps.go` groups them into apps by comm/args and derives CPU% from two samples of the same pid; the watch list lives in `boxes.apps_watch` and is edited only through its own endpoint |
| usage by directory | `du` on top-level home entries, low priority, every few hours | optional, heavy; the 500 largest entries are kept, the rest summed into one |
| plan / price / renewal / PM usage | Pulsed Media customer API `https://pulsedmedia.com/remote/pmapi-feed.php` (`X-API-Key`) | optional; synced every 6 h and on demand; cached in the settings table; matched to boxes by host+user, then unique host, then unique user. Manual plan / renewal on a box override it; datacenter is manual only |

PMSS "month" counters are a rolling 30-day window, not a billing cycle. The UI says so.

## Packages

- `cmd/pmss-fleet-manager` flags, wiring, graceful shutdown
- `internal/store` SQLite (modernc, pure Go): boxes, settings, alerts, per-box caches
- `internal/sshx` SSH client (TOFU host keys, bounded dial/run), unix/tcp dialer; reconnects are the poller's job
- `internal/phpserial` minimal PHP `unserialize`
- `internal/probe` the remote shell probe and its parser -> `probe.Snapshot`. The probe has two cadences: every poll measures (load, memory, quota, traffic, cgroup, processes, listeners); the inventory sections (host identity, the installed qBittorrent binary and its version, the start helper, the crontab, the log catalogue) run on a poller's first poll, every 15 minutes, and on demand (a refresh, an install or autostart action, the logs tab finding its listing older than ten minutes), and the poller carries the last inventory across the polls in between
- `internal/qbt` qBittorrent Web API client over an arbitrary dialer
- `internal/dockerx` minimal Docker Engine API client over an arbitrary dialer
- `internal/pmapi` Pulsed Media customer API client (services feed)
- `internal/collect` per-box poll loop, rate math, state cache, event bus. Every change to a box's live state goes through `Manager.commit`, which matches the services feed of that moment, assigns a fleet-wide revision, stores, runs the alert hooks and hands the event to the bus in that one order; `/api/state` reports the revision it was taken at, and the page merges snapshots and events by revision (a state below what it holds is dropped), so a stream that opens while a snapshot is in flight loses nothing. Everything measured on a box belongs to its generation (SSH host, port, account and pinned host key): an edit that keeps them carries the live state over, one that moves the record or forgets the pin starts from the record alone, drops the cached state and du (rows are tagged and refused across generations anyway) and resolves the old machine's alerts; actions that use facts read from the box (UID, container IDs, log list, the panel-managed marker) wait for the first complete probe of the generation (`BoxState.Ready`, HTTP 409 until then)
- `internal/alerts` rule evaluation, dedup, auto-resolve, ack
- `internal/api` HTTP JSON API, SSE, embedded static UI
- `internal/web` the vanilla JS/CSS frontend (no build step)

## Security

- Binds to 127.0.0.1 by default; optional HTTP basic auth (`-auth user:pass`).
- Box credentials live in the SQLite file (0600). Prefer SSH keys or the agent over passwords.
- Host keys are pinned on first connect (known_hosts style, stored per box).
