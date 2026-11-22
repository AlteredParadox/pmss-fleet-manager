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

// Every caller of sshTimeout() must see the same value, and the detection
// budget derived from it must follow, including at the clamp: a duration of a
// few centuries would otherwise double past what an int64 holds.
func TestSSHTimeoutOverride(t *testing.T) {
	t.Cleanup(func() { SetSSHTimeout(0) })

	cases := []struct {
		name     string
		set      time.Duration
		want     time.Duration
		wantBudg time.Duration
	}{
		{"default", 0, defaultSSHTimeout, 2*defaultSSHTimeout + 45*time.Second},
		{"override", 2 * time.Minute, 2 * time.Minute, 4*time.Minute + 45*time.Second},
		{"clamped at the maximum", 48 * time.Hour, MaxSSHTimeout, 2*MaxSSHTimeout + 45*time.Second},
		{"non-positive restores the default", -1, defaultSSHTimeout, 2*defaultSSHTimeout + 45*time.Second},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			SetSSHTimeout(c.set)
			if got := sshTimeout(); got != c.want {
				t.Fatalf("sshTimeout = %v, want %v", got, c.want)
			}
			if got := sshConfig(&store.Box{Host: "box", Port: 22}).Timeout; got != c.want {
				t.Fatalf("sshConfig Timeout = %v, want %v", got, c.want)
			}
			if got := DetectBudget(); got != c.wantBudg {
				t.Fatalf("DetectBudget = %v, want %v", got, c.wantBudg)
			}
		})
	}
}
