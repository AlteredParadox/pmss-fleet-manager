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

package sshx

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// testServer is an in-process SSH server: no client auth, "exec" of "echo"
// answers hello and exits 0, "exec" of "hang" holds the session open until
// the channel is closed. Global requests are discarded (keepalives get a
// reply). Anything the server would not answer is simulated on the wire by
// the proxy below, which is where a dead path or peer shows up.
func testServer(t *testing.T) net.Addr {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := ssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &ssh.ServerConfig{NoClientAuth: true}
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
			go func() {
				sc, chans, reqs, err := ssh.NewServerConn(conn, cfg)
				if err != nil {
					return
				}
				defer sc.Close()
				go ssh.DiscardRequests(reqs)
				for nc := range chans {
					if nc.ChannelType() == "direct-tcpip" {
						// A forwarded stream: echoes what it reads.
						ch, creqs, err := nc.Accept()
						if err != nil {
							continue
						}
						go ssh.DiscardRequests(creqs)
						go func() {
							defer ch.Close()
							buf := make([]byte, 256)
							for {
								n, err := ch.Read(buf)
								if err != nil {
									return
								}
								ch.Write(buf[:n])
							}
						}()
						continue
					}
					if nc.ChannelType() != "session" {
						nc.Reject(ssh.UnknownChannelType, "no")
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
							var cmd struct{ Cmd string }
							ssh.Unmarshal(r.Payload, &cmd)
							if cmd.Cmd == "hang" {
								continue // never exits; the client closes the channel
							}
							io.WriteString(ch, "hello\n")
							ch.SendRequest("exit-status", false, ssh.Marshal(struct{ Status uint32 }{0}))
							return
						}
					}()
				}
			}()
		}
	}()
	return ln.Addr()
}

// proxy relays TCP between the client and the server and can be frozen:
// from then on nothing is forwarded in either direction, which is what a
// dead path or a peer that stopped answering looks like to the client.
type proxy struct {
	addr   net.Addr
	frozen atomic.Bool
	mu     sync.Mutex
	conns  []net.Conn
}

func newProxy(t *testing.T, upstream net.Addr) *proxy {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	p := &proxy{addr: ln.Addr()}
	t.Cleanup(func() {
		ln.Close()
		p.mu.Lock()
		for _, c := range p.conns {
			c.Close()
		}
		p.mu.Unlock()
	})
	relay := func(dst, src net.Conn) {
		buf := make([]byte, 32<<10)
		for {
			n, err := src.Read(buf)
			if err != nil {
				return
			}
			for p.frozen.Load() {
				time.Sleep(10 * time.Millisecond) // hold the bytes until the connection goes
				if _, err := dst.Write(nil); err != nil {
					return
				}
			}
			if _, err := dst.Write(buf[:n]); err != nil {
				return
			}
		}
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			up, err := net.Dial("tcp", upstream.String())
			if err != nil {
				c.Close()
				continue
			}
			p.mu.Lock()
			p.conns = append(p.conns, c, up)
			p.mu.Unlock()
			go relay(up, c)
			go relay(c, up)
		}
	}()
	return p
}

func dialTest(t *testing.T, addr net.Addr) *Client {
	t.Helper()
	host, port, _ := net.SplitHostPort(addr.String())
	var pn int
	for _, d := range port {
		pn = pn*10 + int(d-'0')
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Dial(ctx, Config{Host: host, Port: pn, User: "u", AuthMethod: "password", Password: "x"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func runEcho(t *testing.T, c *Client) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, _, err := c.Run(ctx, "echo", nil)
	if err != nil || string(out) != "hello\n" {
		t.Fatalf("echo: %q %v", out, err)
	}
}

// goroutinesSettle waits for the goroutine count to come back to about
// where it was, so a leaked waiter shows as a failure rather than noise.
func goroutinesSettle(t *testing.T, base int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= base+2 || time.Now().After(deadline) {
			if n > base+2 {
				t.Fatalf("%d goroutines, was %d: operations were left behind", n, base)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// Every blocking operation (channel open, command wait, keepalive, dial)
// returns soon after its deadline on a path that stopped answering, the
// wedged transport is closed so the next use reconnects, and no goroutine
// is left waiting on it (GPT audit 2026-10-22 finding 11).
func TestWedgedTransportIsClosedAndJoined(t *testing.T) {
	graceWait = 150 * time.Millisecond
	t.Cleanup(func() { graceWait = 2 * time.Second })
	srv := testServer(t)
	for _, op := range []struct {
		name string
		do   func(ctx context.Context, c *Client) error
	}{
		{"channel open", func(ctx context.Context, c *Client) error { _, _, err := c.Run(ctx, "echo", nil); return err }},
		{"command wait", func(ctx context.Context, c *Client) error { _, _, err := c.Run(ctx, "hang", nil); return err }},
		{"keepalive", func(ctx context.Context, c *Client) error { return c.Ping(ctx) }},
		{"dial", func(ctx context.Context, c *Client) error { _, err := c.DialTCP(ctx, "127.0.0.1:1"); return err }},
	} {
		t.Run(op.name, func(t *testing.T) {
			px := newProxy(t, srv)
			c := dialTest(t, px.addr)
			runEcho(t, c)
			base := runtime.NumGoroutine()
			if op.name == "command wait" {
				// The command is running and the session open when the
				// path dies; the kill and close then go unanswered.
				go func() {
					time.Sleep(100 * time.Millisecond)
					px.frozen.Store(true)
				}()
			} else {
				px.frozen.Store(true)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
			defer cancel()
			start := time.Now()
			err := op.do(ctx, c)
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("got %v, want the deadline", err)
			}
			if d := time.Since(start); d > 300*time.Millisecond+3*graceWait {
				t.Fatalf("took %v: not bounded by the deadline and the grace", d)
			}
			if c.transport() != nil {
				t.Fatal("the wedged transport was kept")
			}
			if _, _, err := c.Run(context.Background(), "echo", nil); !errors.Is(err, errNotConnected) {
				t.Fatalf("after the wedge: %v, want not connected (the poller reconnects)", err)
			}
			goroutinesSettle(t, base)
		})
	}
}

// A healthy transport survives a caller giving up: the operation finishes
// within the grace, and the connection is kept for the next use.
func TestCancelOnHealthyTransportKeepsIt(t *testing.T) {
	graceWait = 500 * time.Millisecond
	t.Cleanup(func() { graceWait = 2 * time.Second })
	c := dialTest(t, testServer(t))
	base := runtime.NumGoroutine()
	// A command that never exits, abandoned by its caller: the kill and
	// close are answered, so the session ends and the transport stays.
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	_, _, err := c.Run(ctx, "hang", nil)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) || c.transport() == nil {
		t.Fatalf("abandoned command: %v, transport %v", err, c.transport() != nil)
	}
	runEcho(t, c)
	// An already-ended context starts nothing and touches nothing.
	done, cancelDone := context.WithCancel(context.Background())
	cancelDone()
	if _, _, err := c.Run(done, "echo", nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled before the start: %v", err)
	}
	if err := c.Ping(done); !errors.Is(err, context.Canceled) {
		t.Fatalf("ping cancelled before the start: %v", err)
	}
	if _, err := c.DialTCP(done, "127.0.0.1:1"); !errors.Is(err, context.Canceled) {
		t.Fatalf("dial cancelled before the start: %v", err)
	}
	runEcho(t, c)
	if err := c.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	goroutinesSettle(t, base)
}

// Two operations pending on one wedged transport both return: closing the
// shared connection fails every channel on it, by design.
func TestConcurrentOperationsOnWedgedTransport(t *testing.T) {
	graceWait = 150 * time.Millisecond
	t.Cleanup(func() { graceWait = 2 * time.Second })
	px := newProxy(t, testServer(t))
	c := dialTest(t, px.addr)
	runEcho(t, c)
	base := runtime.NumGoroutine()
	px.frozen.Store(true)
	var wg sync.WaitGroup
	errs := make([]error, 3)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), time.Duration(200+100*i)*time.Millisecond)
			defer cancel()
			switch i {
			case 0:
				_, _, errs[i] = c.Run(ctx, "echo", nil)
			case 1:
				errs[i] = c.Ping(ctx)
			default:
				_, errs[i] = c.DialTCP(ctx, "127.0.0.1:1")
			}
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err == nil || strings.Contains(err.Error(), "hello") {
			t.Fatalf("operation %d: %v", i, err)
		}
	}
	goroutinesSettle(t, base)
}

// Once the context has won, the result is the cancellation even when the
// operation becomes done before settle looks (the kill we sent, or a
// finish just in time): a cancelled command never reads as a success
// whose partial output could pass for a complete result (Codex and fred
// on PR 104).
func TestSettleKeepsCancellation(t *testing.T) {
	c := dialTest(t, testServer(t))
	cli := c.transport()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan struct{})
	close(done) // ready before settle is entered
	if err := c.settle(ctx, cli, done); !errors.Is(err, context.Canceled) {
		t.Fatalf("settle with done ready: %v, want the cancellation", err)
	}
	if c.transport() != cli {
		t.Fatal("a finished operation must not wedge the transport")
	}
	dctx, dcancel := context.WithDeadline(context.Background(), time.Now())
	defer dcancel()
	<-dctx.Done()
	if err := c.settle(dctx, cli, done); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("settle with a deadline: %v", err)
	}
}

// A forwarded stream (the rTorrent SCGI path) on a transport that stops
// answering is ended by Close within the grace, by wedging the transport,
// so a caller's context deadline is honoured although the channel supports
// no deadline of its own (an application that merely answers slowly is
// ended by the channel close alone, as before); a healthy stream's Close
// keeps the transport (daybreak blue re-review 6 of PR 123).
func TestForwardedStreamCloseIsBounded(t *testing.T) {
	graceWait = 150 * time.Millisecond
	t.Cleanup(func() { graceWait = 2 * time.Second })
	srv := testServer(t)
	px := newProxy(t, srv)
	c := dialTest(t, px.addr)
	// Healthy: a round trip, then Close, and the transport survives.
	conn, err := c.DialTCP(context.Background(), "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 16)
	if n, err := conn.Read(buf); err != nil || string(buf[:n]) != "ping\n" {
		t.Fatalf("echo: %q %v", buf[:n], err)
	}
	if err := conn.SetDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("a deadline is accepted as a no-op: %v", err)
	}
	conn.Close()
	if c.transport() == nil {
		t.Fatal("a healthy stream's Close must not wedge the transport")
	}
	runEcho(t, c)
	// Frozen transport: the read blocks; the caller's context ends and
	// closes the stream, as the SCGI round trip does; the read returns
	// within the grace and the transport is wedged, so the next use
	// reconnects.
	conn, err = c.DialTCP(context.Background(), "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	base := runtime.NumGoroutine()
	px.frozen.Store(true)
	go conn.Write([]byte("ping\n")) // may block on the frozen path: it is ended with the stream
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	defer stop()
	start := time.Now()
	_, err = conn.Read(buf)
	took := time.Since(start)
	if err == nil {
		t.Fatal("the read must fail once the stream is closed")
	}
	if took > 200*time.Millisecond+graceWait+time.Second {
		t.Fatalf("the read outlived the deadline and the grace: %v", took)
	}
	if c.transport() != nil {
		t.Fatal("a stream that did not end within the grace must wedge the transport")
	}
	px.frozen.Store(false)
	goroutinesSettle(t, base)
	// A Read after Close (no I/O in flight when the context ended: the
	// decoder was busy with buffered bytes) must not enter the channel and
	// block without a watchdog (fred on PR 123): it fails at once.
	px2 := newProxy(t, srv)
	c2 := dialTest(t, px2.addr)
	conn, err = c2.DialTCP(context.Background(), "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	px2.frozen.Store(true)
	start = time.Now()
	conn.Close() // nothing in flight; the close packet goes into the buffer and no answer comes
	if _, err := conn.Read(buf); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("a read after Close: %v", err)
	}
	if _, err := conn.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("a write after Close: %v", err)
	}
	if took := time.Since(start); took > graceWait+time.Second {
		t.Fatalf("close plus read took %v", took)
	}
	px2.frozen.Store(false)
	// Admission races Close: many readers take turns (an ssh channel has
	// one reader at a time, as the SCGI round trip uses it) as Close
	// begins; each either got in (and is ended by the watchdog) or was
	// refused; none hangs.
	px3 := newProxy(t, srv)
	c3 := dialTest(t, px3.addr)
	conn, err = c3.DialTCP(context.Background(), "127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	px3.frozen.Store(true)
	var wg sync.WaitGroup
	var turn sync.Mutex
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			turn.Lock()
			defer turn.Unlock()
			conn.Read(make([]byte, 4))
		}()
	}
	wg.Add(1)
	go func() { defer wg.Done(); conn.Close() }() // joined below: the test's cleanup resets graceWait
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(graceWait + 2*time.Second):
		t.Fatal("reads racing Close were left hanging")
	}
	px3.frozen.Store(false)
}
