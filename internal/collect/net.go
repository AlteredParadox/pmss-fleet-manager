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
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"pmss-fleet-manager/internal/probe"
	"pmss-fleet-manager/internal/qbt"
	"pmss-fleet-manager/internal/sshx"
	"strings"
	"time"
)

type netConn = net.Conn

// HumanBytes formats a byte count for messages ("1.5 TiB").
func HumanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %ciB", float64(n)/float64(div), "KMGTPE"[exp])
}

// mibToBytes converts a MiB figure from the box to bytes, clamped so a
// corrupt value cannot wrap to a negative int64.
func mibToBytes(mib float64) int64 {
	const ceiling = float64(1 << 62)
	b := mib * 1048576
	switch {
	case math.IsNaN(b) || b < 0:
		return 0
	case b > ceiling:
		return 1 << 62
	}
	return int64(b)
}

// gibToBytes is mibToBytes for GiB figures (same clamp, 1024x the unit).
func gibToBytes(gib float64) int64 {
	return mibToBytes(gib * 1024)
}

// verifyPortNow re-reads the listening sockets on the box and refuses unless
// port is bound by the account we are logged in as. It is the live
// counterpart of the probe's check, run immediately before an action that
// would send WebUI credentials or an rTorrent command over a loopback port.
// socketTableLimit bounds a listener or established-socket table read: a
// row is some 150 bytes and a busy box has thousands of sockets, so this
// is far above any real table and far below the transport's 16 MiB
// (2026-11 audits).
const socketTableLimit = 4 << 20

func verifyPortNow(ctx context.Context, cli *sshx.Client, port int) error {
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _, err := cli.RunLimited(vctx, "sh", strings.NewReader(probe.ListenScript), socketTableLimit)
	if err != nil && len(out) == 0 {
		return fmt.Errorf("could not re-check the owner of port %d: %w", port, err)
	}
	if ok, reason := probe.PortOwnerNow(out, port); !ok {
		return fmt.Errorf("refusing to use port %d: %s", port, reason)
	}
	return nil
}

// tunnel is what verifiedDial needs from an SSH client (sshx.Client, or a
// fake in tests).
type tunnel interface {
	DialTCP(ctx context.Context, addr string) (net.Conn, error)
	Run(ctx context.Context, cmd string, stdin io.Reader) ([]byte, []byte, error)
	RunLimited(ctx context.Context, cmd string, stdin io.Reader, limit int) ([]byte, []byte, error)
}

// verifiedDial returns a qBittorrent dialer over cli's tunnel that, after
// each new connection to 127.0.0.1:port, checks the accepting side of the
// established sockets on that port before a single byte is written. The
// pre-connect listener check (verifyPortNow, the probe) is check-then-
// connect: a co-tenant can bind the port in the gap and accept our
// connection, and a pooled connection to such a peer would outlive a later
// listener check. The script pairs this account's own tunnel socket with
// the socket that accepted it (any TCP state), so the check is of this
// very connection. Verified at connect time, a connection's peer is fixed,
// so reuse is safe. A box where the inspection failed, or found someone
// else's acceptor, does not get the connection.
//
// A box that denies this account its socket tables (socket-table privacy,
// "privacy" from the script) gets the listener-continuity check instead:
// the listening socket on the port is read just before the connection is
// opened and again after it is (and before a byte is written); the same
// socket (cookie), owned by this account, at both readings held the port
// throughout, and nothing else can bind 127.0.0.1:port while it does, so
// the connection went to our own process (see continuity). A changed or
// foreign listener refuses, with no fallback. Only where ss gives the
// owner uid but no cookie may the per-box opt-in (off by default) let the
// uid alone apply; without a trustworthy uid the dial is refused with
// ErrVerifyBlocked. pol.note records what each new connection got.
func verifiedDial(cli tunnel, hp string, port int, pol verifyPolicy) qbt.Dialer {
	return func(ctx context.Context) (net.Conn, error) {
		// The listener before the connection, for the continuity check on a
		// box that hides its socket tables (read only there, below). Its
		// failure costs nothing elsewhere: it is then simply unavailable.
		pctx, pcancel := context.WithTimeout(ctx, 10*time.Second)
		before, _, preErr := cli.RunLimited(pctx, "sh", strings.NewReader(probe.ListenerScript(port)), socketTableLimit)
		pcancel()
		conn, err := cli.DialTCP(ctx, hp)
		if err != nil {
			return nil, err
		}
		vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		var out []byte
		var v probe.Established
		// An accepting socket with no owner yet (the listener has not
		// called accept(), or an orphan is closing) is read again a few
		// times, nothing written meanwhile; it is judged once it has an
		// owner, and refused if it never gets one (probe.EstablishedOwner).
		for attempt := 0; ; attempt++ {
			out, _, err = cli.RunLimited(vctx, "sh", strings.NewReader(probe.EstablishedScript(port)), socketTableLimit)
			if err != nil && len(out) == 0 {
				conn.Close()
				return nil, fmt.Errorf("could not verify who accepted the connection on port %d: %w", port, err)
			}
			prev := v
			if v = probe.CheckEstablished(out, port); !v.Pending || err != nil || attempt >= len(acceptRetryDelays) {
				if v.OK && attempt > 0 && pol.waited != nil {
					pol.waited(attempt, prev.Shape)
				}
				break
			}
			select {
			case <-vctx.Done():
				conn.Close()
				return nil, fmt.Errorf("refusing to use the connection on port %d: %s", port, v.Reason)
			case <-time.After(acceptRetryDelays[attempt]):
			}
		}
		ok, known, reason := v.OK, v.Known, v.Reason
		if !known {
			if preErr != nil || err != nil {
				conn.Close() // a read that failed, partly or wholly, is no evidence either way
				return nil, fmt.Errorf("refusing to use the connection on port %d: could not read the listening socket before and after connecting", port)
			}
			uid := probe.ScriptUID(out)
			switch v, why := continuity(probe.ListenerIdentity(before, port, uid), probe.ListenerIdentity(out, port, uid)); v {
			case contVerified:
				pol.record(VerifyListener)
				return conn, nil
			case contRefused:
				conn.Close()
				return nil, fmt.Errorf("refusing to use the connection on port %d: %s", port, why)
			case contUIDOnly:
				if pol.allow != nil && pol.allow() {
					admitted, ok := conn, true
					if pol.admit != nil {
						admitted, ok = pol.admit(conn)
					}
					if ok {
						pol.record(VerifyListenerOnly)
						return admitted, nil // opted in: the owner uid alone is what applies
					}
				}
			}
			conn.Close() // contUnavailable, or contUIDOnly without the opt-in
			pol.record(VerifyBlocked)
			return nil, ErrVerifyBlocked
		}
		switch {
		case !ok:
			conn.Close()
			return nil, fmt.Errorf("refusing to use the connection on port %d: %s", port, reason)
		}
		pol.record(VerifyFull)
		return conn, nil
	}
}

// acceptRetryDelays are the pauses between readings of the socket table
// while the socket that took a new connection has no owner yet: about 1.5 s
// in all, for a client whose event loop is slow to accept(). A var for tests.
var acceptRetryDelays = []time.Duration{100 * time.Millisecond, 200 * time.Millisecond, 400 * time.Millisecond, 800 * time.Millisecond}

// What the last new WebUI/SCGI connection of a box got (VerifyState.Mode).
const (
	VerifyFull         = "full"          // the accepting socket was checked to be ours
	VerifyListener     = "listener"      // tables denied; the same listening socket of ours before and after the connection
	VerifyListenerOnly = "listener-only" // tables denied, no socket cookie; opted in to the owner uid alone
	VerifyBlocked      = "blocked"       // tables denied; not opted in, refused
)

// The listener-continuity verdicts (continuity).
const (
	contVerified    = iota // the same socket of ours before and after
	contRefused            // changed, foreign, ambiguous or gone: refuse, never fall back
	contUIDOnly            // ours by the kernel's uid, but no cookie to compare: the opt-in's case
	contUnavailable        // no trustworthy owner: blocked
)

// continuity compares the listener read before a connection with the one
// read after it (probe.ListenerIdentity). Anything that shows the port is
// not ours, that its socket changed, or that a reading is incomplete or
// malformed, refuses outright. Missing information leaves a choice only
// when both readings cleanly report our uid and no cookie at all (the
// opt-in's case); a cookie on one side only is a failed check, not an
// unsupported feature (fred on #219).
func continuity(before, after probe.Listener) (verdict int, reason string) {
	for _, l := range []probe.Listener{before, after} {
		switch l.Status {
		case probe.ListenForeign, probe.ListenAmbiguous, probe.ListenNone, probe.ListenMalformed:
			return contRefused, l.Reason
		}
	}
	switch {
	case before.Status == probe.ListenUnavailable || after.Status == probe.ListenUnavailable:
		return contUnavailable, "the box does not report who owns the listening socket"
	case before.Status == probe.ListenOK && after.Status == probe.ListenOK:
		if before.Cookie != after.Cookie {
			return contRefused, "the listening socket changed while connecting (the client restarted, or something else took the port)"
		}
		return contVerified, ""
	case before.Status == probe.ListenNoCookie && after.Status == probe.ListenNoCookie:
		return contUIDOnly, "the box does not report the listening socket's identity"
	}
	return contRefused, "the listening socket's identity was reported in one reading and not the other"
}

// ErrVerifyBlocked refuses a connection the box gives no way to check.
var ErrVerifyBlocked = errors.New("WebUI verification blocked by host socket privacy: the box does not let this account read its socket tables, nor report the identity of the listening socket, so the dashboard cannot check who accepted the connection; see the box's verification page")

// verifyPolicy is a box's say in verifiedDial: allow reports the
// listener-only opt-in at dial time, note receives the outcome. The zero
// value is strict and silent (the add/edit wizard: no box setting yet).
type verifyPolicy struct {
	allow func() bool
	admit func(net.Conn) (net.Conn, bool) // optional final say as a connection is let through: the conn to use, or refuse (poller.admitListenerOnly)
	note  func(mode string)
	// waited, if set, hears of a connection verified only after re-reading
	// the table: how many re-reads, and the shape the acceptor had.
	waited func(rereads int, shape string)
}

func (v verifyPolicy) record(mode string) {
	if v.note != nil {
		v.note(mode)
	}
}

// clip bounds a remote-supplied string so a hostile box cannot bloat state,
// the database or every SSE event. Cuts on a rune boundary.
func clip(s string, limit int) string {
	if limit < 0 {
		limit = 0
	}
	if len(s) <= limit {
		return s
	}
	cut := false
	if len(s) > 4*limit {
		s, cut = s[:4*limit], true // the first limit runes fit in 4*limit bytes; avoids a rune copy of a huge body
	}
	r := []rune(s)
	if len(r) > limit {
		r, cut = r[:limit], true
	}
	if !cut {
		return s
	}
	return string(r) + "…"
}
