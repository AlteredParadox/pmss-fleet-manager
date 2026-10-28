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

package collect

// The transfer form's folder picker: one directory's subdirectories at a
// time, inside the account's home, bounded. It reads; it never changes
// anything, and it is no file manager (no files, previews, renames or
// recursive listings).

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// xferDirsMax bounds how many subdirectories one listing returns.
const xferDirsMax = 1000

// xferDirsScript resolves $P (relative to HOME, absolute, or empty for
// HOME) and prints the directory, HOME, then the names of its immediate
// subdirectories, NUL-separated and capped. find -type d without -L skips
// symlinks, which a copy would not follow either.
const xferDirsScript = `set -u
LC_ALL=C; export LC_ALL
die() { echo "ERR $*"; exit 3; }
home=$(realpath -e -- "$HOME") || die "cannot resolve HOME"
case "$P" in "") p=$home;; /*) p=$P;; *) p="$home/$P";; esac
d=$(realpath -e -- "$p" 2>/dev/null) || die "no such directory"
case "$d/" in "$home"/*) ;; *) die "that folder is outside your home directory";; esac
[ -d "$d" ] || die "not a directory"
[ -r "$d" ] && [ -x "$d" ] || die "cannot read that directory"
printf 'DIR %s\nHOME %s\n' "$d" "$home"
find "$d" -mindepth 1 -maxdepth 1 -type d -printf '%f\0' 2>/dev/null | head -z -n ` + "1001" + `
exit 0
`

// xferInputError is the box's own refusal of a path (outside the home, no
// such folder): shown as it is, answered like any refused input (400).
type xferInputError string

func (e xferInputError) Error() string        { return string(e) }
func (e xferInputError) Is(target error) bool { return target == ErrTransferInput }

// DirListing is one folder of the picker.
type DirListing struct {
	BoxRev    int64    `json:"box_rev"` // the record revision the listing was read under
	Path      string   `json:"path"`    // absolute, resolved
	Rel       string   `json:"rel"`     // relative to home; "" is home itself
	Dirs      []string `json:"dirs"`
	Truncated bool     `json:"truncated"` // more than xferDirsMax subdirectories
	Skipped   int      `json:"skipped"`   // names a path cannot carry (control characters, invalid UTF-8)
}

// parseXferDirs reads the script's output: two header lines, then names.
func parseXferDirs(out []byte) (DirListing, error) {
	path, home, rest, err := xferDirsHeaders(out)
	if err != nil {
		return DirListing{}, err
	}
	l := DirListing{Path: path, Rel: strings.TrimPrefix(strings.TrimPrefix(path, home), "/")}
	l.Dirs, l.Truncated, l.Skipped = xferDirNames(rest)
	return l, nil
}

// xferDirsHeaders reads the DIR and HOME lines (or the box's ERR line).
func xferDirsHeaders(out []byte) (path, home string, rest []byte, err error) {
	for i := 0; i < 2; i++ {
		nl := bytes.IndexByte(out, '\n')
		if nl < 0 {
			return "", "", nil, fmt.Errorf("unexpected listing output")
		}
		line := string(out[:nl])
		out = out[nl+1:]
		if msg, ok := strings.CutPrefix(line, "ERR "); ok {
			return "", "", nil, &xferRefusal{msg}
		}
		if v, ok := strings.CutPrefix(line, "DIR "); ok {
			path = v
		} else if v, ok := strings.CutPrefix(line, "HOME "); ok {
			home = v
		}
	}
	if path == "" || home == "" {
		return "", "", nil, fmt.Errorf("unexpected listing output")
	}
	return path, home, out, nil
}

// xferDirNames keeps up to xferDirsMax usable names, sorted.
func xferDirNames(raw []byte) (dirs []string, truncated bool, skipped int) {
	dirs = []string{}
	for _, n := range bytes.Split(raw, []byte{0}) {
		if len(n) == 0 {
			continue
		}
		if len(dirs) == xferDirsMax {
			truncated = true
			break
		}
		s := string(n)
		if !utf8.ValidString(s) || strings.ContainsFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
			skipped++
			continue
		}
		dirs = append(dirs, s)
	}
	sort.Slice(dirs, func(a, b int) bool {
		x, y := strings.ToLower(dirs[a]), strings.ToLower(dirs[b])
		if x != y {
			return x < y
		}
		return dirs[a] < dirs[b]
	})
	return dirs, truncated, skipped
}

// xferDirsResult turns one listing run into its answer. The box's own
// refusal is an input error; any other failure of the run is an error even
// when the headers arrived, since the script prints them before the names:
// a run cut off by a lost connection or the deadline must not pass for a
// complete (or empty) folder (fred on PR 143).
func xferDirsResult(out, stderr []byte, runErr error) (DirListing, error) {
	l, perr := parseXferDirs(out)
	var refused *xferRefusal
	if errors.As(perr, &refused) {
		return DirListing{}, xferInputError(refused.msg) // the box answered: nothing to retry
	}
	if runErr != nil {
		return DirListing{}, fmt.Errorf("listing not completed: %v %s", runErr, clip(strings.TrimSpace(string(stderr)), 300))
	}
	return l, perr
}

// ListTransferDirs lists one folder on box id for the transfer picker,
// on the poller of the revision ctx names (WithBoxRevision), so a listing
// never comes from a record the form was not drawn from.
func (m *Manager) ListTransferDirs(ctx context.Context, id int64, path string) (DirListing, error) {
	p, err := m.actionPoller(ctx, id)
	if err != nil {
		return DirListing{}, err
	}
	if _, err := p.readyState(); err != nil {
		return DirListing{}, err
	}
	// "" and "~" are home; "~/x" is x, relative to home; absolute stays.
	if path == "~" {
		path = ""
	}
	path = strings.TrimPrefix(path, "~/")
	if path != "" {
		if err := validXferPath(path, false); err != nil {
			return DirListing{}, fmt.Errorf("%w: %v", ErrTransferInput, err)
		}
	}
	cli, err := p.conn(ctx)
	if err != nil {
		return DirListing{}, err
	}
	rctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, stderr, err := cli.Run(rctx, "sh", strings.NewReader("P="+shellQuote(path)+"\n"+xferDirsScript))
	l, err := xferDirsResult(out, stderr, err)
	if err != nil {
		return DirListing{}, err
	}
	l.BoxRev = p.box.Revision
	return l, nil
}
