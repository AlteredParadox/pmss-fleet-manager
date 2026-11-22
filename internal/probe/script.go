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

package probe

import (
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"strings"
	"time"
)

// Script is the POSIX sh probe executed on the box as the tenant user. It
// prints "@@<tag>name" section headers followed by raw content. Every command
// is allowed to fail so one missing file never hides the rest.
//
// The tag is a per-run secret (see ScriptWithTag): sections quote content a
// co-tenant can influence, and without the tag anyone on the shared host
// could inject a header line and overwrite an earlier section. The tag is
// delivered on stdin with the script, never on the command line, because
// /proc/<pid>/cmdline is world-readable.
const Script = `#!/bin/sh
LC_ALL=C; export LC_ALL
uid=$(id -u); user=$(id -un); home=${HOME:-$(eval echo ~$user)}
S="user.slice/user-$uid.slice"
# Each header starts with a newline: files like ~/.trafficLimit have no trailing newline,
# which would otherwise glue the next header onto the value.
sec(){ printf '\n@@%s%s\n' "${PMSS_TAG:-}" "$1"; }
# bounded SECS CMD...: run CMD detached and print what it wrote within SECS.
# timeout is not enough for quota/df: a call stuck in the kernel on a busy
# or hung filesystem (uninterruptible sleep) cannot be killed, and timeout
# then waits for it, which stalled whole probes on one box. Here nothing
# waits on the child; it is abandoned after the deadline and the section is
# simply empty (the parser falls back to ~/.quota for quota).
scratch="${XDG_RUNTIME_DIR:-/run/user/$uid}"
[ -d "$scratch" ] && [ -w "$scratch" ] || scratch="$home"
# A probe killed at its deadline mid-bounded leaves its scratch file; sweep
# old ones (not a concurrent probe's) rather than letting them pile up.
find "$scratch" -maxdepth 1 -type f -name '.pmss-probe.*' -mmin +2 -delete 2>/dev/null
` + boundedShell + `sec uid; echo "$uid"
sec user; echo "$user"
sec hostname; timeout 5 hostname -f 2>/dev/null || hostname
sec now; date +%s
sec tz; date +%z
sec uptime; cat /proc/uptime 2>/dev/null
sec loadavg; cat /proc/loadavg 2>/dev/null
sec stat; head -1 /proc/stat 2>/dev/null
sec meminfo; grep -E '^(MemTotal|MemAvailable|SwapTotal):' /proc/meminfo 2>/dev/null
# Inventory: what changes rarely (host identity, the installed client, the
# crontab, the log catalogue). The poller asks for it on its first poll,
# every quarter hour, and on demand (PMSS_INVENTORY=0 skips every section
# between the markers; the parser reports whether they ran) and carries the
# last answer across the fast polls in between, so the 30 s cadence pays
# for metrics only.
if [ "${PMSS_INVENTORY:-1}" = 1 ]; then
sec inventory; echo 1
sec ncpu; grep -c '^processor' /proc/cpuinfo 2>/dev/null
sec cpu_model; grep -m1 -E '^(model name|Model|cpu model)' /proc/cpuinfo 2>/dev/null | cut -d: -f2-
# Nominal clock for the hardware catalogue (pmdb): the base frequency where
# the cpufreq driver exposes it. The current clock of an idle core is not a
# nominal figure, so there is no fallback here; the parser takes the GHz in
# the model name if there is one, else the clock is unknown.
sec cpu_mhz; awk '{printf "%d\n", $1/1000}' /sys/devices/system/cpu/cpu0/cpufreq/base_frequency 2>/dev/null
sec cpu_flags; grep -m1 -E '^(flags|Features)' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | tr ' ' '\n' | grep -E '^(avx2|avx512f|sse4_2|aes)$' | tr '\n' ' '
sec dri
for d in /dev/dri/renderD*; do
  [ -e "$d" ] || continue
  acc=noaccess; [ -r "$d" ] && [ -w "$d" ] && acc=rw
  n=$(basename "$d"); ven=$(cat /sys/class/drm/$n/device/vendor 2>/dev/null); dev=$(cat /sys/class/drm/$n/device/device 2>/dev/null)
  printf '%s %s %s %s\n' "$d" "$acc" "${ven:-?}" "${dev:-?}"
done
sec kernel; uname -r 2>/dev/null
# For the hardware catalogue (pmdb): whether the host is a KVM guest, its
# distribution and architecture, and the number of user accounts on it
# (pmdb's own count: UIDs above 1000 except nobody). systemd-detect-virt
# prints "none" and exits 1 on bare metal; stdout is what counts.
sec virt; timeout 5 systemd-detect-virt 2>/dev/null
sec os_release; grep -m1 '^PRETTY_NAME=' /etc/os-release 2>/dev/null
sec arch; uname -m 2>/dev/null
sec users; awk -F: '$3 > 1000 && $3 != 65534 {n++} END {print n+0}' /etc/passwd 2>/dev/null
sec debian; cat /etc/debian_version 2>/dev/null
sec pmss_version; cat /etc/seedbox/config/version 2>/dev/null
# For the hardware catalogue (pmdb) only, and only while that integration
# is on (PMSS_PMDB=1; the parser reports whether these ran, and the
# poller carries the last answer over a probe that skipped them): the
# clock and the storage measured the way yabs.sh measures them, so the
# figures agree with YABS uploads of the same host. The clock is the last
# "cpu MHz" line of /proc/cpuinfo (lscpu's maximum on ARM and RISC-V), a
# live reading, not a nominal one. The storage is every local filesystem
# in /proc/mounts of the types YABS counts, each source device once, in
# 1024-byte units, and df over the same types when that yields nothing.
# The total is printed as a float, not with yabs.sh's "%d": the mawk of
# Debian 11 and 12 (before its 20200731 release) keeps "%d" within 32
# bits, so a total above 2 TiB would wrap there.
if [ "${PMSS_PMDB:-0}" = 1 ]; then
sec pmdb_facts; echo 1
sec cpu_mhz_yabs; case "$(uname -m)" in aarch64*|arm*|riscv64*) lscpu 2>/dev/null | sed -n 's/^CPU max MHz: *//p' ;; *) awk -F: '/cpu MHz/ {freq=$2} END {print freq}' /proc/cpuinfo 2>/dev/null ;; esac
yabs_disk=$(cat <<'YABSDISK'
while read -r src mnt type _; do
  case " simfs ext2 ext3 ext4 btrfs xfs vfat exfat ntfs swap " in
    *" $type "*) echo "$src $(stat -f -c '%b %S' "$(printf '%b' "$mnt")" 2>/dev/null)" ;;
  esac
done </proc/mounts 2>/dev/null | awk 'NF == 3 && !seen[$1]++ { total += $2 * $3 / 1024 } END { if (total > 0) printf "%.0f\n", total }'
YABSDISK
)
sec disk_yabs; bounded 10 sh -c "$yabs_disk"
sec disk_yabs_df; bounded 10 df -t simfs -t ext2 -t ext3 -t ext4 -t btrfs -t xfs -t vfat -t exfat -t ntfs -t swap --total 2>/dev/null | awk '/^total/ {print $2}'
fi
fi
sec df_home; bounded 10 df -B1 --output=size,used,avail,fstype "$home" | tail -n 1
sec quota; bounded 10 quota -v -w
sec quota_file; cat "$home/.quota" 2>/dev/null
sec quota_mtime; stat -c %Y "$home/.quota" 2>/dev/null
sec traffic; head -c 262145 "$home/.trafficData" 2>/dev/null
sec traffic_in; head -c 262145 "$home/.trafficDataIngress" 2>/dev/null
sec traffic_limit; cat "$home/.trafficLimit" 2>/dev/null
sec bonus_traffic; cat "$home/.bonusTraffic" 2>/dev/null
sec throttle; [ -e "$home/.throttle" ] && echo 1 || echo 0
sec pmss_user; cat "$home/.config/pmss-user.json" 2>/dev/null
sec cgroup
# memory.usage/current charge page cache (every torrent byte read or
# written) to the slice, so they say nothing about pressure; the stat file
# splits it into process memory (rss/anon) and reclaimable cache, and the
# oom counters say whether the kernel actually ran out.
if [ -f "/sys/fs/cgroup/memory/$S/memory.usage_in_bytes" ]; then
  m=/sys/fs/cgroup/memory/$S
  echo "mem_usage=$(cat $m/memory.usage_in_bytes)"
  echo "mem_limit=$(cat $m/memory.limit_in_bytes)"
  awk '$1=="total_rss"{print "mem_rss="$2} $1=="total_cache"{print "mem_cache="$2}' $m/memory.stat 2>/dev/null
  awk '$1=="oom_kill"{print "oom_kill="$2}' $m/memory.oom_control 2>/dev/null
elif [ -f "/sys/fs/cgroup/$S/memory.current" ]; then
  m=/sys/fs/cgroup/$S
  echo "mem_usage=$(cat $m/memory.current)"
  echo "mem_limit=$(cat $m/memory.max)"
  awk '$1=="anon"{print "mem_rss="$2} $1=="file"{print "mem_cache="$2}' $m/memory.stat 2>/dev/null
  awk '$1=="oom_kill"{print "oom_kill="$2}' $m/memory.events 2>/dev/null
fi
for d in "/sys/fs/cgroup/cpu,cpuacct/$S" "/sys/fs/cgroup/cpuacct/$S"; do
  if [ -f "$d/cpuacct.usage" ]; then echo "cpu_usage_ns=$(cat $d/cpuacct.usage)"; break; fi
done
for d in "/sys/fs/cgroup/cpu,cpuacct/$S" "/sys/fs/cgroup/cpu/$S"; do
  if [ -f "$d/cpu.cfs_quota_us" ]; then
    echo "cpu_quota_us=$(cat $d/cpu.cfs_quota_us)"; echo "cpu_period_us=$(cat $d/cpu.cfs_period_us)"; break
  fi
done
if [ -f "/sys/fs/cgroup/$S/cpu.stat" ]; then
  us=$(awk '/^usage_usec/{print $2}' /sys/fs/cgroup/$S/cpu.stat); [ -n "$us" ] && echo "cpu_usage_ns=$((us*1000))"
  [ -f "/sys/fs/cgroup/$S/cpu.max" ] && echo "cpu_max=$(cat /sys/fs/cgroup/$S/cpu.max)"
fi
if [ -f "/sys/fs/cgroup/pids/$S/pids.current" ]; then echo "pids=$(cat /sys/fs/cgroup/pids/$S/pids.current)"
elif [ -f "/sys/fs/cgroup/$S/pids.current" ]; then echo "pids=$(cat /sys/fs/cgroup/$S/pids.current)"; fi
sec qbt_conf; ` + qbtConfShell + `sec qbt_pid; pgrep -u "$uid" -x qbittorrent-nox 2>/dev/null
sec home; printf '%s\n' "$home"
sec rt_conf; ` + rtConfShell + `sec rt_disable; [ -e "$home/.rtorrentDisable" ] && echo 1 || echo 0
sec listen
` + listenShell + `sec docker_sock; [ -S "/run/user/$uid/docker.sock" ] && echo 1 || echo 0
# Every poll, not inventory: the panel can switch its own qBittorrent on
# at any time, and install/autostart refuse to fight it only if they know.
sec qbt_pmss; [ -e "$home/.qbittorrentEnable" ] && echo 1 || echo 0
if [ "${PMSS_INVENTORY:-1}" = 1 ]; then
sec qbt_bin; ls "$home/bin/qbittorrent-nox" 2>/dev/null
sec qbt_bin_stat; stat -L -c '%s %Y %Z %i' "$home/bin/qbittorrent-nox" 2>/dev/null
sec qbt_bin_version
` + qbtVersionShell + `sec qbt_start_helper; [ -x "$home/bin/qbt-start" ] && echo 1 || echo 0
sec qbt_start_helper_stale; grep -Eq ` + QbtStaleHelperPattern + ` "$home/bin/qbt-start" 2>/dev/null && echo 1 || echo 0
sec qbt_cron; timeout 10 crontab -l 2>/dev/null | grep -Ec ` + QbtCronPattern + `
sec logs
` + logsListShell + `# PMSS's live-stats rings (its ADR 0126): node sums for everyone, this
# account's own samples for its owner. About 40 KB each; a forged file is cut.
sec live_node; head -c 262144 /var/lib/pmss/public/node-live.json 2>/dev/null
sec live_user; head -c 262144 "/var/lib/pmss/live/$user.json" 2>/dev/null
fi
sec probe_pid; echo $$
# The account's own processes (hidepid=2 hides everyone else's anyway):
# fixed numeric columns first, then comm, which may hold spaces. times is
# cumulative CPU seconds; the poller turns two samples into a rate. The
# command lines come separately, streamed through cut so no shell variable
# ever holds the full argv of every process, and end with a marker line:
# without it the listing was cut short and the parser ignores it.
sec procs
` + procsShell + `sec proc_args
` + procArgsShell + `# The ps calls carry a timeout: reading a cmdline takes the target's mmap
# lock, and a process stuck in D state on a dead mount would otherwise hold
# the whole probe past its deadline (a cut section degrades to "unknown").
# CPU in clock ticks (10 ms) and the start tick from /proc/<pid>/stat: ps
# rounds CPU to whole seconds, useless over a 30 s poll, and the start tick
# tells a reused pid from the same process. comm sits in parentheses and
# may hold spaces or parentheses, so the fields are counted from the last ')'.
sec proc_stat; getconf CLK_TCK 2>/dev/null
timeout 10 ps -u "$uid" -o pid= 2>/dev/null | awk '{print "/proc/"$1"/stat"}' | timeout 10 xargs cat 2>/dev/null | awk '{i=match($0,/\)[^)]*$/); if (i==0) next; n=split(substr($0,i+2),f," "); if (n<20) next; print $1, f[12], f[13], f[20]}'
sec listen_pids
# Which of our own processes hold which listening port: ss resolves pids
# only for sockets whose /proc/<pid>/fd it can read, i.e. our own, so a
# co-tenant's listener never appears here. Display only (never a gate).
` + listenPIDsShell + `sec end
`

// QbtStaleHelperPattern is the grep -E pattern, as one single-quoted sh
// word, for a qbt-start helper whose running check keys on $USER, quoted,
// braced or bare: the whole variable name, followed by a quote, a non-name
// character or the end of the line, so a user's own "$USER_ID" does not
// match. $USER is empty under cron, so such a helper's pgrep fails with a
// usage error and the helper starts another copy on every run; autostart
// rewrites it (internal/collect/qbtinstall.go) and the probe flags it.
const QbtStaleHelperPattern = `'pgrep -u "?([$]USER|[$][{]USER[}])("|[^"A-Za-z0-9_]|$)'`

// QbtCronPattern is the grep -E pattern, as one double-quoted sh word, for a
// crontab line whose command is nothing but the qbt-start helper: a schedule
// (five fields or an @keyword), optionally "sleep N;", then the helper's
// path ($HOME/bin, ~/bin or an absolute .../bin, optionally quoted),
// optionally redirections such as ">/dev/null 2>&1", and the end of the
// line. Those are the two shapes autostart writes plus the hand-written
// quiet variant. A line that runs anything else as well, such as
// "backup.sh && qbt-start", is the user's own and does not match, so
// autostart keeps it and the probe does not count it. Both use this
// pattern; run it under LC_ALL=C.
const QbtCronPattern = `"^[[:space:]]*(@[a-z]+|[^#@[:space:]]+([[:space:]]+[^[:space:]]+){4})[[:space:]]+(sleep[[:space:]]+[0-9]+[[:space:]]*;[[:space:]]*)?[\"']?([\$]HOME|[\$][{]HOME[}]|~|/[^[:space:]\"';&|]*)/bin/qbt-start[\"']?([[:space:]]*[0-9]*[<>]{1,2}&?[[:space:]]*[^[:space:];&|<>]*)*[[:space:]]*\$"`

// boundedShell is the probe's bounded() helper. The command runs under a
// per-command lock (flock) in the tenant's scratch dir: a run from an
// earlier probe that is stuck in the kernel (a hung filesystem under df or
// quota cannot act on SIGKILL) still holds the lock, so this probe skips
// the command, and the section is empty, instead of adding another
// unkillable process to the slice every poll. The lock is the exec'd
// command's own fd, so it is released exactly when that returns. Without
// flock the command is skipped altogether.
const boundedShell = `bounded(){
  secs=$1; shift
  # The scratch file lives in the tenant's own runtime dir or home, never
  # in shared /tmp where a co-tenant could plant a guessable name. The
  # runtime dir is tmpfs and outside the home quota, so the live quota
  # reading survives the box being full.
  f=$(mktemp "$scratch/.pmss-probe.XXXXXX" 2>/dev/null) || return 0
  lock="$scratch/.pmss-probe-lock.$(basename "$1")"
  # No flock (util-linux is Essential on Debian, so this is not a PMSS
  # host): nothing can tell a stuck earlier run from none, so the reading
  # is skipped rather than risk a stuck process per poll.
  command -v flock >/dev/null 2>&1 || { rm -f "$f"; return 0; }
  ( exec 9>"$lock" && flock -n 9 && exec "$@" >"$f" 2>/dev/null ) & bp=$!
  i=0; while [ $i -lt $((secs*5)) ] && kill -0 $bp 2>/dev/null; do sleep 0.2; i=$((i+1)); done
  kill -9 $bp 2>/dev/null
  cat "$f" 2>/dev/null; rm -f "$f"
}
`

// ListenScript prints the account's uid and the listening sockets in the
// same form as the probe's listen section, for a live ownership re-check
// right before an action that sends credentials.
const ListenScript = `#!/bin/sh
LC_ALL=C; export LC_ALL
uid=$(id -u)
echo "uid $uid"
` + listenShell + `echo end
`

// EstablishedScript identifies the dashboard's own tunnel connections to
// 127.0.0.1:<port> on the box and who accepted each. sshd opens the tunnel
// as this account, so the client side of every such connection is a socket
// owned by this uid whose peer is 127.0.0.1:<port>; its local (ephemeral)
// port names the accepting socket on the other side, whose owner is the
// process that will read whatever the dashboard writes. The parser pairs
// them (EstablishedOwner). Every TCP state is reported: a hostile
// acceptor can half-close (shutdown SHUT_WR) and still read, so filtering
// on ESTABLISHED would miss it; a missing accepting row refuses too.
//
// Lines: "base hex", then "c <localport> <uid> <state> <peeraddr>" for
// client sides and "s <peerport> <uid> <state> <localaddr> <inode> <timer>"
// for accepting sides, from /proc/net/tcp{,6}.
//
// "privacy" marks a box whose kernel tables this account may not read
// (Pulsed Media's socket-table privacy, ADR 0068): the caller falls back
// to the listener check there. Recognised by the policy's opt-in marker
// file or by the tables being present but denied to us (the PMSS update
// of 2026-10-22 made /etc/seedbox root-only on some hosts, hiding the
// marker). There is no ss fallback for this check: our tunnel's client
// socket belongs to the box's sshd session process, which ss -p cannot
// attribute to a tenant (sshd makes itself non-dumpable), so it lists as
// nobody's and the check could never pass. That is the state of a host
// where the policy's first stage (the tables) applies and its sock_diag
// filter did not load (PMSS #1303): ss still lists every tenant's
// sockets, and every connection was refused. The listener check, which
// ss can serve (it attributes the listener to our qBittorrent), is what
// such a host gets until PMSS offers tenants a view of their own sockets.
// Any other failure (tables absent, or readable but empty) prints
// "failed" and the caller refuses: a gate that cannot run must not pass.
func EstablishedScript(port int) string {
	return "#!/bin/sh\nLC_ALL=C; export LC_ALL\nport=" + strconv.Itoa(port) + "\n" +
		`uid=$(id -u)
echo "uid $uid"
hp=$(printf '%04X' "$port")
tables="${PMSS_TCP_TABLES:-/proc/net/tcp /proc/net/tcp6}"
marker="${PMSS_PRIVACY_MARKER:-/etc/seedbox/config/socket-table-privacy.enabled}"
rows=$(awk -v hp="$hp" '$4!="0A"{n++; l=$2; r=$3; lp=l; sub(/.*:/,"",lp); rp=r; sub(/.*:/,"",rp); la=l; sub(/:.*/,"",la); ra=r; sub(/:.*/,"",ra); if (rp==hp) print "c", lp, $8, $4, ra; if (lp==hp) {tt=$6; sub(/:.*/,"",tt); print "s", rp, $8, $4, la, $10, tt}} END{print "seen", n+0}' $tables 2>/dev/null)
if [ -n "$rows" ] && [ "${rows##*seen }" != "0" ]; then
  echo "base hex"; printf '%s\n' "$rows"
else
  first=${tables%% *}
  if [ -e "$marker" ] || { [ -e "$first" ] && ! [ -r "$first" ]; }; then
    echo "privacy"
    ` + listenIdentShell + `  elif [ -e "$first" ]; then
    echo "failed table-empty"
  else
    echo "failed no-table"
  fi
fi
echo end
`
}

// listenShell lists the listening TCP sockets a 127.0.0.1 dial could reach,
// one per line, for parseListeners. It needs $uid set.
//
// Preferred source: /proc/net/tcp{,6}, as "h <hexport> <uid> <hexaddr>",
// filtered on the box so the output stays small (the whole table is every
// tenant's torrent connection). Kernel-formatted, no attacker-controlled
// text, and it names the owning uid of every socket.
//
// Fallback: PMSS's opt-in socket-table privacy (its ADR 0068) makes those
// tables root-only. Its documented tenant workflow, ss -tlnp, keeps working
// (the stage-2 netlink filter lets LISTEN dumps through), so the listing
// comes from ss -e as "d <port> <uid|x> <addr>" (see ssListenAwk): the
// owner uid the kernel reports for the socket. Not ss -p: it matches
// sockets to processes by inode number, a wrapping 32-bit counter, so a
// listener whose inode number equals one of our sockets' was labelled
// ours.
const listenShell = `l=$(awk '$4=="0A"{x=$2; a=x; sub(/.*:/,"",x); sub(/:.*/,"",a); print "h", x, $8, a}' /proc/net/tcp /proc/net/tcp6 2>/dev/null)
if [ -n "$l" ]; then printf '%s\n' "$l"; else echo "via ss"; ` + listenViaSS + `; fi
`

// listenViaSS is the ss branch of listenShell on its own, so a test can run
// it against the real ss: with -H there is no header, and the columns are
// State Recv-Q Send-Q Local Peer Process. The address is everything before
// the last colon of Local (a %interface suffix dropped); a Process column
// that starts with users:(( is one of our own sockets.
const listenViaSS = `timeout 10 ss -Hltne 2>/dev/null | awk '` + ssListenAwk + ` print "d", p, u, ad}'`

// ssListenAwk reads one ss -Hltne row into p (port), ad (address), u (the
// owner uid, or x) and k (the socket cookie: hex, - when ss printed none,
// ? when what it printed is not a nonzero 64-bit value), for the caller's
// print. Positional, from the kernel-formatted columns only: State Recv-Q
// Send-Q Local Peer, then uid:N (absent for uid 0, as ss leaves it out),
// ino:N, sk:HEX. Everything after (cgroup:<path>, a directory name the
// socket's owner chooses, so text that can read "uid:<ours>") is never
// looked at; a row that does not have that shape is x, which refuses.
// Each line is one kernel record: the only free text ss -e (without -p)
// prints is that cgroup path and an interface scope, and the kernel
// rejects '\n' in both (cgroup_mkdir and cgroup1_rename in
// kernel/cgroup/, "to prevent making /proc/<pid>/cgroup unparsable";
// cgroup v2 forbids renames; dev_valid_name refuses whitespace).
const ssListenAwk = `function ck(f) { if (f !~ /^sk:/) return "-"; f=substr(f,4); return (f ~ /^[0-9a-f]+$/ && length(f) <= 16 && f !~ /^0+$/) ? f : "?" }
$1=="LISTEN"{n=split($4,a,":"); p=a[n]; sc=substr($4,1,length($4)-length(p)-1); ad=sc; sub(/%.*/,"",ad); u="x"; k="-"; i=0
  if ($6 ~ /^uid:[0-9]+$/ && $7 ~ /^ino:[0-9]+$/) { u=substr($6,5); k=ck($8); i=(k=="-") ? 8 : 9 }
  else if ($6 ~ /^ino:[0-9]+$/) { u=0; k=ck($7); i=(k=="-") ? 7 : 8 }
  nl=(i > 0 && $i ~ /^cgroup:/) ? 1 : 0;`

// listenIdentShell prints the listening sockets on $port as ss reports them
// to this account, "l <port> <uid|x> <cookie|-|?> <addr[%scope]> <nl>"
// each (nl: 1 when the row carries the cgroup field right after the kernel
// fields, which only ss's netlink (inet-diag) path prints, never its /proc
// fallback; the field's text, the owner's to choose, is never read),
// "lrc <rc>" for ss's exit status, and "ltab denied" when /proc/net/tcp
// and tcp6 are both unreadable to this account, before and after ss ran.
// The variables that point ss at other inputs are unset. What
// ListenerIdentity reads. It needs $port.
const listenIdentShell = `unset PROC_NET_TCP PROC_ROOT TCPDIAG_FILE
lr=denied; for lt in ${PMSS_TCP_TABLES:-/proc/net/tcp /proc/net/tcp6}; do [ -r "$lt" ] && lr=readable; done
lo=$(timeout 10 ss -Hltne "sport = :$port" 2>/dev/null); lrc=$?
for lt in ${PMSS_TCP_TABLES:-/proc/net/tcp /proc/net/tcp6}; do [ -r "$lt" ] && lr=readable; done; echo "ltab $lr"
echo "lrc $lrc"
[ -n "$lo" ] && printf '%s\n' "$lo" | awk '` + ssListenAwk + ` print "l", p, u, k, sc, nl}'
`

// ListenerScript reports the listening socket on 127.0.0.1:<port> for the
// listener-continuity check (ListenerIdentity): run just before a tunnel
// connection is opened, and again (inside EstablishedScript) after it.
func ListenerScript(port int) string {
	return "#!/bin/sh\nLC_ALL=C; export LC_ALL\nport=" + strconv.Itoa(port) + "\n" + listenIdentShell + "echo end\n"
}

// logDirsShell and logFilesShell are the places under the home where the
// PMSS stack and the apps customers install on it keep their logs, as shell
// words spliced into the probe: directories listed for *.log, *.log.* and
// *.txt files, and single files.
const (
	logDirsShell = ".logs .local/share/qBittorrent/logs .local/share/data/qBittorrent/logs .lighttpd" +
		" .config/Radarr/logs .config/Sonarr/logs .config/Lidarr/logs .config/Readarr/logs .config/Prowlarr/logs" +
		" .config/Bazarr/log .config/jellyfin/log .config/sabnzbd/logs .sabnzbd/logs .config/deluge" +
		" .config/rclone .flexget .pyload/logs .config/pyload/logs .config/cloudplow .config/Jackett/log logs"
	logFilesShell = ".trackerCleaner.log .rtorrent.log rtorrent.log .rtorrent.session/rtorrent.log"
)

// qbtConfShell prints the WebUI settings parseQbt reads, one `WebUI\Key=value`
// line each from both layouts: qBittorrent below 5.3 keeps them as
// `WebUI\Key` under [Preferences] (printed as is); 5.3 moves them to bare
// keys under [WebUI] at migration 11 (printed as `[WebUI]Key=value`), plus
// [Meta]'s MigrationVersion (`Meta\MigrationVersion=n`). PMSS now keeps
// both sections filled, so parseQbt chooses one (QbtInfo.ApplyConf) instead
// of taking the last line. Only the listed keys are printed: the password
// hash stays on the box.
const qbtConfShell = `awk '/^\[/{s=$0; sub(/[ \r]+$/,"",s); next}
s=="[Preferences]" && /^WebUI\\(Port|LocalHostAuth|AuthSubnetWhitelistEnabled|AuthSubnetWhitelist|Username)=/{print; next}
s=="[WebUI]" && /^(Port|LocalHostAuth|AuthSubnetWhitelistEnabled|AuthSubnetWhitelist|Username)=/{print "[WebUI]" $0; next}
s=="[Meta]" && /^MigrationVersion=/{print "Meta\\" $0}' "$home/.config/qBittorrent/qBittorrent.conf" 2>/dev/null
`

// rtConfShell prints the SCGI endpoint lines from the tenant's rTorrent
// configuration: PMSS renders ~/.rtorrent.rc (root-owned, world-readable)
// with `network.scgi.open_local = ~/.rtorrent.socket`, and the tenant's own
// ~/.rtorrent.rc.custom may override it (later lines win, as in rTorrent).
// Only the endpoint, download-directory and peer-discovery directives are
// printed, old and new spelling. PMSS's file ends with `try_import =
// .rtorrent.rc.custom`, so the custom file's lines are the later ones.
const rtConfShell = `grep -hE '^[[:space:]]*(network\.scgi\.open_local|network\.scgi\.open_port|scgi_local|scgi_port|directory\.default\.set|directory|dht\.mode\.set|dht|protocol\.pex\.set|peer_exchange)[[:space:]]*=' "$home/.rtorrent.rc" "$home/.rtorrent.rc.custom" 2>/dev/null | cut -c1-300 | head -n 60
`

// logsListShell lists log files in the places the PMSS stack and the usual
// self-installed apps write them, as "mtime<TAB>size<TAB>path" relative to
// the home ($home must be set): the 40 newest per directory, sorted before
// the cut so a directory full of rotated copies never hides the live file.
// Only a listing: the tab fetches one file on demand (see LogTailScript).
const logsListShell = `(cd "$home" 2>/dev/null && for d in ` + logDirsShell + `; do
  [ -d "$d" ] || continue
  find "$d" -maxdepth 1 -type f \( -name '*.log' -o -name '*.log.*' -o -name '*.txt' \) ! -name '*.xz' ! -name '*.bz2' ! -name '*.zst' ! -name '*.zip' -printf '%T@\t%s\t%p\n' 2>/dev/null | sort -rn | head -n 40
done
for f in ` + logFilesShell + `; do
  [ -f "$f" ] && stat -c '%Y'"$(printf '\t')"'%s'"$(printf '\t')"'%n' "$f" 2>/dev/null
done; :)
`

// LogTailScript prints the last lines of one log file under the home. The
// path (already ValidLogPath-checked) is embedded single-quoted, so it never
// reaches a command line that /proc exposes, and is checked again on the
// box: relative, no ".." component, a regular file. A byte cap comes
// BEFORE the line count in both branches: `tail -n` on a pipe holds the
// whole current line until a newline arrives, so a newline-free stream (a
// gzip bomb, a binary that got rotated) would grow it without bound
// (measured 2.9 GB on a 3 GB blob); `tail -c` keeps a fixed buffer. The
// plain branch reads the last 4 MiB of the file, which bounds a multi-GB
// newline-free "log" too; 300 lines of any real log fit in that.
func LogTailScript(path string, lines int) string {
	q := "'" + strings.ReplaceAll(path, "'", `'\''`) + "'"
	n := strconv.Itoa(lines)
	return "#!/bin/sh\nLC_ALL=C; export LC_ALL\np=" + q + "\n" +
		`cd "${HOME:-~}" || exit 3
case "$p" in ''|/*|..|../*|*/../*|*/..) echo "refused path" >&2; exit 4;; esac
[ -f "$p" ] || { echo "not a regular file" >&2; exit 5; }
case "$p" in
  *.gz) gzip -dc -- "$p" 2>/dev/null | tail -c 1048576 | tail -n ` + n + ` | tail -c 262144;;
  *) tail -c 4194304 -- "$p" 2>/dev/null | tail -n ` + n + ` | tail -c 262144;;
esac
`
}

// procsShell lists the account's processes, but only when ps finished:
// a listing cut by the timeout would pass as complete (the parser calls
// it known as soon as one row parses) and every watched app missing from
// the cut tail would be reported as not running.
const procsShell = `o=$(timeout 10 ps -ww -u "$uid" -o pid=,ppid=,rss=,times=,etimes=,comm= 2>/dev/null) && [ -n "$o" ] && printf '%s\n' "$o"
`

// procArgsShell is the same for the command lines: a cut listing would
// leave the processes past the cut without args, and an interpreter is
// classified by its script.
const procArgsShell = `(timeout 10 ps -ww -u "$uid" -o pid=,args= 2>/dev/null && echo end) | cut -c1-400
`

// listenPIDsShell prints "<port> <pid>" for every listening TCP socket ss
// can attribute to one of our processes (its users:((...,pid=N,...)) column).
const listenPIDsShell = `timeout 10 ss -Hltnp 2>/dev/null | awk '$1=="LISTEN"{n=split($4,a,":"); p=a[n]; s=$0; while (match(s,/pid=[0-9]+/)) { print p, substr(s,RSTART+4,RLENGTH-4); s=substr(s,RSTART+RLENGTH) } }'
`

// qbtVersionShell asks the installed qbittorrent-nox for its version, but
// only when the binary's identity (size, mtime, ctime, inode) differs from
// PMSS_QBT_SEEN, the stat the poller last got an answer for. The ctime and
// inode are there for a replacement that preserves size and mtime (a
// timestamp-preserving copy): nothing a tenant runs can set a ctime, and
// an atomic swap changes the inode. -L describes the executable behind a
// symlink, not the link, so a target replaced under an unchanged link
// counts as a change too. --version initialises the
// application: it appends a "started … termination initiated" block to the
// user's qBittorrent log on every call and rotates the log once that fills
// up, so polling it every 30 s drowned the real log in probe noise.
const qbtVersionShell = `if [ -x "$home/bin/qbittorrent-nox" ] && [ "$(stat -L -c '%s %Y %Z %i' "$home/bin/qbittorrent-nox" 2>/dev/null)" != "${PMSS_QBT_SEEN:-}" ]; then
  timeout 10 "$home/bin/qbittorrent-nox" --version </dev/null 2>/dev/null | head -c 256 | head -n 1
fi
`

// ScriptWithTag returns Script with the section tag set on its first line.
// The tag must be [A-Za-z0-9]+ (NewTag guarantees that); it is quoted anyway.
// The version call runs unconditionally (no PMSS_QBT_SEEN), and the
// catalogue facts are not asked for (the poller carries them).
func ScriptWithTag(tag string) string { return ScriptWith(tag, "", true, false) }

// ScriptWith is ScriptWithTag with the binary stat ("size mtime ctime
// inode", as the qbt_bin_stat section printed it) the caller already holds
// a version for, so the probe skips the --version call while the binary is
// unchanged, and with the inventory sections (see Script) switched on or
// off: off, the probe measures only, and Snapshot.Inventory is false.
// catalogue asks for the two facts the hardware catalogue takes measured
// YABS's way (PMSS_PMDB=1); they are only read while that integration is on.
func ScriptWith(tag, qbtSeen string, inventory, catalogue bool) string {
	seen := strings.Map(func(r rune) rune {
		if (r >= '0' && r <= '9') || r == ' ' {
			return r
		}
		return -1
	}, qbtSeen)
	inv, cat := "0", "0"
	if inventory {
		inv = "1"
	}
	if catalogue {
		cat = "1"
	}
	return "PMSS_TAG='" + strings.ReplaceAll(tag, "'", "") + "'\nPMSS_QBT_SEEN='" + seen + "'\nPMSS_INVENTORY=" + inv + "\nPMSS_PMDB=" + cat + "\n" + Script
}

// NewTag returns a random hex section tag for one probe run.
func NewTag() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(b[:])
}

// DuScript measures top-level directories in the home. It is heavy on big
// trees, so it runs rarely and at the lowest I/O priority.
const DuScript = `#!/bin/sh
LC_ALL=C; export LC_ALL
home=${HOME:-~}
cd "$home" || exit 1
run(){ if command -v ionice >/dev/null 2>&1; then ionice -c3 nice -n19 "$@"; else nice -n19 "$@"; fi; }
run du -xs -B1 -- * .[!.]* 2>/dev/null
`
