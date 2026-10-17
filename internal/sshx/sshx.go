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

// Package sshx wraps golang.org/x/crypto/ssh with the small surface the
// collector needs: connect with key/agent/password, run a command with a
// deadline, and dial unix or tcp endpoints on the remote side.
package sshx

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/agent"
)

// Config describes how to reach one box.
type Config struct {
	Host       string
	Port       int
	User       string
	AuthMethod string // "key", "agent", "password", "" = try everything available
	KeyPath    string
	Password   string
	// HostKey is the pinned public key in authorized_keys format ("ssh-ed25519 AAAA...").
	// Empty means trust on first use; the key seen is then available via Client.HostKey().
	HostKey string
	Timeout time.Duration
}

// ErrHostKeyMismatch is returned when the remote host key differs from the pinned one.
var ErrHostKeyMismatch = errors.New("ssh: host key mismatch")

// Client is a live SSH connection to one box.
type Client struct {
	cfg     Config
	mu      sync.Mutex
	cli     *ssh.Client
	hostKey string
}

// Dial opens a connection. The caller owns the returned client and must Close it.
func Dial(ctx context.Context, cfg Config) (*Client, error) {
	if cfg.Port == 0 {
		cfg.Port = 22
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 15 * time.Second
	}
	auths, cleanup, err := authMethods(cfg)
	if err != nil {
		return nil, err
	}
	defer cleanup() // the agent socket is only needed during the handshake
	c := &Client{cfg: cfg}
	sshCfg := &ssh.ClientConfig{
		User:            cfg.User,
		Auth:            auths,
		Timeout:         cfg.Timeout,
		HostKeyCallback: c.hostKeyCallback,
	}
	addr := net.JoinHostPort(cfg.Host, fmt.Sprint(cfg.Port))
	dialer := net.Dialer{Timeout: cfg.Timeout}
	conn, err := dialer.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	// Bound the handshake by the context too.
	done := make(chan struct{})
	var (
		cc    ssh.Conn
		chans <-chan ssh.NewChannel
		reqs  <-chan *ssh.Request
		herr  error
	)
	go func() {
		cc, chans, reqs, herr = ssh.NewClientConn(conn, addr, sshCfg)
		close(done)
	}()
	select {
	case <-ctx.Done():
		// The handshake may be blocked on the agent (a signer request it
		// never answers), not the box: close that socket too, or <-done
		// never comes and every poller on agent auth hangs here.
		cleanup()
		conn.Close()
		<-done
		return nil, ctx.Err()
	case <-done:
	}
	if herr != nil {
		conn.Close()
		return nil, herr
	}
	c.cli = ssh.NewClient(cc, chans, reqs)
	return c, nil
}

func (c *Client) hostKeyCallback(hostname string, remote net.Addr, key ssh.PublicKey) error {
	seen := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(key)))
	c.hostKey = seen
	if c.cfg.HostKey == "" {
		return nil
	}
	if strings.TrimSpace(c.cfg.HostKey) != seen {
		return fmt.Errorf("%w: got %s", ErrHostKeyMismatch, Fingerprint(seen))
	}
	return nil
}

// HostKey returns the remote host key seen during the handshake, authorized_keys format.
func (c *Client) HostKey() string { return c.hostKey }

// Fingerprint renders a SHA256 fingerprint for an authorized_keys-format public key.
func Fingerprint(authorizedKey string) string {
	pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(authorizedKey))
	if err != nil {
		return "?"
	}
	sum := sha256.Sum256(pk.Marshal())
	return "SHA256:" + strings.TrimRight(base64.StdEncoding.EncodeToString(sum[:]), "=")
}

func authMethods(cfg Config) ([]ssh.AuthMethod, func(), error) {
	var methods []ssh.AuthMethod
	cleanup := func() {}
	useKey := cfg.AuthMethod == "key" || cfg.AuthMethod == ""
	useAgent := cfg.AuthMethod == "agent" || cfg.AuthMethod == ""
	usePass := cfg.AuthMethod == "password" || (cfg.AuthMethod == "" && cfg.Password != "")

	if useKey {
		paths := []string{}
		if cfg.KeyPath != "" {
			paths = append(paths, expandHome(cfg.KeyPath))
		} else {
			home, _ := os.UserHomeDir()
			for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
				paths = append(paths, filepath.Join(home, ".ssh", n))
			}
		}
		var signers []ssh.Signer
		for _, p := range paths {
			pem, err := os.ReadFile(p)
			if err != nil {
				if cfg.KeyPath != "" {
					return nil, cleanup, fmt.Errorf("read key %s: %w", p, err)
				}
				continue
			}
			s, err := ssh.ParsePrivateKey(pem)
			if err != nil {
				if cfg.KeyPath != "" {
					return nil, cleanup, fmt.Errorf("parse key %s: %w", p, err)
				}
				continue
			}
			signers = append(signers, s)
		}
		if len(signers) > 0 {
			methods = append(methods, ssh.PublicKeys(signers...))
		} else if cfg.AuthMethod == "key" {
			return nil, cleanup, errors.New("ssh: no usable private key in ~/.ssh (set a key path, or the key needs a passphrase)")
		}
	}
	if useAgent {
		if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
			if conn, err := net.DialTimeout("unix", sock, 5*time.Second); err == nil {
				ag := agent.NewClient(conn)
				methods = append(methods, ssh.PublicKeysCallback(ag.Signers))
				cleanup = func() { conn.Close() } // only needed during the handshake
			} else if cfg.AuthMethod == "agent" {
				return nil, cleanup, fmt.Errorf("ssh agent: %w", err)
			}
		} else if cfg.AuthMethod == "agent" {
			return nil, cleanup, errors.New("ssh agent: SSH_AUTH_SOCK not set")
		}
	}
	if usePass {
		if cfg.Password == "" {
			return nil, cleanup, errors.New("ssh: password auth selected but no password set")
		}
		methods = append(methods, ssh.Password(cfg.Password))
		pw := cfg.Password
		methods = append(methods, ssh.KeyboardInteractive(func(_, _ string, questions []string, echos []bool) ([]string, error) {
			// Answer only prompts that look like a password prompt (hidden
			// input, or "password" in the text); never type the password
			// into an OTP or verification prompt.
			answers := make([]string, len(questions))
			for i, q := range questions {
				lq := strings.ToLower(q)
				if strings.Contains(lq, "password") && !strings.Contains(lq, "code") && !strings.Contains(lq, "token") && !strings.Contains(lq, "verif") {
					answers[i] = pw
				}
			}
			return answers, nil
		}))
	}
	if len(methods) == 0 {
		return nil, cleanup, errors.New("ssh: no authentication method available (set a key, agent or password)")
	}
	return methods, cleanup, nil
}

func expandHome(p string) string {
	if strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, p[2:])
	}
	return p
}

// maxOutput bounds what a remote command may send back on stdout.
const maxOutput = 16 << 20

// cappedWriter drops bytes beyond its budget instead of growing without
// bound; the session keeps draining so the remote side is not blocked.
type cappedWriter struct {
	mu   sync.Mutex // the session's copier writes; Run reads after a timeout
	w    bytes.Buffer
	left int
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	c.mu.Lock()
	if c.left > 0 {
		if len(p) > c.left {
			p = p[:c.left]
		}
		c.w.Write(p)
		c.left -= len(p)
	}
	c.mu.Unlock()
	return n, nil
}

// snapshot returns a copy of what has been written so far.
func (c *cappedWriter) snapshot() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.w.Bytes()...)
}

// Run executes cmd on the remote with optional stdin and returns stdout/stderr.
// The session is torn down when ctx ends.
func (c *Client) Run(ctx context.Context, cmd string, stdin io.Reader) ([]byte, []byte, error) {
	c.mu.Lock()
	cli := c.cli
	c.mu.Unlock()
	if cli == nil {
		return nil, nil, errors.New("ssh: not connected")
	}
	// NewSession blocks until the peer answers the channel open; bound it
	// by ctx too, or a connection that died after the keepalive hangs the
	// poll for the TCP retransmit timeout.
	type sessRes struct {
		s   *ssh.Session
		err error
	}
	sch := make(chan sessRes, 1)
	go func() { s, err := cli.NewSession(); sch <- sessRes{s, err} }()
	var sess *ssh.Session
	select {
	case <-ctx.Done():
		go func() { // release the session if it does arrive later
			if r := <-sch; r.s != nil {
				r.s.Close()
			}
		}()
		return nil, nil, ctx.Err()
	case r := <-sch:
		if r.err != nil {
			return nil, nil, r.err
		}
		sess = r.s
	}
	defer sess.Close()
	// A compromised box could stream without end inside the deadline; the
	// probe is a few KiB and even a du of a huge home is well under this.
	out := &cappedWriter{left: maxOutput}
	errb := &cappedWriter{left: 1 << 20}
	sess.Stdout, sess.Stderr = out, errb
	if stdin != nil {
		sess.Stdin = stdin
	}
	done := make(chan error, 1)
	go func() { done <- sess.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = sess.Signal(ssh.SIGKILL)
		sess.Close()
		// Let the session's stdout/stderr copiers finish before the
		// buffers are read (bounded: a dead peer must not hold us).
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			// Gave up on the copiers; snapshot() copies under the lock
			// they write under, so a late write cannot tear the result.
		}
		return out.snapshot(), errb.snapshot(), ctx.Err()
	case err := <-done:
		return out.snapshot(), errb.snapshot(), err
	}
}

// DialUnix opens a stream to a unix socket on the remote host.
func (c *Client) DialUnix(ctx context.Context, path string) (net.Conn, error) {
	return c.dial(ctx, "unix", path)
}

// DialTCP opens a stream to host:port as seen from the remote host.
func (c *Client) DialTCP(ctx context.Context, addr string) (net.Conn, error) {
	return c.dial(ctx, "tcp", addr)
}

func (c *Client) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	c.mu.Lock()
	cli := c.cli
	c.mu.Unlock()
	if cli == nil {
		return nil, errors.New("ssh: not connected")
	}
	return cli.DialContext(ctx, network, addr)
}

// Ping sends a keepalive request and reports whether the connection answered.
func (c *Client) Ping(ctx context.Context) error {
	c.mu.Lock()
	cli := c.cli
	c.mu.Unlock()
	if cli == nil {
		return errors.New("ssh: not connected")
	}
	done := make(chan error, 1)
	go func() {
		_, _, err := cli.SendRequest("keepalive@openssh.com", true, nil)
		done <- err
	}()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-done:
		return err
	}
}

// Close tears the connection down.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.cli == nil {
		return nil
	}
	err := c.cli.Close()
	c.cli = nil
	return err
}
