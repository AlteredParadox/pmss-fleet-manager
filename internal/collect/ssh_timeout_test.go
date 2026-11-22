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
	"testing"
	"time"

	"pmss-fleet-manager/internal/store"
)

func TestSSHTimeoutOverride(t *testing.T) {
	t.Cleanup(func() { SetSSHTimeout(0) })

	if got := sshTimeout(); got != defaultSSHTimeout {
		t.Fatalf("default timeout = %v, want %v", got, defaultSSHTimeout)
	}

	SetSSHTimeout(2 * time.Minute)
	if got := sshTimeout(); got != 2*time.Minute {
		t.Fatalf("timeout after SetSSHTimeout = %v, want 2m", got)
	}
	if got := sshConfig(&store.Box{Host: "box", Port: 22}).Timeout; got != 2*time.Minute {
		t.Fatalf("sshConfig Timeout = %v, want the configured 2m", got)
	}

	// A non-positive value restores the default instead of dialling with no
	// deadline at all.
	SetSSHTimeout(-1)
	if got := sshTimeout(); got != defaultSSHTimeout {
		t.Fatalf("timeout after SetSSHTimeout(-1) = %v, want %v", got, defaultSSHTimeout)
	}
}
