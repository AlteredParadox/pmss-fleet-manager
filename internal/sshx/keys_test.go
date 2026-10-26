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

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"golang.org/x/crypto/ssh"
)

func writeKey(t *testing.T, path string) ssh.PublicKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	if err := os.WriteFile(path, pem.EncodeToMemory(blk), 0o600); err != nil {
		t.Fatal(err)
	}
	sp, _ := ssh.NewPublicKey(pub)
	return sp
}

// The -ssh-key default sits between the box's own key path (which always
// wins, alone) and the usual ~/.ssh names (still tried after it); a
// default that is missing or not a key is named in the error, not fatal.
func TestDefaultKey(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Cleanup(func() { DefaultKey = "" })
	std := writeKey(t, filepath.Join(home, ".ssh", "id_ed25519"))
	glob := writeKey(t, filepath.Join(home, "secrets", "fleet"))
	own := writeKey(t, filepath.Join(home, "own"))
	fp := func(signers []ssh.Signer) []string {
		var out []string
		for _, s := range signers {
			out = append(out, ssh.FingerprintSHA256(s.PublicKey()))
		}
		return out
	}
	// Unset: the usual names only, as before.
	s, _, err := keySigners(Config{})
	if err != nil || strings.Join(fp(s), ",") != ssh.FingerprintSHA256(std) {
		t.Fatalf("no default: %v %v", fp(s), err)
	}
	// Set: tried first, then the usual names.
	DefaultKey = filepath.Join(home, "secrets", "fleet")
	s, _, err = keySigners(Config{})
	if err != nil || strings.Join(fp(s), ",") != ssh.FingerprintSHA256(glob)+","+ssh.FingerprintSHA256(std) {
		t.Fatalf("default first: %v %v", fp(s), err)
	}
	// The box's own path wins, alone.
	s, _, err = keySigners(Config{KeyPath: filepath.Join(home, "own")})
	if err != nil || strings.Join(fp(s), ",") != ssh.FingerprintSHA256(own) {
		t.Fatalf("box path: %v %v", fp(s), err)
	}
	// A missing default, or one that is not a key, is reported, not fatal.
	DefaultKey = filepath.Join(home, "secrets", "missing")
	s, notes, err := keySigners(Config{})
	if err != nil || len(s) != 1 || len(notes) != 1 || !strings.Contains(notes[0], "the -ssh-key default: no such file") {
		t.Fatalf("missing default: %v %v %v", fp(s), notes, err)
	}
	os.WriteFile(filepath.Join(home, "secrets", "junk"), []byte("not a key"), 0o600)
	DefaultKey = filepath.Join(home, "secrets", "junk")
	_, notes, _ = keySigners(Config{})
	if len(notes) != 1 || !strings.Contains(notes[0], "not an unencrypted private key") {
		t.Fatalf("junk default: %v", notes)
	}
	// With key auth selected and nothing usable, the error names it.
	os.Remove(filepath.Join(home, ".ssh", "id_ed25519"))
	if _, _, err := keyMethods(Config{AuthMethod: "key"}); err == nil || !strings.Contains(err.Error(), "could not use "+DefaultKey+" (the -ssh-key default") {
		t.Fatalf("key auth: %v", err)
	}
	// "~/" is the home, as for a box's key path.
	DefaultKey = "~/secrets/fleet"
	if p := keyPaths(Config{}); p[0] != filepath.Join(home, "secrets", "fleet") {
		t.Fatalf("tilde: %v", p)
	}
}
