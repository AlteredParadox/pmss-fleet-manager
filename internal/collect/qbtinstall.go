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
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// installScript downloads a static qbittorrent-nox build to ~/bin, verifies it
// is an ELF binary that runs, then swaps it in atomically. $QBT_URL is set by
// the caller through the environment prefix.
const installScript = `#!/bin/sh
set -u
url="$QBT_URL"
mkdir -p "$HOME/bin" || exit 1
# A download interrupted by a lost session leaves its temp file; sweep old ones.
find "$HOME/bin" -maxdepth 1 -type f -name '.qbittorrent-nox.new.*' -mmin +30 -delete 2>/dev/null
tmp="$HOME/bin/.qbittorrent-nox.new.$$"
if command -v wget >/dev/null 2>&1; then wget -qO "$tmp" "$url"; rc=$?
elif command -v curl >/dev/null 2>&1; then curl -fsSL -o "$tmp" "$url"; rc=$?
else echo "neither wget nor curl available"; exit 1; fi
[ "$rc" -eq 0 ] && [ -s "$tmp" ] || { echo "download failed (exit $rc)"; rm -f "$tmp"; exit 1; }
magic=$(head -c 4 "$tmp" | od -An -tx1 | tr -d ' \n')
[ "$magic" = "7f454c46" ] || { echo "downloaded file is not an ELF binary (got $(head -c 200 "$tmp" | tr -d '\n' | cut -c1-120))"; rm -f "$tmp"; exit 1; }
chmod 700 "$tmp"
# stdin is this script: redirect it so neither binary can read the rest of it.
old=$(timeout 10 "$HOME/bin/qbittorrent-nox" --version </dev/null 2>/dev/null | head -n 1)
new=$(timeout 20 "$tmp" --version </dev/null 2>/dev/null | head -n 1)
[ -n "$new" ] || { echo "new binary does not run on this host"; rm -f "$tmp"; exit 1; }
case "$new" in *qBittorrent*) ;; *) echo "downloaded file is not qBittorrent (--version printed: $new)"; rm -f "$tmp"; exit 1;; esac
mv -f "$tmp" "$HOME/bin/qbittorrent-nox" || { rm -f "$tmp"; exit 1; }
echo "installed: $new"
echo "previous: ${old:-none}"
`

// qbtStartHelper is the user's standard launcher, written to ~/bin/qbt-start.
const qbtStartHelper = `#!/bin/bash
# start qbittorrent-nox in a screen session if not already running
if ! pgrep -u "$(id -u)" -x qbittorrent-nox >/dev/null; then
    screen -dmS qbt "$HOME/bin/qbittorrent-nox"
fi
`

// autostartScript installs the helper (if missing) and the two crontab lines,
// idempotently: existing qbt-start lines are replaced, everything else kept.
const autostartScript = `#!/bin/sh
set -u
mkdir -p "$HOME/bin" || exit 1
if [ -e "$HOME/bin/qbt-start" ]; then echo "helper: ~/bin/qbt-start already exists, left as is"
else
cat > "$HOME/bin/qbt-start" <<'PMSSMGR_EOF'
` + qbtStartHelper + `PMSSMGR_EOF
chmod 700 "$HOME/bin/qbt-start" && echo "helper: created ~/bin/qbt-start"
fi
command -v screen >/dev/null 2>&1 || echo "warning: 'screen' not found on this box; qbt-start needs it"
# Read the existing crontab first and distinguish "none yet" from a real
# failure: piping a failed crontab -l into crontab - would wipe every entry.
if ! cur=$(crontab -l 2>/dev/null); then
  msg=$(crontab -l 2>&1 >/dev/null)
  case "$msg" in *"no crontab"*) cur="" ;; *) echo "crontab -l failed: ${msg:-no details (try again)}"; exit 1 ;; esac
fi
# Replace lines that run the helper (either spelling of its path, followed by
# a separator), not merely ones that mention a similarly named file; say which.
pat='bin/qbt-start([[:space:]]|$|[>;&|])'
old=$(printf '%s\n' "$cur" | grep -E "$pat" || true)
[ -n "$old" ] && printf 'cron: replacing existing qbt-start line(s):\n%s\n' "$old"
( printf '%s\n' "$cur" | grep -vE "$pat" | grep -v '^$'
  echo "@reboot     sleep 30; $HOME/bin/qbt-start"
  echo "*/5 * * * * $HOME/bin/qbt-start" ) | crontab - || { echo "crontab update failed"; exit 1; }
echo "cron: @reboot and */5 entries set"
`

// restartScript asks a running qbittorrent-nox to exit (SIGTERM saves resume
// data), waits for it, then starts it again with the given command.
const restartScript = `#!/bin/sh
# Shared host: other tenants run qbittorrent-nox too. Without -u the wait
# loop would see their processes forever and give up after SIGTERMing ours.
me=$(id -u)
if pgrep -u "$me" -x qbittorrent-nox >/dev/null 2>&1; then
  pkill -u "$me" -TERM -x qbittorrent-nox
  i=0
  while pgrep -u "$me" -x qbittorrent-nox >/dev/null 2>&1 && [ $i -lt 60 ]; do sleep 1; i=$((i+1)); done
  if pgrep -u "$me" -x qbittorrent-nox >/dev/null 2>&1; then echo "qbittorrent-nox did not exit within 60s; not starting a second copy"; exit 1; fi
  echo "stopped after ${i}s"
else
  echo "was not running"
fi
`

var safeURL = regexp.MustCompile(`^https://[A-Za-z0-9._~:/?#\[\]@!$&'()*+,;=%-]+$`)

// shellQuote single-quotes a string for POSIX sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

func (p *poller) qbtInstall(ctx context.Context, downloadURL string) (string, error) {
	if !safeURL.MatchString(downloadURL) {
		return "", errors.New("download URL must be https and contain only URL characters")
	}
	cli, err := p.conn(ctx)
	if err != nil {
		return "", err
	}
	ictx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	out, stderr, err := cli.Run(ictx, "QBT_URL="+shellQuote(downloadURL)+" sh", strings.NewReader(installScript))
	text := strings.TrimSpace(string(out) + "\n" + string(stderr))
	if err != nil {
		return text, fmt.Errorf("install failed: %s", firstLine(text, err))
	}
	return text, nil
}

func (p *poller) qbtAutostart(ctx context.Context) (string, error) {
	cli, err := p.conn(ctx)
	if err != nil {
		return "", err
	}
	actx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	out, stderr, err := cli.Run(actx, "sh", strings.NewReader(autostartScript))
	text := strings.TrimSpace(string(out) + "\n" + string(stderr))
	if err != nil {
		return text, fmt.Errorf("autostart setup failed: %s", firstLine(text, err))
	}
	return text, nil
}

func (p *poller) qbtRestart(ctx context.Context) (string, error) {
	cli, err := p.conn(ctx)
	if err != nil {
		return "", err
	}
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, stderr, err := cli.Run(rctx, "sh", strings.NewReader(restartScript))
	text := strings.TrimSpace(string(out) + "\n" + string(stderr))
	if err != nil {
		return text, fmt.Errorf("restart failed: %s", firstLine(text, err))
	}
	cmd := p.startCommand()
	_, stderr, err = cli.Run(rctx, cmd, nil)
	if err != nil {
		return text, fmt.Errorf("stopped, but start command failed: %v %s", err, strings.TrimSpace(string(stderr)))
	}
	return text + "\nstarted with: " + cmd, nil
}

// startCommand picks the configured start command, else ~/bin/qbt-start when
// the probe saw it, else a plain daemonised launch.
func (p *poller) startCommand() string {
	if cmd := strings.TrimSpace(p.box.QbtStartCmd); cmd != "" {
		return cmd
	}
	if st, ok := p.m.State(p.box.ID); ok && st.Qbt.StartHelper {
		return `"$HOME/bin/qbt-start"`
	}
	return defaultQbtCmd
}

func firstLine(text string, fallback error) string {
	for _, l := range strings.Split(text, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return fallback.Error()
}

// latestRelease polls GitHub for the newest userdocs qbittorrent-nox-static
// release and caches the qBittorrent version it carries ("v5.2.3").
type latestRelease struct {
	mu      sync.Mutex
	version string
	at      time.Time
}

var releaseTag = regexp.MustCompile(`release-(\d+\.\d+\.\d+)`)

func (l *latestRelease) get(ctx context.Context) string {
	l.mu.Lock()
	if time.Since(l.at) < 6*time.Hour {
		v := l.version
		l.mu.Unlock()
		return v
	}
	l.at = time.Now() // also rate-limits failures
	l.mu.Unlock()
	// Not tied to the poll's cancellation: a box edit mid-fetch would burn
	// the six-hour window with an empty result for the whole fleet.
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, "https://api.github.com/repos/userdocs/qbittorrent-nox-static/releases/latest", nil)
	if err != nil {
		return l.version
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "pmss-fleet-manager")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return l.version
	}
	defer resp.Body.Close()
	var rel struct {
		TagName string `json:"tag_name"`
	}
	if resp.StatusCode != 200 || json.NewDecoder(resp.Body).Decode(&rel) != nil {
		return l.version
	}
	if m := releaseTag.FindStringSubmatch(rel.TagName); m != nil {
		l.mu.Lock()
		l.version = "v" + m[1]
		l.mu.Unlock()
	}
	return l.version
}

// versionLess reports whether a < b for "v1.2.3" strings; false when unparsable.
func versionLess(a, b string) bool {
	pa, pb := versionParts(a), versionParts(b)
	if pa == nil || pb == nil {
		return false
	}
	for i := 0; i < 3; i++ {
		if pa[i] != pb[i] {
			return pa[i] < pb[i]
		}
	}
	return false
}

func versionParts(v string) []int {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	f := strings.SplitN(v, ".", 3)
	if len(f) != 3 {
		return nil
	}
	out := make([]int, 3)
	for i, s := range f {
		// Keep the leading digits only: "4-1" (distro suffix) and "3rc1" compare as 4 and 3.
		digits := strings.TrimLeftFunc(s, func(r rune) bool { return r < '0' || r > '9' })
		if j := strings.IndexFunc(digits, func(r rune) bool { return r < '0' || r > '9' }); j >= 0 {
			digits = digits[:j]
		}
		n, err := strconv.Atoi(digits)
		if err != nil {
			return nil
		}
		out[i] = n
	}
	return out
}
