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
	sealed := sl.Seal("boxes.password", "hunter2")
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
	again := sl.Seal("boxes.password", "hunter2")
	if again == sealed {
		t.Fatal("nonce reused")
	}
	if got, _ := sl.Open("boxes.password", again); got != "hunter2" {
		t.Fatal("second seal does not open")
	}
}

func TestEmptyAndPlaintextPassThrough(t *testing.T) {
	sl, _ := New([]byte(testSecret))
	if v := sl.Seal("x", ""); v != "" {
		t.Fatalf("empty sealed to %q", v)
	}
	// Plaintext from before the secret was set is returned as is.
	if v, err := sl.Open("x", "plain"); v != "plain" || err != nil {
		t.Fatalf("plaintext: %q %v", v, err)
	}
	var none *Sealer
	if none.Enabled() {
		t.Fatal("nil sealer enabled")
	}
	if v := none.Seal("x", "plain"); v != "plain" {
		t.Fatalf("nil seal: %q", v)
	}
	if v, err := none.Open("x", "plain"); v != "plain" || err != nil {
		t.Fatalf("nil open plain: %q %v", v, err)
	}
	sealed := sl.Seal("x", "secret")
	if _, err := none.Open("x", sealed); !errors.Is(err, ErrNoKey) {
		t.Fatalf("nil open sealed: %v", err)
	}
}

func TestWrongKeyAndDamage(t *testing.T) {
	a, _ := New([]byte(testSecret))
	b, _ := New([]byte("fedcba9876543210fedcba9876543210"))
	sealed := a.Seal("x", "secret")
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
	if l, err := Load(CurrentNames, "", ""); l != nil || err != nil {
		t.Fatalf("neither: %v %v", l, err)
	}
	if _, err := Load(CurrentNames, "a", "b"); err == nil || !strings.Contains(err.Error(), "PMSS_SECRET or -secret-file (PMSS_SECRET_FILE)") {
		t.Fatalf("both: %v", err)
	}
	if _, err := Load(PreviousNames, "a", "b"); err == nil || !strings.Contains(err.Error(), "PMSS_SECRET_PREVIOUS or -previous-secret-file (PMSS_SECRET_PREVIOUS_FILE)") {
		t.Fatalf("both previous: %v", err)
	}
	l, err := Load(CurrentNames, testSecret, "")
	if err != nil || string(l.Master) != testSecret || l.Source != "PMSS_SECRET" {
		t.Fatalf("env: %+v %v", l, err)
	}
	if l, err := Load(PreviousNames, testSecret, ""); err != nil || l.Source != "PMSS_SECRET_PREVIOUS" {
		t.Fatalf("previous env: %+v %v", l, err)
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "secret")
	if err := os.WriteFile(path, []byte(testSecret+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l, err = Load(CurrentNames, "", path)
	if err != nil || string(l.Master) != testSecret || l.Source != path || l.Warning != "" {
		t.Fatalf("file: %+v %v", l, err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	if l, err = Load(CurrentNames, "", path); err != nil || l.Warning == "" {
		t.Fatalf("loose file not warned: %+v %v", l, err)
	}
	if _, err := Load(CurrentNames, "", filepath.Join(dir, "missing")); err == nil {
		t.Fatal("missing file accepted")
	}
}

func TestResolve(t *testing.T) {
	r, err := Resolve("", "", "", "")
	if err != nil || r.Current != nil || r.Previous != nil {
		t.Fatalf("nothing set: %+v %v", r, err)
	}
	if _, err := Resolve("a", "b", "", ""); err == nil {
		t.Fatal("both sources accepted")
	}
	if _, err := Resolve("short", "", "", ""); err == nil || !strings.Contains(err.Error(), "credential secret") {
		t.Fatalf("short current: %v", err)
	}
	if _, err := Resolve(testSecret, "", "short", ""); err == nil || !strings.Contains(err.Error(), "previous secret") {
		t.Fatalf("short previous: %v", err)
	}
	if _, err := Resolve(testSecret, "", "a", "b"); err == nil || !strings.Contains(err.Error(), "PMSS_SECRET_PREVIOUS or -previous-secret-file") {
		t.Fatalf("both previous sources: %v", err)
	}
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte(testSecret), 0o644); err != nil {
		t.Fatal(err)
	}
	r, err = Resolve("", path, "fedcba9876543210fedcba9876543210", "")
	if err != nil || r.Current == nil || r.Previous == nil || r.Source != path || r.Warning == "" {
		t.Fatalf("file + previous: %+v %v", r, err)
	}
	// The previous secret from a file too; its loose mode is reported.
	prevPath := filepath.Join(t.TempDir(), "old")
	if err := os.WriteFile(prevPath, []byte("fedcba9876543210fedcba9876543210\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r2, err := Resolve(testSecret, "", "", prevPath); err != nil || r2.Previous == nil || r2.Warning == "" {
		t.Fatalf("previous from file: %+v %v", r2, err)
	}
	// The two sealers are different keys.
	sealed := r.Previous.Seal("x", "v")
	if _, err := r.Current.Open("x", sealed); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("previous opened with current: %v", err)
	}
}

func TestEnvelope(t *testing.T) {
	sl, _ := New([]byte(testSecret))
	genuine := sl.Seal("x", "v")
	for _, tc := range []struct {
		in      string
		version int
		ok      bool
	}{
		{"", 0, false},
		{"hunter2", 0, false},
		{"pmss", 0, false},
		{"pmss:", 0, false},
		{"pmssx:", 0, false},
		{"pmss1", 0, false},
		{"pmss1:", 1, false},
		{"pmss1:not-really-encrypted", 1, false}, // not base64
		{"pmss1:AAAA", 1, false},                 // too short
		{genuine, 1, true},
		{"pmss2:" + genuine[len(Prefix):], 2, false},
		{"pmss12:whatever", 12, false},
		{"pmss1234:x", 0, false},
		{"pmss01:" + genuine[len(Prefix):], 0, false}, // not canonical: not an envelope
		{"pmss0:x", 0, false},
	} {
		v, ok := Envelope(tc.in)
		if v != tc.version || ok != tc.ok {
			t.Errorf("Envelope(%q) = %d %v, want %d %v", tc.in, v, ok, tc.version, tc.ok)
		}
		// Open agrees: a value Envelope does not call version 1 passes
		// through as plaintext; one it does is decrypted or refused.
		got, err := sl.Open("x", tc.in)
		switch {
		case tc.version != 1 && (err != nil || got != tc.in):
			t.Errorf("Open(%q) = %q %v, want pass-through", tc.in, got, err)
		case tc.version == 1 && tc.ok && (err != nil || got != "v"):
			t.Errorf("Open(%q) = %q %v, want v", tc.in, got, err)
		case tc.version == 1 && !tc.ok && !errors.Is(err, ErrWrongKey):
			t.Errorf("Open(%q) = %q %v, want ErrWrongKey", tc.in, got, err)
		}
	}
}
