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

// Package secrets encrypts the credentials the store keeps (SSH and WebUI
// passwords, the Pulsed Media API key, the pmdb.fyi upload key) under a key
// derived from a secret the operator supplies outside the database. With
// it, a copy of the SQLite file alone (a backup, an appdata share, a file
// attached to a bug report) does not give the credentials away.
//
// What it does not do: protect against anyone who can read the process's
// environment or the secret file, which on the same host is the same
// person who can read a 0600 database. The secret must be random (the
// output of `openssl rand -base64 32`), not a passphrase: the derivation is
// HKDF, which does no stretching, because a short memorable secret would
// be brute-forced offline against the very file this is meant to protect.
//
// Format: "pmss1:" + base64 (raw, standard alphabet) of nonce || AES-256-GCM
// ciphertext. The column's label is the additional data, so a ciphertext
// moved to another column does not open. An empty value stays empty: the
// store treats an empty secret as "none" and an empty PUT field as "keep",
// and both have to survive the round trip unchanged.
package secrets

import (
	"bytes"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
)

// Prefix marks a sealed value in the database.
const Prefix = "pmss1:"

// MinSecretLen is the shortest master secret accepted, in bytes. 24 is the
// base64 length of 16 random bytes; the recommended 32 bytes encode to 44.
const MinSecretLen = 24

// hkdfInfo binds the derived key to this use; a different info string
// would be a different key from the same secret.
const hkdfInfo = "pmss-fleet-manager stored credentials v1"

// ErrNoKey is returned when a stored value is sealed but no secret is
// configured: the process cannot use the credential, and must say so
// rather than send the ciphertext to a server as the password.
var ErrNoKey = errors.New("the credential is encrypted but no PMSS_SECRET (or -secret-file) is set")

// ErrWrongKey is returned when a sealed value does not open with the
// configured secret: it was sealed under another one, or the row was
// damaged.
var ErrWrongKey = errors.New("the credential does not decrypt with the configured PMSS_SECRET: it was encrypted with a different secret, or the row is damaged")

// Sealer seals and opens credentials. A nil *Sealer is valid and means
// "no encryption": Seal returns the plaintext and Open accepts only
// plaintext, so the store can call both unconditionally.
type Sealer struct {
	aead cipher.AEAD
}

// New derives the encryption key from master.
func New(master []byte) (*Sealer, error) {
	if len(master) < MinSecretLen {
		return nil, fmt.Errorf("the secret is %d bytes long; it must be at least %d, e.g. the output of `openssl rand -base64 32`", len(master), MinSecretLen)
	}
	// None of these can fail: the secret is non-empty, the key 32 bytes.
	key := must(hkdf.Key(sha256.New, master, nil, hkdfInfo, 32))
	return &Sealer{aead: must(cipher.NewGCM(must(aes.NewCipher(key))))}, nil
}

// must is for the constructors above, whose errors cannot occur for the
// fixed parameters they are given.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Enabled reports whether values are encrypted.
func (s *Sealer) Enabled() bool { return s != nil }

// IsSealed reports whether a stored value is a sealed one.
func IsSealed(stored string) bool { return strings.HasPrefix(stored, Prefix) }

// Seal encrypts plain for the column named label. "" seals to "".
func (s *Sealer) Seal(label, plain string) string {
	if s == nil || plain == "" {
		return plain
	}
	nonce := make([]byte, s.aead.NonceSize())
	rand.Read(nonce) // cannot fail (crypto/rand since Go 1.24)
	out := s.aead.Seal(nonce, nonce, []byte(plain), []byte(label))
	return Prefix + base64.RawStdEncoding.EncodeToString(out)
}

// Open decrypts a stored value for the column named label. A value without
// the prefix is plaintext from before the secret was configured and is
// returned as is; the store re-seals those at startup.
func (s *Sealer) Open(label, stored string) (string, error) {
	if !IsSealed(stored) {
		return stored, nil
	}
	if s == nil {
		return "", ErrNoKey
	}
	raw, err := base64.RawStdEncoding.DecodeString(stored[len(Prefix):])
	ns := s.aead.NonceSize()
	if err != nil || len(raw) < ns+s.aead.Overhead() {
		return "", ErrWrongKey
	}
	plain, err := s.aead.Open(nil, raw[:ns], raw[ns:], []byte(label))
	if err != nil {
		return "", ErrWrongKey
	}
	return string(plain), nil
}

// Loaded is a master secret and where it came from.
type Loaded struct {
	Master []byte
	Source string // "PMSS_SECRET" or the file's path, for the startup log
	// Warning is set when the secret file is readable by other users, so
	// the operator is told without the start being refused: the file may
	// be a mounted Docker or Kubernetes secret whose mode is not theirs
	// to set.
	Warning string
}

// Load reads the master secret from the environment value or the file,
// whichever is set. Neither set means no encryption (nil, nil). Both set
// is refused: a silent precedence rule is how one ends up encrypting
// under a secret one did not mean to. Trailing whitespace (the newline
// an editor or echo adds) is not part of a file's secret.
func Load(envValue, filePath string) (*Loaded, error) {
	switch {
	case envValue != "" && filePath != "":
		return nil, errors.New("set PMSS_SECRET or -secret-file (PMSS_SECRET_FILE), not both")
	case envValue != "":
		return &Loaded{Master: []byte(envValue), Source: "PMSS_SECRET"}, nil
	case filePath == "":
		return nil, nil
	}
	b, err := os.ReadFile(filePath)
	if err != nil {
		return nil, fmt.Errorf("secret file: %w", err)
	}
	l := &Loaded{Master: bytes.TrimRight(b, " \t\r\n"), Source: filePath}
	if fi, err := os.Stat(filePath); err == nil && fi.Mode().Perm()&0o077 != 0 {
		l.Warning = fmt.Sprintf("the secret file %s is readable by other users (mode %04o); chmod 600 it", filePath, fi.Mode().Perm())
	}
	return l, nil
}

// Resolved is what the process configuration asks for.
type Resolved struct {
	Current  *Sealer // nil: credentials are stored in plain text
	Previous *Sealer // nil: no secret change in progress
	Source   string  // where Current came from, for the startup log
	Warning  string  // a loose secret-file mode, see Loaded
}

// Resolve turns the configuration (the PMSS_SECRET value, the -secret-file
// path, the PMSS_SECRET_PREVIOUS value) into sealers. Errors are meant for
// the operator as they are.
func Resolve(secret, secretFile, previous string) (*Resolved, error) {
	var r Resolved
	loaded, err := Load(secret, secretFile)
	if err != nil {
		return nil, err
	}
	if loaded != nil {
		if r.Current, err = New(loaded.Master); err != nil {
			return nil, fmt.Errorf("credential secret: %w", err)
		}
		r.Source, r.Warning = loaded.Source, loaded.Warning
	}
	if previous != "" {
		if r.Previous, err = New([]byte(previous)); err != nil {
			return nil, fmt.Errorf("PMSS_SECRET_PREVIOUS: %w", err)
		}
	}
	return &r, nil
}
