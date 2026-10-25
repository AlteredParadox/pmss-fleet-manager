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

// authMethods assembles the auth methods cfg allows, in the order key, agent,
// password. The returned cleanup releases the agent socket when one was
// opened and is a no-op otherwise; it is returned on every path so callers
// can always call it.
func authMethods(cfg Config) ([]ssh.AuthMethod, func(), error) {
	var methods []ssh.AuthMethod
	cleanup := func() { /* nothing to release until an agent socket is opened */ }
	useKey := cfg.AuthMethod == "key" || cfg.AuthMethod == ""
	useAgent := cfg.AuthMethod == "agent" || cfg.AuthMethod == ""
	usePass := cfg.AuthMethod == "password" || (cfg.AuthMethod == "" && cfg.Password != "")

	var unreadable []string
	if useKey {
		m, denied, err := keyMethods(cfg)
		if err != nil {
			return nil, cleanup, err
		}
		methods = append(methods, m...)
		unreadable = denied
	}
	if useAgent {
		m, release, err := agentMethod(cfg)
		if err != nil {
			return nil, cleanup, err
		}
		methods = append(methods, m...)
		cleanup = release
	}
	if usePass {
		m, err := passwordMethods(cfg)
		if err != nil {
			return nil, cleanup, err
		}
		methods = append(methods, m...)
	}
	if len(methods) == 0 {
		return nil, cleanup, errors.New("ssh: no authentication method available (set a key, agent or password)" + unreadableHint(unreadable))
	}
	return methods, cleanup, nil
}

// unreadableHint names default keys that exist but could not be read, the
// usual sign of a key mounted into a container that its uid cannot read.
func unreadableHint(paths []string) string {
	switch len(paths) {
	case 0:
		return ""
	case 1:
		return "; found " + paths[0] + " but cannot read it (permission denied)"
	}
	return "; found " + strings.Join(paths, ", ") + " but cannot read them (permission denied)"
}

// keyPaths lists the private key files to try: the configured path, or the
// usual ~/.ssh names.
func keyPaths(cfg Config) []string {
	paths := []string{}
	if cfg.KeyPath != "" {
		paths = append(paths, expandHome(cfg.KeyPath))
	} else {
		home, _ := os.UserHomeDir()
		for _, n := range []string{"id_ed25519", "id_ecdsa", "id_rsa"} {
			paths = append(paths, filepath.Join(home, ".ssh", n))
		}
	}
	return paths
}

// keySigners loads the usable private keys. An unreadable or unparsable
// file is an error only for an explicitly configured key path; the default
// names are skipped, missing ones silently, and the ones that exist but
// cannot be read for lack of permission are returned so the final error can
// name them.
func keySigners(cfg Config) ([]ssh.Signer, []string, error) {
	var signers []ssh.Signer
	var unreadable []string
	for _, p := range keyPaths(cfg) {
		pem, err := os.ReadFile(p)
		if err != nil {
			if cfg.KeyPath != "" {
				return nil, nil, fmt.Errorf("read key %s: %w", p, err)
			}
			if errors.Is(err, os.ErrPermission) {
				unreadable = append(unreadable, p)
			}
			continue
		}
		s, err := ssh.ParsePrivateKey(pem)
		if err != nil {
			if cfg.KeyPath != "" {
				return nil, nil, fmt.Errorf("parse key %s: %w", p, err)
			}
			continue
		}
		signers = append(signers, s)
	}
	return signers, unreadable, nil
}

// keyMethods returns public-key auth from the loaded signers, plus the
// default keys that could not be read; none is an error only when key auth
// was selected explicitly.
func keyMethods(cfg Config) ([]ssh.AuthMethod, []string, error) {
	signers, unreadable, err := keySigners(cfg)
	if err != nil {
		return nil, nil, err
	}
	if len(signers) > 0 {
		return []ssh.AuthMethod{ssh.PublicKeys(signers...)}, unreadable, nil
	} else if cfg.AuthMethod == "key" {
		return nil, nil, errors.New("ssh: no usable private key in ~/.ssh (set a key path, or the key needs a passphrase)" + unreadableHint(unreadable))
	}
	return nil, unreadable, nil
}

// agentMethod returns public-key auth through the agent at SSH_AUTH_SOCK
// with the function that closes the agent socket; without an agent the
// methods are empty and the cleanup a no-op, unless agent auth was selected
// explicitly, which is an error.
func agentMethod(cfg Config) ([]ssh.AuthMethod, func(), error) {
	noop := func() { /* no agent socket was opened */ }
	if sock := os.Getenv("SSH_AUTH_SOCK"); sock != "" {
		if conn, err := net.DialTimeout("unix", sock, 5*time.Second); err == nil {
			ag := agent.NewClient(conn)
			cleanup := func() { conn.Close() } // only needed during the handshake
			return []ssh.AuthMethod{ssh.PublicKeysCallback(ag.Signers)}, cleanup, nil
		} else if cfg.AuthMethod == "agent" {
			return nil, noop, fmt.Errorf("ssh agent: %w", err)
		}
	} else if cfg.AuthMethod == "agent" {
		return nil, noop, errors.New("ssh agent: SSH_AUTH_SOCK not set")
	}
	return nil, noop, nil
}

// passwordMethods returns password auth plus a keyboard-interactive method
// that answers password prompts with the same secret.
func passwordMethods(cfg Config) ([]ssh.AuthMethod, error) {
	if cfg.Password == "" {
		return nil, errors.New("ssh: password auth selected but no password set")
	}
	var methods []ssh.AuthMethod
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
	return methods, nil
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

// ErrOutputLimit is returned by Run when the command wrote more to stdout
// than maxOutput: what was kept is a prefix, not the output, and a caller
// that parses line-oriented output (a du listing) must not take it for a
// complete one.
var ErrOutputLimit = errors.New("ssh: command output exceeded the limit; the result is incomplete")

// cappedWriter drops bytes beyond its budget instead of growing without
// bound; the session keeps draining so the remote side is not blocked. It
// remembers that it dropped something, so Run can say so.
type cappedWriter struct {
	mu   sync.Mutex // the session's copier writes; Run reads after a timeout
	w    bytes.Buffer
	left int
	over bool
}

func (c *cappedWriter) Write(p []byte) (int, error) {
	n := len(p)
	c.mu.Lock()
	if len(p) > c.left {
		c.over = true
		p = p[:c.left]
	}
	c.w.Write(p)
	c.left -= len(p)
	c.mu.Unlock()
	return n, nil
}

// overflowed reports whether bytes were dropped.
func (c *cappedWriter) overflowed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.over
}

// snapshot returns a copy of what has been written so far.
func (c *cappedWriter) snapshot() []byte {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]byte(nil), c.w.Bytes()...)
}

// graceWait is how long an operation whose context ended gets to finish
// on its own before the transport is declared wedged. A healthy peer
// answers a channel open, a keepalive or a session close within it, so a
// caller's cancel (a browser navigating away) costs nothing; a peer or
// path that has stopped answering does not, and the connection is closed
// so the operation returns instead of waiting on it forever. A variable
// so tests can shorten it.
var graceWait = 2 * time.Second

// errNotConnected is returned by every operation on a closed client.
var errNotConnected = errors.New("ssh: not connected")

// transport returns the live connection, or nil once closed.
func (c *Client) transport() *ssh.Client {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.cli
}

// wedged drops cli as the client's transport and closes it, which fails
// every operation pending on it: the one reliable way to unblock an
// x/crypto/ssh request whose reply never comes. Other channels on the
// shared connection fail too; the poller reconnects at its next use. The
// lock is not held across Close (which writes to the possibly dead peer).
func (c *Client) wedged(cli *ssh.Client) {
	c.mu.Lock()
	if c.cli == cli {
		c.cli = nil
	}
	c.mu.Unlock()
	cli.Close()
}

// await waits for an operation that runs on cli to signal done. When ctx
// ends first, the operation is settled (see settle) and ctx.Err() is
// returned; else nil.
func (c *Client) await(ctx context.Context, cli *ssh.Client, done <-chan struct{}) error {
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return c.settle(ctx, cli, done)
	}
}

// settle ends an operation whose context has already ended: it gets
// graceWait to finish on its own; if it does not, the transport is wedged
// (see wedged) and the operation, now failing, is joined. The join is
// itself bounded, so a library that did not unblock could at worst leak
// the one goroutine, never hang the caller. Always returns ctx.Err(): the
// caller established that the context won, and an operation that finishes
// during the grace (because it was killed, or just in time) is still the
// cancelled one, never a success with a complete result.
func (c *Client) settle(ctx context.Context, cli *ssh.Client, done <-chan struct{}) error {
	select {
	case <-done:
		return ctx.Err() // finished within the grace: the transport is fine
	case <-time.After(graceWait):
	}
	c.wedged(cli)
	select {
	case <-done:
	case <-time.After(graceWait):
	}
	return ctx.Err()
}

// Run executes cmd on the remote with optional stdin and returns stdout/stderr.
// The session is torn down when ctx ends; a transport that does not let it
// be torn down is closed (see await).
func (c *Client) Run(ctx context.Context, cmd string, stdin io.Reader) ([]byte, []byte, error) {
	cli := c.transport()
	if cli == nil {
		return nil, nil, errNotConnected
	}
	if err := ctx.Err(); err != nil {
		return nil, nil, err // nothing started: the transport stays as it is
	}
	// NewSession blocks until the peer answers the channel open; bound it
	// by ctx too, or a connection that died after the keepalive hangs the
	// poll for the TCP retransmit timeout.
	var (
		sess    *ssh.Session
		sessErr error
	)
	opened := make(chan struct{})
	go func() {
		sess, sessErr = cli.NewSession()
		close(opened)
	}()
	if err := c.await(ctx, cli, opened); err != nil {
		select {
		case <-opened:
			if sess != nil {
				go sess.Close() // arrived within the grace, after the caller gave up; the close is a write that may block too
			}
		default: // left behind on a closed transport; it fails on its own
		}
		return nil, nil, err
	}
	if sessErr != nil {
		return nil, nil, sessErr
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
	var runErr error
	done := make(chan struct{})
	go func() {
		runErr = sess.Run(cmd)
		close(done)
	}()
	select {
	case <-done:
		return out.snapshot(), errb.snapshot(), runResult(runErr, out.overflowed())
	case <-ctx.Done():
	}
	// Ask the command to stop. Both writes go to the peer and would block
	// on a wedged transport themselves, so they run aside; settle handles
	// the transport if the session does not end within the grace, and
	// reports the cancellation whether or not the session ended meanwhile
	// (a command killed by us is not a command that completed).
	go func() {
		_ = sess.Signal(ssh.SIGKILL)
		sess.Close()
	}()
	err := c.settle(ctx, cli, done)
	// snapshot() copies under the lock the copiers write under, so a late
	// write cannot tear the result.
	return out.snapshot(), errb.snapshot(), err
}

// runResult is Run's error: an overflowed stdout takes precedence over the
// command's own exit status (a du that exits 1 for an unreadable entry and
// also overran the cap is incomplete, not finished), and the exit error is
// named in the message but not wrapped, so no errors.As finds an
// ssh.ExitError in an incomplete result.
func runResult(err error, overflowed bool) error {
	if !overflowed {
		return err
	}
	if err != nil {
		return fmt.Errorf("%w (%d MiB kept; the command then reported: %v)", ErrOutputLimit, maxOutput>>20, err)
	}
	return fmt.Errorf("%w (%d MiB kept)", ErrOutputLimit, maxOutput>>20)
}

// DialUnix opens a stream to a unix socket on the remote host.
func (c *Client) DialUnix(ctx context.Context, path string) (net.Conn, error) {
	return c.dial(ctx, "unix", path)
}

// DialTCP opens a stream to host:port as seen from the remote host.
func (c *Client) DialTCP(ctx context.Context, addr string) (net.Conn, error) {
	return c.dial(ctx, "tcp", addr)
}

// dial opens a direct-tcpip or direct-streamlocal channel; the peer's
// answer is awaited like a session open (ssh.Client.DialContext would
// leave its goroutine behind on a wedged transport).
func (c *Client) dial(ctx context.Context, network, addr string) (net.Conn, error) {
	cli := c.transport()
	if cli == nil {
		return nil, errNotConnected
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var (
		conn    net.Conn
		dialErr error
	)
	done := make(chan struct{})
	go func() {
		conn, dialErr = cli.Dial(network, addr)
		close(done)
	}()
	if err := c.await(ctx, cli, done); err != nil {
		select {
		case <-done:
			if conn != nil {
				go conn.Close() // a channel close is a write that may block too
			}
		default:
		}
		return nil, err
	}
	if dialErr != nil {
		return nil, dialErr
	}
	return &boundedConn{Conn: conn, c: c, cli: cli, idle: make(chan struct{}, 1)}, nil
}

// boundedConn is a forwarded stream whose Close is guaranteed to end it.
// An x/crypto/ssh channel supports no deadline (SetDeadline is refused),
// and its Close only sends a close packet: it neither unblocks a pending
// Read nor stops a later one, so a stream whose peer stopped answering
// would hold a Read forever, and with it the poll, the edit, the removal
// or the shutdown waiting on that poll (daybreak blue re-review 6 of PR
// 123). Close therefore closes admission first, under the same lock the
// I/O paths take, so no Read or Write enters after it began (they fail
// with net.ErrClosed; fred on PR 123); then it gives the I/O already in
// flight, and the channel close itself, graceWait to finish; past that
// the owning transport is wedged (see wedged), which fails them. The
// poller reconnects at its next use, as for any other wedged operation.
type boundedConn struct {
	net.Conn
	c   *Client
	cli *ssh.Client
	mu  sync.Mutex
	// closing is set by Close before anything else; inflight counts the
	// Reads and Writes admitted and not yet returned; idle is signalled
	// when the last of them returns after closing was set.
	closing  bool
	inflight int
	idle     chan struct{}
}

// enter admits one I/O operation, or refuses it once Close began.
func (b *boundedConn) enter() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closing {
		return false
	}
	b.inflight++
	return true
}

// leave retires one admitted operation.
func (b *boundedConn) leave() {
	b.mu.Lock()
	b.inflight--
	last := b.closing && b.inflight == 0
	b.mu.Unlock()
	if last {
		select {
		case b.idle <- struct{}{}:
		default:
		}
	}
}

func (b *boundedConn) Read(p []byte) (int, error) {
	if !b.enter() {
		return 0, net.ErrClosed
	}
	defer b.leave()
	return b.Conn.Read(p)
}

func (b *boundedConn) Write(p []byte) (int, error) {
	if !b.enter() {
		return 0, net.ErrClosed
	}
	defer b.leave()
	return b.Conn.Write(p)
}

// Close ends the stream: bounded by graceWait, after which the transport
// is wedged. Idempotent; a second Close returns at once.
func (b *boundedConn) Close() error {
	b.mu.Lock()
	if b.closing {
		b.mu.Unlock()
		return nil
	}
	b.closing = true
	busy := b.inflight > 0
	b.mu.Unlock()
	closed := make(chan struct{})
	go func() { b.Conn.Close(); close(closed) }()
	timer := time.NewTimer(graceWait)
	defer timer.Stop()
	for {
		select {
		case <-closed:
			closed = nil // observed once; never readable again (Codex on PR 123)
			if !busy {
				return nil // the close went through and nothing was in flight: the transport is fine
			}
		case <-b.idle:
			busy = false
			if closed == nil {
				return nil
			}
		case <-timer.C:
			b.c.wedged(b.cli)
			return nil
		}
	}
}

// SetDeadline and friends are refused by the channel; they are no-ops
// here so a caller that sets one as a courtesy gets Close's guarantee
// instead of an error it would have to special-case.
func (b *boundedConn) SetDeadline(time.Time) error      { return nil }
func (b *boundedConn) SetReadDeadline(time.Time) error  { return nil }
func (b *boundedConn) SetWriteDeadline(time.Time) error { return nil }

// Ping sends a keepalive request and reports whether the connection answered.
func (c *Client) Ping(ctx context.Context) error {
	cli := c.transport()
	if cli == nil {
		return errNotConnected
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	var pingErr error
	done := make(chan struct{})
	go func() {
		_, _, pingErr = cli.SendRequest("keepalive@openssh.com", true, nil)
		close(done)
	}()
	if err := c.await(ctx, cli, done); err != nil {
		return err
	}
	return pingErr
}

// Close tears the connection down. The lock is released before the close
// itself, which may write to a dead peer.
func (c *Client) Close() error {
	c.mu.Lock()
	cli := c.cli
	c.cli = nil
	c.mu.Unlock()
	if cli == nil {
		return nil
	}
	return cli.Close()
}
