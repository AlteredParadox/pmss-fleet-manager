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

package main

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialOptions(t *testing.T) {
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	t.Setenv("PMSS_SECRET", "")
	t.Setenv("PMSS_SECRET_PREVIOUS", "")
	opts, enc, err := credentialOptions("", "", false, log)
	if err != nil || enc || len(opts) != 2 {
		t.Fatalf("plain: %d %v %v", len(opts), enc, err)
	}
	t.Setenv("PMSS_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("PMSS_SECRET_PREVIOUS", "fedcba9876543210fedcba9876543210")
	opts, enc, err = credentialOptions("", "", true, log)
	if err != nil || !enc || len(opts) != 3 {
		t.Fatalf("encrypted+previous+forget: %d %v %v", len(opts), enc, err)
	}
	if _, _, err = credentialOptions("", "", false, log); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PMSS_SECRET", "short")
	if _, _, err = credentialOptions("", "", false, log); err == nil {
		t.Fatal("short secret accepted")
	}
	t.Setenv("PMSS_SECRET", "")
	t.Setenv("PMSS_SECRET_PREVIOUS", "")
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("0123456789abcdef0123456789abcdef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, enc, err = credentialOptions(path, "", false, log); err != nil || !enc {
		t.Fatalf("file: %v %v", enc, err)
	}
	if _, _, err = credentialOptions(filepath.Join(t.TempDir(), "missing"), "", false, log); err == nil || !strings.Contains(err.Error(), "secret file") {
		t.Fatalf("missing file: %v", err)
	}
	// The previous secret from a file, with the current one from the environment.
	t.Setenv("PMSS_SECRET", "fedcba9876543210fedcba9876543210")
	if opts, enc, err = credentialOptions("", path, false, log); err != nil || !enc || len(opts) != 2 {
		t.Fatalf("previous from file: %d %v %v", len(opts), enc, err)
	}
	if _, _, err = credentialOptions("", filepath.Join(t.TempDir(), "missing"), false, log); err == nil || !strings.Contains(err.Error(), "previous secret") {
		t.Fatalf("missing previous file: %v", err)
	}
}

// The login file: one trailing line ending is dropped and nothing else;
// a configured file that does not yield user:password stops the start;
// value and file together are refused; errors never carry the contents.
func TestLoginFrom(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string, mode os.FileMode) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), mode); err != nil {
			t.Fatal(err)
		}
		os.Chmod(p, mode)
		return p
	}
	if l, w, err := loginFrom("me:pw", ""); err != nil || l != "me:pw" || w != "" {
		t.Fatalf("value only: %q %q %v", l, w, err)
	}
	for body, want := range map[string]string{
		"me:s3cret":     "me:s3cret",
		"me:s3cret\n":   "me:s3cret",
		"me:s3cret\r\n": "me:s3cret",
		"me:ends in ":   "me:ends in ", // a trailing space is part of the password
		"me:a:b\n":      "me:a:b",      // the first colon splits
	} {
		p := write("ok", body, 0o600)
		if l, w, err := loginFrom("", p); err != nil || l != want || w != "" {
			t.Errorf("%q: %q %q %v", body, l, w, err)
		}
	}
	for body, why := range map[string]string{
		"":                                       "is empty",
		"\n":                                     "is empty",
		"me:pw\n\n":                              "one line",
		"me:pw\nother:x":                         "one line",
		"mepw":                                   "user:password",
		":pw":                                    "user:password",
		"me:\n":                                  "user:password",
		strings.Repeat("a", maxLoginFile) + ":b": "larger than",
	} {
		p := write("bad", body, 0o600)
		_, _, err := loginFrom("", p)
		if err == nil || !strings.Contains(err.Error(), why) {
			t.Errorf("%q: %v", body, err)
		}
		if err != nil && len(body) > 3 && strings.Contains(err.Error(), body) {
			t.Errorf("error echoes the contents: %v", err)
		}
	}
	if _, _, err := loginFrom("", filepath.Join(dir, "missing")); err == nil || !strings.Contains(err.Error(), "cannot be read by uid") {
		t.Fatalf("missing: %v", err)
	}
	p := write("both", "me:pw", 0o600)
	if _, _, err := loginFrom("me:pw", p); err == nil || !strings.Contains(err.Error(), "not both") {
		t.Fatalf("both: %v", err)
	}
	p = write("loose", "me:hunter2", 0o644)
	l, w, err := loginFrom("", p)
	if err != nil || l != "me:hunter2" || !strings.Contains(w, "readable by other users") || strings.Contains(w, "hunter2") {
		t.Fatalf("loose mode: %q %q %v", l, w, err)
	}
}

func TestSSHKeyWarning(t *testing.T) {
	if w := sshKeyWarning(""); w != "" {
		t.Fatalf("unset: %q", w)
	}
	p := filepath.Join(t.TempDir(), "key")
	os.WriteFile(p, []byte("k"), 0o600)
	if w := sshKeyWarning(p); w != "" {
		t.Fatalf("readable: %q", w)
	}
	if w := sshKeyWarning(p + ".missing"); !strings.Contains(w, "cannot be read by uid") || !strings.Contains(w, "fall back") {
		t.Fatalf("missing: %q", w)
	}
}
