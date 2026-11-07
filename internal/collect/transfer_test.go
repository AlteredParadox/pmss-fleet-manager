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
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestValidXferPath(t *testing.T) {
	for _, c := range []struct {
		p    string
		dest bool
		ok   bool
	}{
		{"data/films", false, true},
		{"/home/u/data", false, true},
		{"it's \"q\" dir", false, true},
		{"it's", true, false},
		{`a"b`, true, false},
		{`a\b`, true, false},
		{"a,b", true, false},
		{"new copy", true, true},
		{"", false, false},
		{"-rf", false, false},
		{"a/../b", false, false},
		{"..", true, false},
		{"a\nb", false, false},
		{"a\rb", false, false},
		{"a\x7fb", false, false},
		{"/", true, false},
		{".", true, false},
	} {
		if err := validXferPath(c.p, c.dest); (err == nil) != c.ok {
			t.Errorf("validXferPath(%q, %v) = %v, want ok=%v", c.p, c.dest, err, c.ok)
		}
	}
}

func needTools(t *testing.T, tools ...string) {
	t.Helper()
	for _, x := range tools {
		if _, err := exec.LookPath(x); err != nil {
			if os.Getenv("CI") != "" {
				t.Fatalf("%s not available (CI installs it)", x)
			}
			t.Skipf("%s not available", x)
		}
	}
}

// runCap feeds input to cap.pl under a 64 MiB address-space limit and
// returns the directory it wrote to.
func runCap(t *testing.T, mode string, input func(w *os.File)) string {
	t.Helper()
	needTools(t, "perl")
	dir := t.TempDir()
	script := filepath.Join(dir, "cap.pl")
	if err := os.WriteFile(script, []byte(xferCapScript), 0o600); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", `ulimit -v 65536 && exec perl "$0" "$1" "$2"`, script, mode, dir)
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stdin = r
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	r.Close()
	input(w)
	w.Close()
	if err := cmd.Wait(); err != nil {
		t.Fatalf("cap.pl: %v %s", err, stderr.String())
	}
	return dir
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeN(t *testing.T, w *os.File, chunk []byte, total int) {
	t.Helper()
	for n := 0; n < total; n += len(chunk) {
		if _, err := w.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
}

func TestXferCapNoSeparators(t *testing.T) {
	chunk := bytes.Repeat([]byte("x"), 1<<20)
	for _, mode := range []string{"progress", "errors"} {
		dir := runCap(t, mode, func(w *os.File) { writeN(t, w, chunk, 200<<20) })
		var got string
		if mode == "progress" {
			// Not a progress line, however long: nothing is kept of it.
			if got = readFile(t, filepath.Join(dir, "progress")); got != "\n" {
				t.Errorf("progress: %d bytes, %q...", len(got), got[:min(40, len(got))])
			}
		} else {
			got = readFile(t, filepath.Join(dir, "errors"))
			if len(got) > 257+len(" [truncated]") || readFile(t, filepath.Join(dir, "errors.count")) != "1\n" {
				t.Errorf("errors: %d bytes", len(got))
			}
		}
	}
}

func TestXferCapProgress(t *testing.T) {
	dir := runCap(t, "progress", func(w *os.File) {
		var b strings.Builder
		b.WriteString("skipping non-regular file \"link\"\n")
		for i := 0; i < 3000; i++ { // ~150 KiB of \r-only progress, crossing many 4 KiB reads
			b.WriteString("     1,234,567  45%   12.34MB/s    0:01:23 (xfr#1, to-chk=5/9)\r")
		}
		b.WriteString("skipping non-regular file \"fifo\"\n")
		b.WriteString("    20,971,520 100%    1.95MB/s    0:00:10 (xfr#11, to-chk=0/14)") // no terminator at EOF
		w.WriteString(b.String())
	})
	if got := readFile(t, filepath.Join(dir, "progress")); got != "    20,971,520 100%    1.95MB/s    0:00:10 (xfr#11, to-chk=0/14)\n" {
		t.Errorf("progress = %q", got)
	}
	if got := readFile(t, filepath.Join(dir, "skipped")); got != "2\n" {
		t.Errorf("skipped = %q", got)
	}
}

// Existing files kept (mode keep) are counted by the shape of rsync's
// skip2 lines, conflicts apart; a name over the cap counts for nothing,
// and no such line ever stands in for the progress (GPT review of the
// plan: a long "exists" line used to replace it).
func TestXferCapKeptAndConflicts(t *testing.T) {
	long := strings.Repeat("d/", 300) + "f"
	dir := runCap(t, "progress", func(w *os.File) {
		w.WriteString("     1,234,567  45%   12.34MB/s    0:01:23 (xfr#1, to-chk=5/9)\r")
		w.WriteString("a.txt exists\n")
		w.WriteString("sub/b.txt exists (uptodate)\n")
		w.WriteString("c exists (file change)\n")
		w.WriteString("d exists (attr change)\n")
		w.WriteString("e exists (type change)\n")
		w.WriteString("sub exists (type change)\n")
		w.WriteString("it exists exists\n")
		w.WriteString(long + " exists\n")
		w.WriteString("created directory /x\n")
		w.WriteString("skipping non-regular file \"link\"\n")
	})
	for name, want := range map[string]string{
		"progress":  "     1,234,567  45%   12.34MB/s    0:01:23 (xfr#1, to-chk=5/9)\n",
		"kept":      "5\n",
		"conflicts": "2\n",
		"skipped":   "1\n",
	} {
		if got := readFile(t, filepath.Join(dir, name)); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
}

func TestXferCapErrors(t *testing.T) {
	dir := runCap(t, "errors", func(w *os.File) {
		for i := 1; i <= 100; i++ {
			w.WriteString("rsync: error " + strings.Repeat("e", i*5) + "\n")
		}
	})
	got := readFile(t, filepath.Join(dir, "errors"))
	lines := strings.Split(strings.TrimSuffix(got, "\n"), "\n")
	if len(lines) != 61 || lines[40] != "... 40 more ..." || len(got) > 16384 {
		t.Fatalf("errors: %d lines, %d bytes, line 41 %q", len(lines), len(got), lines[min(40, len(lines)-1)])
	}
	if readFile(t, filepath.Join(dir, "errors.count")) != "100\n" {
		t.Error("count")
	}
	for _, l := range lines {
		if len(l) > 256+len(" [truncated]") {
			t.Errorf("line of %d bytes", len(l))
		}
	}
}

// runJobScript runs a job script locally with HOME=home.
func runJobScript(t *testing.T, home, vars, script string, extraEnv ...string) string {
	t.Helper()
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(vars + xferPrelude + script)
	cmd.Env = append(append(os.Environ(), "HOME="+home), extraEnv...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	if err := cmd.Run(); err != nil || strings.Contains(out.String(), "ERR ") {
		t.Fatalf("script failed: %v\n%s", err, out.String())
	}
	return out.String()
}

const testBlob = "AAAAC3NzaC1lZDI1NTE5AAAAIGq0b1mXc7m1v0sYx0kq9xX0eWQ9b6m1C2n8Jt2P1aBc"

func TestXferKeyAddRemoveRestoresFile(t *testing.T) {
	needTools(t, "perl", "flock", "rrsync")
	tok := "0123456789abcdef0123456789abcdef"
	for name, before := range map[string]*string{
		"no dot ssh":          nil,
		"trailing newline":    ptr("ssh-ed25519 AAAAone a@b\nssh-rsa AAAAtwo c@d\n"),
		"no trailing newline": ptr("ssh-ed25519 AAAAone a@b"),
		"crlf":                ptr("ssh-ed25519 AAAAone a@b\r\nssh-rsa AAAAtwo c@d\r\n"),
		"empty":               ptr(""),
	} {
		t.Run(name, func(t *testing.T) {
			home := t.TempDir()
			ak := filepath.Join(home, ".ssh", "authorized_keys")
			if before != nil {
				os.Mkdir(filepath.Join(home, ".ssh"), 0o700)
				if err := os.WriteFile(ak, []byte(*before), 0o640); err != nil {
					t.Fatal(err)
				}
			}
			out := runJobScript(t, home, xferVars("TOKEN", tok, "DST", "copy dest", "BLOB", testBlob), xferDstClaimScript+xferKeyAddScript)
			line := xferField(strings.Split(out, "\n"), "KEYLINE")
			if !strings.HasPrefix(line, `restrict,expiry-time="`) || !strings.Contains(line, xferRrsyncOpts+` '`+home+`/copy dest'" ssh-ed25519 `+testBlob+" pfm-xfer-"+tok) {
				t.Fatalf("key line %q", line)
			}
			mid := readFile(t, ak)
			if !strings.HasSuffix(mid, line+"\n") {
				t.Fatalf("not appended: %q", mid)
			}
			if fi, _ := os.Stat(ak); before != nil && fi.Mode().Perm() != 0o640 {
				t.Errorf("mode %v", fi.Mode().Perm())
			}
			runJobScript(t, home, xferVars("TOKEN", tok, "BLOB", testBlob), xferKeyRemoveScript)
			if before == nil {
				if _, err := os.Stat(filepath.Join(home, ".ssh")); !os.IsNotExist(err) {
					t.Errorf("~/.ssh left behind: %v", err)
				}
			} else if got := readFile(t, ak); got != *before {
				t.Errorf("after removal %q, want %q", got, *before)
			}
			// A second removal finds nothing and changes nothing.
			if out := runJobScript(t, home, xferVars("TOKEN", tok, "BLOB", testBlob), xferKeyRemoveScript); !strings.Contains(out, "NOTFOUND") {
				t.Errorf("second removal: %s", out)
			}
		})
	}
}

func TestXferKeyRemoveKeepsLineAddedAfterOurs(t *testing.T) {
	needTools(t, "perl", "flock", "rrsync")
	tok := "0123456789abcdef0123456789abcdef"
	home := t.TempDir()
	os.Mkdir(filepath.Join(home, ".ssh"), 0o700)
	ak := filepath.Join(home, ".ssh", "authorized_keys")
	os.WriteFile(ak, []byte("ssh-ed25519 AAAAone a@b"), 0o600)
	runJobScript(t, home, xferVars("TOKEN", tok, "DST", "d", "BLOB", testBlob), xferDstClaimScript+xferKeyAddScript)
	f, _ := os.OpenFile(ak, os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString("ssh-rsa AAAAlater x@y\n")
	f.Close()
	runJobScript(t, home, xferVars("TOKEN", tok, "BLOB", testBlob), xferKeyRemoveScript)
	if got := readFile(t, ak); got != "ssh-ed25519 AAAAone a@b\nssh-rsa AAAAlater x@y\n" {
		t.Errorf("got %q", got)
	}
}

func TestXferDstClaimRefusesExisting(t *testing.T) {
	needTools(t, "rrsync")
	home := t.TempDir()
	os.Mkdir(filepath.Join(home, "taken"), 0o700)
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(xferVars("TOKEN", "0123456789abcdef0123456789abcdef", "DST", "taken") + xferPrelude + xferDstClaimScript)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "ERR destination already exists") {
		t.Errorf("got %s", out)
	}
	// No DST line: the Go side marks the directory as this job's (and so
	// removable while empty) only on that answer (Codex on PR 142).
	if xferField(strings.Split(string(out), "\n"), "DST") != "" {
		t.Errorf("a refused claim named a directory: %s", out)
	}
}

func TestXferSrcRefusesControlDirOverlap(t *testing.T) {
	needTools(t, "ssh-keygen", "rsync", "perl", "setsid")
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".config", "pmss-fleet-manager"), 0o700)
	for _, src := range []string{".config", ".config/pmss-fleet-manager", home} {
		cmd := exec.Command("sh")
		cmd.Stdin = strings.NewReader(xferVars("TOKEN", "0123456789abcdef0123456789abcdef", "SRC", src) + xferPrelude + xferSrcPrepareScript)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, _ := cmd.CombinedOutput()
		if !strings.Contains(string(out), "ERR source") {
			t.Errorf("%s: got %s", src, out)
		}
	}
}

// A transfers directory that is a symlink into the source is refused
// before anything is changed through it: no chmod on the source, no job
// directory or key inside it (daybreak review B1).
func TestXferSrcRefusesSymlinkedTransfersDir(t *testing.T) {
	needTools(t, "ssh-keygen", "rsync", "perl", "setsid")
	home := t.TempDir()
	src := filepath.Join(home, "data")
	os.Mkdir(src, 0o755)
	os.MkdirAll(filepath.Join(home, ".config", "pmss-fleet-manager"), 0o700)
	os.Symlink(src, filepath.Join(home, ".config", "pmss-fleet-manager", "transfers"))
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(xferVars("TOKEN", "0123456789abcdef0123456789abcdef", "SRC", "data") + xferPrelude + xferSrcPrepareScript)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "ERR") || !strings.Contains(string(out), "is a symlink") {
		t.Errorf("got %s", out)
	}
	if fi, _ := os.Stat(src); fi.Mode().Perm() != 0o755 {
		t.Errorf("source mode changed to %v", fi.Mode().Perm())
	}
	if ents, _ := os.ReadDir(src); len(ents) != 0 {
		t.Errorf("source gained %v", ents)
	}
}

// The destination goes into command="..." resolved: a parent reached
// through a symlink must not bring in a quote or a comma (daybreak review).
func TestXferDstClaimRefusesUnsafeResolvedPath(t *testing.T) {
	needTools(t, "rrsync")
	home := t.TempDir()
	os.Mkdir(filepath.Join(home, `a,"b`), 0o700)
	os.Symlink(filepath.Join(home, `a,"b`), filepath.Join(home, "ln"))
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(xferVars("TOKEN", "0123456789abcdef0123456789abcdef", "DST", "ln/new") + xferPrelude + xferDstClaimScript)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "ERR the resolved destination") {
		t.Errorf("got %s", out)
	}
	if _, err := os.Stat(filepath.Join(home, `a,"b`, "new")); !os.IsNotExist(err) {
		t.Errorf("directory created: %v", err)
	}
}

// A symlink on the way can resolve a clean typed path to one with a
// newline, which would split the SRC line: refused on the box before the
// control directory or a key exists, and an answer with a second SRC or
// PUB line is refused on the dashboard (daybreak review B1).
func TestXferSrcRefusesControlCharInResolvedPath(t *testing.T) {
	needTools(t, "ssh-keygen", "rsync", "perl", "setsid")
	home := t.TempDir()
	odd := filepath.Join(home, "a\nb")
	os.MkdirAll(filepath.Join(odd, "child"), 0o700)
	os.Symlink(odd, filepath.Join(home, "safe"))
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(xferVars("TOKEN", "0123456789abcdef0123456789abcdef", "SRC", "safe/child") + xferPrelude + xferSrcPrepareScript)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "ERR the resolved source path contains a control character") {
		t.Errorf("got %s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".config")); !os.IsNotExist(err) {
		t.Errorf("control directory created: %v", err)
	}
	pub := "PUB ssh-ed25519 " + testBlob + " pfm-xfer-0123456789abcdef0123456789abcdef"
	for name, lines := range map[string][]string{
		"second SRC":  {"SRC /home/u/x", "SRC /home/u", pub},
		"second PUB":  {"SRC /home/u/x", pub, pub},
		"relative":    {"SRC home/u/x", pub},
		"no PUB":      {"SRC /home/u/x"},
		"bad PUB":     {"SRC /home/u/x", "PUB ssh-rsa AAAA x"},
		"invalid utf": {"SRC /home/u/\xff", pub},
	} {
		if r, _, err := parseSrcPrepare(lines); err == nil {
			t.Errorf("%s: accepted %q", name, r)
		}
	}
	if r, b, err := parseSrcPrepare([]string{"SRC /home/u/x y", pub}); err != nil || r != "/home/u/x y" || b != testBlob {
		t.Errorf("clean answer: %q %q %v", r, b, err)
	}
}

// Two ways a name could still lose bytes on the box (fred on PR 145): a
// literal backslash-n, which dash's echo would turn into a newline, must
// round-trip exactly; a name that is only a newline, reached as "safe/.",
// must not be dropped by $(...) and leave $HOME as the source.
func TestXferSrcPathBytesSurvive(t *testing.T) {
	needTools(t, "ssh-keygen", "rsync", "perl", "setsid")
	prepare := func(home, src string) (string, string) {
		cmd := exec.Command("sh")
		cmd.Stdin = strings.NewReader(xferVars("TOKEN", "0123456789abcdef0123456789abcdef", "SRC", src) + xferPrelude + xferSrcPrepareScript)
		cmd.Env = append(os.Environ(), "HOME="+home)
		out, _ := cmd.CombinedOutput()
		return string(out), home
	}
	home := t.TempDir()
	os.Mkdir(filepath.Join(home, `\nchild`), 0o700)
	out, _ := prepare(home, `\nchild`)
	if r, _, err := parseSrcPrepare(strings.Split(strings.TrimRight(out, "\n"), "\n")); err != nil || r != home+`/\nchild` {
		t.Errorf("backslash-n source came back as %q (%v):\n%s", r, err, out)
	}
	home = t.TempDir()
	os.Mkdir(filepath.Join(home, "\n"), 0o700)
	os.Symlink(filepath.Join(home, "\n"), filepath.Join(home, "safe"))
	out, _ = prepare(home, "safe/.")
	if !strings.Contains(out, "ERR the resolved source path contains a control character") {
		t.Errorf("newline-named source: %s", out)
	}
	if _, err := os.Stat(filepath.Join(home, ".config")); !os.IsNotExist(err) {
		t.Errorf("control directory created: %v", err)
	}
}

// The destination's parent is resolved byte for byte too: a parent whose
// name ends in a newline is refused, not taken for its newline-less
// neighbour (fred on PR 145).
func TestXferDstParentBytesSurvive(t *testing.T) {
	needTools(t, "rrsync")
	home := t.TempDir()
	os.Mkdir(filepath.Join(home, "x"), 0o700)
	os.Mkdir(filepath.Join(home, "x\n"), 0o700)
	os.Symlink(filepath.Join(home, "x\n"), filepath.Join(home, "ln"))
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(xferVars("TOKEN", "0123456789abcdef0123456789abcdef", "DST", "ln/new") + xferPrelude + xferDstClaimScript)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, _ := cmd.CombinedOutput()
	if !strings.Contains(string(out), "ERR the resolved destination path") {
		t.Errorf("got %s", out)
	}
	if _, err := os.Stat(filepath.Join(home, "x", "new")); !os.IsNotExist(err) {
		t.Errorf("created in the neighbour: %v", err)
	}
}

// The job key is forced into the system's rrsync, whatever PATH finds
// first, and only one that is root's and root-only writable (daybreak
// review B2).
func TestXferDstClaimPinsRrsync(t *testing.T) {
	needTools(t, "perl", "flock", "rrsync")
	if _, err := os.Stat(xferRrsync); err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("%s not present (CI installs rsync)", xferRrsync)
		}
		t.Skipf("%s not present", xferRrsync)
	}
	home, bin := t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(bin, "rrsync"), []byte("#!/bin/sh\nexec sh\n"), 0o755)
	// Group-writable: untrusted whoever runs the test, root included
	// (Codex on PR 145).
	os.Chmod(filepath.Join(bin, "rrsync"), 0o775)
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	out := runJobScript(t, home, xferVars("TOKEN", "0123456789abcdef0123456789abcdef", "DST", "copy", "BLOB", testBlob), xferDstClaimScript+xferKeyAddScript, env...)
	line := xferField(strings.Split(out, "\n"), "KEYLINE")
	if !strings.Contains(line, `command="`+xferRrsync+` `+xferRrsyncOpts+` '`) || strings.Contains(line, bin) {
		t.Errorf("key line %q", line)
	}
	// One the account could replace is refused before anything changes.
	home = t.TempDir()
	script := strings.Replace(xferDstClaimScript, "rr="+xferRrsync+"\n", "rr="+filepath.Join(bin, "rrsync")+"\n", 1)
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(xferVars("TOKEN", "0123456789abcdef0123456789abcdef", "DST", "copy", "BLOB", testBlob) + xferPrelude + script + xferKeyAddScript)
	cmd.Env = append(os.Environ(), "HOME="+home)
	b, _ := cmd.CombinedOutput()
	if !strings.Contains(string(b), "not trusting it as rrsync") {
		t.Errorf("user-owned rrsync: %s", b)
	}
	if _, err := os.Stat(filepath.Join(home, "copy")); !os.IsNotExist(err) {
		t.Errorf("destination created: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".ssh")); !os.IsNotExist(err) {
		t.Errorf("~/.ssh touched: %v", err)
	}
}

// A runner that cannot publish its pid could not be cancelled: it records
// an error and never starts rsync; the key goes (daybreak review S1).
func TestXferRunnerNeedsPid(t *testing.T) {
	j := startLocalJob(t, true, func(dir string) { os.Mkdir(filepath.Join(dir, "pid.tmp"), 0o700) })
	l := j.waitStatus(t, func(l xferLive) bool { return l.rc != "" && !l.alive })
	if l.rc != "error: cannot write the pid file" || xferDecide(xferRunning, l, false) != xferFinished {
		t.Fatalf("status %+v", l)
	}
	if !j.keyGone(t) {
		t.Error("key left")
	}
	if pid := readFileOr(filepath.Join(j.bin, "ssh.pid")); pid != "" {
		t.Errorf("rsync connected (ssh stand-in pid %s)", pid)
	}
}

// Neither the pid nor the rc can be written (a full disk): the runner
// takes its started marker back, so the launch stays unconfirmed instead
// of the job reading as running forever with nothing to cancel; cancel
// then ends it, and it cannot be launched again (daybreak review).
func TestXferRunnerNoPidNoRc(t *testing.T) {
	j := startLocalJob(t, true, func(dir string) {
		os.Mkdir(filepath.Join(dir, "pid.tmp"), 0o700)
		os.Mkdir(filepath.Join(dir, "rc.tmp"), 0o700)
	})
	if !strings.Contains(j.launch, "UNCONFIRMED") {
		t.Errorf("launch: %s", j.launch)
	}
	l := j.waitStatus(t, func(l xferLive) bool { return !l.started && !l.alive })
	if l.rc != "" || xferDecide(xferUnconfirmed, l, false) != xferUnconfirmed || xferDecide(xferUnconfirmed, l, true) != xferInterrupted {
		t.Errorf("status %+v", l)
	}
	if !j.keyGone(t) {
		t.Error("key left")
	}
	run := xferVars("SRC", "x", "DEST", "u@h:", "PORT", "22", "HKALG", "ecdsa-sha2-nistp256", "SSH", filepath.Join(j.bin, "ssh"), "ROPTS", "")
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(xferVars("TOKEN", j.tok, "KNOWN", "x", "RUNVARS", run) + xferPrelude + xferLaunchScript)
	cmd.Env = append(append(os.Environ(), "HOME="+j.home), j.env...)
	if out, _ := cmd.CombinedOutput(); !strings.Contains(string(out), "ERR job key missing") {
		t.Errorf("relaunch: %s", out)
	}
	if pid := readFileOr(filepath.Join(j.bin, "ssh.pid")); pid != "" {
		t.Errorf("rsync connected (ssh stand-in pid %s)", pid)
	}
}

// fakeSSH stands in for ssh in the local runner test: it drops the
// options, then runs the remote command the way sshd runs a forced
// command, through rrsync confined to $FAKE_DST.
const fakeSSH = `#!/bin/sh
while [ $# -gt 0 ]; do
  case "$1" in -F|-p|-i|-o|-l) shift 2;; -*) shift;; *) break;; esac
done
shift
[ -n "${FAKE_PIDFILE:-}" ] && echo $$ >"$FAKE_PIDFILE"
[ -n "${FAKE_SLEEP:-}" ] && sleep "$FAKE_SLEEP"
SSH_ORIGINAL_COMMAND="$*" exec rrsync ` + xferRrsyncOpts + ` "$FAKE_DST"
`

type localJob struct {
	home, bin, tok, blob, dst string
	env                       []string
	launch                    string // the launch script's answer
}

// startLocalJob runs prepare, claim and launch locally, with real rsync
// and rrsync and a stand-in for ssh; with slow, the stand-in waits before
// connecting, so the runner is still running when the test acts on it.
// The destination is a new directory "copy" in the home.
func startLocalJob(t *testing.T, slow bool, beforeLaunch ...func(jobDir string)) localJob {
	t.Helper()
	return startLocalJobTo(t, xferModeNew, "copy", slow, nil, beforeLaunch...)
}

// startLocalJobTo is startLocalJob with the destination mode and path
// chosen; prepare, if given, fills the home (an existing destination,
// say) before the claim.
func startLocalJobTo(t *testing.T, mode, dstPath string, slow bool, prepare func(home string), beforeLaunch ...func(jobDir string)) localJob {
	t.Helper()
	needTools(t, "perl", "flock", "rrsync", "rsync", "ssh-keygen", "setsid", "mkfifo")
	home := t.TempDir()
	if prepare != nil {
		prepare(home)
	}
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "ssh"), []byte(fakeSSH), 0o700)
	src := filepath.Join(home, "src dir")
	os.MkdirAll(filepath.Join(src, "sub"), 0o700)
	os.WriteFile(filepath.Join(src, `it's "q".txt`), []byte("hello"), 0o600)
	os.WriteFile(filepath.Join(src, "sub", "big"), bytes.Repeat([]byte("z"), 3<<20), 0o600)
	os.Symlink("/etc/passwd", filepath.Join(src, "link"))
	tok := "0123456789abcdef0123456789abcdef"
	env := []string{"PATH=" + bin + string(os.PathListSeparator) + os.Getenv("PATH")}
	if slow {
		env = append(env, "FAKE_SLEEP=30", "FAKE_PIDFILE="+filepath.Join(bin, "ssh.pid"))
	}
	out := runJobScript(t, home, xferVars("TOKEN", tok, "SRC", "src dir"), xferSrcPrepareScript, env...)
	lines := strings.Split(out, "\n")
	if xferField(lines, "SRC") != src {
		t.Fatalf("prepare: %s", out)
	}
	blob := strings.Fields(xferField(lines, "PUB"))[1]
	out = runJobScript(t, home, xferVars("TOKEN", tok, "DST", dstPath, "BLOB", blob, "MODE", mode), xferDstClaimScript+xferKeyAddScript, env...)
	dst, created, err := parseDstClaim(strings.Split(out, "\n"))
	if err != nil || created != (mode == xferModeNew) {
		t.Fatalf("claim: %v created=%v\n%s", err, created, out)
	}
	env = append(env, "FAKE_DST="+dst)
	for _, f := range beforeLaunch {
		f(filepath.Join(home, xferJobDir, tok))
	}
	run := xferVars("SRC", src, "DEST", "u@example.invalid:", "PORT", "22", "HKALG", "ecdsa-sha2-nistp256", "SSH", filepath.Join(bin, "ssh"), "ROPTS", xferRsyncOpts(mode))
	out = runJobScript(t, home, xferVars("TOKEN", tok, "KNOWN", "example.invalid ecdsa-sha2-nistp256 AAAA", "RUNVARS", run), xferLaunchScript, env...)
	if !strings.Contains(out, "LAUNCHED") && len(beforeLaunch) == 0 {
		t.Fatalf("launch: %s", out)
	}
	j := localJob{home: home, bin: bin, tok: tok, blob: blob, dst: dst, env: env, launch: out}
	t.Cleanup(func() { // never leave a runner behind
		if pid := strings.TrimSpace(readFileOr(filepath.Join(home, xferJobDir, tok, "pid"))); pid != "" {
			exec.Command("kill", "-KILL", "--", "-"+pid).Run()
		}
	})
	return j
}

func readFileOr(p string) string { b, _ := os.ReadFile(p); return string(b) }

// procGone: pid has exited. A zombie counts: in a container whose PID 1
// does not reap (GitHub's job containers run under tail -f), a killed
// orphan keeps its /proc entry, state Z, until something waits for it.
func procGone(pid string) bool {
	b, err := os.ReadFile("/proc/" + pid + "/stat")
	if err != nil {
		return true
	}
	// "pid (comm) S ..."; comm may hold spaces or parens, so take the
	// state after the last ')'.
	i := strings.LastIndexByte(string(b), ')')
	return i < 0 || i+2 >= len(b) || b[i+2] == 'Z'
}

func (j localJob) status(t *testing.T) xferLive {
	t.Helper()
	return parseXferStatus(runJobScript(t, j.home, xferVars("TOKEN", j.tok), xferStatusScript))
}

// waitStatus polls the job's status until cond holds (10 s at most).
func (j localJob) waitStatus(t *testing.T, cond func(xferLive) bool) xferLive {
	t.Helper()
	var l xferLive
	for i := 0; i < 100; i++ {
		if l = j.status(t); cond(l) {
			return l
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("status never matched: %+v", l)
	return l
}

func (j localJob) keyGone(t *testing.T) bool {
	_, err := os.Stat(filepath.Join(j.home, xferJobDir, j.tok, "key"))
	return os.IsNotExist(err)
}

// TestXferRunnerLocal: a copy runs to its exit code, a second launch is a
// no-op, symlinks are skipped, and cleanup leaves nothing.
func TestXferRunnerLocal(t *testing.T) {
	j := startLocalJob(t, false)
	l := j.waitStatus(t, func(l xferLive) bool { return l.rc != "" })
	// Launching again, after the runner ended and deleted its key, is
	// still a no-op: the started marker is checked first.
	run := xferVars("SRC", "x", "DEST", "u@h:", "PORT", "22", "HKALG", "ecdsa-sha2-nistp256", "ROPTS", "")
	if out := runJobScript(t, j.home, xferVars("TOKEN", j.tok, "KNOWN", "x", "RUNVARS", run), xferLaunchScript, j.env...); !strings.Contains(out, "LAUNCHED already") {
		t.Errorf("relaunch: %s", out)
	}
	if l.rc != "0" || l.skipped != 1 || xferDecide(xferRunning, l, false) != xferFinished {
		t.Fatalf("status: %+v", l)
	}
	if res := xferResult(xferFinished, l); !strings.HasPrefix(res, "copied 3.0 MiB in 2 files; 1 symlinks") {
		t.Errorf("result %q", res)
	}
	if !j.keyGone(t) {
		t.Error("key left on the source")
	}
	if readFile(t, filepath.Join(j.dst, `it's "q".txt`)) != "hello" {
		t.Error("file not copied")
	}
	if _, err := os.Lstat(filepath.Join(j.dst, "link")); !os.IsNotExist(err) {
		t.Error("symlink copied")
	}
	runJobScript(t, j.home, xferVars("TOKEN", j.tok, "BLOB", j.blob), xferKeyRemoveScript)
	runJobScript(t, j.home, xferVars("TOKEN", j.tok), xferSrcCleanupScript)
	if _, err := os.Stat(filepath.Join(j.home, xferJobDir)); !os.IsNotExist(err) {
		t.Error("transfers directory left behind")
	}
	if _, err := os.Stat(filepath.Join(j.home, ".ssh")); !os.IsNotExist(err) {
		t.Error("~/.ssh left behind")
	}
}

// TestXferRunnerCancel: cancel signals the runner's process group; the
// trap records "cancelled" and deletes the key, and nothing of the group
// keeps running.
func TestXferRunnerCancel(t *testing.T) {
	j := startLocalJob(t, true)
	j.waitStatus(t, func(l xferLive) bool { return l.alive })
	// Cleanup refuses while the runner lives (so nothing else is cleaned).
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(xferVars("TOKEN", j.tok) + xferPrelude + xferSrcCleanupScript)
	cmd.Env = append(os.Environ(), "HOME="+j.home)
	if out, _ := cmd.CombinedOutput(); !strings.Contains(string(out), "ERR the runner is still running") {
		t.Errorf("cleanup of a live job: %s", out)
	}
	sshPid := ""
	for i := 0; i < 50 && sshPid == ""; i++ {
		sshPid = strings.TrimSpace(readFileOr(filepath.Join(j.bin, "ssh.pid")))
		time.Sleep(50 * time.Millisecond)
	}
	out := runJobScript(t, j.home, xferVars("TOKEN", j.tok), xferCancelScript)
	if !strings.Contains(out, "SIGNALLED") {
		t.Fatalf("cancel: %s", out)
	}
	// The trap writes rc just before the runner exits: wait for both.
	l := j.waitStatus(t, func(l xferLive) bool { return l.rc != "" && !l.alive })
	if l.rc != "cancelled" || xferDecide(xferRunning, l, true) != xferFinished || xferResult(xferFinished, l) != "cancelled" {
		t.Fatalf("after cancel: %+v %q", l, xferResult(xferFinished, l))
	}
	if !j.keyGone(t) {
		t.Error("key left after cancel")
	}
	if sshPid != "" {
		for i := 0; i < 40 && !procGone(sshPid); i++ {
			time.Sleep(50 * time.Millisecond)
		}
		if !procGone(sshPid) {
			t.Error("the transport process survived the cancel")
		}
	}
	// A second cancel finds nothing running.
	if out := runJobScript(t, j.home, xferVars("TOKEN", j.tok), xferCancelScript); !strings.Contains(out, "NOTRUNNING") {
		t.Errorf("second cancel: %s", out)
	}
}

// TestXferRunnerInterrupted: a runner killed outright (a reboot, an OOM
// kill) leaves no exit code; the status read proves it gone, the job is
// interrupted (not failed, not finished), and cleanup still removes the
// key files the trap never got to and the destination key line.
func TestXferRunnerInterrupted(t *testing.T) {
	j := startLocalJob(t, true)
	l := j.waitStatus(t, func(l xferLive) bool { return l.alive })
	if xferDecide(xferRunning, l, false) != xferRunning {
		t.Fatal("a live runner must stay running")
	}
	pid := strings.TrimSpace(readFile(t, filepath.Join(j.home, xferJobDir, j.tok, "pid")))
	if err := exec.Command("kill", "-KILL", "--", "-"+pid).Run(); err != nil {
		t.Fatal(err)
	}
	l = j.waitStatus(t, func(l xferLive) bool { return !l.alive })
	if l.rc != "" || !l.started || !l.pidFile {
		t.Fatalf("after kill: %+v", l)
	}
	if got := xferDecide(xferRunning, l, false); got != xferInterrupted {
		t.Fatalf("decided %s", got)
	}
	if res := xferResult(xferInterrupted, l); !strings.HasPrefix(res, "interrupted: the runner ended without a result") {
		t.Errorf("result %q", res)
	}
	if j.keyGone(t) {
		t.Fatal("SIGKILL cannot run the trap; the key should still be there for cleanup to remove")
	}
	if out := runJobScript(t, j.home, xferVars("TOKEN", j.tok, "BLOB", j.blob), xferKeyRemoveScript); !strings.Contains(out, "REMOVED") {
		t.Errorf("key removal: %s", out)
	}
	if out := runJobScript(t, j.home, xferVars("TOKEN", j.tok), xferSrcCleanupScript); !strings.Contains(out, "REMOVED") {
		t.Errorf("source cleanup: %s", out)
	}
	if _, err := os.Stat(filepath.Join(j.home, xferJobDir)); !os.IsNotExist(err) {
		t.Error("job files left behind")
	}
}

// TestXferRrsyncRefuses checks that the restricted command refuses a read
// and --delete, through the same stand-in.
func TestXferRrsyncRefuses(t *testing.T) {
	needTools(t, "rrsync", "rsync")
	bin, dst, src := t.TempDir(), t.TempDir(), t.TempDir()
	os.WriteFile(filepath.Join(bin, "ssh"), []byte(fakeSSH), 0o700)
	os.WriteFile(filepath.Join(dst, "secret"), []byte("s"), 0o600)
	env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_DST="+dst)
	for name, args := range map[string][]string{
		"read":   {"-rt", "-e", "ssh", "--", "u@h:", src + "/"},
		"delete": {"-rt", "--delete", "-e", "ssh", "--", src + "/", "u@h:"},
	} {
		cmd := exec.Command("rsync", args...)
		cmd.Env = env
		if out, err := cmd.CombinedOutput(); err == nil {
			t.Errorf("%s was allowed: %s", name, out)
		}
	}
	if _, err := os.Stat(filepath.Join(dst, "secret")); err != nil {
		t.Error("destination file touched")
	}
}

// TestXferRrsyncHostile: a source that turned hostile holds the job key
// and can run any rsync client. A planted symlink used as --backup-dir
// must not let it write outside the directory (here: over
// ~/.ssh/authorized_keys); rrsync's -munge is what stops it (Codex
// security review on PR 142). Absolute and ".." paths in options are
// refused by rrsync itself.
func TestXferRrsyncHostile(t *testing.T) {
	needTools(t, "rrsync", "rsync")
	home, bin, seed, link := t.TempDir(), t.TempDir(), t.TempDir(), t.TempDir()
	dst := filepath.Join(home, "copy")
	os.Mkdir(dst, 0o700)
	os.Mkdir(filepath.Join(home, ".ssh"), 0o700)
	ak := filepath.Join(home, ".ssh", "authorized_keys")
	os.WriteFile(ak, []byte("ORIGINAL\n"), 0o600)
	os.WriteFile(filepath.Join(bin, "ssh"), []byte(fakeSSH), 0o700)
	env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_DST="+dst)
	rsync := func(args ...string) {
		cmd := exec.Command("rsync", args...)
		cmd.Env = env
		cmd.CombinedOutput() // refusals are the point; the files tell
	}
	os.WriteFile(filepath.Join(seed, "authorized_keys"), []byte("ATTACKER\n"), 0o600)
	rsync("-rt", "-e", "ssh", "--", seed+"/", "u@h:")
	os.Symlink("../.ssh", filepath.Join(link, ".backup"))
	os.WriteFile(filepath.Join(link, "authorized_keys"), []byte("new\n"), 0o600)
	rsync("-rlt", "-b", "--backup-dir=.backup", "-e", "ssh", "--", link+"/", "u@h:")
	rsync("-rt", "--backup-dir=../.ssh", "-b", "-e", "ssh", "--", link+"/", "u@h:")
	rsync("-rt", "--temp-dir=/tmp", "-e", "ssh", "--", seed+"/", "u@h:")
	if got := readFile(t, ak); got != "ORIGINAL\n" {
		t.Fatalf("authorized_keys outside the directory was replaced: %q", got)
	}
	if fi, err := os.Lstat(filepath.Join(dst, ".backup")); err == nil && fi.Mode()&os.ModeSymlink != 0 {
		if target, _ := os.Readlink(filepath.Join(dst, ".backup")); !strings.HasPrefix(target, "/rsyncd-munged/") {
			t.Errorf("symlink arrived unmunged: %s", target)
		}
	}
}

func TestXferLaunchVars(t *testing.T) {
	for _, c := range []struct {
		key, host string
		port      int
		known     string
		alg       string
	}{
		{"ecdsa-sha2-nistp256 AAAAE2", "Box.Example.com", 22, "box.example.com ecdsa-sha2-nistp256 AAAAE2", "ecdsa-sha2-nistp256"},
		{"ssh-ed25519 AAAAC3", "h", 2222, "[h]:2222 ssh-ed25519 AAAAC3", "ssh-ed25519"},
		// An RSA pin stays exactly as pinned in known_hosts, and is
		// negotiated with SHA-2 signatures, never SHA-1 (fred on PR 142).
		{"ssh-rsa AAAAB3", "h", 22, "h ssh-rsa AAAAB3", "rsa-sha2-512,rsa-sha2-256"},
	} {
		v, err := xferLaunchVars(strings.Repeat("a", 32), "/home/u/src", xferDest{User: "u", Host: c.host, Port: c.port, HostKey: c.key}, xferModeNew)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(v, "KNOWN="+shellQuote(c.known)+"\n") || !strings.Contains(v, "HKALG="+strings.ReplaceAll(shellQuote(c.alg), "'", `'\''`)) {
			t.Errorf("%s: %s", c.key, v)
		}
	}
	if _, err := xferLaunchVars("t", "s", xferDest{HostKey: ""}, xferModeNew); err == nil {
		t.Error("no pinned key accepted")
	}
	if _, err := xferLaunchVars("t", "s", xferDest{HostKey: "BAD;TYPE AAAA"}, xferModeNew); err == nil {
		t.Error("odd key type accepted")
	}
}

func ptr(s string) *string { return &s }

// runClaim runs the claim script and returns its output, refusal or not.
func runClaim(t *testing.T, home, vars, script string) string {
	t.Helper()
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader(vars + xferPrelude + script)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// An existing destination (modes keep and replace) is answered DIR, never
// DST, and refused when it is not a writable directory inside the home,
// is or touches the home, ~/.ssh or the control directory, or holds a
// symlink anywhere (a link already there is a way out of the directory
// for a source that turned hostile). The scan fails closed.
func TestXferDstClaimExisting(t *testing.T) {
	needTools(t, "rrsync", "timeout")
	home := t.TempDir()
	tok := "0123456789abcdef0123456789abcdef"
	for _, d := range []string{"data/sub", "withlink/sub", ".config/pmss-fleet-manager", ".ssh", "unreadable/sub", "keyed"} {
		os.MkdirAll(filepath.Join(home, d), 0o700)
	}
	os.WriteFile(filepath.Join(home, "data", "f"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(home, "file"), []byte("x"), 0o600)
	os.Symlink("f", filepath.Join(home, "withlink", "sub", "l"))
	os.Symlink(t.TempDir(), filepath.Join(home, "outlink"))
	os.Symlink("data", filepath.Join(home, "inlink"))
	os.Chmod(filepath.Join(home, "unreadable", "sub"), 0)
	t.Cleanup(func() { os.Chmod(filepath.Join(home, "unreadable", "sub"), 0o700) })
	for _, c := range []struct{ dst, mode, want string }{
		{"data", "keep", "DIR " + home + "/data\n"},
		{"data/", "replace", "DIR " + home + "/data\n"},
		{"data/sub", "keep", "DIR " + home + "/data/sub\n"},
		{"inlink", "keep", "DIR " + home + "/data\n"}, // a symlink leaf resolving inside the home
		{"missing", "keep", "ERR destination folder does not exist"},
		{"file", "keep", "ERR destination is not a directory"},
		{".", "keep", "ERR destination is your whole home directory"},
		{".config", "keep", "ERR destination is, contains or lies inside " + home + "/.config/pmss-fleet-manager"},
		{".config/pmss-fleet-manager", "replace", "ERR destination is, contains or lies inside"},
		{".ssh", "keep", "ERR destination is, contains or lies inside " + home + "/.ssh"},
		{"outlink", "keep", "ERR destination is not inside your home directory"},
		{"withlink", "keep", "ERR the folder contains symlinks"},
		{"unreadable", "keep", "ERR could not check the folder for symlinks: find:"},
		{"data", "new", "ERR destination already exists"},
		{"newdir", "keep", "ERR destination folder does not exist"},
		{"newdir", "new", "DST " + home + "/newdir\n"},
		{"data", "merge", "ERR bad destination mode"},
	} {
		if c.dst == "unreadable" && os.Getuid() == 0 {
			continue
		}
		out := runClaim(t, home, xferVars("TOKEN", tok, "DST", c.dst, "MODE", c.mode), xferDstClaimScript)
		if !strings.Contains(out, c.want) {
			t.Errorf("%s (%s): got %q, want %q", c.dst, c.mode, out, c.want)
		}
		if c.mode != "new" && strings.Contains(out, "DST ") {
			t.Errorf("%s (%s): an existing directory answered as created: %s", c.dst, c.mode, out)
		}
	}
	for _, d := range []string{"missing", "newdir/x"} {
		if _, err := os.Lstat(filepath.Join(home, d)); !os.IsNotExist(err) {
			t.Errorf("%s created", d)
		}
	}
	// The scan's own bound, apart from the script deadline: a walk that
	// does not finish refuses, it does not pass.
	slow := strings.Replace(xferDstClaimScript, `timeout `+xferScanTimeout+` find "$d" -type l -print -quit`, `timeout 0.1 sleep 5`, 1)
	if slow == xferDstClaimScript {
		t.Fatal("scan command not found in the script")
	}
	if out := runClaim(t, home, xferVars("TOKEN", tok, "DST", "data", "MODE", "keep"), slow); !strings.Contains(out, "ERR could not finish checking the folder for symlinks within "+xferScanTimeout+" s") {
		t.Errorf("timeout: %s", out)
	}
	// The key line names the existing directory, with the usual options.
	out := runJobScript(t, home, xferVars("TOKEN", tok, "DST", "keyed", "BLOB", testBlob, "MODE", "keep"), xferDstClaimScript+xferKeyAddScript)
	if line := xferField(strings.Split(out, "\n"), "KEYLINE"); !strings.Contains(line, `command="`+xferRrsync+` `+xferRrsyncOpts+` '`+home+`/keyed'" ssh-ed25519 `+testBlob) {
		t.Errorf("key line %q", line)
	}
}

func TestParseDstClaim(t *testing.T) {
	for _, c := range []struct {
		lines   []string
		dir     string
		created bool
		ok      bool
	}{
		{[]string{"KEYLINE x", "DST /h/new"}, "/h/new", true, true},
		{[]string{"DIR /h/data", "KEYLINE x"}, "/h/data", false, true},
		{[]string{"DST /h/new", "DIR /h/data"}, "", false, false},
		{[]string{"DIR /h/a", "DIR /h/b"}, "", false, false},
		{[]string{"DIR rel"}, "", false, false},
		{[]string{"DIR /h/a\x01"}, "", false, false},
		{[]string{"DIR /h/\xff"}, "", false, false},
		{[]string{"ERR nope"}, "", false, false},
		{nil, "", false, false},
	} {
		dir, created, err := parseDstClaim(c.lines)
		if dir != c.dir || created != c.created || (err == nil) != c.ok {
			t.Errorf("%q: %q %v %v", c.lines, dir, created, err)
		}
	}
}

// TestXferRrsyncHostilePreexistingLink: rrsync -munge covers links the
// copy writes, not one already in the directory. With a link there, a
// hostile source names its parent as --partial-dir (a relative one that
// rrsync cannot resolve, it is per file) and cuts the transfer: the
// partial file, named like the destination file, lands through the link
// in ~/.ssh (reproduced against rsync/rrsync 3.4.1; --backup-dir and a
// linked directory in the tree are replaced by real directories instead).
// That is why an existing destination holding any symlink is refused at
// the claim.
func TestXferRrsyncHostilePreexistingLink(t *testing.T) {
	needTools(t, "rrsync", "rsync")
	home, bin, seed := t.TempDir(), t.TempDir(), t.TempDir()
	dst := filepath.Join(home, "data")
	os.MkdirAll(filepath.Join(dst, "x"), 0o700)
	os.Mkdir(filepath.Join(home, ".ssh"), 0o700)
	ak := filepath.Join(home, ".ssh", "authorized_keys")
	os.WriteFile(ak, []byte("ORIGINAL\n"), 0o600)
	os.Symlink("../../.ssh", filepath.Join(dst, "x", "pd"))
	os.WriteFile(filepath.Join(bin, "ssh"), []byte(fakeSSH), 0o700)
	env := append(os.Environ(), "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"), "FAKE_DST="+dst)
	os.MkdirAll(filepath.Join(seed, "x"), 0o700)
	os.WriteFile(filepath.Join(seed, "x", "authorized_keys"), append([]byte("ssh-ed25519 ATTACKER\n"), bytes.Repeat([]byte("x"), 20<<20)...), 0o600)
	cmd := exec.Command("rsync", "-rt", "--partial-dir=pd", "--bwlimit=2000", "-e", "ssh", "--", seed+"/", "u@h:")
	cmd.Env = env
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Second)
	cmd.Process.Kill()
	cmd.Wait()
	if got := readFile(t, ak); !strings.HasPrefix(got, "ssh-ed25519 ATTACKER\n") {
		t.Logf("this rsync/rrsync did not write through the link (authorized_keys %q); the refusal below is still required", got)
	}
	out := runClaim(t, home, xferVars("TOKEN", "0123456789abcdef0123456789abcdef", "DST", "data", "MODE", "keep"), xferDstClaimScript)
	if !strings.Contains(out, "ERR the folder contains symlinks") {
		t.Errorf("claim: %s", out)
	}
}

func mtimeOf(t *testing.T, p string) time.Time {
	t.Helper()
	fi, err := os.Stat(p)
	if err != nil {
		t.Fatal(err)
	}
	return fi.ModTime()
}

// Mode keep into an existing directory: a file already there is kept
// whatever its content, missing files arrive, destination-only files
// stay, and the kept count is reported.
func TestXferRunnerKeep(t *testing.T) {
	j := startLocalJobTo(t, xferModeKeep, "data", false, func(home string) {
		os.MkdirAll(filepath.Join(home, "data"), 0o700)
		os.WriteFile(filepath.Join(home, "data", `it's "q".txt`), []byte("OLD"), 0o600)
		os.WriteFile(filepath.Join(home, "data", "extra.txt"), []byte("mine"), 0o600)
	})
	l := j.waitStatus(t, func(l xferLive) bool { return l.rc != "" })
	if l.rc != "0" || l.kept != 1 || l.conflicts != 0 || l.skipped != 1 {
		t.Fatalf("status: %+v", l)
	}
	if res := xferResult(xferFinished, l); !strings.Contains(res, "; 1 existing files kept") {
		t.Errorf("result %q", res)
	}
	for name, want := range map[string]string{`it's "q".txt`: "OLD", "extra.txt": "mine"} {
		if got := readFile(t, filepath.Join(j.dst, name)); got != want {
			t.Errorf("%s = %q", name, got)
		}
	}
	if fi, err := os.Stat(filepath.Join(j.dst, "sub", "big")); err != nil || fi.Size() != 3<<20 {
		t.Errorf("missing file not copied: %v", err)
	}
	if !j.keyGone(t) {
		t.Error("key left")
	}
}

// Mode keep where everything exists already: nothing is written and the
// directory's own timestamp is left alone (-O), not set to the source's;
// a file meeting a directory of its name, or the reverse, is a conflict
// counted apart from kept files. Both are left as they are; a source
// directory meeting a file still has its contents fail, so the run ends
// partial (rsync 23), which the result says along with the counts.
func TestXferRunnerKeepConflictsAndTimes(t *testing.T) {
	old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	j := startLocalJobTo(t, xferModeKeep, "data", false, func(home string) {
		os.MkdirAll(filepath.Join(home, "data", `it's "q".txt`), 0o700) // a directory where the source has a file
		os.WriteFile(filepath.Join(home, "data", "sub"), []byte("a file where the source has a directory"), 0o600)
		os.Chtimes(filepath.Join(home, "data"), old, old)
	})
	l := j.waitStatus(t, func(l xferLive) bool { return l.rc != "" })
	if l.rc != "23" || l.kept != 0 || l.conflicts != 2 {
		t.Fatalf("status: %+v", l)
	}
	if res := xferResult(xferFinished, l); !strings.HasPrefix(res, "partial: some files could not be transferred; ") || !strings.Contains(res, "; 2 name conflicts (a file against a folder) skipped — ") {
		t.Errorf("result %q", res)
	}
	if fi, _ := os.Stat(filepath.Join(j.dst, `it's "q".txt`)); fi == nil || !fi.IsDir() {
		t.Error("directory replaced by the source's file")
	}
	if got := readFile(t, filepath.Join(j.dst, "sub")); got != "a file where the source has a directory" {
		t.Errorf("file replaced by the source's directory: %q", got)
	}
	if got := mtimeOf(t, j.dst); !got.Equal(old) {
		t.Errorf("directory mtime changed to %v", got)
	}
}

// Mode replace: a file that differs is replaced by rename (a second name
// of it keeps the old content: nothing is written through a link), one
// that matches in size and time is left alone, destination-only files
// stay, a file gives way to the source's directory of that name and an
// empty directory to the source's file.
func TestXferRunnerReplace(t *testing.T) {
	j := startLocalJobTo(t, xferModeReplace, "data", false, func(home string) {
		// A second source file, hard-linked on the destination: the copy
		// replaces data/shared, and other/shared must keep the old content.
		os.MkdirAll(filepath.Join(home, "src dir"), 0o700)
		os.WriteFile(filepath.Join(home, "src dir", "shared"), []byte("NEW SHARED"), 0o600)
		os.MkdirAll(filepath.Join(home, "data", `it's "q".txt`), 0o700) // empty directory vs source file
		os.WriteFile(filepath.Join(home, "data", "sub"), []byte("file vs source directory"), 0o600)
		os.WriteFile(filepath.Join(home, "data", "extra.txt"), []byte("mine"), 0o600)
		os.WriteFile(filepath.Join(home, "data", "link"), []byte("a file where the source has a symlink"), 0o600)
		os.MkdirAll(filepath.Join(home, "other"), 0o700)
		os.WriteFile(filepath.Join(home, "other", "shared"), []byte("OLD SHARED"), 0o600)
		os.Link(filepath.Join(home, "other", "shared"), filepath.Join(home, "data", "shared"))
		old := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC) // same size as the source's: the time must differ
		os.Chtimes(filepath.Join(home, "other", "shared"), old, old)
	})
	l := j.waitStatus(t, func(l xferLive) bool { return l.rc != "" })
	if l.rc != "0" || l.kept != 0 || l.conflicts != 0 || l.skipped != 1 {
		t.Fatalf("status: %+v", l)
	}
	for name, want := range map[string]string{`it's "q".txt`: "hello", "extra.txt": "mine", "shared": "NEW SHARED", "link": "a file where the source has a symlink"} {
		if got := readFile(t, filepath.Join(j.dst, name)); got != want {
			t.Errorf("%s = %q", name, got)
		}
	}
	if got := readFile(t, filepath.Join(j.home, "other", "shared")); got != "OLD SHARED" {
		t.Errorf("written through the hard link: %q", got)
	}
	if fi, err := os.Stat(filepath.Join(j.dst, "sub", "big")); err != nil || fi.Size() != 3<<20 {
		t.Errorf("sub did not become the source's directory: %v", err)
	}
}

// Mode replace, a non-empty directory where the source has a file: that
// item fails (rsync 23, partial), the rest of the copy arrives, and
// nothing of the directory is removed.
func TestXferRunnerReplaceNonEmptyConflict(t *testing.T) {
	j := startLocalJobTo(t, xferModeReplace, "data", false, func(home string) {
		os.MkdirAll(filepath.Join(home, "data", `it's "q".txt`, "inner"), 0o700)
		os.WriteFile(filepath.Join(home, "data", `it's "q".txt`, "inner", "keepme"), []byte("k"), 0o600)
	})
	l := j.waitStatus(t, func(l xferLive) bool { return l.rc != "" })
	if l.rc != "23" {
		t.Fatalf("status: %+v", l)
	}
	if res := xferResult(xferFinished, l); !strings.HasPrefix(res, "partial: some files could not be transferred") {
		t.Errorf("result %q", res)
	}
	if got := readFile(t, filepath.Join(j.dst, `it's "q".txt`, "inner", "keepme")); got != "k" {
		t.Errorf("non-empty directory touched: %q", got)
	}
	if fi, err := os.Stat(filepath.Join(j.dst, "sub", "big")); err != nil || fi.Size() != 3<<20 {
		t.Errorf("the rest did not arrive: %v", err)
	}
}

func TestXferRsyncOpts(t *testing.T) {
	for mode, want := range map[string]string{xferModeNew: "--info=progress2", "": "--info=progress2", xferModeKeep: "-O --ignore-existing --info=progress2,skip2", xferModeReplace: "-O --info=progress2"} {
		if got := xferRsyncOpts(mode); got != want {
			t.Errorf("%q: %q", mode, got)
		}
	}
	v, err := xferLaunchVars(strings.Repeat("a", 32), "/home/u/src", xferDest{User: "u", Host: "h", Port: 22, HostKey: "ssh-ed25519 AAAAC3"}, xferModeKeep)
	if err != nil || !strings.Contains(v, "ROPTS="+strings.ReplaceAll(shellQuote("-O --ignore-existing --info=progress2,skip2"), "'", `'\''`)) {
		t.Errorf("launch vars: %v %s", err, v)
	}
}
