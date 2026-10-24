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

package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSecret = "0123456789abcdef0123456789abcdef" // 32 bytes

func TestRoundTripAndLabels(t *testing.T) {
	sl, err := New([]byte(testSecret))
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := sl.Seal("boxes.password", "hunter2")
	if err != nil {
		t.Fatal(err)
	}
	if !IsSealed(sealed) || strings.Contains(sealed, "hunter2") {
		t.Fatalf("sealed = %q", sealed)
	}
	if got, err := sl.Open("boxes.password", sealed); err != nil || got != "hunter2" {
		t.Fatalf("open: %q %v", got, err)
	}
	// Another column's label is other additional data: the value does not move.
	if _, err := sl.Open("boxes.qbt_pass", sealed); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("moved ciphertext opened: %v", err)
	}
	// Two seals of one value differ (random nonce) and both open.
	again, _ := sl.Seal("boxes.password", "hunter2")
	if again == sealed {
		t.Fatal("nonce reused")
	}
	if got, _ := sl.Open("boxes.password", again); got != "hunter2" {
		t.Fatal("second seal does not open")
	}
}

func TestEmptyAndPlaintextPassThrough(t *testing.T) {
	sl, _ := New([]byte(testSecret))
	if v, err := sl.Seal("x", ""); v != "" || err != nil {
		t.Fatalf("empty sealed to %q %v", v, err)
	}
	// Plaintext from before the secret was set is returned as is.
	if v, err := sl.Open("x", "plain"); v != "plain" || err != nil {
		t.Fatalf("plaintext: %q %v", v, err)
	}
	var none *Sealer
	if none.Enabled() {
		t.Fatal("nil sealer enabled")
	}
	if v, err := none.Seal("x", "plain"); v != "plain" || err != nil {
		t.Fatalf("nil seal: %q %v", v, err)
	}
	if v, err := none.Open("x", "plain"); v != "plain" || err != nil {
		t.Fatalf("nil open plain: %q %v", v, err)
	}
	sealed, _ := sl.Seal("x", "secret")
	if _, err := none.Open("x", sealed); !errors.Is(err, ErrNoKey) {
		t.Fatalf("nil open sealed: %v", err)
	}
}

func TestWrongKeyAndDamage(t *testing.T) {
	a, _ := New([]byte(testSecret))
	b, _ := New([]byte("fedcba9876543210fedcba9876543210"))
	sealed, _ := a.Seal("x", "secret")
	if _, err := b.Open("x", sealed); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("other key: %v", err)
	}
	for _, bad := range []string{Prefix, Prefix + "!!!", Prefix + "AAAA", sealed[:len(sealed)-2] + "zz"} {
		if _, err := a.Open("x", bad); !errors.Is(err, ErrWrongKey) {
			t.Fatalf("%q: %v", bad, err)
		}
	}
}

func TestShortSecretRefused(t *testing.T) {
	if _, err := New([]byte("short")); err == nil {
		t.Fatal("short secret accepted")
	}
	if _, err := New([]byte(strings.Repeat("x", MinSecretLen))); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	if l, err := Load("", ""); l != nil || err != nil {
		t.Fatalf("neither: %v %v", l, err)
	}
	if _, err := Load("a", "b"); err == nil {
		t.Fatal("both accepted")
	}
	l, err := Load(testSecret, "")
	if err != nil || string(l.Master) != testSecret || l.Source != "PMSS_SECRET" {
		t.Fatalf("env: %+v %v", l, err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte(testSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err = Load("", path)
	if err != nil || string(l.Master) != testSecret || l.Source != path || l.Warning != "" {
		t.Fatalf("file: %+v %v", l, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if l, err = Load("", path); err != nil || l.Warning == "" {
		t.Fatalf("loose file not warned: %+v %v", l, err)
	}
	if _, err := Load("", filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
}
