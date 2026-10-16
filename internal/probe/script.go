package probe

// Script is the POSIX sh probe executed on the box as the tenant user. It
// prints "@@name" section headers followed by raw content. Every command is
// allowed to fail so one missing file never hides the rest.
const Script = `#!/bin/sh
LC_ALL=C; export LC_ALL
uid=$(id -u); user=$(id -un); home=${HOME:-$(eval echo ~$user)}
S="user.slice/user-$uid.slice"
# Each header starts with a newline: files like ~/.trafficLimit have no trailing newline,
# which would otherwise glue the next header onto the value.
sec(){ printf '\n@@%s\n' "$1"; }
sec uid; echo "$uid"
sec user; echo "$user"
sec home; echo "$home"
sec hostname; hostname -f 2>/dev/null || hostname
sec now; date +%s
sec uptime; cat /proc/uptime 2>/dev/null
sec loadavg; cat /proc/loadavg 2>/dev/null
sec meminfo; grep -E '^(MemTotal|MemAvailable):' /proc/meminfo 2>/dev/null
sec ncpu; grep -c '^processor' /proc/cpuinfo 2>/dev/null
sec cpu_model; grep -m1 -E '^(model name|Model|cpu model)' /proc/cpuinfo 2>/dev/null | cut -d: -f2-
sec cpu_flags; grep -m1 -E '^(flags|Features)' /proc/cpuinfo 2>/dev/null | cut -d: -f2- | tr ' ' '\n' | grep -E '^(avx2|avx512f|sse4_2|aes)$' | tr '\n' ' '
sec dri
for d in /dev/dri/renderD*; do
  [ -e "$d" ] || continue
  acc=noaccess; [ -r "$d" ] && [ -w "$d" ] && acc=rw
  n=$(basename "$d"); ven=$(cat /sys/class/drm/$n/device/vendor 2>/dev/null); dev=$(cat /sys/class/drm/$n/device/device 2>/dev/null)
  printf '%s %s %s %s\n' "$d" "$acc" "${ven:-?}" "${dev:-?}"
done
sec kernel; uname -r 2>/dev/null
sec debian; cat /etc/debian_version 2>/dev/null
sec pmss_version; cat /etc/seedbox/config/version 2>/dev/null
sec df_home; df -B1 --output=size,used,avail,fstype "$home" 2>/dev/null | tail -n 1
sec quota; quota -v -w 2>/dev/null
sec quota_file; cat "$home/.quota" 2>/dev/null
sec quota_mtime; stat -c %Y "$home/.quota" 2>/dev/null
sec traffic; cat "$home/.trafficData" 2>/dev/null
sec traffic_in; cat "$home/.trafficDataIngress" 2>/dev/null
sec traffic_limit; cat "$home/.trafficLimit" 2>/dev/null
sec bonus_traffic; cat "$home/.bonusTraffic" 2>/dev/null
sec throttle; [ -e "$home/.throttle" ] && echo 1 || echo 0
sec resource; cat "$home/.resourceData" 2>/dev/null
sec pmss_user; cat "$home/.config/pmss-user.json" 2>/dev/null
sec cgroup
if [ -f "/sys/fs/cgroup/memory/$S/memory.usage_in_bytes" ]; then
  echo "mem_usage=$(cat /sys/fs/cgroup/memory/$S/memory.usage_in_bytes)"
  echo "mem_limit=$(cat /sys/fs/cgroup/memory/$S/memory.limit_in_bytes)"
elif [ -f "/sys/fs/cgroup/$S/memory.current" ]; then
  echo "mem_usage=$(cat /sys/fs/cgroup/$S/memory.current)"
  echo "mem_limit=$(cat /sys/fs/cgroup/$S/memory.max)"
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
sec qbt_conf; grep -E '^WebUI\\(Port|Address|LocalHostAuth|AuthSubnetWhitelistEnabled|AuthSubnetWhitelist|HostHeaderValidation|ReverseProxySupportEnabled|TrustedReverseProxiesList|Username|MaxAuthenticationFailCount|BanDuration)=' "$home/.config/qBittorrent/qBittorrent.conf" 2>/dev/null
sec qbt_pid; pgrep -x qbittorrent-nox 2>/dev/null
sec qbt_exe; for pid in $(pgrep -x qbittorrent-nox 2>/dev/null); do printf '%s %s %s\n' "$pid" "$(readlink -f /proc/$pid/exe 2>/dev/null)" "$(tr '\0' ' ' < /proc/$pid/cmdline 2>/dev/null)"; done
sec qbt_bin_ls; ls -la --time-style=long-iso "$home/bin/qbittorrent-nox" 2>/dev/null; md5sum "$home/bin/qbittorrent-nox" 2>/dev/null
sec qbt_bin; ls "$home/bin/qbittorrent-nox" 2>/dev/null
sec qbt_bin_version; [ -x "$home/bin/qbittorrent-nox" ] && timeout 10 "$home/bin/qbittorrent-nox" --version 2>/dev/null | head -n 1
sec qbt_start_helper; [ -x "$home/bin/qbt-start" ] && echo 1 || echo 0
sec qbt_cron; crontab -l 2>/dev/null | grep -c 'bin/qbt-start' 
sec docker_sock; [ -S "/run/user/$uid/docker.sock" ] && echo 1 || echo 0
sec docker_pid; pgrep -x dockerd 2>/dev/null
sec end
`

// DuScript measures top-level directories in the home. It is heavy on big
// trees, so it runs rarely and at the lowest I/O priority.
const DuScript = `#!/bin/sh
LC_ALL=C; export LC_ALL
home=${HOME:-~}
cd "$home" || exit 1
run(){ if command -v ionice >/dev/null 2>&1; then ionice -c3 nice -n19 "$@"; else nice -n19 "$@"; fi; }
run du -xs -B1 -- * .[!.]* 2>/dev/null
`
