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

![Fleet overview: five boxes as cards with disk, upload, rates and CPU/RAM, and headline tiles for storage, upload, torrents, containers, renewal and monthly cost](docs/screenshot.png)

## Features

**Fleet view.** Every box as a card or table row with status, disk against
quota, upload for the rolling 30-day window PMSS enforces, live up/down rates,
torrent count and share ratio, CPU/RAM of your systemd slice, host CPU model
(and hardware encoder, e.g. Intel QSV on N100 hosts), qBittorrent version with
an update flag, and plan tags. Headline tiles for total storage, upload,
aggregate bandwidth, torrents, containers, next renewal and monthly cost.

**Torrents.** Every seedbox's torrents in one table, busiest first, with the
box name (and whichever of DHT / PeX / LSD is enabled on it in parentheses)
linking to that box's torrents tab; the same columns (size, progress, rates,
lifetime uploaded / downloaded, ratio, connected seeds and peers with the
swarm size), filter and per-torrent stop / start / remove controls as the
per-box tab.

**Per box.**

- Overview: disk, upload with limit/bonus/throttle state, bandwidth, CPU/RAM,
  client and autostart status, plan and billing.
- Traffic: daily upload/download bars for the rolling 30 days PMSS tracks.
- Torrents: DHT / PeX / local peer discovery state in the header, each a
  toggle (and a card tag when any is on, for deciding where private-tracker
  torrents may go);
  the full list from qBittorrent with filter; add torrents by
  magnet, URL or `.torrent` file (optionally into a category, or stopped);
  stop / start / remove (with or without the files) per torrent; pause all /
  resume all; and start / stop / restart of the client.
- Containers: rootless Docker containers with CPU and memory, start / stop /
  restart.
- Storage: usage by top-level directory from a low-priority `du`, every few
  hours or on demand.
- Host: uptime, load, memory and CPU of the shared host, GPU/render node
  access, PMSS account settings.

![Box overview: disk, upload, bandwidth and CPU/RAM tiles, box status and client details, plan and billing with per-box links](docs/screenshot-box.png)

![Torrents tab: client version and counts with DHT / PeX / LSD toggles, filter, pause all / resume all / add, and stop / remove / remove with files per torrent](docs/screenshot-torrents.png)

**qBittorrent lifecycle.** Install or update `~/bin/qbittorrent-nox` from the
[userdocs static build](https://github.com/userdocs/qbittorrent-nox-static)
(URL configurable). The download's sha256 is checked against the digest
GitHub publishes for the release asset (any GitHub release URL; other
sources install unverified and say so), then an ELF and `--version` check
runs before the binary is swapped in atomically. The dashboard checks the latest GitHub release every
six hours and flags boxes that are behind or that need a restart to pick up a
new binary. "Set up autostart" writes a `~/bin/qbt-start` screen launcher and
the `@reboot` and `*/5 * * * *` crontab entries without touching other
crontab lines (a line that runs the helper along with something else is
kept), after saving the previous crontab to
`~/.config/pmss-fleet-manager/crontab.bak`. Where qBittorrent is enabled in
the Pulsed Media panel, PMSS runs and configures it itself: the dashboard
still monitors it, but refuses to install or autostart a build of its own.

**Alerts.** Unreachable box, suspended account, disk at threshold or over
quota, traffic at threshold, throttled, qBittorrent or Docker down, container
exited non-zero, process memory near the slice limit, a kernel OOM kill in
the slice, renewal within N days or overdue (counted in calendar days of the
dashboard host's time zone; keep it the same as yours). Alerts
auto-resolve, can be acknowledged or dismissed, and the count shows in the
sidebar. Live updates arrive over server-sent events.

**Pulsed Media customer API.** Paste a read-only API key from the client area
(`https://pulsedmedia.com/clients/index.php?m=pmapi`) into settings. Services
are matched to boxes by hostname and username, which fills in plan name, price
per cycle and per month, next due date, web UI and permanent-address links, and
Pulsed Media's own usage accounting. Services you have not added yet get an
"add box" shortcut. The feed is synced every six hours and on demand. Manual
plan and renewal values on a box take precedence. Only Active and Suspended
services count towards costs and renewal alerts; any other status (Cancelled,
Terminated, Pending, Fraud, Completed) stays listed but counts towards
neither.

## Run

Requires Go 1.27 or later to build (go.mod names the exact toolchain and the go command fetches it). No other runtime dependencies.

```sh
make build                                 # or: go build -o pmss-fleet-manager ./cmd/pmss-fleet-manager
./pmss-fleet-manager                       # http://127.0.0.1:8080, data in ./data
./pmss-fleet-manager -listen 127.0.0.1:8080,10.11.0.13:8080 -auth me:secret -data /var/lib/pmss-fleet-manager
```

Flags, or the matching environment variables `PMSS_LISTEN`, `PMSS_DATA`,
`PMSS_AUTH`, `PMSS_LOG`, `PMSS_ALLOWED_HOSTS`, `PMSS_BASE_PATH`:

| flag | default | meaning |
|---|---|---|
| `-listen` | `127.0.0.1:8080` | bind address, or several comma-separated |
| `-data` | `./data` | directory for the SQLite database (created 0700) |
| `-auth` | none | HTTP basic auth as `user:password`; set it if you bind to a non-loopback address |
| `-log` | `info` | `debug`, `info`, `warn`, `error` |
| `-allowed-hosts` | none | extra Host header names to serve, comma-separated, e.g. the name a reverse proxy uses; the listen addresses and localhost are always allowed, and on a wildcard bind (`0.0.0.0`, `::`) so is any IP address |
| `-base-path` | none | URL prefix when a reverse proxy serves the dashboard under a sub-path and forwards it unchanged, e.g. `/fleet` (then open `https://proxy/fleet/`); leave unset if the proxy strips the prefix before forwarding |

### Running in a container

Each release also publishes a multi-arch image (linux amd64 and arm64) built
from the `Dockerfile`: a distroless base with nothing but the binary, CA
roots and time-zone data, running as an unprivileged user.

```sh
docker run -d --name pmss-fleet-manager \
  -p 127.0.0.1:8080:8080 \
  -v pmss-data:/data \
  -e TZ=America/Chicago \
  ghcr.io/alteredparadox/pmss-fleet-manager:latest
```

- The image listens on `0.0.0.0:8080` and keeps its database in `/data`.
  Every flag is available as the matching environment variable, so `-e
  PMSS_AUTH=me:secret` or `-e PMSS_BASE_PATH=/fleet` configure it.
- The process runs as uid 65532. A named volume as above works out of the
  box; a bind-mounted directory has to be writable by that uid (`chown
  65532:65532 ./data`), or run the container with `--user "$(id -u)"`. If
  it is not, the container exits at once with a log line naming the
  directory, the uid it ran as and both fixes.
- **Unraid:** appdata folders belong to `nobody:users`, so either put
  `--user 99:100` in the template's *Extra Parameters* or `chown -R
  65532:65532 /mnt/user/appdata/pmss-fleet-manager`. The image does not read
  `PUID`/`PGID`; `--user` is the equivalent. SSH keys are looked up in
  `/home/nonroot/.ssh` whatever `--user` says, since the image sets `HOME`.
- Set `TZ` to the zone you want renewal days counted in; without it the
  container is UTC.
- Boxes that authenticate with an SSH key need the key inside the container:
  mount it read-only, for example `-v ~/.ssh/id_ed25519:/home/nonroot/.ssh/id_ed25519:ro`,
  or point the box's key path at wherever you mount it. The mounted key must
  be readable by the uid the container runs as: copy it, `chown 65532` and
  `chmod 600` the copy, or run with `--user` matching the file's owner. An
  unreadable default key is named in the connection error. For an agent,
  mount the socket and pass `SSH_AUTH_SOCK`. Password auth needs nothing extra.
- Because it listens on all interfaces, the Host-header check serves
  `localhost` and any IP address; any other name, such as the one a reverse
  proxy uses, has to be listed in `PMSS_ALLOWED_HOSTS`. Publishing the port on
  `127.0.0.1` as above and fronting it with a TLS reverse proxy is the
  intended setup.

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
host key", and a key the wizard saw never replaces an existing pin. Because
the first connect after forgetting trusts whatever key answers, and with
password auth sends the SSH password to it, ticking "forget" asks you to
confirm.

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
shared host another tenant could bind it while your client is down. It reads
`/proc/net/tcp`; on hosts where Pulsed Media's socket-table privacy option
has made that root-only it falls back to `ss -tlnp`, which still lists every
listener and can attribute your own to you.

"Start client" runs the per-box start command; when none is set it uses
`~/bin/qbt-start` if present, else `"$HOME/bin/qbittorrent-nox" -d`.

### Failures and backoff

A box that fails to poll is retried at the normal interval for the first few
failures, then the retry interval doubles up to ten minutes. The card shows
when the next try is due and offers "retry now", which reconnects at once.
The failure count and status survive edits and dashboard restarts, so a box
that is down stays "offline" (and its alert stays acknowledged) across them.
Disabling a box stops polling, resolves its open alerts and greys out its
actions; enabling it again starts from a clean slate. An alert whose source
could not be read on an otherwise good poll (quota command hung, traffic
file missing, docker daemon down) is kept rather than resolved and re-raised;
switching a source off for the box resolves its alerts.

## Security and hardening

The code went through a fourteen-round adversarial review before 1.0 (SSH
resiliency, remote-command safety, web/API, concurrency, parsers, dead code).
These are the design decisions that came out of it.

- **Tenant-only on a shared host.** Everything the dashboard reads comes from
  your own account: `/proc`, your cgroup, your home directory and the kernel's
  socket table. Probe output is tagged with a per-run secret delivered on
  stdin, so another process on the host cannot inject a section, and every
  remote string is length-bounded before it reaches state, the cache or the
  browser.
- **WebUI credentials never go to a port you do not own.** The qBittorrent
  password is sent only when the socket table (`/proc/net/tcp`, or `ss -tlnp`
  where that is root-only) shows the port bound by your uid
  alone, re-read live over the same SSH connection immediately before any
  stop, pause or resume. One login attempt runs at a time per box and its
  outcome is shared with every request that was waiting on it, so a burst of
  actions sends the credentials once. A rejected login is held until the
  credentials change or the client is restarted from the dashboard, and a
  ban is held 65 minutes, so a bad password cannot walk the box's loopback
  into qBittorrent's IP ban.
- **Host keys.** Trust on first use, pinned to host and port. A supplied key
  never silently replaces a pin; a host or port change, or "forget the pinned
  SSH host key" after a reinstall, clears it. Saved SSH and WebUI passwords
  are reused only for the exact host, port and user they were saved for;
  moving a box means re-entering them.
- **Secrets on disk.** Box credentials and the Pulsed Media API key are stored
  in plain text in the SQLite file, which is created mode 0600 before SQLite
  opens it so the -wal and -shm side files inherit the mode; the data
  directory is created 0700. Prefer SSH keys or the agent. The API key is
  never sent to the browser and passwords are never serialised.
- **Bounded everything.** SSH dials, handshakes (including the agent),
  sessions and remote commands are bounded by contexts; a hung `quota` is run
  through a detached helper so it cannot stall the probe; remote stdout is
  capped at 16 MiB; error texts and alert titles are clipped; directory
  listings keep the 500 largest entries.
- **Honest state across edits and restarts.** Failed polls are persisted too,
  so a box that is down comes back as down after a dashboard restart and its
  acknowledged alert stays acknowledged. An alert whose source could not be
  read on a poll is kept rather than resolved and re-raised. Retries back off
  to every ten minutes, with "retry now" to reconnect at once.
- **Web surface.** No authentication by default and bound to loopback: use
  `-auth`, and put it behind a reverse proxy with TLS before exposing it
  further. Requests whose Host header is not a listen address, localhost or an
  `-allowed-hosts` entry are refused (on a wildcard bind, whose addresses
  cannot be listed, any IP address is served as well); every mutating request
  must be same-origin. A reverse proxy must pass the client's Host header
  through, port included (nginx: `proxy_set_header Host $http_host;`), and
  that name must be listed in `-allowed-hosts`; otherwise mutating requests
  fail the same-origin check or every request is refused. CSP is set; request
  bodies are capped at 1 MiB, torrent uploads at 40 MiB; there are no
  state-changing GETs; every API string is HTML-escaped. The page makes no
  request to any third party: its JetBrains Mono web font is embedded
  (SIL Open Font License, see `THIRD_PARTY_LICENSES.md`), so viewers' addresses
  stay with your dashboard and it works offline. Settings saves are
  serialised and the fleet-view shortcuts write only their own field, and
  other tabs are told to refresh, so one tab never reverts another.
- **Scripts on the box** run as your user and are embedded in the binary
  (documented in `internal/collect/qbtinstall.go`). Install verifies the
  download's sha256 against the GitHub release digest (fetched by the
  dashboard host, compared on the box, so a swapped file on either path
  fails), then the ELF magic and `--version` output, before swapping the
  binary in atomically;
  restart only signals processes of your uid; autostart distinguishes "no
  crontab" from a failed read before it ever pipes into `crontab -`, saves
  the previous crontab first, replaces only lines that run nothing but the
  helper, and prints what it replaced. Downloads use curl, which refuses a
  redirect to plain HTTP; the wget fallback relies on the source not
  redirecting to http.

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

[SonarCloud](https://sonarcloud.io/project/overview?id=AlteredParadox_pmss-fleet-manager)
analyses every push and pull request and is kept at zero open issues. Rules
the project deliberately does not follow are listed in
`sonar-project.properties` with a reason each (the escaped-innerHTML rendering
style of the single-file frontend, the two loopback `http://` URLs, and
cognitive complexity in the 17-25 band for parsers and view renderers);
anything scoring higher than that has been split.

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

1.2.4 is the current release. The code was written against five real Pulsed
Media boxes (KVM, Dragon-R, Mesitorvi N100, M1000) running PMSS with
qBittorrent 5.2.x and reviewed as described under *Security and hardening*.
1.2.0 added torrent management: per-torrent stop, start and remove (with or
without files), adding torrents by magnet, URL or .torrent file, DHT / PeX /
LSD shown per box and toggled from the torrents header, a fleet-wide
torrents view, lifetime transfer and swarm columns, and per-box links to the
data directory and qBittorrent web UI. 1.2.1 is a hardening release from two
rounds of an independent external review: one qBittorrent login attempt at a
time per box, shared across concurrent actions, so a dropped or rejected
login can never be repeated into a ban; the Pulsed Media feed cache is tied
to the API key that fetched it and dropped the moment the key changes;
cancelled services no longer count towards costs or renewal alerts; box edits
clear and save secrets in one write; the API key is never forwarded on a
redirect; the installer downloads over HTTPS under a size limit; renewal
days are calendar days in the dashboard host's zone; and the browser
recovers from a failed first load and coalesces torrent-view refreshes.
1.2.2 adds `-base-path` for running under a reverse-proxy sub-path, the
first outside contribution (thanks, doomio). 1.2.3 adds the container image
on GHCR, built by the same release pipeline as the binaries. 1.2.4 keeps the
WebUI port-ownership check working on hosts where Pulsed Media's
socket-table privacy option has made `/proc/net/tcp` root-only (the probe
falls back to `ss -tlnp`), and names the fix when a container cannot write
its data directory (Unraid: `--user 99:100`).
Other setups should work but have not been
tried. Issues and pull requests welcome. Releases on GitHub carry linux amd64
and arm64 binaries with checksums, and a matching container image on GHCR.
