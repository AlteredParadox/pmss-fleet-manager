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
// would send WebUI credentials.
func verifyPortNow(ctx context.Context, cli *sshx.Client, port int) error {
	vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, _, err := cli.Run(vctx, "sh", strings.NewReader(probe.ListenScript))
	if err != nil && len(out) == 0 {
		return fmt.Errorf("could not re-check the WebUI port's owner: %w", err)
	}
	if ok, reason := probe.PortOwnerNow(out, port); !ok {
		return errors.New("refusing to send WebUI credentials: " + reason)
	}
	return nil
}

// tunnel is what verifiedDial needs from an SSH client (sshx.Client, or a
// fake in tests).
type tunnel interface {
	DialTCP(ctx context.Context, addr string) (net.Conn, error)
	Run(ctx context.Context, cmd string, stdin io.Reader) ([]byte, []byte, error)
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
// so reuse is safe. A box that hides non-listening sockets by policy falls
// back to the listener check; a box where the inspection merely failed
// does not get the connection.
func verifiedDial(cli tunnel, hp string, port int) qbt.Dialer {
	return func(ctx context.Context) (net.Conn, error) {
		conn, err := cli.DialTCP(ctx, hp)
		if err != nil {
			return nil, err
		}
		vctx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		out, _, err := cli.Run(vctx, "sh", strings.NewReader(probe.EstablishedScript(port)))
		if err != nil && len(out) == 0 {
			conn.Close()
			return nil, fmt.Errorf("could not verify who accepted the connection on port %d: %w", port, err)
		}
		ok, known, reason := probe.EstablishedOwner(out, port)
		if !known {
			return conn, nil // socket-table privacy: the listener check is what applies
		}
		if !ok {
			conn.Close()
			return nil, fmt.Errorf("refusing to use the connection on port %d: %s", port, reason)
		}
		return conn, nil
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
