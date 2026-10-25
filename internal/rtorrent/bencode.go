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

package rtorrent

import (
	"crypto/sha1"
	"encoding/base32"
	"encoding/hex"
	"errors"
	"net/url"
	"strconv"
	"strings"
)

// ErrNoInfoHash is a torrent file or magnet link whose info hash cannot be
// read: the label cannot then be set on the torrent by hash.
var ErrNoInfoHash = errors.New("rtorrent: cannot determine the torrent's info hash")

// InfoHash returns the upper-case hex SHA-1 of the bencoded "info"
// dictionary of a .torrent file, which is the hash rTorrent addresses the
// torrent by once loaded. The scan walks the top-level dictionary only as
// far as the info key and never allocates for values; a malformed file is
// ErrNoInfoHash.
func InfoHash(torrent []byte) (string, error) {
	if len(torrent) < 2 || torrent[0] != 'd' {
		return "", ErrNoInfoHash
	}
	i := 1
	for i < len(torrent) && torrent[i] != 'e' {
		// key: a byte string
		key, n, err := benString(torrent[i:])
		if err != nil {
			return "", ErrNoInfoHash
		}
		i += n
		start := i
		n, err = benSkip(torrent[i:], 0)
		if err != nil {
			return "", ErrNoInfoHash
		}
		i += n
		if string(key) == "info" {
			sum := sha1.Sum(torrent[start:i])
			return strings.ToUpper(hex.EncodeToString(sum[:])), nil
		}
	}
	return "", ErrNoInfoHash
}

// benString reads "<len>:<bytes>" and returns the bytes and the total
// length consumed.
func benString(b []byte) ([]byte, int, error) {
	colon := -1
	for j := 0; j < len(b) && j < 12; j++ {
		if b[j] == ':' {
			colon = j
			break
		}
	}
	if colon <= 0 {
		return nil, 0, ErrNoInfoHash
	}
	n, err := strconv.Atoi(string(b[:colon]))
	if err != nil || n < 0 || colon+1+n > len(b) {
		return nil, 0, ErrNoInfoHash
	}
	return b[colon+1 : colon+1+n], colon + 1 + n, nil
}

// benSkip returns the length of the bencoded value at the start of b.
func benSkip(b []byte, depth int) (int, error) {
	if len(b) == 0 || depth > 64 {
		return 0, ErrNoInfoHash
	}
	switch c := b[0]; {
	case c == 'i':
		end := strings.IndexByte(string(b[:min(len(b), 24)]), 'e')
		if end < 2 {
			return 0, ErrNoInfoHash
		}
		return end + 1, nil
	case c >= '0' && c <= '9':
		_, n, err := benString(b)
		return n, err
	case c == 'l' || c == 'd':
		i := 1
		for {
			if i >= len(b) {
				return 0, ErrNoInfoHash
			}
			if b[i] == 'e' {
				return i + 1, nil
			}
			if c == 'd' { // key
				_, n, err := benString(b[i:])
				if err != nil {
					return 0, err
				}
				i += n
				if i >= len(b) || b[i] == 'e' {
					return 0, ErrNoInfoHash // a key without a value
				}
			}
			n, err := benSkip(b[i:], depth+1)
			if err != nil {
				return 0, err
			}
			i += n
		}
	}
	return 0, ErrNoInfoHash
}

// MagnetHash returns the upper-case hex info hash named by a magnet link's
// xt=urn:btih: parameter (40 hex or 32 base32 characters), or
// ErrNoInfoHash. A v2-only magnet (urn:btmh) has no v1 hash for rTorrent.
func MagnetHash(magnet string) (string, error) {
	if !strings.HasPrefix(strings.ToLower(magnet), "magnet:?") {
		return "", ErrNoInfoHash
	}
	q, err := url.ParseQuery(magnet[len("magnet:?"):])
	if err != nil {
		return "", ErrNoInfoHash
	}
	for _, xt := range q["xt"] {
		if !strings.HasPrefix(strings.ToLower(xt), "urn:btih:") {
			continue
		}
		h := xt[len("urn:btih:"):]
		switch len(h) {
		case 40:
			if ValidHash(h) {
				return strings.ToUpper(h), nil
			}
		case 32:
			raw, err := base32.StdEncoding.DecodeString(strings.ToUpper(h))
			if err == nil && len(raw) == 20 {
				return strings.ToUpper(hex.EncodeToString(raw)), nil
			}
		}
	}
	return "", ErrNoInfoHash
}
