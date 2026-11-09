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
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

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
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go serveTestSSH(conn, cfg, root)
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, strings.TrimSpace(string(ssh.MarshalAuthorizedKey(signer.PublicKey())))
}

func serveTestSSH(conn net.Conn, cfg *ssh.ServerConfig, root string) {
	sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
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
				if err := cmd.Run(); err != nil {
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
