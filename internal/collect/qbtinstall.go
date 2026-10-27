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
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"pmss-fleet-manager/internal/probe"
)

// installScript downloads a static qbittorrent-nox build to ~/bin, verifies
// its sha256 against the digest GitHub publishes for the release asset when
// the caller could fetch one ($QBT_SHA256, 64 hex digits or empty), checks
// it is an ELF binary of plausible size that runs and calls itself
// qBittorrent, then swaps it in atomically. Without a digest the ELF and
// --version checks are a sanity check, not authentication: the download is
// then trusted as much as its source. $QBT_URL and $QBT_SHA256 are set by
// the caller through the environment prefix.
const installScript = `#!/bin/sh
set -u
LC_ALL=C; export LC_ALL
url="$QBT_URL"; want="${QBT_SHA256:-}"
mkdir -p "$HOME/bin" || exit 1
# A download interrupted by a lost session leaves its temp file; sweep old ones.
find "$HOME/bin" -maxdepth 1 -type f -name '.qbittorrent-nox.new.*' -mmin +30 -delete 2>/dev/null
tmp="$HOME/bin/.qbittorrent-nox.new.$$"
# curl first: --proto / --proto-redir stop a redirect from downgrading the
# download to plain HTTP. wget is only the fallback: its --https-only applies
# to recursive downloads alone, so that path relies on the source not
# redirecting to http. wget has no size option either: the subshell's
# file-size limit (ulimit -f, 512-byte blocks under dash = 200 MiB) stops a
# runaway download as it is written.
` + downloadShell + `[ "$rc" -eq 0 ] && [ -s "$tmp" ] || { echo "download failed (exit $rc)"; rm -f "$tmp"; exit 1; }
# The static build is ~30 MB; anything far above that is not it.
size=$(wc -c < "$tmp")
[ "$size" -le 209715200 ] || { echo "downloaded file is too large to be qbittorrent-nox ($size bytes)"; rm -f "$tmp"; exit 1; }
` + verifyChecksumShell + `magic=$(head -c 4 "$tmp" | od -An -tx1 | tr -d ' \n')
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

// downloadShell fetches $url into $tmp. The URL goes to the downloader on
// ITS stdin (curl's config file, wget's URL list), never on its command
// line: a custom URL may carry a private-build token, and on a host
// without process hiding another tenant could read it from
// /proc/<pid>/cmdline for the whole download. safeURL admits neither a
// double quote nor a backslash, so the curl config line needs no escaping.
const (
	curlDownload  = `printf 'url = "%s"\n' "$url" | curl -fsSL -K - --proto '=https' --proto-redir '=https' --max-filesize 209715200 -o "$tmp"; rc=$?`
	wgetDownload  = `(ulimit -f 409600; printf '%s\n' "$url" | wget -q -i - -O "$tmp"); rc=$?`
	downloadShell = `if command -v curl >/dev/null 2>&1; then ` + curlDownload + `
elif command -v wget >/dev/null 2>&1; then ` + wgetDownload + `
else echo "neither curl nor wget available"; exit 1; fi
`
)

// qbtStartHelper is the user's standard launcher, written to ~/bin/qbt-start.
// It keys the running check on id -u: $USER is empty under cron, pgrep -u ""
// is a usage error (exit 2), and "! pgrep" then reads that as "not running"
// and starts another copy every five minutes. Earlier copies of this helper
// (and the hand-written one it came from) used $USER; autostart replaces
// those, see probe.QbtStaleHelperPattern (the probe flags them too).
const qbtStartHelper = `#!/bin/bash
# start qbittorrent-nox in a screen session if not already running
if ! pgrep -u "$(id -u)" -x qbittorrent-nox >/dev/null; then
    screen -dmS qbt "$HOME/bin/qbittorrent-nox"
fi
`

// autostartScript installs the helper (if missing, or if it is one of ours
// that keyed on $USER) and the two crontab lines, idempotently: existing
// lines that run only the helper are replaced, everything else kept, the
// previous crontab and a replaced helper are saved first.
const autostartScript = `#!/bin/sh
set -u
# Byte-wise: under a UTF-8 locale grep silently drops lines with invalid
# UTF-8, and they would then be missing from what is piped into crontab -.
LC_ALL=C; export LC_ALL
mkdir -p "$HOME/bin" || exit 1
write=1
if [ -e "$HOME/bin/qbt-start" ]; then
  if grep -Eq ` + probe.QbtStaleHelperPattern + ` "$HOME/bin/qbt-start"; then
    # $USER is empty under cron, so this helper's pgrep fails with a usage
    # error and the helper starts another copy on every run.
    hbak="$HOME/.config/pmss-fleet-manager/qbt-start.bak"
    ( umask 077; mkdir -p "$HOME/.config/pmss-fleet-manager" && rm -f "$hbak" && cp "$HOME/bin/qbt-start" "$hbak" ) || { echo "could not save the current helper to $hbak; not changing it"; exit 1; }
    echo "helper: ~/bin/qbt-start keyed on \$USER, which is empty under cron (so it started another copy every run); saved to ~/.config/pmss-fleet-manager/qbt-start.bak and rewritten"
  else
    echo "helper: ~/bin/qbt-start already exists, left as is"; write=0
  fi
fi
if [ "$write" = 1 ]; then
cat > "$HOME/bin/qbt-start" <<'PMSSMGR_EOF'
` + qbtStartHelper + `PMSSMGR_EOF
chmod 700 "$HOME/bin/qbt-start" && echo "helper: wrote ~/bin/qbt-start"
fi
command -v screen >/dev/null 2>&1 || echo "warning: 'screen' not found on this box; qbt-start needs it"
# Read the existing crontab first and distinguish "none yet" from a real
# failure: piping a failed crontab -l into crontab - would wipe every entry.
if ! cur=$(crontab -l 2>/dev/null); then
  msg=$(crontab -l 2>&1 >/dev/null)
  case "$msg" in *"no crontab"*) cur="" ;; *) echo "crontab -l failed: ${msg:-no details (try again)}"; exit 1 ;; esac
fi
# Replace only lines whose whole command is the helper (the shapes written
# below); a line that runs it together with something else is the user's and
# stays. Say which lines go. The probe counts cron entries with the same
# pattern (probe.QbtCronPattern).
pat=` + probe.QbtCronPattern + `
old=$(printf '%s\n' "$cur" | grep -E "$pat" || true)
[ -n "$old" ] && printf 'cron: replacing existing qbt-start line(s):\n%s\n' "$old"
# Keep a copy of the crontab as it was, overwritten on every run.
bak="$HOME/.config/pmss-fleet-manager/crontab.bak"
( umask 077; mkdir -p "$HOME/.config/pmss-fleet-manager" && rm -f "$bak" && printf '%s\n' "$cur" > "$bak" ) || { echo "could not save the current crontab to $bak; not changing it"; exit 1; }
echo "cron: previous crontab saved to ~/.config/pmss-fleet-manager/crontab.bak"
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

// verifyChecksumShell compares $tmp's sha256 with $want when one was given
// (the download is deleted on a mismatch) and says either way whether the
// file was verified. Its own constant so a test can run it on a file.
const verifyChecksumShell = `if [ -n "$want" ]; then
  got=$(sha256sum "$tmp" 2>/dev/null | cut -d' ' -f1)
  [ -n "$got" ] || { echo "sha256sum is not available on this box; cannot verify the download"; rm -f "$tmp"; exit 1; }
  [ "$got" = "$want" ] || { echo "checksum mismatch: the download's sha256 is $got, the release says $want"; rm -f "$tmp"; exit 1; }
  echo "checksum: sha256 matches the GitHub release digest"
else
  echo "checksum: not verified (no release digest for this URL); relying on the ELF and --version checks"
fi
`

var safeURL = regexp.MustCompile(`^https://[A-Za-z0-9._~:/?#\[\]@!$&'()*+,;=%-]+$`)

// maxDownloadURL bounds the download URL setting.
const maxDownloadURL = 2048

// ValidDownloadURL is the one rule for the qBittorrent download URL, at
// save time and again at install: https, URL characters only, bounded.
func ValidDownloadURL(u string) bool {
	return len(u) <= maxDownloadURL && safeURL.MatchString(u)
}

// githubAPI is the API base the digest lookup uses; a test points it at a
// local server.
var githubAPI = "https://api.github.com"

// releaseAssetAPI maps a GitHub release download URL to the API endpoint
// describing that release and the asset's file name:
//
//	https://github.com/O/R/releases/latest/download/NAME -> /repos/O/R/releases/latest
//	https://github.com/O/R/releases/download/TAG/NAME    -> /repos/O/R/releases/tags/TAG
//
// ok is false for any other URL (a mirror, a fork's raw file), which then
// installs without a digest.
func releaseAssetAPI(download string) (api, asset string, ok bool) {
	u, err := url.Parse(download)
	if err != nil || u.Scheme != "https" || !strings.EqualFold(u.Host, "github.com") {
		return "", "", false
	}
	p := strings.Split(strings.Trim(u.Path, "/"), "/")
	switch {
	case len(p) == 6 && p[2] == "releases" && p[3] == "latest" && p[4] == "download":
		return githubAPI + "/repos/" + p[0] + "/" + p[1] + "/releases/latest", p[5], true
	case len(p) == 6 && p[2] == "releases" && p[3] == "download":
		return githubAPI + "/repos/" + p[0] + "/" + p[1] + "/releases/tags/" + p[4], p[5], true
	}
	return "", "", false
}

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

// releaseDigest fetches the sha256 GitHub records for the asset a download
// URL names. It returns the lower-case hex digest, or "" with the reason it
// could not be had; the install then goes ahead unverified and says so.
func releaseDigest(ctx context.Context, download string) (digest, reason string) {
	api, asset, ok := releaseAssetAPI(download)
	if !ok {
		return "", "not a GitHub release asset URL"
	}
	rctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, api, nil)
	if err != nil {
		return "", err.Error()
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "pmss-fleet-manager")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "GitHub API: " + err.Error()
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", fmt.Sprintf("GitHub API answered HTTP %d", resp.StatusCode)
	}
	var rel struct {
		Assets []struct {
			Name   string `json:"name"`
			Digest string `json:"digest"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return "", "GitHub API: " + err.Error()
	}
	for _, a := range rel.Assets {
		if a.Name != asset {
			continue
		}
		d, found := strings.CutPrefix(a.Digest, "sha256:")
		d = strings.ToLower(d)
		if !found || !sha256Hex.MatchString(d) {
			return "", "the release lists no sha256 digest for " + asset
		}
		return d, ""
	}
	return "", "the release has no asset named " + asset
}

// shellQuote single-quotes a string for POSIX sh.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// errPMSSManagedQbt refuses install and autostart on an account where the
// PMSS panel has qBittorrent enabled: its watchdog starts the system
// qbittorrent-nox and enforces its own config, so a second, self-managed
// build would fight it.
var errPMSSManagedQbt = errors.New("qBittorrent on this account is managed by Pulsed Media's panel; disable it there before installing or autostarting your own build")

// errPMSSManagedStart refuses start for the same reason: the panel's
// watchdog starts its own client, so ours would be a second one. Restart
// only stops it and leaves the start to the watchdog.
var errPMSSManagedStart = errors.New("qBittorrent on this account is managed by Pulsed Media's panel: its watchdog starts it")

// pmssManagedQbt reports whether the last snapshot saw the PMSS panel's
// qBittorrent marker on this account.
func (p *poller) pmssManagedQbt() bool {
	st, ok := p.m.State(p.box.ID)
	return ok && st.Qbt.PMSSManaged
}

func (p *poller) qbtInstall(ctx context.Context, downloadURL string) (string, error) {
	if _, err := p.readyState(); err != nil {
		return "", err // the panel-managed check below needs a probe of this machine
	}
	if p.pmssManagedQbt() {
		return "", errPMSSManagedQbt
	}
	if !ValidDownloadURL(downloadURL) {
		return "", errors.New("download URL must be https, contain only URL characters and be at most 2048 bytes")
	}
	// The digest comes from GitHub's API on the dashboard host, the file
	// from GitHub's CDN on the box: a swapped download on either path fails
	// the comparison. No digest is not an error, only an unverified install.
	digest, why := releaseDigest(ctx, downloadURL)
	note := ""
	if digest == "" {
		note = "digest: " + why + "\n"
	}
	cli, err := p.conn(ctx)
	if err != nil {
		return "", err
	}
	ictx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	// The URL (which may carry a token for a private build) and the digest
	// travel in the script on stdin, never on the command line /proc shows.
	out, stderr, err := cli.Run(ictx, "sh", strings.NewReader("QBT_URL="+shellQuote(downloadURL)+"\nQBT_SHA256="+shellQuote(digest)+"\n"+installScript))
	text := strings.TrimSpace(note + string(out) + "\n" + string(stderr))
	if err != nil {
		return text, fmt.Errorf("install failed: %s", firstLine(strings.TrimPrefix(text, note), err))
	}
	return text, nil
}

func (p *poller) qbtAutostart(ctx context.Context) (string, error) {
	if _, err := p.readyState(); err != nil {
		return "", err
	}
	if p.pmssManagedQbt() {
		return "", errPMSSManagedQbt
	}
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
	if _, err := p.readyState(); err != nil {
		return "", err
	}
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
	if p.pmssManagedQbt() {
		return text + "\nnot started here: Pulsed Media's watchdog starts the panel's qBittorrent again", nil
	}
	cmd := p.startCommand()
	_, stderr, err = cli.Run(rctx, startViaStdin, strings.NewReader(cmd+"\n"))
	if err != nil {
		return text, fmt.Errorf("stopped, but start command failed: %v %s", err, strings.TrimSpace(string(stderr)))
	}
	return text + "\nstarted with: " + cmd, nil
}

// startViaStdin is the SSH command that runs the start command read from
// stdin in the account's login shell: the command (which may carry a
// token) then never appears in /proc/<pid>/cmdline, where other tenants
// could read it on a host without process hiding.
const startViaStdin = `"${SHELL:-sh}" -s`

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
	// The release object is a few KiB; anything past 1 MiB is not one.
	if resp.StatusCode != 200 || json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel) != nil {
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
		// Keep the leading digits only: "4-1" (distro suffix) and "3rc1"
		// compare as 4 and 3; a part that does not start with a digit
		// ("foo2") is unparsable.
		digits := s
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
