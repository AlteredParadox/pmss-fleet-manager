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

// Box-to-box copy: the source box runs rsync over ssh straight to the
// destination box with a key made for the one job. The destination accepts
// that key only for rrsync writing into the new directory, only for 30
// minutes from setup, with forwarding, PTY and user rc off. The dashboard
// carries no data and no private key: it runs short setup, launch, status
// and cleanup scripts over its own connections.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"pmss-fleet-manager/internal/sshx"
)

// xferJobDir is where a job's files live on the source, relative to HOME.
// A source directory that is, holds or lies inside the control directory is
// refused, so the key and the live job files are never copied.
const xferJobDir = ".config/pmss-fleet-manager/transfers"

// xferRrsyncOpts restrict the job key on the destination: write only, no
// --delete/--remove, and symlinks munged on the receiving side. Without
// -munge a source that turned hostile (it holds the key) could plant a
// symlink and use it as a --backup-dir to write outside the directory,
// for instance over ~/.ssh/authorized_keys (Codex security review on PR
// 142, reproduced against rrsync 3.2.7).
const xferRrsyncOpts = "-wo -no-del -munge"

// xferRrsync is the rrsync the job key is forced into: the system's, at
// a fixed path (Debian's rsync package; all PMSS boxes), never whatever
// PATH finds first (daybreak review: a user-writable rrsync ahead in PATH
// would run instead of the confinement). The claim checks it is root's.
const xferRrsync = "/usr/bin/rrsync"

// xferOutCap bounds what any job script may send back; the largest, a
// status read, stays under 18 KiB (the job files are bounded by cap.pl and
// read with head -c). More is not a job script's answer, and is refused
// before it is split: a box sending megabytes of bare newlines must not
// cost the dashboard millions of strings (daybreak review B3).
const xferOutCap = 32768

var (
	// A key line's base64 blob, as ssh-keygen prints an ed25519 key.
	xferBlobRe = regexp.MustCompile(`^AAAAC3NzaC1lZDI1NTE5[A-Za-z0-9+/]{48,80}={0,2}$`)
	// A host key algorithm name, as it goes unquoted into the ssh options.
	xferAlgRe = regexp.MustCompile(`^[a-z0-9@.-]{3,64}$`)
)

func newXferToken() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// validXferPath checks a path as typed: absolute or relative to HOME, no
// control characters, no leading '-', no '..' component. The destination
// is quoted inside command="..." in authorized_keys, so it also refuses
// the characters that would need escaping there. Whether a path resolves
// inside HOME is checked on the box (realpath), not here.
func validXferPath(p string, dest bool) error {
	if p == "" || len(p) > 1024 {
		return errors.New("path must be 1-1024 bytes")
	}
	if strings.HasPrefix(p, "-") {
		return errors.New("path must not start with '-'")
	}
	for _, r := range p {
		if r < 0x20 || r == 0x7f {
			return errors.New("path contains a control character")
		}
		if dest && strings.ContainsRune(`"'\,`, r) {
			return errors.New(`destination path must not contain " ' \ or ,`)
		}
	}
	for _, c := range strings.Split(p, "/") {
		if c == ".." {
			return errors.New("path must not contain '..'")
		}
	}
	if dest && strings.Trim(p, "/.") == "" {
		return errors.New("destination must name a new directory")
	}
	return nil
}

// xferPrelude starts every job script: strict, byte-wise, private files,
// and the helpers the scripts share. ERR lines are what the Go side shows.
const xferPrelude = `set -u
LC_ALL=C; export LC_ALL
umask 077
die() { echo "ERR $*"; exit 3; }
case "$TOKEN" in *[!0-9a-f]*|"") die "bad token";; esac
home=$(realpath -e -- "$HOME") || die "cannot resolve HOME"
ctl="$home/.config/pmss-fleet-manager"
J="$ctl/transfers/$TOKEN"
case "$J" in *[!A-Za-z0-9/._-]*) die "home path has characters this feature does not handle";; esac
abspath() { case "$1" in /*) printf '%s' "$1";; *) printf '%s/%s' "$home" "$1";; esac; }
inside() { case "$1/" in "$2"/*) return 0;; esac; return 1; }
# alive: the job's runner is running (its pid's cmdline names this job's
# run.sh; a reused pid does not count).
alive() { pid=$(head -c 20 "$J/pid" 2>/dev/null | tr -cd 0-9); [ -n "$pid" ] && [ -r "/proc/$pid/cmdline" ] && tr '\0' ' ' <"/proc/$pid/cmdline" | grep -qF "$J/run.sh"; }
`

// xferSrcPrepareScript checks the source and makes the job key. The job
// directory is created with mkdir (not -p), so a token is used once.
const xferSrcPrepareScript = `p=$(abspath "$SRC")
[ -L "$p" ] && die "source is a symlink"
src=$(realpath -e -- "$p") || die "source does not exist"
# The typed path has no control characters, but a symlink on the way can
# resolve to one; a newline would break the SRC line below (daybreak).
case "$src" in *[[:cntrl:]]*) die "the resolved source path contains a control character";; esac
[ -d "$src" ] || die "source is not a directory"
inside "$src" "$home" || die "source is not inside your home directory"
[ "$src" = "$home" ] && die "source is your whole home directory (it holds the transfer control directory)"
c=$(realpath -m -- "$ctl")
if inside "$src" "$c" || inside "$c" "$src" || [ "$src" = "$c" ]; then die "source overlaps the dashboard's control directory $c"; fi
for t in rsync ssh ssh-keygen perl setsid mkfifo; do command -v "$t" >/dev/null 2>&1 || die "$t not found on the source"; done
mkdir -p -- "$ctl/transfers" || die "cannot create $ctl/transfers"
# A symlinked transfers directory could point into the source: the chmod
# and the key would land there, and the copy would carry the key.
[ "$(realpath -e -- "$ctl/transfers")" = "$c/transfers" ] || die "$ctl/transfers is a symlink; not using it"
chmod 700 -- "$ctl/transfers" || die "cannot chmod $ctl/transfers"
mkdir -- "$J" || die "job directory already exists"
ssh-keygen -q -t ed25519 -N '' -C "pfm-xfer-$TOKEN" -f "$J/key" </dev/null >/dev/null || die "ssh-keygen failed"
echo "SRC $src"
echo "PUB $(cat "$J/key.pub")"
`

// xferDstClaimScript checks the destination and claims it: mkdir (not -p)
// fails if the path exists, so a collision or a second job aimed at the
// same path is refused here.
const xferDstClaimScript = `rr=` + xferRrsync + `
rt=$(realpath -e -- "$rr" 2>/dev/null) || die "$rr not found on the destination: a restricted transfer key is not possible"
# Root's, and writable only by root, as is the folder it sits in: a
# program the account could replace would run in place of the confinement.
for f in "$rt" "$(dirname -- "$rt")"; do
  [ "$(stat -c %u -- "$f")" = 0 ] && [ $(( 0$(stat -c %a -- "$f") & 022 )) = 0 ] || die "$f is not root-owned and root-only writable: not trusting it as rrsync"
done
[ -f "$rt" ] && [ -x "$rt" ] || die "$rr is not an executable file"
p=$(abspath "$DST")
p=${p%/}
[ -e "$p" ] || [ -L "$p" ] && die "destination already exists"
parent=$(realpath -e -- "$(dirname -- "$p")") || die "destination's parent directory does not exist"
d="$parent/$(basename -- "$p")"
inside "$d" "$home" || die "destination is not inside your home directory"
inside "$d" "$home/.ssh" && die "destination is inside ~/.ssh"
inside "$d" "$(realpath -m -- "$ctl")" && die "destination is inside the dashboard's control directory"
# It goes into command="..." in authorized_keys: the typed path was
# checked, but a symlinked parent can resolve to anything.
case "$d" in *[\"\\,\']*|*[[:cntrl:]]*) die "the resolved destination path has characters authorized_keys cannot carry";; esac
mkdir -- "$d" || die "could not create the destination directory"
echo "DST $d"
`

// xferKeyAddScript appends the job's key line to authorized_keys under a
// lock on ~/.ssh: copy, append, rename, so no other line is rewritten and
// the file keeps its mode. The comment records what this step created
// (+dir ~/.ssh, +new the file, +nl the newline before the line) so removal
// can put the file back byte for byte. Needs $d and $rr from the claim.
const xferKeyAddScript = `case "$BLOB" in *[!A-Za-z0-9+/=]*|"") die "bad key";; esac
flags=""
s="$home/.ssh"; ak="$s/authorized_keys"
[ -L "$s" ] && die "~/.ssh is a symlink; not editing it"
if [ ! -d "$s" ]; then mkdir -m 700 -- "$s" || die "cannot create ~/.ssh"; flags="$flags+dir"; fi
[ -L "$ak" ] && die "authorized_keys is a symlink; not editing it"
exec 9<"$s"
flock -w 10 9 || die "authorized_keys is locked by another job"
tmp="$s/.authorized_keys.pfm-xfer.$$"
if [ -e "$ak" ]; then
  cp -p -- "$ak" "$tmp" || { rm -f -- "$tmp"; die "cannot copy authorized_keys"; }
  if [ -s "$tmp" ] && [ "$(tail -c 1 -- "$tmp" | od -An -tx1 | tr -d ' \n')" != 0a ]; then printf '\n' >>"$tmp"; flags="$flags+nl"; fi
else
  : >"$tmp" || die "cannot create authorized_keys"
  flags="$flags+new"
fi
exp=$(date -d '+30 minutes' +%Y%m%d%H%M) || die "date failed"
line="restrict,expiry-time=\"$exp\",command=\"$rr ` + xferRrsyncOpts + ` '$d'\" ssh-ed25519 $BLOB pfm-xfer-$TOKEN$flags"
printf '%s\n' "$line" >>"$tmp" && mv -f -- "$tmp" "$ak" || { rm -f -- "$tmp"; die "cannot write authorized_keys"; }
echo "KEYLINE $line"
`

// xferKeyRemoveScript removes the one line carrying this job's blob and
// token comment, and undoes what the comment says the add created. The
// newline added before the line goes only if the line is still the last
// one (otherwise the following line would join the previous one).
const xferKeyRemoveScript = `case "$BLOB" in *[!A-Za-z0-9+/=]*|"") die "bad key";; esac
s="$home/.ssh"; ak="$s/authorized_keys"
[ -d "$s" ] || { echo "NOTFOUND no ~/.ssh"; exit 0; }
[ -L "$s" ] || [ -L "$ak" ] && die "~/.ssh or authorized_keys is a symlink; not editing it"
[ -f "$ak" ] || { echo "NOTFOUND no authorized_keys"; exit 0; }
[ "$(stat -c %s -- "$ak")" -le 1048576 ] || die "authorized_keys is over 1 MiB; not editing it"
exec 9<"$s"
flock -w 10 9 || die "authorized_keys is locked by another job"
perl -e '
use strict; use warnings;
my ($ak, $tok, $blob) = @ARGV;
open(my $in, "<", $ak) or die "ERR cannot read authorized_keys\n";
binmode $in; local $/; my $c = <$in>; close $in; $c = "" unless defined $c;
my @lines = split /(?<=\n)/, $c;
my ($at, $flags) = (-1, "");
for my $i (0 .. $#lines) {
  (my $b = $lines[$i]) =~ s/\r?\n\z//;
  my @f = split / /, $b;
  if (@f >= 3 && $f[-2] eq $blob && $f[-1] =~ /\Apfm-xfer-\Q$tok\E((?:\+(?:nl|new|dir))*)\z/) { ($at, $flags) = ($i, $1); last }
}
if ($at < 0) { print "NOTFOUND\n"; exit 0 }
my $last = $at == $#lines;
splice @lines, $at, 1;
$lines[-1] =~ s/\n\z// if $last && @lines && $flags =~ /\+nl/;
my $out = join "", @lines;
if ($out eq "" && $flags =~ /\+new/) {
  unlink $ak or die "ERR cannot remove authorized_keys\n";
  print "REMOVED file\n";
  (my $sd = $ak) =~ s{/[^/]*\z}{};
  rmdir $sd and print "REMOVED dir\n" if $flags =~ /\+dir/;
  exit 0;
}
my $mode = (stat $ak)[2] & 07777;
my $tmp = "$ak.pfm-xfer.$$";
open(my $o, ">", $tmp) or die "ERR cannot write authorized_keys\n";
binmode $o; print $o $out or die "ERR write failed\n"; close $o or die "ERR write failed\n";
chmod $mode, $tmp; rename $tmp, $ak or do { unlink $tmp; die "ERR cannot replace authorized_keys\n" };
print "REMOVED line\n";
' "$ak" "$TOKEN" "$BLOB"
`

// xferCapScript is the one helper that drains rsync's output: fixed 4 KiB
// reads, segments split on \r or \n by hand, every kept piece capped, so
// neither memory nor the files it writes grow with the output, even output
// without any separator. "progress" keeps the latest segment (512 B) and
// counts skipped non-regular files; "errors" keeps the first 40 and last
// 20 segments (256 B each) and a total. Files are rewritten (temp +
// rename) at most once a second and at EOF.
const xferCapScript = `use strict; use warnings;
my ($mode, $dir) = @ARGV;
my $cap = $mode eq "progress" ? 512 : 256;
my ($seg, $trunc, $dirty, $wrote) = ("", 0, 0, 0);
my ($last, $skipped, $count, @head, @ring) = ("", 0, 0);
sub put { my ($n, $d) = @_; my $t = "$dir/$n.tmp"; open(my $f, ">", $t) or return; print $f $d; close $f; rename $t, "$dir/$n"; }
sub out {
  if ($mode eq "progress") { put("progress", "$last\n"); put("skipped", "$skipped\n"); }
  else {
    my $more = $count - @head - @ring;
    put("errors", join("", map("$_\n", @head)) . ($more > 0 ? "... $more more ...\n" : "") . join("", map("$_\n", @ring)));
    put("errors.count", "$count\n");
  }
  ($dirty, $wrote) = (0, time);
}
sub seg_end {
  my $s = $seg . ($trunc ? " [truncated]" : "");
  ($seg, $trunc) = ("", 0);
  return unless $s =~ /\S/;
  if ($mode eq "progress") { if ($s =~ /^skipping non-regular file/) { $skipped++ } else { $last = $s } }
  else { $count++; if (@head < 40) { push @head, $s } else { push @ring, $s; shift @ring if @ring > 20 } }
  $dirty = 1;
}
out();
my $buf;
while (1) {
  my $n = sysread(STDIN, $buf, 4096);
  if (!defined $n) { next if $!{EINTR}; last }
  last if $n == 0;
  for my $p (split /([\r\n])/, $buf, -1) {
    if ($p eq "\r" || $p eq "\n") { seg_end(); next }
    my $room = $cap - length $seg;
    if (length $p > $room) { $seg .= substr($p, 0, $room) if $room > 0; $trunc = 1 } else { $seg .= $p }
  }
  out() if $dirty && time > $wrote;
}
seg_end(); out();
`

// xferRunScript is the detached runner on the source, after the SRC,
// DEST, PORT and HKALG assignments the launch writes in front of it. mkdir "started" makes
// it run at most once per job, whoever launches it how often. The key
// files go when it ends, however it ends; rc is written last, atomically.
const xferRunScript = `set -u
J=$(cd "$(dirname -- "$0")" && pwd -P) || exit 1
LC_ALL=C; export LC_ALL
umask 077
mkdir "$J/started" 2>/dev/null || exit 0
rpid=""
finish() { rm -f "$J/key" "$J/key.pub" "$J/out" "$J/err"; printf '%s\n' "$1" >"$J/rc.tmp" && mv -f "$J/rc.tmp" "$J/rc"; }
trap 'rm -f "$J/key" "$J/key.pub"' EXIT
trap '[ -n "$rpid" ] && kill "$rpid" 2>/dev/null; finish cancelled; exit 143' TERM INT HUP
# Without a pid the copy could not be cancelled: no pid, no copy.
{ echo $$ >"$J/pid.tmp" && mv -f "$J/pid.tmp" "$J/pid"; } 2>/dev/null || { finish "error: cannot write the pid file"; exit 1; }
date +%s >"$J/started_at"
mkfifo "$J/out" "$J/err" || { finish "error: mkfifo failed"; exit 1; }
perl "$J/cap.pl" progress "$J" <"$J/out" &
perl "$J/cap.pl" errors "$J" <"$J/err" &
rsync -rt --no-inc-recursive --info=progress2 \
  -e "ssh -F /dev/null -p $PORT -i $J/key -o StrictHostKeyChecking=yes -o UserKnownHostsFile=$J/known_hosts -o GlobalKnownHostsFile=/dev/null -o HostKeyAlgorithms=$HKALG -o IdentitiesOnly=yes -o BatchMode=yes -o PreferredAuthentications=publickey -o ForwardAgent=no -o ClearAllForwardings=yes -o ConnectTimeout=30 -o ServerAliveInterval=60" \
  -- "$SRC/" "$DEST" >"$J/out" 2>"$J/err" &
rpid=$!
wait "$rpid"; rc=$?
wait
finish "$rc"
`

// xferLaunchScript writes the runner, the helper and the pinned
// known_hosts into the job directory and starts the runner in its own
// session, detached from this SSH session. It waits up to 3 s for the
// "started" marker; without it the launch is unconfirmed, not failed.
const xferLaunchScript = `[ -d "$J" ] || die "job directory missing"
[ -e "$J/started" ] && { echo "LAUNCHED already"; exit 0; }
[ -f "$J/key" ] || die "job key missing"
printf '%s\n' "$KNOWN" >"$J/known_hosts" || die "cannot write known_hosts"
cat >"$J/cap.pl" <<'PFM_CAP_EOF'
` + xferCapScript + `PFM_CAP_EOF
{ printf '%s\n' "$RUNVARS"; cat <<'PFM_RUN_EOF'
` + xferRunScript + `PFM_RUN_EOF
} >"$J/run.sh" || die "cannot write run.sh"
setsid nohup sh "$J/run.sh" </dev/null >/dev/null 2>&1 &
i=0; while [ $i -lt 30 ] && [ ! -d "$J/started" ]; do sleep 0.1; i=$((i+1)); done
if [ -d "$J/started" ]; then echo "LAUNCHED"; else echo "UNCONFIRMED"; fi
`

// xferStatusScript reads a job's bounded files. Liveness is read before rc:
// the runner writes rc before it exits, so "not alive" followed by "no rc"
// means it ended without a result, never that rc was about to appear.
const xferStatusScript = `[ -d "$J" ] || { echo "NOJOB"; exit 0; }
[ -d "$J/started" ] && echo "STARTED"
[ -f "$J/pid" ] && echo "PIDFILE"
alive && echo "ALIVE"
for f in rc pid started_at skipped errors.count; do [ -f "$J/$f" ] && printf '%s=%s\n' "$f" "$(head -c 64 "$J/$f" | tr -d '\n')"; done
[ -f "$J/progress" ] && printf 'progress=%s\n' "$(head -c 512 "$J/progress" | tr -d '\r\n')"
[ -f "$J/errors" ] && { echo "--errors"; head -c 16384 "$J/errors"; }
exit 0
`

// xferSrcCleanupScript deletes the job directory once the runner is not
// running, and the transfers directory if that leaves it empty.
const xferSrcCleanupScript = `[ -d "$J" ] || { rmdir -- "$ctl/transfers" 2>/dev/null; echo "GONE"; exit 0; }
alive && die "the runner is still running"
rm -rf -- "$J" || die "cannot remove $J"
rmdir -- "$ctl/transfers" 2>/dev/null
echo "REMOVED"
exit 0
`

// xferDstRmdirScript removes the claimed destination directory (resolved
// from the typed path as the claim did) if it is still empty: a setup that
// failed after the claim. Never a non-empty one.
const xferDstRmdirScript = `p=$(abspath "$DST")
p=${p%/}
parent=$(realpath -e -- "$(dirname -- "$p")" 2>/dev/null) || { echo "GONE"; exit 0; }
d="$parent/$(basename -- "$p")"
inside "$d" "$home" || die "not inside home"
[ -d "$d" ] && [ ! -L "$d" ] || { echo "GONE"; exit 0; }
if rmdir -- "$d" 2>/dev/null; then echo "REMOVED"; else echo "KEPT not empty"; fi
`

// xferCancelScript stops a running runner: TERM to its process group (the
// runner leads its own session, see the launch), whose trap records
// "cancelled" and deletes the key.
const xferCancelScript = `[ -d "$J" ] || { echo "NOJOB"; exit 0; }
if alive; then kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" || die "could not signal the runner"; echo "SIGNALLED"; else echo "NOTRUNNING"; fi
`

// xferVars renders shell assignments for a job script's stdin.
func xferVars(kv ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		b.WriteString(kv[i] + "=" + shellQuote(kv[i+1]) + "\n")
	}
	return b.String()
}

// xferRefusal is a script's own "ERR ..." answer: the script ran and
// refused, so nothing after that point happened. Any other error (a lost
// connection, a timeout) leaves the outcome unknown.
type xferRefusal struct{ msg string }

func (e *xferRefusal) Error() string { return e.msg }

// xferRun runs one job script with a 2-minute bound and returns its stdout
// lines; an "ERR ..." line becomes the error.
func xferRun(ctx context.Context, cli *sshx.Client, vars, script string) ([]string, error) {
	rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	out, stderr, err := cli.Run(rctx, "sh", strings.NewReader(vars+xferPrelude+script))
	if len(out) > xferOutCap {
		return nil, fmt.Errorf("the box sent %d bytes, more than a job script writes", len(out))
	}
	lines := strings.Split(strings.TrimRight(string(out), "\n"), "\n")
	for _, l := range lines {
		if msg, ok := strings.CutPrefix(l, "ERR "); ok {
			return lines, &xferRefusal{msg}
		}
	}
	if err != nil {
		return lines, fmt.Errorf("%v %s", err, clip(strings.TrimSpace(string(stderr)), 300))
	}
	return lines, nil
}

func xferField(lines []string, key string) string {
	for _, l := range lines {
		if v, ok := strings.CutPrefix(l, key+" "); ok {
			return v
		}
	}
	return ""
}

// xferSrcPrepare checks the source and generates the job key on it,
// returning the resolved source path and the public key blob.
func xferSrcPrepare(ctx context.Context, cli *sshx.Client, token, src string) (resolved, blob string, err error) {
	src = strings.TrimPrefix(src, "~/")
	if err := validXferPath(src, false); err != nil {
		return "", "", fmt.Errorf("source: %w", err)
	}
	lines, err := xferRun(ctx, cli, xferVars("TOKEN", token, "SRC", src), xferSrcPrepareScript)
	if err != nil {
		return "", "", err
	}
	return parseSrcPrepare(lines)
}

// parseSrcPrepare reads the source's answer: its resolved path and the
// job key's blob.
func parseSrcPrepare(lines []string) (resolved, blob string, err error) {
	// Exactly one of each, well formed: the script refuses a resolved
	// path with a newline, and an answer that still has a second SRC or
	// PUB line is not one this script wrote (daybreak review).
	pub, ok := xferOnly(lines, "PUB")
	f := strings.Fields(pub)
	if !ok || len(f) != 3 || f[0] != "ssh-ed25519" || !xferBlobRe.MatchString(f[1]) {
		return "", "", errors.New("source returned no usable public key")
	}
	resolved, ok = xferOnly(lines, "SRC")
	if !ok || !strings.HasPrefix(resolved, "/") || strings.ContainsFunc(resolved, unicode.IsControl) || !utf8.ValidString(resolved) {
		return "", "", errors.New("source returned no usable resolved path")
	}
	return resolved, f[1], nil
}

// xferOnly is key's value when exactly one line carries it.
func xferOnly(lines []string, key string) (string, bool) {
	v, n := "", 0
	for _, l := range lines {
		if rest, ok := strings.CutPrefix(l, key+" "); ok {
			v, n = rest, n+1
		}
	}
	return v, n == 1
}

// xferDstPrepare claims the new destination directory and adds the job's
// restricted key line. The resolved directory comes back even on an
// error after the claim, so the caller can try to remove it.
func xferDstPrepare(ctx context.Context, cli *sshx.Client, token, dst, blob string) (dir string, err error) {
	if !xferBlobRe.MatchString(blob) {
		return "", errors.New("bad key")
	}
	dst = strings.TrimPrefix(dst, "~/")
	if err := validXferPath(dst, true); err != nil {
		return "", fmt.Errorf("destination: %w", err)
	}
	lines, err := xferRun(ctx, cli, xferVars("TOKEN", token, "DST", dst, "BLOB", blob), xferDstClaimScript+xferKeyAddScript)
	return xferField(lines, "DST"), err
}

// xferDest is where a runner copies to: the destination's account, its
// address, and its pinned host key ("type base64"), written as the only
// entry of the job's known_hosts.
type xferDest struct {
	User, Host string
	Port       int
	HostKey    string
}

// xferHostKeyAlgs is the HostKeyAlgorithms value that makes ssh negotiate
// the pinned key's type. An RSA key's type name, ssh-rsa, would select
// its SHA-1 signatures, which current servers refuse: RSA pins negotiate
// rsa-sha2-512 or rsa-sha2-256 with the same key (fred on PR 142).
func xferHostKeyAlgs(keyType string) string {
	if keyType == "ssh-rsa" {
		return "rsa-sha2-512,rsa-sha2-256"
	}
	return keyType
}

// xferLaunchVars renders the launch script's input: the job's known_hosts
// line (the pinned key, exactly) and the runner's assignments.
func xferLaunchVars(token, src string, d xferDest) (string, error) {
	kf := strings.Fields(d.HostKey)
	if len(kf) < 2 || !xferAlgRe.MatchString(kf[0]) {
		return "", errors.New("destination has no pinned host key")
	}
	host := strings.ToLower(d.Host)
	name := host
	if d.Port != 22 {
		name = "[" + host + "]:" + strconv.Itoa(d.Port)
	}
	run := xferVars("SRC", src, "DEST", d.User+"@"+host+":", "PORT", strconv.Itoa(d.Port), "HKALG", xferHostKeyAlgs(kf[0]))
	return xferVars("TOKEN", token, "KNOWN", name+" "+kf[0]+" "+kf[1], "RUNVARS", run), nil
}

// xferLaunch starts the runner on the source.
func xferLaunch(ctx context.Context, cli *sshx.Client, token, src string, d xferDest) (confirmed bool, err error) {
	vars, err := xferLaunchVars(token, src, d)
	if err != nil {
		return false, err
	}
	lines, err := xferRun(ctx, cli, vars, xferLaunchScript)
	if err != nil {
		return false, err
	}
	return len(lines) > 0 && strings.HasPrefix(lines[len(lines)-1], "LAUNCHED"), nil
}

// xferStatus returns the job's status lines, capped.
func xferStatus(ctx context.Context, cli *sshx.Client, token string) (string, error) {
	lines, err := xferRun(ctx, cli, xferVars("TOKEN", token), xferStatusScript)
	return strings.Join(lines, "\n"), err
}

func xferDstRemoveKey(ctx context.Context, cli *sshx.Client, token, blob string) (string, error) {
	lines, err := xferRun(ctx, cli, xferVars("TOKEN", token, "BLOB", blob), xferKeyRemoveScript)
	return strings.Join(lines, "; "), err
}

func xferSrcCleanup(ctx context.Context, cli *sshx.Client, token string) (string, error) {
	lines, err := xferRun(ctx, cli, xferVars("TOKEN", token), xferSrcCleanupScript)
	return strings.Join(lines, "; "), err
}

func xferDstRmdirEmpty(ctx context.Context, cli *sshx.Client, token, dst string) (string, error) {
	lines, err := xferRun(ctx, cli, xferVars("TOKEN", token, "DST", strings.TrimPrefix(dst, "~/")), xferDstRmdirScript)
	return strings.Join(lines, "; "), err
}

func xferCancel(ctx context.Context, cli *sshx.Client, token string) (string, error) {
	lines, err := xferRun(ctx, cli, xferVars("TOKEN", token), xferCancelScript)
	return strings.Join(lines, "; "), err
}
