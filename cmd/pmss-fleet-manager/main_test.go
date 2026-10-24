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
	opts, enc, err := credentialOptions("", false, log)
	if err != nil || enc || len(opts) != 2 {
		t.Fatalf("plain: %d %v %v", len(opts), enc, err)
	}
	t.Setenv("PMSS_SECRET", "0123456789abcdef0123456789abcdef")
	t.Setenv("PMSS_SECRET_PREVIOUS", "fedcba9876543210fedcba9876543210")
	opts, enc, err = credentialOptions("", true, log)
	if err != nil || !enc || len(opts) != 3 {
		t.Fatalf("encrypted+previous+forget: %d %v %v", len(opts), enc, err)
	}
	if _, _, err = credentialOptions("", false, log); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PMSS_SECRET", "short")
	if _, _, err = credentialOptions("", false, log); err == nil {
		t.Fatal("short secret accepted")
	}
	t.Setenv("PMSS_SECRET", "")
	t.Setenv("PMSS_SECRET_PREVIOUS", "")
	path := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(path, []byte("0123456789abcdef0123456789abcdef\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, enc, err = credentialOptions(path, false, log); err != nil || !enc {
		t.Fatalf("file: %v %v", enc, err)
	}
	if _, _, err = credentialOptions(filepath.Join(t.TempDir(), "missing"), false, log); err == nil || !strings.Contains(err.Error(), "secret file") {
		t.Fatalf("missing file: %v", err)
	}
}
