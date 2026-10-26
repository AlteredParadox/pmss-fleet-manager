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
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"pmss-fleet-manager/internal/rtorrent"
	"pmss-fleet-manager/internal/store"
)

// rTorrent's peer discovery (DHT and peer exchange; it has no local service
// discovery) is switched from the torrents tab like qBittorrent's. The
// setting is written to ~/.rtorrent.rc.custom, which PMSS's own
// ~/.rtorrent.rc imports last (`try_import = .rtorrent.rc.custom`), so it
// survives a restart, and then applied to the running client. What rTorrent
// reports afterwards is what the UI shows: a client started with DHT
// "disable" cannot turn it on until it is restarted, and the toast says so.

// RtDiscoveryKeys are the settings SetRtDiscovery may change.
var RtDiscoveryKeys = map[string]bool{rtorrent.DiscoveryDHT: true, rtorrent.DiscoveryPeX: true}

// RtDiscoveryResult is a SetRtDiscovery outcome: the file was saved; Live
// says the running client now has every value asked for, Note why not.
type RtDiscoveryResult struct {
	Live bool   `json:"live"`
	Note string `json:"note,omitempty"`
}

// rtConfLine is the line written for key. Values come from this closed
// set only: nothing the user or the client sends reaches the script.
func rtConfLine(key string, on bool) string {
	switch {
	case key == rtorrent.DiscoveryDHT && on:
		return "dht.mode.set = on"
	case key == rtorrent.DiscoveryDHT:
		return "dht.mode.set = off" // "off", not "disable": see rtorrent.Client.SetDiscovery
	case key == rtorrent.DiscoveryPeX && on:
		return "protocol.pex.set = yes"
	}
	return "protocol.pex.set = no"
}

// rtDiscoveryVars is the script's prologue: the DHT and PeX lines to
// write, empty for a setting left alone.
func rtDiscoveryVars(want map[string]bool) string {
	var d, p string
	if v, ok := want[rtorrent.DiscoveryDHT]; ok {
		d = rtConfLine(rtorrent.DiscoveryDHT, v)
	}
	if v, ok := want[rtorrent.DiscoveryPeX]; ok {
		p = rtConfLine(rtorrent.DiscoveryPeX, v)
	}
	return "d='" + d + "'\np='" + p + "'\n"
}

// rtDiscoveryScript rewrites ~/.rtorrent.rc.custom with the lines from the
// prologue. The first line already setting DHT (or PeX, either spelling) is
// replaced in place and any later ones dropped, so the file keeps a single
// line per setting; a setting not there yet is appended. Everything else is
// kept as it was. The new file is written next to the old one and renamed
// over it, keeping its mode; a symbolic link, a non-regular file, one this
// account cannot read and write, or one over 1 MiB is refused unchanged.
const rtDiscoveryScript = `LC_ALL=C; export LC_ALL
umask 077
f="$HOME/.rtorrent.rc.custom"
src=/dev/null
if [ -L "$f" ]; then echo "~/.rtorrent.rc.custom is a symbolic link; not editing it"; exit 3; fi
if [ -e "$f" ]; then
  [ -f "$f" ] || { echo "~/.rtorrent.rc.custom is not a regular file; not editing it"; exit 3; }
  { [ -r "$f" ] && [ -w "$f" ]; } || { echo "~/.rtorrent.rc.custom is not readable and writable by this account"; exit 3; }
  [ "$(wc -c < "$f")" -le 1048576 ] || { echo "~/.rtorrent.rc.custom is over 1 MiB; not editing it"; exit 3; }
  src="$f"
fi
tmp=$(mktemp "$f.XXXXXX") || { echo "cannot create a temporary file in the home"; exit 4; }
trap 'rm -f "$tmp"' EXIT
awk -v d="$d" -v p="$p" '
d != "" && /^[ \t]*(dht\.mode\.set|dht)[ \t]*=/ { if (!dd) print d; dd = 1; next }
p != "" && /^[ \t]*(protocol\.pex\.set|peer_exchange)[ \t]*=/ { if (!pd) print p; pd = 1; next }
{ print }
END { if (d != "" && !dd) print d; if (p != "" && !pd) print p }' "$src" > "$tmp" || { echo "could not write the new file"; exit 4; }
if [ "$src" = "$f" ]; then chmod "$(stat -c %a "$f")" "$tmp" || { echo "could not keep the file's mode"; exit 4; }; fi
mv -f "$tmp" "$f" || { echo "could not replace ~/.rtorrent.rc.custom"; exit 4; }
trap - EXIT
echo ok
`

// rtDiscWrite is one SetRtDiscovery write, kept so a poll that read the
// box before it cannot publish the old values (see overlayRtDiscovery).
type rtDiscWrite struct {
	conf      bool // the value written to the file
	live      bool // the client's value read back after the write
	liveKnown bool
	at        time.Time
}

// SetRtDiscovery writes DHT and/or peer exchange (keys dht, pex) to the
// box's ~/.rtorrent.rc.custom, applies them to the running client and
// reflects both in the cached state at once; the next poll re-reads them.
func (m *Manager) SetRtDiscovery(ctx context.Context, id int64, want map[string]bool) (RtDiscoveryResult, error) {
	for k := range want {
		if k == "lsd" {
			return RtDiscoveryResult{}, errors.New("rTorrent has no local service discovery")
		}
		if !RtDiscoveryKeys[k] {
			return RtDiscoveryResult{}, fmt.Errorf("unknown discovery setting %q", k)
		}
	}
	if len(want) == 0 {
		return RtDiscoveryResult{}, errors.New("nothing to change")
	}
	p := m.poller(id)
	if p == nil {
		return RtDiscoveryResult{}, store.ErrNotFound
	}
	if !p.box.Enabled {
		return RtDiscoveryResult{}, ErrDisabled
	}
	res, err := p.rtSetDiscovery(ctx, want)
	p.wake()
	return res, err
}

// rtSetDiscovery saves the settings on the box, then applies them live,
// reads back what the client has and publishes it, all under the per-box
// lease: two requests cannot publish out of order (fred on PR 126). A
// failure to save leaves the client alone; a failure to apply live is
// reported in the result, not as an error, since the file (what a restart
// uses) was written.
func (p *poller) rtSetDiscovery(ctx context.Context, want map[string]bool) (RtDiscoveryResult, error) {
	actx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	cli, sshc, _, err := p.rtReady(actx)
	if err != nil {
		return RtDiscoveryResult{}, err
	}
	run := sshRunner(sshc)
	if p.runConf != nil {
		run = p.runConf
	}
	if run == nil {
		return RtDiscoveryResult{}, ErrUnavailable
	}
	release, err := p.m.rtLease(p.box.ID).Acquire(actx)
	if err != nil {
		return RtDiscoveryResult{}, err
	}
	defer release()
	sctx, scancel := context.WithTimeout(actx, 20*time.Second)
	out, stderr, err := run.Run(sctx, "sh", strings.NewReader(rtDiscoveryVars(want)+rtDiscoveryScript))
	scancel()
	if err != nil {
		return RtDiscoveryResult{}, rtSaveError(out, stderr, err)
	}
	keys := make([]string, 0, len(want))
	for k := range want {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var liveErr error
	for _, k := range keys {
		if err := p.onRetired(actx, cli, func(c *rtorrent.Client) error { return c.SetDiscovery(actx, k, want[k]) }); err != nil {
			liveErr = err
			break
		}
	}
	var stats *rtorrent.Stats
	if err := p.onRetired(actx, cli, func(c *rtorrent.Client) error {
		var err error
		stats, err = c.Stats(actx)
		return err
	}); err != nil {
		stats = nil
		if liveErr == nil {
			liveErr = err
		}
	}
	if p.beforeRtPublish != nil {
		p.beforeRtPublish()
	}
	p.publishRtDiscovery(want, stats)
	return rtDiscoveryOutcome(want, stats, liveErr), nil
}

// rtSaveError words a failed save. Every message the script prints comes
// from a check before the rename (or the rename itself failing), so a
// script message means the file is unchanged. Without one (the connection
// dropped, the request ended) the rename may have happened all the same:
// that is said as it is, without promising anything about the file
// (fred on PR 126).
func rtSaveError(out, stderr []byte, err error) error {
	msg := strings.TrimSpace(string(out))
	if msg != "" && !strings.HasSuffix(msg, "ok") {
		return fmt.Errorf("could not save to ~/.rtorrent.rc.custom, nothing was changed: %s", clip(strings.TrimSpace(msg+" "+string(stderr)), 300))
	}
	return fmt.Errorf("the save to ~/.rtorrent.rc.custom could not be confirmed (%s); the running client was not changed; check the file on the box", clip(describeErr(err), 200))
}

// publishRtDiscovery puts a write into the cached state and the overlay
// that keeps an in-flight poll from undoing it, provided this poller is
// still the box's: a replacement (an edit) reads the box itself.
func (p *poller) publishRtDiscovery(want map[string]bool, stats *rtorrent.Stats) {
	p.m.commit(p.box.ID, func(s *BoxState) *BoxState {
		if s == nil || p.m.pollers[p.box.ID] != p { // m.mu is held here
			return nil
		}
		for k, w := range p.noteRtDiscovery(want, stats) {
			w.apply(k, &s.Rt)
		}
		return s
	}, commitOpts{})
}

// rtDiscoveryOutcome words what the running client has after a save.
func rtDiscoveryOutcome(want map[string]bool, stats *rtorrent.Stats, liveErr error) RtDiscoveryResult {
	if liveErr != nil || stats == nil {
		msg := "saved to ~/.rtorrent.rc.custom; rTorrent did not confirm the change live, so it applies when rTorrent restarts"
		if liveErr != nil {
			msg += " (" + clip(describeErr(liveErr), 200) + ")"
		}
		return RtDiscoveryResult{Note: msg}
	}
	var pending []string
	if v, ok := want[rtorrent.DiscoveryDHT]; ok && (!stats.DHTKnown || stats.DHT != v) {
		why := ""
		if v {
			why = " (a client started with DHT disabled cannot start it live)"
		}
		pending = append(pending, "DHT"+why)
	}
	if v, ok := want[rtorrent.DiscoveryPeX]; ok && (!stats.PeXKnown || stats.PeX != v) {
		pending = append(pending, "PeX")
	}
	if len(pending) > 0 {
		return RtDiscoveryResult{Note: "saved to ~/.rtorrent.rc.custom; " + strings.Join(pending, ", ") + " changes when rTorrent restarts"}
	}
	return RtDiscoveryResult{Live: true}
}

// noteRtDiscovery records a write (and what the client said after it) and
// returns it per key.
func (p *poller) noteRtDiscovery(want map[string]bool, stats *rtorrent.Stats) map[string]rtDiscWrite {
	p.actionMu.Lock()
	defer p.actionMu.Unlock()
	if p.rtDiscWant == nil {
		p.rtDiscWant = map[string]rtDiscWrite{}
	}
	now := time.Now()
	out := make(map[string]rtDiscWrite, len(want))
	for k, v := range want {
		w := rtDiscWrite{conf: v, at: now}
		if stats != nil {
			switch k {
			case rtorrent.DiscoveryDHT:
				w.live, w.liveKnown = stats.DHT, stats.DHTKnown
			case rtorrent.DiscoveryPeX:
				w.live, w.liveKnown = stats.PeX, stats.PeXKnown
			}
		}
		p.rtDiscWant[k] = w
		out[k] = w
	}
	return out
}

// overlayRtDiscovery applies the writes made after start on top of what a
// poll read, so the toggles never flip back for a poll.
func (p *poller) overlayRtDiscovery(start time.Time, r *RtState) {
	p.actionMu.Lock()
	defer p.actionMu.Unlock()
	if !r.Enabled {
		return
	}
	for k, w := range p.rtDiscWant {
		if w.at.After(start) {
			w.apply(k, r)
		}
	}
}

// apply puts one write into r: the file's value, and the client's when it
// was read back.
func (w rtDiscWrite) apply(k string, r *RtState) {
	conf := "off"
	if w.conf {
		conf = "on"
	}
	switch k {
	case rtorrent.DiscoveryDHT:
		r.ConfDHT = conf
		if w.liveKnown {
			r.DHT = w.live
		}
	case rtorrent.DiscoveryPeX:
		r.ConfPeX = conf
		if w.liveKnown {
			r.PeX, r.PeXKnown = w.live, true
		}
	}
}
