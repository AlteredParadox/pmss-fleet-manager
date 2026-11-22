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
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"pmss-fleet-manager/internal/sshx"
	"pmss-fleet-manager/internal/store"
)

// testSSHServer is an in-process sshd standing in for a box, so the
// manager's own paths (poller.conn, the job scripts, cleanup) run end to
// end instead of stopping at a refused dial. Any user with password "x"
// is let in; "exec" runs the command through the local sh with HOME (and
// XDG_RUNTIME_DIR, where the probe keeps its lock files) set to
// root/<user>, stdin, stdout and stderr relayed, and the exit status
// sent. Other channel requests are refused and global requests
// discarded. It returns the listener's port and the host key in
// authorized_keys format, for the box record's pin.
func testSSHServer(t *testing.T, root string) (port int, hostKey string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if string(pw) != "x" {
				return nil, errors.New("wrong password")
			}
			return nil, nil
		},
	}
	cfg.AddHostKey(signer)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &testSSHState{procs: map[*exec.Cmd]struct{}{}}
	// Registered after the caller's TempDir for root, so it runs before
	// that is removed: no command of this server, a stand-in still sleeping
	// or a poll of a box, may write into a home being deleted (the flaky
	// "directory not empty" of TestChangePasswordLostConnection).
	t.Cleanup(func() { ln.Close(); srv.shutdown() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveTestSSH(conn, cfg, root, srv)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

// testSSHState is what a test server's cleanup must end: its client
// connections (so no session starts after it) and the commands it runs,
// each in its own process group so a stand-in's children go too.
type testSSHState struct {
	mu     sync.Mutex
	closed bool
	conns  []*ssh.ServerConn
	procs  map[*exec.Cmd]struct{}
	wg     sync.WaitGroup
}

// start runs cmd unless the server is shutting down.
func (s *testSSHState) start(cmd *exec.Cmd) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return false
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return false
	}
	s.procs[cmd] = struct{}{}
	s.wg.Add(1)
	return true
}

func (s *testSSHState) done(cmd *exec.Cmd) {
	s.mu.Lock()
	delete(s.procs, cmd)
	s.mu.Unlock()
	s.wg.Done()
}

// shutdown closes every connection, kills every running command's process
// group and waits for them all.
func (s *testSSHState) shutdown() {
	s.mu.Lock()
	s.closed = true
	for _, c := range s.conns {
		c.Close()
	}
	for cmd := range s.procs {
		syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	s.mu.Unlock()
	s.wg.Wait()
}

func serveTestSSH(conn net.Conn, cfg *ssh.ServerConfig, root string, srv *testSSHState) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	srv.mu.Lock()
	if srv.closed {
		srv.mu.Unlock()
		return
	}
	srv.conns = append(srv.conns, sc)
	srv.mu.Unlock()
	go ssh.DiscardRequests(reqs)
	home := filepath.Join(root, sc.User())
	for nc := range chans {
		if nc.ChannelType() != "session" {
			nc.Reject(ssh.UnknownChannelType, "sessions only")
			continue
		}
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			defer ch.Close()
			for r := range creqs {
				if r.Type != "exec" {
					r.Reply(false, nil)
					continue
				}
				r.Reply(true, nil)
				var p struct{ Cmd string }
				ssh.Unmarshal(r.Payload, &p)
				os.MkdirAll(home, 0o700)
				cmd := exec.Command("sh", "-c", p.Cmd)
				cmd.Env = append(os.Environ(), "HOME="+home, "XDG_RUNTIME_DIR="+home)
				cmd.Dir = home
				cmd.Stdin, cmd.Stdout, cmd.Stderr = ch, ch, ch.Stderr()
				status := 0
				if !srv.start(cmd) {
					ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{255}))
					return
				}
				err := cmd.Wait()
				srv.done(cmd)
				if err != nil {
					var ee *exec.ExitError
					if errors.As(err, &ee) {
						status = ee.ExitCode()
					} else {
						status = 255
					}
				}
				ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{uint32(status)}))
				return
			}
		}()
	}
}

// sshTestBox adds a box that reaches the test server as user, pinned to
// its host key, and waits for its poller.
func sshTestBox(t *testing.T, m *Manager, name, user string, port int, hostKey string) *store.Box {
	t.Helper()
	b := &store.Box{Name: name, Host: "127.0.0.1", Port: port, User: user, AuthMethod: "password", Password: "x", HostKey: hostKey, Enabled: true}
	if err := m.AddBox(context.Background(), b); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 500 && m.poller(b.ID) == nil; i++ {
		time.Sleep(10 * time.Millisecond)
	}
	if m.poller(b.ID) == nil {
		t.Fatal("no poller for " + name)
	}
	return b
}

// The test server ends its commands when its test ends, before the test's
// directories are removed: a command still running then (a stand-in that
// sleeps, a poll) must not write into a home being deleted. Here one that
// would write a file a second later is still running when its subtest
// ends; the file never appears.
func TestSSHServerEndsCommands(t *testing.T) {
	keep := t.TempDir()
	late := filepath.Join(keep, "late")
	t.Run("server", func(t *testing.T) {
		root := t.TempDir()
		port, hostKey := testSSHServer(t, root)
		cli, err := sshx.Dial(context.Background(), sshx.Config{Host: "127.0.0.1", Port: port, User: "u", AuthMethod: "password", Password: "x", HostKey: hostKey, Timeout: 5 * time.Second})
		if err != nil {
			t.Fatal(err)
		}
		defer cli.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
		defer cancel()
		cli.Run(ctx, "sh", strings.NewReader("sleep 1; touch '"+late+"'\n")) // returns at the deadline; the command runs on
	})
	time.Sleep(1500 * time.Millisecond)
	if _, err := os.Stat(late); err == nil {
		t.Fatal("a command of the test server outlived its test")
	}
}
