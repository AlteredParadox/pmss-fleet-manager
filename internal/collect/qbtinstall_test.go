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
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
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

// A helper that keys its running check on $USER (empty under cron, so it
// started another copy on every run) is saved and rewritten by autostart;
// any other existing helper is the user's and left alone.
func TestAutostartRewritesUserKeyedHelper(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	stub := t.TempDir()
	fake := "#!/bin/sh\ncase \"$1\" in\n-l) cat \"$TAB\" ;;\n-) cat > \"$TAB\" ;;\n*) exit 2 ;;\nesac\n"
	if err := os.WriteFile(filepath.Join(stub, "crontab"), []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	run := func(t *testing.T, helper string) (string, string) {
		home := t.TempDir()
		tab := filepath.Join(home, "tab")
		if err := os.WriteFile(tab, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if helper != "" {
			if err := os.MkdirAll(filepath.Join(home, "bin"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(home, "bin", "qbt-start"), []byte(helper), 0o700); err != nil {
				t.Fatal(err)
			}
		}
		cmd := exec.Command("sh")
		cmd.Stdin = strings.NewReader(autostartScript)
		cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+stub+string(os.PathListSeparator)+os.Getenv("PATH"), "TAB="+tab, "USER=")
		var out bytes.Buffer
		cmd.Stdout, cmd.Stderr = &out, &out
		if err := cmd.Run(); err != nil {
			t.Fatalf("autostart failed: %v\n%s", err, out.String())
		}
		got, err := os.ReadFile(filepath.Join(home, "bin", "qbt-start"))
		if err != nil {
			t.Fatal(err)
		}
		return string(got), out.String()
	}
	for _, old := range []string{
		"#!/bin/bash\nif ! pgrep -u \"$USER\" -f \"$HOME/bin/qbittorrent-nox\" >/dev/null; then\n    screen -dmS qbt \"$HOME/bin/qbittorrent-nox\"\nfi\n",
		"#!/bin/bash\nif ! pgrep -u $USER -x qbittorrent-nox; then screen -dmS qbt \"$HOME/bin/qbittorrent-nox\"; fi\n",
	} {
		got, out := run(t, old)
		if got != qbtStartHelper {
			t.Errorf("helper keyed on $USER not rewritten:\n%s", got)
		}
		if !strings.Contains(out, "rewritten") || !strings.Contains(out, "qbt-start.bak") {
			t.Errorf("output does not say the helper was saved and rewritten:\n%s", out)
		}
	}
	custom := "#!/bin/sh\n# mine\npgrep -u \"$(id -u)\" -x qbittorrent-nox >/dev/null || \"$HOME/bin/qbittorrent-nox\" -d\n"
	if got, out := run(t, custom); got != custom || !strings.Contains(out, "left as is") {
		t.Errorf("the user's own helper was touched:\n%s\n%s", got, out)
	}
	if got, out := run(t, ""); got != qbtStartHelper || !strings.Contains(out, "wrote") {
		t.Errorf("no helper: got\n%s\n%s", got, out)
	}
}

// Install and autostart refuse before connecting when the last snapshot saw
// the PMSS panel's qBittorrent marker.
func TestQbtInstallRefusesPMSSManaged(t *testing.T) {
	m := &Manager{states: map[int64]*BoxState{7: {Ready: true, Qbt: QbtState{PMSSManaged: true}}}}
	p := &poller{m: m, box: &store.Box{ID: 7}}
	if _, err := p.qbtInstall(context.Background(), "https://example.com/qbittorrent-nox"); !errors.Is(err, errPMSSManagedQbt) {
		t.Errorf("install: %v", err)
	}
	if _, err := p.qbtAutostart(context.Background()); !errors.Is(err, errPMSSManagedQbt) {
		t.Errorf("autostart: %v", err)
	}
	// Before a probe of this generation the marker is unknown: refuse
	// outright rather than trust the record's default of "not managed".
	m.states[7].Ready = false
	if _, err := p.qbtInstall(context.Background(), "https://example.com/qbittorrent-nox"); !errors.Is(err, ErrNotReady) {
		t.Errorf("install before a probe: %v", err)
	}
	if _, err := p.qbtAutostart(context.Background()); !errors.Is(err, ErrNotReady) {
		t.Errorf("autostart before a probe: %v", err)
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

// Only GitHub release asset URLs map to an API endpoint; the default
// download URL is one of them.
func TestReleaseAssetAPI(t *testing.T) {
	cases := map[string][2]string{
		store.DefaultQbtDownloadURL: {githubAPI + "/repos/userdocs/qbittorrent-nox-static/releases/latest", "x86_64-qbittorrent-nox"},
		"https://github.com/o/r/releases/download/release-5.2.4_v2.0.15/aarch64-qbittorrent-nox": {githubAPI + "/repos/o/r/releases/tags/release-5.2.4_v2.0.15", "aarch64-qbittorrent-nox"},
		"https://GitHub.com/o/r/releases/latest/download/n":                                      {githubAPI + "/repos/o/r/releases/latest", "n"},
	}
	for in, want := range cases {
		api, asset, ok := releaseAssetAPI(in)
		if !ok || api != want[0] || asset != want[1] {
			t.Errorf("%s: %q %q %v", in, api, asset, ok)
		}
	}
	for _, in := range []string{
		"https://example.com/o/r/releases/latest/download/n",
		"https://github.com/o/r/raw/main/n",
		"https://github.com/o/r/releases/latest/download/",
		"https://github.com/o/r/releases/download/tag",
		"http://github.com/o/r/releases/latest/download/n",
		"https://raw.githubusercontent.com/o/r/main/n",
	} {
		if _, _, ok := releaseAssetAPI(in); ok {
			t.Errorf("%s must not map to a release", in)
		}
	}
}

// The digest comes from the asset of that name; a missing asset, a missing
// digest or an API failure yield no digest and a reason.
func TestReleaseDigest(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/releases/latest":
			w.Write([]byte(`{"tag_name":"release-5.2.4_v2.0.15","assets":[{"name":"other","digest":"sha256:` + strings.Repeat("a", 64) + `"},{"name":"x86_64-qbittorrent-nox","digest":"sha256:` + strings.Repeat("AB", 32) + `"},{"name":"nodigest"}]}`))
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	old := githubAPI
	githubAPI = srv.URL
	defer func() { githubAPI = old }()
	ctx := context.Background()
	if d, why := releaseDigest(ctx, "https://github.com/o/r/releases/latest/download/x86_64-qbittorrent-nox"); d != strings.Repeat("ab", 32) || why != "" {
		t.Fatalf("digest %q %q", d, why)
	}
	for url, want := range map[string]string{
		"https://github.com/o/r/releases/latest/download/nodigest":           "no sha256 digest",
		"https://github.com/o/r/releases/latest/download/missing":            "no asset named",
		"https://github.com/o/r/releases/download/v9/x86_64-qbittorrent-nox": "HTTP 404",
		"https://example.com/x86_64-qbittorrent-nox":                         "not a GitHub release",
	} {
		if d, why := releaseDigest(ctx, url); d != "" || !strings.Contains(why, want) {
			t.Errorf("%s: digest %q reason %q, want reason with %q", url, d, why, want)
		}
	}
}

// The install script's checksum step: a matching digest passes and says so,
// a mismatch deletes the download and fails, no digest passes with a note.
// The downloader gets the URL on its stdin, never in argv: a fake curl and
// a fake wget record both.
func TestDownloadShellKeepsURLOffArgv(t *testing.T) {
	const token = "https://example.test/private/qbt?token=SECRET123"
	for tool, line := range map[string]string{"curl": curlDownload, "wget": wgetDownload} {
		bin := t.TempDir()
		fake := "#!/bin/sh\nprintf '%s\\n' \"$@\" > " + bin + "/argv\ncat > " + bin + "/stdin\n: > \"$OUT\"\n"
		os.WriteFile(bin+"/"+tool, []byte(fake), 0o700)
		tmp := bin + "/out.bin"
		cmd := exec.Command("sh", "-c", "url=$1; tmp=$2; "+line+"; exit $rc", "sh", token, tmp)
		cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "OUT="+tmp)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v %s", tool, err, out)
		}
		argv, _ := os.ReadFile(bin + "/argv")
		stdin, _ := os.ReadFile(bin + "/stdin")
		if strings.Contains(string(argv), "SECRET123") {
			t.Fatalf("%s: the URL is on the command line: %q", tool, argv)
		}
		if !strings.Contains(string(stdin), token) {
			t.Fatalf("%s: the URL did not reach stdin: %q", tool, stdin)
		}
	}
}

func TestVerifyChecksumShell(t *testing.T) {
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh")
	}
	if _, err := exec.LookPath("sha256sum"); err != nil {
		t.Skip("no sha256sum")
	}
	dir := t.TempDir()
	run := func(want string) (string, error, bool) {
		tmp := filepath.Join(dir, "dl")
		if err := os.WriteFile(tmp, []byte("not really qbittorrent"), 0o600); err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command("sh", "-c", "tmp=$1; want=$2; "+verifyChecksumShell, "sh", tmp, want)
		out, err := cmd.CombinedOutput()
		_, statErr := os.Stat(tmp)
		return string(out), err, statErr == nil
	}
	sum := sha256.Sum256([]byte("not really qbittorrent"))
	good := hex.EncodeToString(sum[:])
	if out, err, kept := run(good); err != nil || !kept || !strings.Contains(out, "matches the GitHub release digest") {
		t.Fatalf("good digest: %v kept=%v %s", err, kept, out)
	}
	if out, err, kept := run(strings.Repeat("0", 64)); err == nil || kept || !strings.Contains(out, "checksum mismatch") || !strings.Contains(out, good) {
		t.Fatalf("bad digest: %v kept=%v %s", err, kept, out)
	}
	if out, err, kept := run(""); err != nil || !kept || !strings.Contains(out, "not verified") {
		t.Fatalf("no digest: %v kept=%v %s", err, kept, out)
	}
}
