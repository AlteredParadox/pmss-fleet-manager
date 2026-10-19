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
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"pmss-fleet-manager/internal/probe"
	"pmss-fleet-manager/internal/store"
)

// TestAutostartKeepsOtherCronLines runs autostartScript with the local sh
// against a stand-in crontab command: the tool's own two lines are
// replaced, a line that runs the helper along with something else, a
// comment and a line that is not valid UTF-8 are kept, and the crontab as
// it was is saved first.
func TestAutostartKeepsOtherCronLines(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	home, stub := t.TempDir(), t.TempDir()
	tab := filepath.Join(home, "tab")
	// -l prints the table file, - replaces it with stdin.
	fake := "#!/bin/sh\ncase \"$1\" in\n-l) cat \"$TAB\" ;;\n-) cat > \"$TAB\" ;;\n*) exit 2 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(stub, "crontab"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	kept := "MAILTO=\"\"\n" +
		"0 3 * * * ~/bin/backup.sh && ~/bin/qbt-start\n" +
		"# */5 * * * * ~/bin/qbt-start\n" +
		"5 4 * * * echo caf\xe9\n"
	own := "@reboot     sleep 30; " + home + "/bin/qbt-start\n" +
		"*/5 * * * * " + home + "/bin/qbt-start\n"
	before := kept + own
	if err := os.WriteFile(tab, []byte(before), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(autostartScript)
	cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"), "TAB="+tab, "LC_ALL=C.UTF-8")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("autostart failed: %v\n%s", err, out.String())
	}
	got, err := os.ReadFile(tab)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != kept+own {
		t.Errorf("crontab after autostart:\n%s\nwant:\n%s", got, kept+own)
	}
	bak := filepath.Join(home, ".config", "pmss-fleet-manager", "crontab.bak")
	saved, err := os.ReadFile(bak)
	if err != nil || string(saved) != before {
		t.Errorf("backup %q, %v; want the crontab as it was", saved, err)
	}
	if fi, err := os.Stat(bak); err != nil {
		t.Error(err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("backup mode %v, want 0600", fi.Mode().Perm())
	}
	if !strings.Contains(out.String(), "previous crontab saved to") {
		t.Errorf("output does not say where the backup is:\n%s", out.String())
	}
}

// Install and autostart refuse before connecting when the last snapshot saw
// the PMSS panel's qBittorrent marker.
func TestQbtInstallRefusesPMSSManaged(t *testing.T) {
	m := &Manager{states: map[int64]*BoxState{7: {Qbt: QbtState{PMSSManaged: true}}}}
	p := &poller{m: m, box: &store.Box{ID: 7}}
	if _, err := p.qbtInstall(context.Background(), "https://example.com/qbittorrent-nox"); !errors.Is(err, errPMSSManagedQbt) {
		t.Errorf("install: %v", err)
	}
	if _, err := p.qbtAutostart(context.Background()); !errors.Is(err, errPMSSManagedQbt) {
		t.Errorf("autostart: %v", err)
	}
}

// A panel-managed qBittorrent that is not running stays enabled: PMSS's
// watchdog runs the system binary, so there is nothing in ~/bin to find.
func TestDetectQbtPMSSManaged(t *testing.T) {
	for _, managed := range []bool{true, false} {
		snap := &probe.Snapshot{}
		snap.Qbt.PMSSManaged = managed
		snap.Qbt.ConfPort = 12345
		b := &store.Box{}
		var steps []Step
		add := func(status, format string, args ...any) {
			steps = append(steps, Step{Status: status, Text: fmt.Sprintf(format, args...)})
		}
		detectQbt(context.Background(), nil, snap, b, &DetectResult{}, add)
		if len(steps) != 1 {
			t.Fatalf("managed=%v: steps %+v", managed, steps)
		}
		if b.QbtEnabled != managed || strings.Contains(steps[0].Text, "Pulsed Media panel") != managed {
			t.Errorf("managed=%v: enabled=%v, step %q", managed, b.QbtEnabled, steps[0].Text)
		}
	}
}
