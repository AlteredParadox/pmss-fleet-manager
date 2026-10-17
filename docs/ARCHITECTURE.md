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
| ram / cpu / pids | cgroup slice `user-<uid>.slice` (v1 controllers, v2 fallback) | CPU% from usage delta between polls |
| account config | `~/.config/pmss-user.json` | quota GiB, ramMiB, dockerEnabled, trafficCapMbit |
| host | `/proc/uptime`, `/proc/loadavg`, `/proc/meminfo`, `df /home` | shared host, informational |
| torrents | qBittorrent Web API over SSH tunnel | user-installed `~/bin/qbittorrent-nox`; port from `~/.config/qBittorrent/qBittorrent.conf` or box setting |
| containers | Docker Engine API on `/run/user/<uid>/docker.sock` over SSH | rootless dockerd |
| usage by directory | `du` on top-level home entries, low priority, every few hours | optional, heavy |
| plan / price / renewal / PM usage | Pulsed Media customer API `https://pulsedmedia.com/remote/pmapi-feed.php` (`X-API-Key`) | optional; synced every 6 h and on demand; cached in the settings table; matched to boxes by host+user, then unique host, then unique user. Manual plan / renewal on a box override it; datacenter is manual only |

PMSS "month" counters are a rolling 30-day window, not a billing cycle. The UI says so.

## Packages

- `cmd/pmss-fleet-manager` flags, wiring, graceful shutdown
- `internal/store` SQLite (modernc, pure Go): boxes, settings, alerts, per-box caches
- `internal/sshx` SSH client with reconnect, command runner, unix/tcp dialer
- `internal/phpserial` minimal PHP `unserialize`
- `internal/probe` the remote shell probe and its parser -> `probe.Snapshot`
- `internal/qbt` qBittorrent Web API client over an arbitrary dialer
- `internal/dockerx` minimal Docker Engine API client over an arbitrary dialer
- `internal/pmapi` Pulsed Media customer API client (services feed)
- `internal/collect` per-box poll loop, rate math, state cache, event bus
- `internal/alerts` rule evaluation, dedup, auto-resolve, ack
- `internal/api` HTTP JSON API, SSE, embedded static UI
- `internal/web` the vanilla JS/CSS frontend (no build step)

## Security

- Binds to 127.0.0.1 by default; optional HTTP basic auth (`-auth user:pass`).
- Box credentials live in the SQLite file (0600). Prefer SSH keys or the agent over passwords.
- Host keys are pinned on first connect (known_hosts style, stored per box).
