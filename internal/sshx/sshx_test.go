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

import "testing"

// The output cap keeps a prefix and remembers that it dropped the rest, so
// Run can report an incomplete result instead of a short one (GPT audit
// 2026-10-22 finding 7).
func TestCappedWriterRecordsOverflow(t *testing.T) {
	c := &cappedWriter{left: 5}
	for _, chunk := range []string{"ab", "cd"} {
		if n, err := c.Write([]byte(chunk)); n != 2 || err != nil {
			t.Fatalf("write %q: %d %v", chunk, n, err)
		}
	}
	if c.overflowed() || string(c.snapshot()) != "abcd" {
		t.Fatalf("within budget: over %v kept %q", c.overflowed(), c.snapshot())
	}
	if n, err := c.Write([]byte("efg")); n != 3 || err != nil {
		t.Fatalf("write past the budget: %d %v (the copier must keep draining)", n, err)
	}
	if !c.overflowed() || string(c.snapshot()) != "abcde" {
		t.Fatalf("past the budget: over %v kept %q", c.overflowed(), c.snapshot())
	}
	if _, err := c.Write(nil); err != nil {
		t.Fatal(err)
	}
}
