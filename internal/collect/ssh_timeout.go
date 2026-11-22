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
	"sync/atomic"
	"time"
)

// defaultSSHTimeout bounds what this package dials and runs over a new
// connection: connection setup, the detection probe and the poll's probe. It
// does not cover every SSH-backed step: the liveness check on an already open
// connection keeps its own short deadline, and rTorrent's detached transport
// has its own. A box busy enough to answer slowly (a shared Pulsed Media host
// under load) fails the covered steps with "context deadline exceeded", so
// the operator can raise it with -ssh-timeout / PMSS_SSH_TIMEOUT.
const defaultSSHTimeout = 45 * time.Second

// sshTimeoutNanos holds an override set by SetSSHTimeout; zero means the
// default. Atomic because the pollers of every box read it concurrently with
// the server's startup.
var sshTimeoutNanos atomic.Int64

// SetSSHTimeout sets the SSH-side deadline. A value of zero or less restores
// the default.
func SetSSHTimeout(d time.Duration) {
	if d <= 0 {
		sshTimeoutNanos.Store(0)
		return
	}
	sshTimeoutNanos.Store(int64(d))
}

// sshTimeout returns the deadline in force.
func sshTimeout() time.Duration {
	if n := sshTimeoutNanos.Load(); n > 0 {
		return time.Duration(n)
	}
	return defaultSSHTimeout
}
