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
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha512"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"golang.org/x/crypto/bcrypt"
	"golang.org/x/crypto/ssh"

	"pmss-fleet-manager/internal/sshx"
	"pmss-fleet-manager/internal/store"
)

// Changing an account's password, as the tenant.
//
// PMSS sets one password in three places when it provisions an account
// (scripts/changePw.php): the Linux account, the per-user lighttpd's
// ~/.lighttpd/.htpasswd, and the WebUI hash in qBittorrent.conf. Pulsed
// Media's client panel keeps that password readable, so an operator who
// wants one the panel does not know changes all three from here.
//
// What leaves the dashboard, and how:
//
//   - Linux account: the box's own passwd, the only way a tenant has. It
//     must be given the current and the new password; both travel in a
//     here-document of the script sent on the SSH session's stdin, so they
//     are never part of a command line and never in a file.
//   - .htpasswd: a bcrypt hash made here. The password itself is not sent.
//   - qBittorrent: a PBKDF2 hash made here, written into qBittorrent.conf
//     while the client is stopped (it rewrites the whole file from memory
//     when it exits). The password itself is not sent.
//
// Deluge and Transmission are left alone, as PMSS leaves them: Deluge has
// its own generated service password (its auth file is plain text), and
// PMSS's Transmission has no password at all.

// The fixtures a password change can cover, in the order they are done.
const (
	PwAccount = "account" // the Linux account: SSH, SFTP, FTP
	PwWeb     = "web"     // ~/.lighttpd/.htpasswd
	PwQbt     = "qbt"     // the WebUI hash in qBittorrent.conf
)

// The outcomes of one fixture.
const (
	PwChanged = "changed"
	PwFailed  = "failed"  // refused or broken; the fixture is as it was unless the detail says otherwise
	PwSkipped = "skipped" // not attempted: not on this box, or an earlier step did not succeed
	PwUnknown = "unknown" // attempted, and the outcome could not be read or confirmed
)

// PasswordRequest is one password change. Current is needed for the Linux
// account only; blank means the SSH password the dashboard has saved.
type PasswordRequest struct {
	Current string
	New     string
	Targets []string
}

// PasswordStep is what happened to one fixture.
type PasswordStep struct {
	Target string `json:"target"`
	Status string `json:"status"`
	Detail string `json:"detail,omitempty"`
}

// PasswordResult reports every requested fixture, the stored logins the
// dashboard replaced with the new password ("ssh", "webui"), and a warning
// when one that had to be replaced could not be.
type PasswordResult struct {
	Steps   []PasswordStep `json:"steps"`
	Saved   []string       `json:"saved,omitempty"`
	Warning string         `json:"warning,omitempty"`
}

var (
	// ErrPasswordInput wraps a request the dashboard refuses before it
	// reaches the box.
	ErrPasswordInput = errors.New("password change refused")
	// ErrPasswordBusy refuses a second change while one runs on the box.
	ErrPasswordBusy = errors.New("a password change is already running on this box")
)

func pwInputErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrPasswordInput, fmt.Sprintf(format, a...))
}

// The new password's bounds. 72 bytes is bcrypt's limit (the .htpasswd
// hash); printable ASCII because the same password is typed into HTTP
// basic auth, FTP and SSH clients, which disagree about how anything else
// is encoded.
const (
	pwMinLen = 8
	pwMaxLen = 72
)

// ValidNewPassword checks a new password against the dashboard's own
// rules. The box's passwd applies its own on top.
func ValidNewPassword(pw string) error {
	if len(pw) < pwMinLen {
		return pwInputErr("the new password must be at least %d characters", pwMinLen)
	}
	if len(pw) > pwMaxLen {
		return pwInputErr("the new password must be at most %d characters (bcrypt, which the web panel's password file uses, reads no more)", pwMaxLen)
	}
	for i := 0; i < len(pw); i++ {
		if pw[i] < 0x20 || pw[i] > 0x7e {
			return pwInputErr("the new password may hold printable ASCII characters only (letters, digits, punctuation, spaces inside): web, FTP and SSH clients do not agree on how any other character is sent")
		}
	}
	if pw[0] == ' ' || pw[len(pw)-1] == ' ' {
		return pwInputErr("the new password must not start or end with a space")
	}
	return nil
}

// validCurrentPassword bounds the current password: whatever it is, it is
// one line of the script's here-document.
func validCurrentPassword(pw string) error {
	if pw == "" {
		return pwInputErr("enter the account's current password (the box's passwd asks for it); none is saved for this box")
	}
	if len(pw) > 256 {
		return pwInputErr("the current password is longer than 256 bytes")
	}
	for i := 0; i < len(pw); i++ {
		if pw[i] < 0x20 || pw[i] == 0x7f {
			return pwInputErr("the current password holds a control character")
		}
	}
	return nil
}

// pwTargets returns the requested fixtures as a set.
func pwTargets(names []string) (map[string]bool, error) {
	want := map[string]bool{}
	for _, n := range names {
		switch n {
		case PwAccount, PwWeb, PwQbt:
			want[n] = true
		default:
			return nil, pwInputErr("unknown target %q (account, web, qbt)", n)
		}
	}
	if len(want) == 0 {
		return nil, pwInputErr("choose at least one of account, web, qbt")
	}
	return want, nil
}

// htpasswdUser is the account name as an .htpasswd entry can carry it.
var htpasswdUser = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)

// htpasswdEntry is the account's .htpasswd line for pw: bcrypt at PHP's
// default cost under the $2y$ prefix, which is what PMSS writes itself and
// what lighttpd (through crypt) and rclone's GUI both read. Go's bcrypt
// emits $2a$; for the passwords ValidNewPassword admits the two are the
// same algorithm, byte for byte.
func htpasswdEntry(user, pw string) (string, error) {
	if !htpasswdUser.MatchString(user) {
		return "", errors.New("the account name cannot be written as a password-file entry")
	}
	h, err := bcrypt.GenerateFromPassword([]byte(pw), bcrypt.DefaultCost)
	if err != nil {
		return "", err
	}
	hash := string(h)
	if !strings.HasPrefix(hash, "$2a$") {
		return "", errors.New("unexpected bcrypt hash format")
	}
	return user + ":$2y$" + hash[4:], nil
}

// qbtPasswordValue is qBittorrent's stored WebUI password for pw, as the
// value of its Password_PBKDF2 key: PBKDF2-HMAC-SHA512, 100 000 rounds, a
// 16-byte salt and a 64-byte key, both base64 (Utils::Password::PBKDF2 in
// qBittorrent; PMSS's pmssQbittorrentPasswordHashGenerate makes the same).
func qbtPasswordValue(pw string, salt []byte) (string, error) {
	key, err := pbkdf2.Key(sha512.New, pw, salt, 100000, 64)
	if err != nil {
		return "", err
	}
	return `"@ByteArray(` + base64.StdEncoding.EncodeToString(salt) + ":" + base64.StdEncoding.EncodeToString(key) + `)"`, nil
}

// pwPrelude opens every script of this file. The secrets reach the shell
// in here-documents: dash (Debian's /bin/sh, so every PMSS box's) and
// busybox hand those to the command through a pipe, while bash before 5.1,
// zsh and ksh may write a temporary file. Those are refused before
// anything is read.
const pwPrelude = `#!/bin/sh
LC_ALL=C; export LC_ALL
[ -z "${BASH_VERSION-}${ZSH_VERSION-}${KSH_VERSION-}" ] || { echo "this box's /bin/sh is not a plain POSIX shell (dash); not handing it a password"; exit 97; }
umask 077
`

// hereDoc returns a quoted here-document holding lines, under a delimiter
// no line can equal. Quoted, so the shell expands nothing inside it.
func hereDoc(lines ...string) (string, error) {
	var r [16]byte
	if _, err := rand.Read(r[:]); err != nil {
		return "", err
	}
	delim := "PFM_" + hex.EncodeToString(r[:])
	var b strings.Builder
	b.WriteString("<<'" + delim + "'\n")
	for _, l := range lines {
		if strings.ContainsAny(l, "\r\n\x00") || l == delim {
			return "", errors.New("value cannot be passed to the box as one line")
		}
		b.WriteString(l + "\n")
	}
	b.WriteString(delim + "\n")
	return b.String(), nil
}

// passwdScript runs the box's passwd with the current password and the
// new one twice on its stdin, the three lines it reads when it has no
// terminal (libpam_misc then reads one line per prompt; observed on Debian
// 10, 11, 12 and 13). Its exit status is the script's: 0 only when the
// password was updated, 10 for anything PAM refused, with nothing changed.
func passwdScript(cur, neu string) (string, error) {
	doc, err := hereDoc(cur, neu, neu)
	if err != nil {
		return "", err
	}
	return pwPrelude + `command -v passwd >/dev/null 2>&1 || { echo "passwd is not available to this account"; exit 96; }
passwd 2>&1 ` + doc, nil
}

// htpasswdScript replaces the account's line in ~/.lighttpd/.htpasswd
// with $entry and keeps every other line: a copy beside the file, the
// file's own mode, renamed over it, so a reader sees the old file or the
// new one and an older root-owned file is replaced through the account's
// own directory, as PMSS does it. Only shell builtins handle the entry
// (read, case, printf): no command line carries the hash. The copy is
// renamed only if every line was written and it counts as many lines as
// were meant for it: an account at its disk quota keeps its old file
// (Codex on PR 184).
// Exit 3: no such file on this box. 4, 5: refused or failed, unchanged.
const htpasswdScript = `d="$HOME/.lighttpd"; f="$d/.htpasswd"
[ -d "$d" ] && [ ! -L "$d" ] || { echo "no ~/.lighttpd directory on this box"; exit 3; }
[ -e "$f" ] || [ -L "$f" ] || { echo "no ~/.lighttpd/.htpasswd on this box"; exit 3; }
[ -f "$f" ] && [ ! -L "$f" ] || { echo "~/.lighttpd/.htpasswd is not a regular file"; exit 4; }
size=$(stat -c %s -- "$f") && mode=$(stat -c %a -- "$f") || { echo "cannot stat ~/.lighttpd/.htpasswd"; exit 4; }
[ "$size" -le 65536 ] || { echo "~/.lighttpd/.htpasswd is larger than 64 KiB"; exit 4; }
tmp=$(mktemp -- "$d/.htpasswd.pfm.XXXXXXXX") || { echo "cannot create a file in ~/.lighttpd"; exit 5; }
trap 'rm -f -- "$tmp"' EXIT
full() { echo "cannot write the new password file in ~/.lighttpd (out of space or over quota?); the old one is unchanged" >&2; exit 5; }
# n: lines of this account seen (the first becomes the entry, any further
# one is dropped); k: lines written. Every write is checked where it is
# made: a failed one inside the loop would otherwise be hidden by the
# commands after it, and a short copy renamed over the file.
n=0; k=0
while IFS= read -r line || [ -n "$line" ]; do
  case $line in
    "$U:"*) n=$((n+1)); [ "$n" -eq 1 ] || continue; line=$entry ;;
  esac
  printf '%s\n' "$line" || full
  k=$((k+1))
done < "$f" > "$tmp" || { echo "cannot read ~/.lighttpd/.htpasswd"; exit 5; }
if [ "$n" -eq 0 ]; then printf '%s\n' "$entry" >> "$tmp" || full; k=$((k+1)); fi
[ "$(wc -l < "$tmp")" -eq "$k" ] || full
chmod "$mode" "$tmp" && mv -T -- "$tmp" "$f" || { echo "cannot replace ~/.lighttpd/.htpasswd"; exit 5; }
trap - EXIT
echo "replaced=$n"
`

// qbtProcName is the process the qBittorrent step stops. A variable so the
// tests can aim the script at a stand-in rather than at whatever
// qbittorrent-nox the machine running them has.
var qbtProcName = "qbittorrent-nox"

// qbtConfScript writes $new over every WebUI password line of
// qBittorrent.conf: WebUI\Password_PBKDF2 in [Preferences] (builds below
// 5.3) and Password_PBKDF2 in [WebUI] (5.3 and later, which moved the
// keys), whichever the file holds, so no section keeps a stale hash for a
// build that reads it. Lines are replaced, never added: a file without one
// has no WebUI password to change and is left alone, before anything is
// stopped.
//
// qBittorrent writes its whole configuration from memory when it exits, so
// the file is only edited while no qbittorrent-nox of this account runs:
// a running one is asked to exit (SIGTERM saves its state) and waited for.
// Something else may start it at any moment (the panel's watchdog, the
// cron helper), and a client started before the rename has the old hash in
// memory, to be written back at its next exit: so after the rename the
// script looks again, and a client that is running then is stopped and the
// file rewritten once more. With none running after a rename, whatever
// starts next reads the new file; the lines are then read back.
//
// Output: "was_running=1" when a client was stopped, "needs_start=1" when
// the script ends with that client still down. Exit 3: nothing to change
// here. 4 to 9: failed, the reason in the last line.
const qbtConfScript = `conf="$HOME/.config/qBittorrent/qBittorrent.conf"
[ -e "$conf" ] || [ -L "$conf" ] || { echo "no ~/.config/qBittorrent/qBittorrent.conf on this box"; exit 3; }
[ -f "$conf" ] && [ ! -L "$conf" ] || { echo "qBittorrent.conf is not a regular file"; exit 4; }
size=$(stat -c %s -- "$conf") && mode=$(stat -c %a -- "$conf") || { echo "cannot stat qBittorrent.conf"; exit 4; }
[ "$size" -le 1048576 ] || { echo "qBittorrent.conf is larger than 1 MiB"; exit 4; }
me=$(id -u)
# A client that exited but was not reaped yet (a zombie) is still listed by
# pgrep; it holds no configuration and is not running.
running() {
  for pid in $(pgrep -u "$me" -x "$PROC" 2>/dev/null); do
    case $(ps -o stat= -p "$pid" 2>/dev/null) in ''|Z*) ;; *) return 0 ;; esac
  done
  return 1
}
# pass copies the file to $1 with the password lines replaced; n counts
# them, k the lines written. A write that fails ends it at once: hidden
# by the next command, it would leave a short copy to be renamed over the
# configuration.
pass() {
  n=0; k=0; sec=
  while IFS= read -r line || [ -n "$line" ]; do
    case $line in '['*']') sec=$line ;; esac
    case "$sec:$line" in
      '[Preferences]:WebUI\Password_PBKDF2='*) line='WebUI\Password_PBKDF2='$new; n=$((n+1)) ;;
      '[WebUI]:Password_PBKDF2='*) line="Password_PBKDF2=$new"; n=$((n+1)) ;;
    esac
    printf '%s\n' "$line" || return 1
    k=$((k+1))
  done < "$conf" > "$1"
}
# check reads the file back: at least one password line, each the new one.
check() {
  ok=0; bad=0; sec=
  while IFS= read -r line || [ -n "$line" ]; do
    case $line in '['*']') sec=$line ;; esac
    case "$sec:$line" in
      '[Preferences]:WebUI\Password_PBKDF2='*) if [ "$line" = 'WebUI\Password_PBKDF2='"$new" ]; then ok=$((ok+1)); else bad=$((bad+1)); fi ;;
      '[WebUI]:Password_PBKDF2='*) if [ "$line" = "Password_PBKDF2=$new" ]; then ok=$((ok+1)); else bad=$((bad+1)); fi ;;
    esac
  done < "$conf"
  [ "$ok" -gt 0 ] && [ "$bad" -eq 0 ]
}
pass /dev/null || { echo "cannot read qBittorrent.conf"; exit 5; }
[ "$n" -gt 0 ] || { echo "qBittorrent.conf holds no WebUI password"; exit 3; }
ran=0; tmp=
trap '[ -z "$tmp" ] || rm -f -- "$tmp"; if [ "$ran" = 1 ] && ! running; then echo "needs_start=1"; fi' EXIT
try=0
while :; do
  try=$((try+1))
  if running; then
    [ "$ran" = 1 ] || echo "was_running=1"
    ran=1
    pkill -u "$me" -TERM -x "$PROC"
    i=0
    while running && [ "$i" -lt 60 ]; do sleep 1; i=$((i+1)); done
    if running; then echo "qBittorrent did not exit within 60 s; its configuration was not changed"; exit 6; fi
  fi
  if [ -s "${conf%.conf}_new.conf" ]; then echo "an interrupted save is pending (qBittorrent_new.conf), which qBittorrent would load in place of the edited file; start and stop the client once, then try again"; exit 7; fi
  tmp=$(mktemp -- "$conf.pfm.XXXXXXXX") || { echo "cannot create a file in ~/.config/qBittorrent"; exit 5; }
  pass "$tmp" && [ "$n" -gt 0 ] && [ "$(wc -l < "$tmp")" -eq "$k" ] || { echo "cannot write the new qBittorrent.conf (out of space or over quota?); the old one is unchanged" >&2; exit 5; }
  chmod "$mode" "$tmp" && mv -T -- "$tmp" "$conf" || { echo "cannot replace qBittorrent.conf"; exit 5; }
  tmp=
  if ! running; then
    check || { echo "qBittorrent.conf does not hold the new hash after the write"; exit 8; }
    break
  fi
  [ "$try" -lt 3 ] || { echo "qBittorrent kept being started while its configuration was rewritten; the new password may not last, try again"; exit 9; }
done
echo "result=ok"
`

// pwStepTimeout bounds one script. The qBittorrent step waits up to 60 s
// per stop, at most three times. Variables so tests can shorten them.
var (
	pwStepTimeout    = 45 * time.Second
	pwQbtStepTimeout = 4 * time.Minute
)

// pwTotalTimeout bounds one whole change.
const pwTotalTimeout = 6 * time.Minute

// pwOutLimit bounds a script's output: a few lines.
const pwOutLimit = 16 << 10

// pwScrub removes the passwords from text read from the box before it is
// kept: passwd does not echo them, and the scripts print nothing they
// were given, but what comes back from a box is not ours to vouch for.
func pwScrub(s string, secrets ...string) string {
	for _, x := range secrets {
		if x != "" {
			s = strings.ReplaceAll(s, x, "[redacted]")
		}
	}
	return s
}

// pwClean makes box output fit to show: printable ASCII, one line, short.
func pwClean(s string) string {
	var b strings.Builder
	for _, r := range strings.TrimSpace(s) {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			b.WriteByte(' ')
		case r >= 0x20 && r < 0x7f:
			b.WriteRune(r)
		}
	}
	return clipTo(strings.Join(strings.Fields(b.String()), " "), 240)
}

func clipTo(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// pwMarker matches the scripts' own status lines ("was_running=1").
var pwMarker = regexp.MustCompile(`^[a-z_]+=[a-z0-9]+$`)

// lastLine is the last line of a script's output that is not one of its
// status markers: the reason it gave.
func lastLine(out string) string {
	lines := strings.Split(strings.TrimSpace(out), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		if l := strings.TrimSpace(lines[i]); l != "" && !pwMarker.MatchString(l) {
			return l
		}
	}
	return ""
}

// exitCode is the remote command's exit status when err is one: a command
// that ran to its end and said no. Anything else (a dropped connection, a
// deadline, a signal) leaves the outcome open.
func exitCode(err error) (int, bool) {
	var ee *ssh.ExitError
	if errors.As(err, &ee) && ee.Signal() == "" {
		return ee.ExitStatus(), true
	}
	return 0, false
}

// passwdPrompts are the texts passwd writes around the three answers.
var passwdPrompts = strings.NewReplacer(
	"Current password:", "\n", "(current) UNIX password:", "\n",
	"New password:", "\n", "Retype new password:", "\n",
	"Enter new UNIX password:", "\n", "Retype new UNIX password:", "\n",
)

// passwdRefusal turns passwd's output for a change PAM refused (exit
// status 10) into the reason: whatever passwd said beyond its prompts and
// boilerplate ("You must choose a longer password."). When it said nothing
// more and never asked for the new password, the current one was not
// accepted.
func passwdRefusal(out string) string {
	asked := strings.Contains(out, "New password:") || strings.Contains(out, "new UNIX password:")
	var why []string
	for _, l := range strings.Split(passwdPrompts.Replace(out), "\n") {
		l = strings.TrimSpace(l)
		switch {
		case l == "", strings.HasPrefix(l, "passwd: "), strings.HasPrefix(l, "Changing password for"),
			l == "Password change has been aborted.", l == "Password change aborted.": // Debian 11+, Debian 10
		default:
			why = append(why, l)
		}
	}
	switch {
	case len(why) > 0 && asked:
		return "the box's passwd refused the new password: " + pwClean(strings.Join(why, " "))
	case len(why) > 0:
		return "the box's passwd refused the change: " + pwClean(strings.Join(why, " "))
	case asked:
		return "the box's passwd refused the change"
	}
	return "the current password was not accepted by the box"
}

// pwAccount changes the Linux account's password with the box's passwd.
func (p *poller) pwAccount(ctx context.Context, cur, neu string) PasswordStep {
	step := PasswordStep{Target: PwAccount, Status: PwFailed}
	script, err := passwdScript(cur, neu)
	if err != nil {
		step.Detail = err.Error()
		return step
	}
	cli, err := p.conn(ctx)
	if err != nil {
		step.Detail = "no connection to the box: " + pwClean(err.Error())
		return step
	}
	sctx, cancel := context.WithTimeout(ctx, pwStepTimeout)
	defer cancel()
	out, _, err := cli.RunLimited(sctx, "/bin/sh", strings.NewReader(script), pwOutLimit)
	text := pwScrub(string(out), cur, neu)
	if err == nil {
		step.Status = PwChanged
		return step
	}
	if code, ok := exitCode(err); ok {
		switch code {
		case 10: // PAM said no: nothing was changed
			step.Detail = passwdRefusal(text)
		case 96, 97: // the script's own refusals
			step.Detail = pwClean(lastLine(text))
		default: // passwd's other failures (1 to 6), or no /bin/sh at all
			step.Detail = fmt.Sprintf("the box's passwd ended with status %d: %s", code, pwClean(lastLine(text)))
		}
		return step
	}
	// The script was sent and its end was not seen: passwd may or may not
	// have run. One login with the new password, over a connection of its
	// own, settles it without guessing.
	pctx, pcancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer pcancel()
	probe := p.pwProbe
	if probe == nil {
		probe = p.pwProbeLogin
	}
	switch accepted, perr := probe(pctx, neu); {
	case perr != nil:
		step.Status = PwUnknown
		step.Detail = "the connection failed while passwd ran (" + pwClean(pwScrub(err.Error(), cur, neu)) + ") and a login with the new password could not be tried (" + pwClean(pwScrub(perr.Error(), cur, neu)) + "): either password may be the valid one"
	case accepted:
		step.Status = PwChanged
		step.Detail = "the connection failed while passwd ran; a login with the new password then succeeded"
	default:
		step.Detail = "the connection failed while passwd ran, and the box does not accept the new password: the old one is still valid"
	}
	return step
}

// pwProbeLogin reports whether the box accepts pw as the account's SSH
// password, on a fresh connection pinned to the box's host key. An error
// means the question was not answered.
func (p *poller) pwProbeLogin(ctx context.Context, pw string) (bool, error) {
	p.mu.Lock()
	cfg := sshConfig(p.box) // the pin is written under this lock (conn)
	p.mu.Unlock()
	if cfg.HostKey == "" {
		return false, errors.New("no pinned host key")
	}
	cfg.AuthMethod, cfg.Password, cfg.KeyPath = "password", pw, ""
	cli, err := sshx.Dial(ctx, cfg)
	if err == nil {
		cli.Close()
		return true, nil
	}
	// x/crypto names the methods it tried: a refusal answers the question
	// only if the password was among them.
	if msg := err.Error(); strings.Contains(msg, "unable to authenticate") {
		if strings.Contains(msg, "password") || strings.Contains(msg, "keyboard-interactive") {
			return false, nil
		}
		return false, errors.New("the box does not offer password logins over SSH")
	}
	return false, err
}

// pwWeb replaces the account's .htpasswd entry.
func (p *poller) pwWeb(ctx context.Context, neu string) PasswordStep {
	step := PasswordStep{Target: PwWeb, Status: PwFailed}
	entry, err := htpasswdEntry(p.box.User, neu)
	if err != nil {
		step.Detail = err.Error()
		return step
	}
	doc, err := hereDoc(entry)
	if err != nil {
		step.Detail = err.Error()
		return step
	}
	script := pwPrelude + "U=" + shellQuote(p.box.User) + "\nIFS= read -r entry " + doc + htpasswdScript
	out, err := p.pwRun(ctx, script, pwStepTimeout)
	text := pwScrub(out, entry, neu)
	if err == nil {
		step.Status = PwChanged
		return step
	}
	return pwScriptFailure(step, text, err, "the password file may or may not have been replaced")
}

// pwQbt writes the new WebUI hash into qBittorrent.conf with the client
// stopped, and starts a client it stopped. stopped reports that a running
// client was stopped for the edit, started that this step's start command
// then succeeded (a panel-managed client is left to PMSS's watchdog).
func (p *poller) pwQbt(ctx context.Context, neu string) (step PasswordStep, stopped bool, started bool) {
	step = PasswordStep{Target: PwQbt, Status: PwFailed}
	var salt [16]byte
	if _, err := rand.Read(salt[:]); err != nil {
		step.Detail = err.Error()
		return step, false, false
	}
	value, err := qbtPasswordValue(neu, salt[:])
	if err != nil {
		step.Detail = err.Error()
		return step, false, false
	}
	doc, err := hereDoc(value)
	if err != nil {
		step.Detail = err.Error()
		return step, false, false
	}
	script := pwPrelude + "PROC=" + shellQuote(qbtProcName) + "\nIFS= read -r new " + doc + qbtConfScript
	out, err := p.pwRun(ctx, script, pwQbtStepTimeout)
	text := pwScrub(out, value, neu)
	stopped = strings.Contains(text, "was_running=1")
	if err == nil {
		step.Status = PwChanged
	} else {
		step = pwScriptFailure(step, text, err, "qBittorrent.conf may or may not have been rewritten, and the client may be stopped")
	}
	if !strings.Contains(text, "needs_start=1") {
		if step.Status == PwChanged && !stopped {
			step.Detail = "the client was not running; it reads the new password when it starts"
		}
		return step, stopped, false
	}
	// The client this step stopped is down: bring it back, whatever the
	// edit came to.
	note := ""
	if p.pmssManagedQbt() {
		note = "the client was stopped for the edit; Pulsed Media's watchdog starts it again"
	} else if serr := p.pwStartQbt(ctx); serr != nil {
		note = "the client was stopped for the edit and did NOT start again: " + pwClean(serr.Error())
	} else {
		started = true
		note = "the client was stopped for the edit and started again"
	}
	if step.Detail != "" {
		note = step.Detail + "; " + note
	}
	step.Detail = note
	return step, stopped, started
}

// pwStartQbt runs the box's start command, as a restart does.
func (p *poller) pwStartQbt(ctx context.Context) error {
	cli, err := p.conn(ctx)
	if err != nil {
		return err
	}
	sctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	_, stderr, err := cli.Run(sctx, startViaStdin, strings.NewReader(p.startCommand()+"\n"))
	if err != nil {
		return fmt.Errorf("%v %s", err, strings.TrimSpace(string(stderr)))
	}
	return nil
}

// pwRun sends one script to the box's /bin/sh and returns what it printed.
func (p *poller) pwRun(ctx context.Context, script string, timeout time.Duration) (string, error) {
	cli, err := p.conn(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %v", errPwNotSent, err)
	}
	sctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, stderr, err := cli.RunLimited(sctx, "/bin/sh", strings.NewReader(script), pwOutLimit)
	return string(out) + "\n" + string(stderr), err
}

// errPwNotSent marks a step that never reached the box.
var errPwNotSent = errors.New("no connection to the box")

// pwScriptFailure fills step from a script that did not exit 0: exit 3 is
// "this box has no such fixture", any other exit status a failure the
// script explained, and no exit status at all an open outcome.
func pwScriptFailure(step PasswordStep, text string, err error, open string) PasswordStep {
	code, ok := exitCode(err)
	switch {
	case errors.Is(err, errPwNotSent):
		step.Detail = pwClean(err.Error())
	case ok && code == 3:
		step.Status = PwSkipped
		step.Detail = pwClean(lastLine(text))
	case ok:
		step.Detail = pwClean(lastLine(text))
		if step.Detail == "" {
			step.Detail = fmt.Sprintf("the script on the box ended with status %d", code)
		}
	default:
		step.Status = PwUnknown
		step.Detail = "the connection failed mid-step (" + pwClean(err.Error()) + "): " + open
	}
	return step
}

// ChangePassword sets a new password on the requested fixtures of box id,
// account first: if the box's passwd refuses, nothing else is touched, so
// the three never end up apart by accident. Where the dashboard itself
// logs in with the password that changed (a saved SSH password, saved
// WebUI credentials), the saved one is replaced and the box's poller
// restarted on it.
//
// An error means nothing was attempted on the box. Otherwise every
// requested fixture is reported, including the ones that failed.
func (m *Manager) ChangePassword(ctx context.Context, id int64, req PasswordRequest) (PasswordResult, error) {
	p, err := m.actionPoller(ctx, id)
	if err != nil {
		return PasswordResult{}, err
	}
	want, err := pwTargets(req.Targets)
	if err != nil {
		return PasswordResult{}, err
	}
	if err := ValidNewPassword(req.New); err != nil {
		return PasswordResult{}, err
	}
	cur := req.Current
	if cur == "" {
		cur = p.box.Password
	}
	if want[PwAccount] {
		if err := validCurrentPassword(cur); err != nil {
			return PasswordResult{}, err
		}
		if cur == req.New {
			return PasswordResult{}, pwInputErr("the new password is the current one")
		}
	}
	if _, err := p.readyState(); err != nil {
		return PasswordResult{}, err
	}
	if !p.pwMu.TryLock() {
		return PasswordResult{}, ErrPasswordBusy
	}
	defer p.pwMu.Unlock()
	// From here on the change runs to its end whether or not the browser
	// that asked is still there: a tab closed between passwd and the
	// password file would leave the three apart, and the dashboard holding
	// a login the box no longer takes. An edit or removal of the box, or a
	// shutdown, still stops it (the poller's own context, see conn).
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pwTotalTimeout)
	defer cancel()

	var res PasswordResult
	qbtRestarted := false // a running client was stopped by this change
	status := map[string]string{}
	add := func(s PasswordStep) {
		res.Steps = append(res.Steps, s)
		status[s.Target] = s.Status
	}
	if want[PwAccount] {
		add(p.pwAccount(ctx, cur, req.New))
	}
	// The other fixtures follow the account's password, not a refused one.
	held := ""
	switch status[PwAccount] {
	case PwFailed:
		held = "not attempted: the account password was not changed"
	case PwUnknown:
		held = "not attempted: the account password change could not be confirmed"
	}
	if want[PwWeb] {
		if held != "" {
			add(PasswordStep{Target: PwWeb, Status: PwSkipped, Detail: held})
		} else {
			add(p.pwWeb(ctx, req.New))
		}
	}
	if want[PwQbt] {
		if held != "" {
			add(PasswordStep{Target: PwQbt, Status: PwSkipped, Detail: held})
		} else {
			step, stopped, started := p.pwQbt(ctx, req.New)
			add(step)
			m.pwQbtStopped(id, p, stopped, started)
			qbtRestarted = stopped
		}
	}

	// The logins the dashboard holds for what just changed.
	sshPw, qbtPw := "", ""
	if status[PwAccount] == PwChanged && p.box.Password != "" {
		sshPw = req.New
	}
	if status[PwQbt] == PwChanged && p.box.QbtPass != "" {
		qbtPw = req.New
	}
	if sshPw != "" || qbtPw != "" {
		saved, err := m.savePasswords(p.box, sshPw, qbtPw)
		res.Saved = saved
		if err != nil {
			res.Warning = "changed on the box, but the dashboard could not save the new password for its own logins (" + pwClean(err.Error()) + "): edit the box and enter it, or its next connection is refused"
		}
		// The save replaced the poller: the one that polls next must know
		// too that the client it finds is a new process.
		if np := m.poller(id); np != nil && np != p && qbtRestarted {
			np.noteRestartDone()
		}
	} else {
		p.wake()
	}
	m.log.Info("password change", "box", p.box.Name, PwAccount, status[PwAccount], PwWeb, status[PwWeb], PwQbt, status[PwQbt], "saved", strings.Join(res.Saved, ","), "save_failed", res.Warning != "")
	return res, nil
}

// pwQbtStopped records what a stop and a start of the client mean for the
// poller, as QbtAction does for a restart: the port may have changed
// hands while the client was down, and a client that came back is a new
// process with a fresh failed-login counter.
func (m *Manager) pwQbtStopped(id int64, p *poller, stopped, started bool) {
	if !stopped {
		return
	}
	if started {
		p.guard.Clear()
		detectGuard(p.box).Clear()
		p.noteRestartDone()
	}
	m.mutateState(id, func(s *BoxState) {
		p.noteAction()
		s.Qbt.PortOK, s.Qbt.Reachable = false, false
	})
}

// savePasswords writes the new password over the box's saved SSH and/or
// WebUI password and restarts its poller on the record, like an edit that
// typed them in. It runs on the manager's context: the password is already
// changed on the box, and a browser that went away must not be what leaves
// the dashboard holding the old one. The record is read again under the
// edit lock, and only a login it still stores, for the endpoint the change
// ran on, is replaced.
func (m *Manager) savePasswords(acted *store.Box, sshPw, qbtPw string) ([]string, error) {
	m.boxMu.Lock()
	defer m.boxMu.Unlock()
	sctx, cancel := context.WithTimeout(m.ctx, 30*time.Second)
	defer cancel()
	fresh, err := m.st.GetBox(sctx, acted.ID)
	if err != nil {
		return nil, err
	}
	if AuthEndpoint(fresh) != AuthEndpoint(acted) {
		return nil, errors.New("the box record was moved to another host, port or user meanwhile")
	}
	var saved []string
	b := *fresh
	b.Password, b.QbtPass = "", "" // blank keeps what is stored
	if sshPw != "" && fresh.Password != "" {
		b.Password = sshPw
		saved = append(saved, "ssh")
	}
	if qbtPw != "" && fresh.QbtPass != "" {
		b.QbtPass = qbtPw
		saved = append(saved, "webui")
	}
	if len(saved) == 0 {
		return nil, nil
	}
	if err := m.updateBoxLocked(sctx, &b, store.Clears{}); err != nil {
		return nil, err
	}
	return saved, nil
}
