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

package phpserial

import "testing"

func TestUnserializeTrafficData(t *testing.T) {
	in := `a:2:{s:3:"raw";a:5:{s:5:"month";d:6963.25;s:4:"week";i:1200;s:3:"day";d:0;s:4:"hour";d:12.5;s:5:"15min";d:3;}s:5:"daily";a:2:{s:10:"2026/10/15";d:220.5;s:10:"2026/10/16";i:40;}}`
	v, err := Unserialize([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	arr, ok := v.(*Array)
	if !ok {
		t.Fatalf("want array, got %T", v)
	}
	raw := arr.Array("raw")
	if m, _ := raw.Float("month"); m != 6963.25 {
		t.Fatalf("month = %v", m)
	}
	if w, _ := raw.Float("week"); w != 1200 {
		t.Fatalf("week = %v", w)
	}
	daily := arr.Array("daily")
	if len(daily.Keys) != 2 || daily.Keys[0] != "2026/10/15" {
		t.Fatalf("daily keys = %v", daily.Keys)
	}
	if d, _ := daily.Float("2026/10/16"); d != 40 {
		t.Fatalf("daily = %v", d)
	}
}

func TestUnserializeScalars(t *testing.T) {
	cases := map[string]any{
		`i:-5;`:        int64(-5),
		`d:1.5;`:       1.5,
		`b:1;`:         true,
		`N;`:           nil,
		`s:5:"hello";`: "hello",
	}
	for in, want := range cases {
		got, err := Unserialize([]byte(in))
		if err != nil {
			t.Fatalf("%s: %v", in, err)
		}
		if got != want {
			t.Fatalf("%s: got %v want %v", in, got, want)
		}
	}
	if _, err := Unserialize([]byte(`a:1:{s:1:"x";`)); err == nil {
		t.Fatal("want error on truncated input")
	}
}
