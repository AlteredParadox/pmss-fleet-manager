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
	"bytes"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestScriptRunsLocally executes the probe scripts with the local sh. The
// machine is not a PMSS box, so most sections are empty; the point is that
// the script is valid POSIX sh, terminates, and the parser accepts it.
func TestScriptRunsLocally(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(Script)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		t.Fatalf("script failed: %v\n%s", err, errb.String())
	}
	if errb.Len() > 0 {
		t.Logf("stderr: %s", errb.String())
	}
	snap, err := Parse(out.Bytes(), time.Now())
	if err != nil {
		t.Fatalf("parse: %v\n%s", err, out.String())
	}
	if snap.UID == 0 || snap.User == "" {
		t.Fatalf("identity missing: %+v", snap)
	}
	if snap.Host.UptimeSec <= 0 || snap.Host.NCPU <= 0 || snap.Host.MemTotal <= 0 {
		t.Fatalf("host info missing: %+v", snap.Host)
	}
	if snap.HomeFS.Size <= 0 {
		t.Fatalf("df missing: %+v", snap.HomeFS)
	}
	t.Logf("uid=%d user=%s cgroup=%+v quota=%+v", snap.UID, snap.User, snap.Cgroup, snap.Quota)

	cmd = exec.Command("sh")
	cmd.Stdin = strings.NewReader(DuScript)
	out.Reset()
	cmd.Stdout = &out
	cmd.Stderr = &errb
	_ = cmd.Run() // du may exit non-zero on unreadable entries; output is still usable
	rows := ParseDu(out.Bytes())
	if len(rows) == 0 {
		t.Fatalf("du produced no rows: %s", errb.String())
	}
}

// QbtCronPattern decides which crontab lines autostart replaces and the
// probe counts: only lines whose whole command is the helper.
func TestQbtCronPattern(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	cases := []struct {
		line string
		want bool
	}{
		{"@reboot     sleep 30; /home/ragnar/bin/qbt-start", true}, // the two lines autostart writes
		{"*/5 * * * * /home/ragnar/bin/qbt-start", true},
		{`*/5 * * * * "$HOME/bin/qbt-start"`, true},
		{"@reboot ~/bin/qbt-start", true},
		{"*/5 * * * * '${HOME}/bin/qbt-start'", true},
		{"0 3 * * * ~/bin/backup.sh && ~/bin/qbt-start", false}, // the user's own compound line
		{"*/5 * * * * ~/bin/qbt-start; ~/bin/other", false},
		{"@reboot sleep 30; nice -n5 ~/bin/qbt-start", false},
		{"# */5 * * * * /home/ragnar/bin/qbt-start", false}, // commented out
		{"*/5 * * * * ~/bin/qbt-starter", false},
		{"*/5 * * * * ~/bin/qbt-start.old", false},
		{"MAILTO=qbt-start", false},
	}
	var in strings.Builder
	for _, c := range cases {
		in.WriteString(c.line + "\n")
	}
	cmd := exec.Command("sh", "-c", "LC_ALL=C; export LC_ALL; grep -E "+QbtCronPattern)
	cmd.Stdin = strings.NewReader(in.String())
	out, _ := cmd.Output() // grep exits 1 when nothing matches
	matched := map[string]bool{}
	for _, l := range strings.Split(string(out), "\n") {
		matched[l] = true
	}
	for _, c := range cases {
		if matched[c.line] != c.want {
			t.Errorf("%q: matched = %v, want %v", c.line, matched[c.line], c.want)
		}
	}
}
