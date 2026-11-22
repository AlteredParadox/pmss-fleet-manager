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

// MaxSSHTimeout is the largest value SetSSHTimeout accepts. A day is already
// far past useful, and it keeps the derived detection budget far from
// overflowing the int64 a time.Duration holds.
const MaxSSHTimeout = 24 * time.Hour

// sshTimeoutNanos holds an override set by SetSSHTimeout; zero means the
// default. Atomic because the pollers of every box read it concurrently with
// the server's startup.
var sshTimeoutNanos atomic.Int64

// SetSSHTimeout sets the SSH-side deadline. A value of zero or less restores
// the default; anything above MaxSSHTimeout is clamped to it.
func SetSSHTimeout(d time.Duration) {
	if d <= 0 {
		sshTimeoutNanos.Store(0)
		return
	}
	sshTimeoutNanos.Store(int64(min(d, MaxSSHTimeout)))
}

// sshTimeout returns the deadline in force.
func sshTimeout() time.Duration {
	if n := sshTimeoutNanos.Load(); n > 0 {
		return time.Duration(n)
	}
	return defaultSSHTimeout
}

// DetectBudget is how long one detection may take end to end: the connection
// deadline, the probe deadline and the detection-only steps that keep their
// own 15 s each (qBittorrent, docker, du). A caller that caps detection below
// this cancels a probe that respects -ssh-timeout.
func DetectBudget() time.Duration {
	return 2*sshTimeout() + 45*time.Second
}
