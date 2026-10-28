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
			got = readFile(t, filepath.Join(dir, "progress"))
			if len(got) > 513+len(" [truncated]") || !strings.HasSuffix(got, " [truncated]\n") {
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
			if !strings.HasPrefix(line, `restrict,expiry-time="`) || !strings.Contains(line, `-wo -no-del '`+home+`/copy dest'" ssh-ed25519 `+testBlob+" pfm-xfer-"+tok) {
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
SSH_ORIGINAL_COMMAND="$*" exec rrsync -wo -no-del "$FAKE_DST"
`

type localJob struct {
	home, bin, tok, blob, dst string
	env                       []string
}

// startLocalJob runs prepare, claim and launch locally, with real rsync
// and rrsync and a stand-in for ssh; with slow, the stand-in waits before
// connecting, so the runner is still running when the test acts on it.
func startLocalJob(t *testing.T, slow bool) localJob {
	t.Helper()
	needTools(t, "perl", "flock", "rrsync", "rsync", "ssh-keygen", "setsid", "mkfifo")
	home := t.TempDir()
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
	out = runJobScript(t, home, xferVars("TOKEN", tok, "DST", "copy", "BLOB", blob), xferDstClaimScript+xferKeyAddScript, env...)
	dst := xferField(strings.Split(out, "\n"), "DST")
	env = append(env, "FAKE_DST="+dst)
	run := xferVars("SRC", src, "DEST", "u@example.invalid:", "PORT", "22", "HKALG", "ecdsa-sha2-nistp256")
	out = runJobScript(t, home, xferVars("TOKEN", tok, "KNOWN", "example.invalid ecdsa-sha2-nistp256 AAAA", "RUNVARS", run), xferLaunchScript, env...)
	if !strings.Contains(out, "LAUNCHED") {
		t.Fatalf("launch: %s", out)
	}
	j := localJob{home: home, bin: bin, tok: tok, blob: blob, dst: dst, env: env}
	t.Cleanup(func() { // never leave a runner behind
		if pid := strings.TrimSpace(readFileOr(filepath.Join(home, xferJobDir, tok, "pid"))); pid != "" {
			exec.Command("kill", "-KILL", "--", "-"+pid).Run()
		}
	})
	return j
}

func readFileOr(p string) string { b, _ := os.ReadFile(p); return string(b) }

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
	run := xferVars("SRC", "x", "DEST", "u@h:", "PORT", "22", "HKALG", "ecdsa-sha2-nistp256")
	if out := runJobScript(t, j.home, xferVars("TOKEN", j.tok, "KNOWN", "x", "RUNVARS", run), xferLaunchScript, j.env...); !strings.Contains(out, "LAUNCHED already") {
		t.Errorf("relaunch: %s", out)
	}
	l := j.waitStatus(t, func(l xferLive) bool { return l.rc != "" })
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
	sshPid := ""
	for i := 0; i < 50 && sshPid == ""; i++ {
		sshPid = strings.TrimSpace(readFileOr(filepath.Join(j.bin, "ssh.pid")))
		time.Sleep(50 * time.Millisecond)
	}
	out := runJobScript(t, j.home, xferVars("TOKEN", j.tok), xferCancelScript)
	if !strings.Contains(out, "SIGNALLED") {
		t.Fatalf("cancel: %s", out)
	}
	l := j.waitStatus(t, func(l xferLive) bool { return l.rc != "" })
	if l.rc != "cancelled" || l.alive || xferDecide(xferRunning, l, true) != xferFinished || xferResult(xferFinished, l) != "cancelled" {
		t.Fatalf("after cancel: %+v %q", l, xferResult(xferFinished, l))
	}
	if !j.keyGone(t) {
		t.Error("key left after cancel")
	}
	if sshPid != "" {
		for i := 0; i < 40; i++ {
			if _, err := os.Stat("/proc/" + sshPid); os.IsNotExist(err) {
				break
			}
			time.Sleep(50 * time.Millisecond)
		}
		if _, err := os.Stat("/proc/" + sshPid); err == nil {
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

func ptr(s string) *string { return &s }
