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
	"strconv"
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

// IsSealed reports whether a stored value carries the version-1 envelope.
// Whether it is ciphertext is the store's call (its encoding marker),
// since a plaintext password may start with anything.
func IsSealed(stored string) bool {
	v, _ := Envelope(stored)
	return v == 1
}

// Envelope is the one parser of the envelope grammar, shared by Open and
// the store's classification so the two never disagree. It reports the
// envelope version a stored value is written in (0: none) and, for
// version 1, whether the body is a well-formed nonce and ciphertext. The
// grammar is canonical: "pmss", a version of one to three digits with no
// leading zero, ":". A plaintext that happens to begin with "pmss1:" is
// almost never well-formed: the body has to be raw standard base64 of at
// least 28 bytes.
func Envelope(stored string) (version int, wellFormed bool) {
	rest, ok := strings.CutPrefix(stored, "pmss")
	if !ok {
		return 0, false
	}
	digits, _, ok := strings.Cut(rest, ":")
	if !ok || digits == "" || len(digits) > 3 || digits[0] == '0' {
		return 0, false
	}
	for _, r := range digits {
		if r < '0' || r > '9' {
			return 0, false
		}
	}
	version, _ = strconv.Atoi(digits)
	if version != 1 {
		return version, false
	}
	raw, err := base64.RawStdEncoding.DecodeString(stored[len(Prefix):])
	return 1, err == nil && len(raw) >= nonceSize+tagSize
}

// AES-GCM parameters, fixed by the construction.
const (
	nonceSize = 12
	tagSize   = 16
)

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
// the version-1 envelope is plaintext (from before the secret was
// configured; the store re-seals those at startup) and is returned as is.
// One with the envelope but a malformed body is a damaged ciphertext.
func (s *Sealer) Open(label, stored string) (string, error) {
	version, wellFormed := Envelope(stored)
	if version != 1 {
		return stored, nil
	}
	if s == nil {
		return "", ErrNoKey
	}
	if !wellFormed {
		return "", ErrWrongKey
	}
	raw, _ := base64.RawStdEncoding.DecodeString(stored[len(Prefix):])
	ns := s.aead.NonceSize()
	plain, err := s.aead.Open(nil, raw[:ns], raw[ns:], []byte(label))
	if err != nil {
		return "", ErrWrongKey
	}
	return string(plain), nil
}

// Names are the configuration options a secret can come from, for
// messages: the environment variable holding the secret itself, the flag
// naming a file, and that flag's environment twin.
type Names struct{ Env, Flag, FileEnv string }

// The two secrets the process can be given.
var (
	CurrentNames  = Names{"PMSS_SECRET", "-secret-file", "PMSS_SECRET_FILE"}
	PreviousNames = Names{"PMSS_SECRET_PREVIOUS", "-previous-secret-file", "PMSS_SECRET_PREVIOUS_FILE"}
)

// Loaded is a master secret and where it came from.
type Loaded struct {
	Master []byte
	Source string // the environment variable's name or the file's path, for the startup log
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
func Load(n Names, envValue, filePath string) (*Loaded, error) {
	switch {
	case envValue != "" && filePath != "":
		return nil, fmt.Errorf("set %s or %s (%s), not both", n.Env, n.Flag, n.FileEnv)
	case envValue != "":
		return &Loaded{Master: []byte(envValue), Source: n.Env}, nil
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

// Resolve turns the configuration (the PMSS_SECRET value or the
// -secret-file path; the PMSS_SECRET_PREVIOUS value or the
// -previous-secret-file path) into sealers. Errors are meant for the
// operator as they are.
func Resolve(secret, secretFile, previous, previousFile string) (*Resolved, error) {
	var r Resolved
	loaded, err := Load(CurrentNames, secret, secretFile)
	if err != nil {
		return nil, err
	}
	if loaded != nil {
		if r.Current, err = New(loaded.Master); err != nil {
			return nil, fmt.Errorf("credential secret: %w", err)
		}
		r.Source, r.Warning = loaded.Source, loaded.Warning
	}
	prev, err := Load(PreviousNames, previous, previousFile)
	if err != nil {
		return nil, fmt.Errorf("previous secret: %w", err)
	}
	if prev != nil {
		if r.Previous, err = New(prev.Master); err != nil {
			return nil, fmt.Errorf("previous secret: %w", err)
		}
		if prev.Warning != "" && r.Warning == "" {
			r.Warning = prev.Warning
		}
	}
	return &r, nil
}
