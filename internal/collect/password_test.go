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
	"crypto/pbkdf2"
	"crypto/sha512"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"

	"pmss-fleet-manager/internal/store"
)

func TestValidNewPassword(t *testing.T) {
	for _, c := range []struct {
		pw string
		ok bool
	}{
		{"Tr0ub4dor&3x", true},
		{"with space inside", true},
		{strings.Repeat("a", 72), true},
		{"short7!", false},
		{strings.Repeat("a", 73), false},
		{" leading-space", false},
		{"trailing-space ", false},
		{"new\nline-in-it", false},
		{"tab\tin-the-middle", false},
		{"ümlaut-password", false},
		{"nul\x00byte-inside", false},
	} {
		err := ValidNewPassword(c.pw)
		if (err == nil) != c.ok {
			t.Errorf("%q: %v, want ok=%v", c.pw, err, c.ok)
		}
		if err != nil && !errors.Is(err, ErrPasswordInput) {
			t.Errorf("%q: not an input error: %v", c.pw, err)
		}
		if err != nil && strings.Contains(err.Error(), c.pw) {
			t.Errorf("the refusal quotes the password: %v", err)
		}
	}
}

// The entry is what PMSS writes itself: the account, bcrypt under $2y$ at
// cost 10, and it verifies against the password.
func TestHtpasswdEntry(t *testing.T) {
	e, err := htpasswdEntry("rc2", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(e, "rc2:$2y$10$") || len(e) != len("rc2:")+60 {
		t.Fatalf("entry shape: %q", e)
	}
	if err := bcrypt.CompareHashAndPassword([]byte("$2a$"+e[len("rc2:$2y$"):]), []byte("correct horse battery")); err != nil {
		t.Errorf("the hash does not verify: %v", err)
	}
	for _, u := range []string{"", "a:b", "a b", "a\nb", "-x", strings.Repeat("u", 65)} {
		if _, err := htpasswdEntry(u, "correct horse battery"); err == nil {
			t.Errorf("user %q accepted", u)
		}
	}
}

// The vector is Python's hashlib.pbkdf2_hmac('sha512', pw, salt, 100000, 64)
// for salt bytes 0..15, in qBittorrent's "@ByteArray(salt:key)" spelling.
func TestQbtPasswordValue(t *testing.T) {
	salt := make([]byte, 16)
	for i := range salt {
		salt[i] = byte(i)
	}
	got, err := qbtPasswordValue("correct horse battery", salt)
	if err != nil {
		t.Fatal(err)
	}
	want := `"@ByteArray(AAECAwQFBgcICQoLDA0ODw==:xfNv5sfjsFq9EBwFsrlao8ynI2+zZDlLHi4cWnEYKlXcH1T8o2dIo3CGf3XoU2p0sW7aP+RvqVso5LGWh7rgiQ==)"`
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

// passwd's own words, as Debian 10 to 13 print them without a terminal
// (LC_ALL=C, stderr joined to stdout).
func TestPasswdRefusal(t *testing.T) {
	for _, c := range []struct{ out, want string }{
		{"Current password: passwd: Authentication token manipulation error\npasswd: password unchanged\nChanging password for tenant.\n",
			"the current password was not accepted by the box"},
		{"Current password: New password: Retype new password: You must choose a longer password.\nNew password: Password change has been aborted.\npasswd: Authentication token manipulation error\npasswd: password unchanged\nChanging password for tenant.\n",
			"the box's passwd refused the new password: You must choose a longer password."},
		{"Current password: New password: Retype new password: The password has not been changed.\nNew password: Password change has been aborted.\npasswd: Authentication token manipulation error\npasswd: password unchanged\n",
			"the box's passwd refused the new password: The password has not been changed."},
		{"Current password: New password: Password change has been aborted.\npasswd: Authentication token manipulation error\npasswd: password unchanged\n",
			"the box's passwd refused the change"},
		{"Current password: New password: Retype new password: You must choose a longer password\nNew password: Password change aborted.\npasswd: Authentication token manipulation error\npasswd: password unchanged\n",
			"the box's passwd refused the new password: You must choose a longer password"}, // Debian 10's wording
		{"Current password: You must wait longer to change your password.\npasswd: Authentication token manipulation error\npasswd: password unchanged\n",
			"the box's passwd refused the change: You must wait longer to change your password."},
		{"", "the current password was not accepted by the box"},
	} {
		if got := passwdRefusal(c.out); got != c.want {
			t.Errorf("%q:\n got  %q\n want %q", c.out, got, c.want)
		}
	}
}

func TestHereDocAndScrub(t *testing.T) {
	d, err := hereDoc("a b", `$(id) \n 'x'`)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(d, "\n")
	if len(lines) != 5 || !strings.HasPrefix(lines[0], "<<'PFM_") || lines[0] != "<<'"+lines[3]+"'" || lines[1] != "a b" || lines[2] != `$(id) \n 'x'` {
		t.Errorf("here-document: %q", d)
	}
	for _, bad := range []string{"a\nb", "a\rb", "a\x00b"} {
		if _, err := hereDoc(bad); err == nil {
			t.Errorf("%q accepted as one line", bad)
		}
	}
	if got := pwScrub("old=hunter22 new=Sup3r-secret!", "hunter22", "Sup3r-secret!", ""); got != "old=[redacted] new=[redacted]" {
		t.Errorf("scrub: %q", got)
	}
	if got := pwClean("a\x1b[31m\nb\tc  \u00e9 d"); got != "a[31m b c d" {
		t.Errorf("clean: %q", got)
	}
	if got := lastLine("was_running=1\nthe reason\nneeds_start=1\n"); got != "the reason" {
		t.Errorf("lastLine: %q", got)
	}
}

// needDash skips where sh is not a shell the scripts accept (they refuse
// bash, zsh and ksh: see pwPrelude).
func needDash(t *testing.T) {
	t.Helper()
	out, err := exec.Command("sh", "-c", `printf %s "${BASH_VERSION-}${ZSH_VERSION-}${KSH_VERSION-}"`).Output()
	if err != nil || len(out) != 0 {
		if os.Getenv("CI") != "" {
			t.Fatalf("sh is not dash here (%q, %v); CI runs on Debian or Ubuntu", out, err)
		}
		t.Skip("sh is not a plain POSIX shell here")
	}
}

// fakePasswd is a stand-in for the box's passwd: the account's password
// is the content of $HOME/shadow, it reads the three lines the real one
// reads without a terminal and answers in its words. It wants 12
// characters, so a password the dashboard admits can still be refused.
// With $HOME/passwd-hangs present it outlasts the step's deadline.
const fakePasswd = `#!/bin/sh
[ $# -eq 0 ] || { echo "passwd: unexpected arguments"; exit 2; }
[ ! -e "$HOME/passwd-hangs" ] || exec sleep 3
echo "Changing password for tenant." >&2
printf 'Current password: '
IFS= read -r cur || cur=
if [ "$cur" != "$(cat "$HOME/shadow")" ]; then
  echo "passwd: Authentication token manipulation error"; echo "passwd: password unchanged"; exit 10
fi
printf 'New password: '
IFS= read -r n1 || n1=
printf 'Retype new password: '
IFS= read -r n2 || n2=
if [ "$n1" != "$n2" ]; then echo "Sorry, passwords do not match."; echo "passwd: password unchanged"; exit 10; fi
if [ ${#n1} -lt 12 ]; then
  echo "You must choose a longer password."; printf 'New password: '; echo "Password change has been aborted."
  echo "passwd: Authentication token manipulation error"; echo "passwd: password unchanged"; exit 10
fi
printf '%s' "$n1" > "$HOME/shadow"
echo "passwd: password updated successfully"
`

const pwTestConf = `[Application]
FileLogger\Enabled=true

[Preferences]
WebUI\LocalHostAuth=true
WebUI\Password_PBKDF2="@ByteArray(b2xk:b2xk)"
WebUI\Port=16209
WebUI\Username=u

[WebUI]
Password_PBKDF2="@ByteArray(b2xk:b2xk)"
Port=16209
Username=u
`

// pwBox is a box on the in-process sshd whose home holds the three
// fixtures, with a stand-in passwd first on PATH and a stand-in process
// for the script to stop.
type pwBox struct {
	m    *Manager
	st   *store.Store
	b    *store.Box
	home string
	ctx  context.Context
}

func newPwBox(t *testing.T) *pwBox {
	t.Helper()
	needDash(t)
	needTools(t, "pgrep", "pkill", "mktemp")
	// The script stops processes by name: a name of this test's own, never
	// the qbittorrent-nox the machine may run.
	prev := qbtProcName
	qbtProcName = fmt.Sprintf("pfmq%d", os.Getpid()%100000000)
	t.Cleanup(func() { qbtProcName = prev })
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "passwd"), []byte(fakePasswd), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	m, st, _ := genTestManager(t)
	root := t.TempDir()
	port, hostKey := testSSHServer(t, root)
	home := filepath.Join(root, "u")
	os.MkdirAll(filepath.Join(home, ".lighttpd"), 0o755)
	os.MkdirAll(filepath.Join(home, ".config", "qBittorrent"), 0o755)
	os.WriteFile(filepath.Join(home, "shadow"), []byte("x"), 0o600)
	os.WriteFile(filepath.Join(home, ".lighttpd", ".htpasswd"), []byte("pm-agent:$2y$10$keepme\nu:$apr1$old$old\nu:$apr1$dup$dup\nlast:nonewline"), 0o640)
	os.WriteFile(filepath.Join(home, ".config", "qBittorrent", "qBittorrent.conf"), []byte(pwTestConf), 0o600)
	b := &store.Box{Name: "box", Host: "127.0.0.1", Port: port, User: "u", AuthMethod: "password", Password: "x", HostKey: hostKey, Enabled: true,
		QbtUser: "u", QbtPass: "old-webui-pass", QbtStartCmd: `touch "$HOME/started"`}
	if err := m.AddBox(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500 && m.poller(b.ID) == nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	seedRemote(m, b.ID)
	x := &pwBox{m: m, st: st, b: b, home: home}
	x.ctx = WithBoxRevision(context.Background(), m.poller(b.ID).box.Revision)
	return x
}

// client starts the stand-in for a running qbittorrent-nox and reaps it
// when it is stopped (a zombie would still be listed by pgrep).
func (x *pwBox) client(t *testing.T) (gone <-chan struct{}) {
	t.Helper()
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	bin := filepath.Join(t.TempDir(), qbtProcName)
	data, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "600")
	if err := cmd.Start(); err != nil {
		t.Skipf("cannot run a copy of sleep: %v", err)
	}
	done := make(chan struct{})
	go func() { cmd.Wait(); close(done) }()
	t.Cleanup(func() { cmd.Process.Kill(); <-done })
	return done
}

func (x *pwBox) file(t *testing.T, rel string) string {
	t.Helper()
	return readFile(t, filepath.Join(x.home, rel))
}

func steps(r PasswordResult) string {
	var s []string
	for _, st := range r.Steps {
		s = append(s, st.Target+"="+st.Status)
	}
	return strings.Join(s, " ")
}

func stepOf(t *testing.T, r PasswordResult, target string) PasswordStep {
	t.Helper()
	for _, st := range r.Steps {
		if st.Target == target {
			return st
		}
	}
	t.Fatalf("no %s step in %+v", target, r)
	return PasswordStep{}
}

// qbtVerifies reports whether a Password_PBKDF2 line's value is pw's hash.
func qbtVerifies(t *testing.T, line, pw string) bool {
	t.Helper()
	_, v, ok := strings.Cut(line, "=")
	v = strings.TrimSuffix(strings.TrimPrefix(v, `"@ByteArray(`), `)"`)
	s64, k64, ok2 := strings.Cut(v, ":")
	if !ok || !ok2 {
		t.Fatalf("not a password line: %q", line)
	}
	salt, err1 := base64.StdEncoding.DecodeString(s64)
	key, err2 := base64.StdEncoding.DecodeString(k64)
	if err1 != nil || err2 != nil || len(salt) != 16 || len(key) != 64 {
		t.Fatalf("bad value in %q", line)
	}
	want, err := pbkdf2.Key(sha512.New, pw, salt, 100000, 64)
	if err != nil {
		t.Fatal(err)
	}
	return string(want) == string(key)
}

// All three fixtures over the in-process sshd, through the manager's own
// path: passwd is fed the saved SSH password and the new one, the
// account's .htpasswd line becomes a bcrypt hash with the other lines and
// the mode kept, both password lines of qBittorrent.conf carry the new
// PBKDF2 hash with the client stopped for the edit and started again, and
// the dashboard's own saved logins follow.
func TestChangePasswordAllFixtures(t *testing.T) {
	x := newPwBox(t)
	gone := x.client(t)
	const neu = "N3w-pass word!"
	rev := x.m.poller(x.b.ID).box.Revision
	res, err := x.m.ChangePassword(x.ctx, x.b.ID, PasswordRequest{New: neu, Targets: []string{PwQbt, PwWeb, PwAccount}})
	if err != nil {
		t.Fatal(err)
	}
	if got := steps(res); got != "account=changed web=changed qbt=changed" {
		t.Fatalf("steps: %s (%+v)", got, res)
	}
	if res.Warning != "" || strings.Join(res.Saved, ",") != "ssh,webui" {
		t.Errorf("saved %v, warning %q", res.Saved, res.Warning)
	}
	if got := x.file(t, "shadow"); got != neu {
		t.Errorf("the account password is %q", got)
	}

	ht := strings.Split(x.file(t, ".lighttpd/.htpasswd"), "\n")
	if len(ht) != 4 || ht[0] != "pm-agent:$2y$10$keepme" || ht[2] != "last:nonewline" || ht[3] != "" || !strings.HasPrefix(ht[1], "u:$2y$10$") {
		t.Fatalf(".htpasswd: %q", ht)
	}
	if err := bcrypt.CompareHashAndPassword([]byte("$2a$"+ht[1][len("u:$2y$"):]), []byte(neu)); err != nil {
		t.Errorf("the .htpasswd hash does not verify: %v", err)
	}
	if fi, _ := os.Stat(filepath.Join(x.home, ".lighttpd", ".htpasswd")); fi.Mode().Perm() != 0o640 {
		t.Errorf(".htpasswd mode %v", fi.Mode().Perm())
	}

	select {
	case <-gone:
	default:
		t.Error("the client was not stopped for the edit")
	}
	conf := x.file(t, ".config/qBittorrent/qBittorrent.conf")
	lines := strings.Split(conf, "\n")
	want := strings.Split(pwTestConf, "\n")
	if len(lines) != len(want) {
		t.Fatalf("qBittorrent.conf has %d lines, had %d:\n%s", len(lines), len(want), conf)
	}
	changed := 0
	for i := range want {
		if strings.Contains(want[i], "Password_PBKDF2=") {
			changed++
			key, _, _ := strings.Cut(want[i], "=")
			if !strings.HasPrefix(lines[i], key+`="@ByteArray(`) || !qbtVerifies(t, lines[i], neu) {
				t.Errorf("line %d does not hold the new hash: %q", i, lines[i])
			}
		} else if lines[i] != want[i] {
			t.Errorf("line %d changed: %q, was %q", i, lines[i], want[i])
		}
	}
	if changed != 2 || lines[5] != `WebUI\`+lines[10] {
		t.Errorf("the two sections do not carry one value: %q / %q", lines[5], lines[10])
	}
	if fi, _ := os.Stat(filepath.Join(x.home, ".config", "qBittorrent", "qBittorrent.conf")); fi.Mode().Perm() != 0o600 {
		t.Errorf("qBittorrent.conf mode %v", fi.Mode().Perm())
	}
	if _, err := os.Stat(filepath.Join(x.home, "started")); err != nil {
		t.Errorf("the client was not started again: %v (%q)", err, stepOf(t, res, PwQbt).Detail)
	}
	for _, dir := range []string{".lighttpd", ".config/qBittorrent"} {
		es, _ := os.ReadDir(filepath.Join(x.home, dir))
		for _, e := range es {
			if strings.Contains(e.Name(), ".pfm.") {
				t.Errorf("left behind: %s/%s", dir, e.Name())
			}
		}
	}

	// The dashboard's own logins: both saved passwords are the new one,
	// under a new record revision, and nothing else of the record moved.
	got, err := x.st.GetBox(context.Background(), x.b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Password != neu || got.QbtPass != neu || got.QbtUser != "u" || got.HostKey != x.b.HostKey || got.QbtStartCmd != x.b.QbtStartCmd {
		t.Errorf("stored record: ssh=%q webui=%q user=%q", got.Password, got.QbtPass, got.QbtUser)
	}
	if got.Revision <= rev || x.m.poller(x.b.ID).box.Revision != got.Revision {
		t.Errorf("revision %d → %d, poller on %d", rev, got.Revision, x.m.poller(x.b.ID).box.Revision)
	}
	for _, s := range res.Steps {
		if strings.Contains(s.Detail, neu) {
			t.Errorf("a detail quotes the password: %+v", s)
		}
	}
}

// A refusal by the box's passwd stops there: the reason is reported, the
// other fixtures are not touched, and nothing the dashboard saved changes.
func TestChangePasswordRefusedByPasswd(t *testing.T) {
	x := newPwBox(t)
	gone := x.client(t)
	all := []string{PwAccount, PwWeb, PwQbt}
	htBefore, confBefore := x.file(t, ".lighttpd/.htpasswd"), x.file(t, ".config/qBittorrent/qBittorrent.conf")
	for _, c := range []struct{ cur, neu, why string }{
		{"not-the-password", "N3w-pass word!", "the current password was not accepted by the box"},
		{"", "eight-ch", "the box's passwd refused the new password: You must choose a longer password."},
	} {
		res, err := x.m.ChangePassword(x.ctx, x.b.ID, PasswordRequest{Current: c.cur, New: c.neu, Targets: all})
		if err != nil {
			t.Fatal(err)
		}
		if got := steps(res); got != "account=failed web=skipped qbt=skipped" {
			t.Fatalf("steps: %s (%+v)", got, res)
		}
		if d := stepOf(t, res, PwAccount).Detail; d != c.why {
			t.Errorf("reason %q, want %q", d, c.why)
		}
		if len(res.Saved) != 0 || res.Warning != "" {
			t.Errorf("saved %v, warning %q", res.Saved, res.Warning)
		}
	}
	if x.file(t, "shadow") != "x" || x.file(t, ".lighttpd/.htpasswd") != htBefore || x.file(t, ".config/qBittorrent/qBittorrent.conf") != confBefore {
		t.Error("a fixture changed after a refusal")
	}
	select {
	case <-gone:
		t.Error("the client was stopped although nothing was to be written")
	default:
	}
	if got, _ := x.st.GetBox(context.Background(), x.b.ID); got.Password != "x" || got.QbtPass != "old-webui-pass" {
		t.Error("the saved logins changed after a refusal")
	}
}

// Requests the dashboard refuses before the box is reached.
func TestChangePasswordInput(t *testing.T) {
	x := newPwBox(t)
	for name, req := range map[string]PasswordRequest{
		"short":          {New: "short", Targets: []string{PwWeb}},
		"unknown target": {New: "N3w-pass word!", Targets: []string{"deluge"}},
		"no target":      {New: "N3w-pass word!"},
		"same":           {Current: "N3w-pass word!", New: "N3w-pass word!", Targets: []string{PwAccount}},
		"same as saved":  {New: "x-is-not-it", Current: "x-is-not-it", Targets: []string{PwAccount}},
		"control char":   {Current: "a\nb", New: "N3w-pass word!", Targets: []string{PwAccount}},
	} {
		if _, err := x.m.ChangePassword(x.ctx, x.b.ID, req); !errors.Is(err, ErrPasswordInput) {
			t.Errorf("%s: %v", name, err)
		}
	}
	p := x.m.poller(x.b.ID)
	if _, err := x.m.ChangePassword(WithBoxRevision(context.Background(), p.box.Revision+1), x.b.ID, PasswordRequest{New: "N3w-pass word!", Targets: []string{PwWeb}}); !errors.Is(err, ErrStaleBox) {
		t.Errorf("another revision: %v", err)
	}
	x.m.mutateState(x.b.ID, func(s *BoxState) { s.Ready = false })
	if _, err := x.m.ChangePassword(x.ctx, x.b.ID, PasswordRequest{New: "N3w-pass word!", Targets: []string{PwWeb}}); !errors.Is(err, ErrNotReady) {
		t.Errorf("not probed: %v", err)
	}
	seedRemote(x.m, x.b.ID)
	p.pwMu.Lock()
	if _, err := x.m.ChangePassword(x.ctx, x.b.ID, PasswordRequest{New: "N3w-pass word!", Targets: []string{PwWeb}}); !errors.Is(err, ErrPasswordBusy) {
		t.Errorf("a second change: %v", err)
	}
	p.pwMu.Unlock()
	// No current password typed and none saved: the account cannot be done.
	b := *p.box
	b.AuthMethod = "key"
	if err := x.m.UpdateBox(context.Background(), &b, store.Clears{Password: true}); err != nil {
		t.Fatal(err)
	}
	p = x.m.poller(x.b.ID)
	if _, err := x.m.ChangePassword(WithBoxRevision(context.Background(), p.box.Revision), x.b.ID, PasswordRequest{New: "N3w-pass word!", Targets: []string{PwAccount}}); !errors.Is(err, ErrPasswordInput) || !strings.Contains(err.Error(), "current password") {
		t.Errorf("no current password: %v", err)
	}
	if x.file(t, "shadow") != "x" {
		t.Error("the account password changed")
	}
}

// Fixtures a box does not have are skipped, not created; a file that is
// not what it should be is refused; and the web and qBittorrent steps
// alone need no current password and leave the account as it is.
func TestChangePasswordFixtureStates(t *testing.T) {
	x := newPwBox(t)
	const neu = "N3w-pass word!"
	two := PasswordRequest{New: neu, Targets: []string{PwWeb, PwQbt}}
	ht, conf := filepath.Join(x.home, ".lighttpd", ".htpasswd"), filepath.Join(x.home, ".config", "qBittorrent", "qBittorrent.conf")
	run := func() PasswordResult {
		t.Helper()
		res, err := x.m.ChangePassword(WithBoxRevision(context.Background(), x.m.poller(x.b.ID).box.Revision), x.b.ID, two)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// A configuration without a password line: nothing to change, and the
	// running client is not stopped for it.
	gone := x.client(t)
	os.WriteFile(conf, []byte("[Preferences]\nWebUI\\Port=8080\n[BitTorrent]\nPassword_PBKDF2=not-in-a-webui-section\n"), 0o600)
	os.Remove(ht)
	res := run()
	if got := steps(res); got != "web=skipped qbt=skipped" {
		t.Fatalf("absent fixtures: %s (%+v)", got, res)
	}
	if _, err := os.Lstat(ht); err == nil {
		t.Error(".htpasswd was created")
	}
	select {
	case <-gone:
		t.Error("the client was stopped for a configuration without a password")
	default:
	}
	if x.file(t, "shadow") != "x" {
		t.Error("the account password changed without being asked for")
	}

	// A symlink in place of either file is refused and its target kept.
	outside := filepath.Join(t.TempDir(), "target")
	os.WriteFile(outside, []byte("u:keep\n[WebUI]\nPassword_PBKDF2=keep\n"), 0o600)
	os.Remove(conf)
	os.Symlink(outside, ht)
	os.Symlink(outside, conf)
	res = run()
	if got := steps(res); got != "web=failed qbt=failed" {
		t.Fatalf("symlinks: %s (%+v)", got, res)
	}
	if readFile(t, outside) != "u:keep\n[WebUI]\nPassword_PBKDF2=keep\n" {
		t.Error("a symlink's target was written")
	}

	// An interrupted save pending: the edit is refused, and the client the
	// step stopped is started again all the same.
	os.Remove(ht)
	os.Remove(conf)
	os.WriteFile(conf, []byte(pwTestConf), 0o600)
	os.WriteFile(strings.TrimSuffix(conf, ".conf")+"_new.conf", []byte("[Preferences]\n"), 0o600)
	res = run()
	q := stepOf(t, res, PwQbt)
	if q.Status != PwFailed || !strings.Contains(q.Detail, "interrupted save") || !strings.Contains(q.Detail, "started again") {
		t.Errorf("pending save: %+v", q)
	}
	if x.file(t, ".config/qBittorrent/qBittorrent.conf") != pwTestConf {
		t.Error("the configuration was edited under a pending save")
	}
	if _, err := os.Stat(filepath.Join(x.home, "started")); err != nil {
		t.Errorf("the stopped client was not started again: %v", err)
	}
	if len(res.Saved) != 0 {
		t.Errorf("saved %v after a failed step", res.Saved)
	}

	// With the client down, the edit goes through and nothing is started.
	os.Remove(strings.TrimSuffix(conf, ".conf") + "_new.conf")
	os.Remove(filepath.Join(x.home, "started"))
	res = run()
	q = stepOf(t, res, PwQbt)
	if q.Status != PwChanged || !strings.Contains(q.Detail, "not running") {
		t.Errorf("stopped client: %+v", q)
	}
	if _, err := os.Stat(filepath.Join(x.home, "started")); err == nil {
		t.Error("a client that was not running was started")
	}
	if strings.Join(res.Saved, ",") != "webui" {
		t.Errorf("saved %v", res.Saved)
	}
	if got, _ := x.st.GetBox(context.Background(), x.b.ID); got.Password != "x" || got.QbtPass != neu {
		t.Errorf("stored: ssh=%q webui=%q", got.Password, got.QbtPass)
	}
}

// A connection lost while passwd runs leaves the outcome open: one login
// with the new password decides it, and when even that cannot be tried
// the answer is "unknown" and nothing else is touched or saved.
func TestChangePasswordLostConnection(t *testing.T) {
	x := newPwBox(t)
	prev := pwStepTimeout
	pwStepTimeout = 400 * time.Millisecond
	t.Cleanup(func() { pwStepTimeout = prev })
	os.WriteFile(filepath.Join(x.home, "passwd-hangs"), nil, 0o600)
	const neu = "N3w-pass word!"
	htBefore := x.file(t, ".lighttpd/.htpasswd")
	for _, c := range []struct {
		accepted bool
		err      error
		want     string
		saved    string
	}{
		{false, errors.New("dial tcp: connection refused"), "account=unknown web=skipped qbt=skipped", ""},
		{false, nil, "account=failed web=skipped qbt=skipped", ""},
		{true, nil, "account=changed web=changed qbt=changed", "ssh,webui"},
	} {
		p := x.m.poller(x.b.ID)
		var asked string
		p.pwProbe = func(_ context.Context, pw string) (bool, error) { asked = pw; return c.accepted, c.err }
		res, err := x.m.ChangePassword(WithBoxRevision(context.Background(), p.box.Revision), x.b.ID, PasswordRequest{New: neu, Targets: []string{PwAccount, PwWeb, PwQbt}})
		if err != nil {
			t.Fatal(err)
		}
		if got := steps(res); got != c.want {
			t.Fatalf("probe (%v, %v): %s (%+v)", c.accepted, c.err, got, res)
		}
		if asked != neu {
			t.Errorf("the probe tried %q", asked)
		}
		if strings.Join(res.Saved, ",") != c.saved {
			t.Errorf("saved %v, want %q", res.Saved, c.saved)
		}
		if !c.accepted && x.file(t, ".lighttpd/.htpasswd") != htBefore {
			t.Error(".htpasswd changed although the account password is not known to have")
		}
	}
}

// The record moved to another endpoint while the change ran: the new
// password is not written onto it, and the result says so.
func TestSavePasswordsRefusesMovedRecord(t *testing.T) {
	x := newPwBox(t)
	acted := *x.m.poller(x.b.ID).box
	acted.User = "someone-else"
	saved, err := x.m.savePasswords(&acted, "N3w-pass word!", "")
	if err == nil || len(saved) != 0 {
		t.Fatalf("saved %v, err %v", saved, err)
	}
	if got, _ := x.st.GetBox(context.Background(), x.b.ID); got.Password != "x" {
		t.Errorf("stored SSH password %q", got.Password)
	}
	// A record that no longer stores a login gets none added.
	p := x.m.poller(x.b.ID)
	b := *p.box
	b.AuthMethod = "key"
	if err := x.m.UpdateBox(context.Background(), &b, store.Clears{Password: true, QbtPass: true}); err != nil {
		t.Fatal(err)
	}
	saved, err = x.m.savePasswords(x.m.poller(x.b.ID).box, "N3w-pass word!", "N3w-pass word!")
	if err != nil || len(saved) != 0 {
		t.Fatalf("saved %v, err %v", saved, err)
	}
	if got, _ := x.st.GetBox(context.Background(), x.b.ID); got.Password != "" || got.QbtPass != "" {
		t.Errorf("a login was added: ssh=%q webui=%q", got.Password, got.QbtPass)
	}
}
