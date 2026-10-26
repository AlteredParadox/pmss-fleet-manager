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
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
)

// runConfScript runs the real script with HOME=home and the given settings.
func runConfScript(t *testing.T, home string, want map[string]bool) (string, error) {
	t.Helper()
	c := exec.Command("sh")
	c.Env = append(os.Environ(), "HOME="+home)
	c.Stdin = strings.NewReader(rtDiscoveryVars(want) + rtDiscoveryScript)
	out, err := c.CombinedOutput()
	return string(out), err
}

// The script edits ~/.rtorrent.rc.custom in place: the first line setting
// DHT or PeX (either spelling) is replaced, later ones dropped, anything
// else kept, a missing setting appended, the file's mode kept; links,
// non-regular and unwritable files are refused unchanged.
func TestRtDiscoveryScript(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home := t.TempDir()
	f := home + "/.rtorrent.rc.custom"
	// No file yet: created with the lines, private.
	if out, err := runConfScript(t, home, map[string]bool{"dht": true}); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if b, _ := os.ReadFile(f); string(b) != "dht.mode.set = on\n" {
		t.Fatalf("created: %q", b)
	}
	if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode())
	}
	// pm1's file (2026-10-26) with a duplicate and an old spelling added.
	pm1 := "# You can insert here custom rtorrent configurations\n\ndht.mode.set = disable\nprotocol.pex.set = no\n\nschedule2 = dht_node1, 1, 3600, \"dht.add_node=dht.libtorrent.org:25401\"\n  dht = auto\npeer_exchange = yes\nlog.open_file = \"rtorrent.log\", (cat,~/.logs/rtorrent.log.,(system.time))"
	os.WriteFile(f, []byte(pm1), 0o640)
	os.Chmod(f, 0o640)
	if out, err := runConfScript(t, home, map[string]bool{"dht": false, "pex": true}); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	want := "# You can insert here custom rtorrent configurations\n\ndht.mode.set = off\nprotocol.pex.set = yes\n\nschedule2 = dht_node1, 1, 3600, \"dht.add_node=dht.libtorrent.org:25401\"\nlog.open_file = \"rtorrent.log\", (cat,~/.logs/rtorrent.log.,(system.time))\n"
	if b, _ := os.ReadFile(f); string(b) != want {
		t.Fatalf("edited:\n%s\nwant:\n%s", b, want)
	}
	if fi, _ := os.Stat(f); fi.Mode().Perm() != 0o640 {
		t.Fatalf("mode not kept: %v", fi.Mode())
	}
	// One setting leaves the other's line alone; no temporary file is left.
	if out, err := runConfScript(t, home, map[string]bool{"pex": false}); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	if b, _ := os.ReadFile(f); !strings.Contains(string(b), "dht.mode.set = off\nprotocol.pex.set = no\n") {
		t.Fatalf("pex only: %q", b)
	}
	if ents, _ := os.ReadDir(home); len(ents) != 1 {
		t.Fatalf("left behind: %v", ents)
	}
	// A symbolic link is refused and its target untouched.
	other := home + "/elsewhere"
	os.WriteFile(other, []byte("keep\n"), 0o600)
	os.Remove(f)
	os.Symlink(other, f)
	if out, err := runConfScript(t, home, map[string]bool{"dht": true}); err == nil || !strings.Contains(out, "symbolic link") {
		t.Fatalf("link: %v %s", err, out)
	}
	if b, _ := os.ReadFile(other); string(b) != "keep\n" {
		t.Fatalf("link target changed: %q", b)
	}
	os.Remove(f)
	// A directory is refused.
	os.Mkdir(f, 0o700)
	if out, err := runConfScript(t, home, map[string]bool{"dht": true}); err == nil || !strings.Contains(out, "not a regular file") {
		t.Fatalf("dir: %v %s", err, out)
	}
	os.Remove(f)
	// A file this account cannot write is refused (root writes anyway).
	if os.Geteuid() != 0 {
		os.WriteFile(f, []byte("dht.mode.set = auto\n"), 0o400)
		if out, err := runConfScript(t, home, map[string]bool{"dht": false}); err == nil || !strings.Contains(out, "readable and writable") {
			t.Fatalf("read-only: %v %s", err, out)
		}
		if b, _ := os.ReadFile(f); string(b) != "dht.mode.set = auto\n" {
			t.Fatalf("read-only changed: %q", b)
		}
	}
}

// The prologue holds only the closed set of lines.
func TestRtDiscoveryVars(t *testing.T) {
	if got := rtDiscoveryVars(map[string]bool{"dht": true, "pex": false}); got != "d='dht.mode.set = on'\np='protocol.pex.set = no'\n" {
		t.Fatalf("%q", got)
	}
	if got := rtDiscoveryVars(map[string]bool{"pex": true}); got != "d=''\np='protocol.pex.set = yes'\n" {
		t.Fatalf("%q", got)
	}
}

// discFake is a fake rTorrent whose DHT and PeX follow the setters; with
// disabled set it was started with DHT "disable" and ignores "on".
type discFake struct {
	mu       sync.Mutex
	dht, pex bool
	disabled bool
}

func (d *discFake) reply(method, body string) string {
	d.mu.Lock()
	defer d.mu.Unlock()
	switch method {
	case "dht.mode.set":
		if strings.Contains(body, "<string>on</string>") {
			d.dht = !d.disabled
		} else {
			d.dht = false
		}
	case "protocol.pex.set": // answers the value it set, as rTorrent 0.9.8 does
		d.pex = strings.Contains(body, ">1</")
		if d.pex {
			return xmlResp(i8(1))
		}
	case "system.multicall":
		b := func(v bool) string {
			if v {
				return i8(1)
			}
			return i8(0)
		}
		mode := "auto"
		if d.disabled {
			mode = "disable"
		}
		return xmlResp(`<array><data>` + xmlRow(xs("0.9.8")) + xmlRow(xs("0.13.8")) + xmlRow(i8(0)) + xmlRow(i8(0)) + xmlRow(i8(59112)) +
			`<value><array><data><value><struct><member><name>dht</name><value><string>` + mode + `</string></value></member><member><name>active</name><value>` + b(d.dht) + `</value></member></struct></value></data></array></value>` +
			xmlRow(b(d.pex)) + `</data></array>`)
	}
	return rtReplies(method, body)
}

// SetRtDiscovery writes the file first, then the client, and publishes
// both at once; a client that cannot follow live says so (restart).
func TestSetRtDiscovery(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	f := newFakeSCGI(t)
	d := &discFake{}
	f.reply = d.reply
	p, _ := rtFixture(t, f)
	p.box.Enabled = true
	home := t.TempDir()
	p.runConf = &localRunner{home: home}
	readyRt(t, p, "unix:/home/u/.rtorrent.socket")
	ctx := context.Background()
	m := p.m
	if _, err := m.SetRtDiscovery(ctx, 1, map[string]bool{"lsd": true}); err == nil || !strings.Contains(err.Error(), "no local service discovery") {
		t.Fatalf("lsd: %v", err)
	}
	res, err := m.SetRtDiscovery(ctx, 1, map[string]bool{"dht": true, "pex": true})
	if err != nil || !res.Live || res.Note != "" {
		t.Fatalf("%+v %v", res, err)
	}
	if b, _ := os.ReadFile(home + "/.rtorrent.rc.custom"); string(b) != "dht.mode.set = on\nprotocol.pex.set = yes\n" {
		t.Fatalf("file %q", b)
	}
	st, _ := m.State(1)
	if !st.Rt.DHT || !st.Rt.PeX || !st.Rt.PeXKnown || st.Rt.ConfDHT != "on" || st.Rt.ConfPeX != "on" {
		t.Fatalf("state %+v", st.Rt)
	}
	// Started with DHT disabled: saved, but on only after a restart.
	d.mu.Lock()
	d.dht, d.disabled = false, true
	d.mu.Unlock()
	res, err = m.SetRtDiscovery(ctx, 1, map[string]bool{"dht": true})
	if err != nil || res.Live || !strings.Contains(res.Note, "started with DHT disabled cannot start it live") {
		t.Fatalf("%+v %v", res, err)
	}
	if st, _ = m.State(1); st.Rt.DHT || st.Rt.ConfDHT != "on" {
		t.Fatalf("state after restart-needed: %+v", st.Rt)
	}
	// A file that cannot be written changes nothing in the client.
	os.Remove(home + "/.rtorrent.rc.custom")
	os.Mkdir(home+"/.rtorrent.rc.custom", 0o700)
	before := len(f.called())
	if _, err := m.SetRtDiscovery(ctx, 1, map[string]bool{"pex": false}); err == nil || !strings.Contains(err.Error(), "nothing was changed") {
		t.Fatalf("%v", err)
	}
	for _, c := range f.called()[before:] {
		if c == "protocol.pex.set" {
			t.Fatal("client changed after a failed save")
		}
	}
	// A poll that started before a write cannot publish the old values.
	var r RtState
	r.Enabled = true
	p.overlayRtDiscovery(st.LastPoll, &r)
	if r.ConfDHT != "on" || r.ConfPeX != "on" {
		t.Fatalf("overlay %+v", r)
	}
}
