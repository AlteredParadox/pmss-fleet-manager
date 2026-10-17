# pmss-fleet-manager

A single-pane-of-glass dashboard for [Pulsed Media](https://pulsedmedia.com)
seedboxes and storage boxes running the
[PMSS](https://github.com/MagnaCapax/PMSS) stack. One Go binary, no agents on
the boxes: it logs into each one over SSH as your own tenant user and reads
what PMSS already exposes, plus your self-installed `qbittorrent-nox` Web API
and rootless Docker socket through the same connection. Optionally it also
pulls plan names, prices and renewal dates from the Pulsed Media customer API.

Built for the common case of a customer with several boxes on shared hosts:
no root, nothing to install server-side, and nothing exposed to the internet
beyond the dashboard itself.

## Features

**Fleet view.** Every box as a card or table row with status, disk against
quota, upload for the rolling 30-day window PMSS enforces, live up/down rates,
torrent count and share ratio, CPU/RAM of your systemd slice, host CPU model
(and hardware encoder, e.g. Intel QSV on N100 hosts), qBittorrent version with
an update flag, and plan tags. Headline tiles for total storage, upload,
aggregate bandwidth, torrents, containers, next renewal and monthly cost.

**Per box.**

- Overview: disk, upload with limit/bonus/throttle state, bandwidth, CPU/RAM,
  client and autostart status, plan and billing.
- Traffic: daily upload/download bars for the rolling 30 days PMSS tracks.
- Torrents: the full list from qBittorrent with filter, plus pause all / resume
  all / start / stop / restart of the client.
- Containers: rootless Docker containers with CPU and memory, start / stop /
  restart.
- Storage: usage by top-level directory from a low-priority `du`, every few
  hours or on demand.
- Host: uptime, load, memory and CPU of the shared host, GPU/render node
  access, PMSS account settings.

**qBittorrent lifecycle.** Install or update `~/bin/qbittorrent-nox` from the
[userdocs static build](https://github.com/userdocs/qbittorrent-nox-static)
(URL configurable), with an ELF and `--version` check before the binary is
swapped in atomically. The dashboard checks the latest GitHub release every
six hours and flags boxes that are behind or that need a restart to pick up a
new binary. "Set up autostart" writes a `~/bin/qbt-start` screen launcher and
the `@reboot` and `*/5 * * * *` crontab entries without touching other
crontab lines.

**Alerts.** Unreachable box, suspended account, disk at threshold or over
quota, traffic at threshold, throttled, qBittorrent or Docker down, container
exited non-zero, process memory near the slice limit, a kernel OOM kill in
the slice, renewal within N days or overdue. Alerts
auto-resolve, can be acknowledged or dismissed, and the count shows in the
sidebar. Live updates arrive over server-sent events.

**Pulsed Media customer API.** Paste a read-only API key from the client area
(`https://pulsedmedia.com/clients/index.php?m=pmapi`) into settings. Services
are matched to boxes by hostname and username, which fills in plan name, price
per cycle and per month, next due date, web UI and permanent-address links, and
Pulsed Media's own usage accounting. Services you have not added yet get an
"add box" shortcut. The feed is synced every six hours and on demand. Manual
plan and renewal values on a box take precedence.

## Run

Requires Go 1.26 or later to build. No other runtime dependencies.

```sh
make build                                 # or: go build -o pmss-fleet-manager ./cmd/pmss-fleet-manager
./pmss-fleet-manager                       # http://127.0.0.1:8080, data in ./data
./pmss-fleet-manager -listen 127.0.0.1:8080,10.11.0.13:8080 -auth me:secret -data /var/lib/pmss-fleet-manager
```

Flags, or the matching environment variables `PMSS_LISTEN`, `PMSS_DATA`,
`PMSS_AUTH`, `PMSS_LOG`, `PMSS_ALLOWED_HOSTS`:

| flag | default | meaning |
|---|---|---|
| `-listen` | `127.0.0.1:8080` | bind address, or several comma-separated |
| `-data` | `./data` | directory for the SQLite database (created 0700) |
| `-auth` | none | HTTP basic auth as `user:password`; set it if you bind to a non-loopback address |
| `-log` | `info` | `debug`, `info`, `warn`, `error` |
| `-allowed-hosts` | none | extra Host header names to serve, comma-separated, e.g. the name a reverse proxy uses; the listen addresses and localhost are always allowed |

### Adding a box

Open the UI and use **add box**. The wizard connects, pins the host key, runs
the probe and shows what it found (quota, traffic limit, qBittorrent binary and
WebUI port, Docker socket, CPU) before you confirm. SSH auth can be a key file,
your ssh-agent, or a password; with "auto" it tries `~/.ssh/id_ed25519`,
`id_ecdsa` and `id_rsa`, then the agent, then a password if one is set.

### Editing a box

Blank password fields keep the saved passwords. They are only ever used for
the host, port and user they were saved for: moving a box to a different
endpoint with a blank SSH or WebUI password is refused until you re-enter
it (or remove it), both on save and on "test connection". Before any action
that logs in to the WebUI the dashboard re-reads the box's socket table to
confirm the port is still bound by your account. Switching SSH auth to a key or the agent
removes the saved SSH password; a checkbox removes the saved WebUI password.
The pinned host key belongs to the host and port: it is kept while they are
unchanged, cleared when they change or when you tick "forget the pinned SSH
host key", and a key the wizard saw never replaces an existing pin.

### qBittorrent access

The WebUI port is read from `~/.config/qBittorrent/qBittorrent.conf` on the
box unless you set one. Requests reach qBittorrent through the SSH tunnel, so
they arrive from the box's own loopback address. On a shared host other tenants
can reach that same loopback port, so keep WebUI authentication on and give the
dashboard the username and password rather than enabling the localhost bypass.

The poller never retries a rejected login: qBittorrent bans an IP after five
failures, its counter never decays, and on PMSS that loopback address is
shared with the `/user-<name>/` web path, so a ban would lock you out of
both. A rejected username/password is held until you change it, re-enter it
under edit, or restart the client from the dashboard (a fresh process has a
fresh counter). A detected ban is held for 65 minutes. Before any login the
probe also checks that the WebUI port is bound by your own uid, since on a
shared host another tenant could bind it while your client is down.

"Start client" runs the per-box start command; when none is set it uses
`~/bin/qbt-start` if present, else `"$HOME/bin/qbittorrent-nox" -d`.

### Failures and backoff

A box that fails to poll is retried at the normal interval for the first few
failures, then the retry interval doubles up to ten minutes. The card shows
when the next try is due and offers "retry now", which reconnects at once.
The failure count and status survive edits and dashboard restarts, so a box
that is down stays "offline" (and its alert stays acknowledged) across them.
Disabling a box stops polling, resolves its open alerts and greys out its
actions; enabling it again starts from a clean slate.

## Security notes

- Box credentials (SSH password, qBittorrent password) and the Pulsed Media
  API key are stored in plain text in the SQLite file, which is created mode
  0600 (its -wal and -shm side files too). Prefer SSH keys or the agent. The API key is never sent to the browser.
- The server has no authentication by default and binds to loopback. Use
  `-auth`, and put it behind a reverse proxy with TLS before exposing it
  further.
- Host keys are trusted on first use and pinned; a changed key fails the poll
  until you edit the box, which clears the pin when the host or port changes
  or when you tick "forget the pinned SSH host key" after a reinstall.
- Requests whose Host header is not a listen address, localhost or an
  `-allowed-hosts` entry are refused, which stops DNS-rebinding pages from
  reaching the API through the browser. Binding to a wildcard address
  disables that check unless `-allowed-hosts` is set.
- Install and autostart actions run shell scripts on the box as your user.
  They are embedded in the binary and documented in
  `internal/collect/qbtinstall.go`.

## Development

```sh
make vet     # gofmt -l + go vet
make test    # go test ./...
make build   # static binary with the version stamped in (`pmss-fleet-manager --version`)
```

CI runs the same targets plus the race suite and govulncheck on every pull
request; `check` is the required status on `main`. Pushing a `vX.Y.Z` tag
builds linux amd64 and arm64 binaries and publishes a GitHub Release with
checksums. Dependabot keeps Go modules and pinned actions current weekly.

The frontend is vanilla JS and CSS in `internal/web/static`, embedded into the
binary; there is no build step. `docs/ARCHITECTURE.md` lists the tenant-readable
data sources on a PMSS box and the package layout. The design mockup the UI
follows is in `design/`.

Set `PMSS_PMAPI_URL` to point the customer API client at a mock server when
working on that feature.

## License

pmss-fleet-manager is free software under the **GNU Affero General Public
License v3 or later** ([`LICENSE`](LICENSE)).

The AGPL's §13 obligation is to offer the Corresponding Source to everyone who
interacts with the program *over a network*, so these three routes are
deliberately served **without basic auth**, since a login wall would defeat
the offer:

| Route | Serves |
| --- | --- |
| `/license` | the AGPL text embedded in this binary |
| `/third-party-licenses` | every linked dependency's licence and notices |
| `/source` | redirects to the Corresponding Source, pinned to the running version |

They are linked from **Settings → about & licensing**. A stamped release
points `/source` at its own tag, so the source you are offered is the source
that is running; an unstamped build can only point at the repository.

`THIRD_PARTY_LICENSES.md` is generated from the modules **actually linked into
the binary** (read back off a probe build with `go version -m`):

```sh
make notices        # regenerate (also runs as part of `make build`)
make notices-check  # fail if the committed copy is stale
```

`notices-check` gates the release workflow rather than every PR, because
Dependabot bumps go.mod but cannot regenerate the file. Every first-party
source file carries the AGPL header; `make license-check` enforces that on
every PR and `make license-headers` stamps any new file.

## Status

Written against five real Pulsed Media boxes (KVM, Dragon-R, Mesitorvi N100,
M1000) running PMSS with qBittorrent 5.2.x. Other setups should work but have
not been tried. Issues and pull requests welcome.
