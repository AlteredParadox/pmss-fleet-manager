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

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func listLocal(t *testing.T, home, p string) (DirListing, error) {
	t.Helper()
	cmd := exec.Command("sh")
	cmd.Stdin = strings.NewReader("P=" + shellQuote(p) + "\n" + xferDirsScript)
	cmd.Env = append(os.Environ(), "HOME="+home)
	out, _ := cmd.Output() // a refusal exits 3 after its ERR line
	return parseXferDirs(out)
}

// One folder at a time: its subdirectories only (no files, no symlinks,
// nothing deeper), names a path cannot carry skipped, sorted.
func TestXferDirsListsOneFolder(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	for _, d := range []string{"a", "B", ".hidden", "a/deep", "x\ny", "it's \"q\""} {
		os.MkdirAll(filepath.Join(home, d), 0o700)
	}
	os.WriteFile(filepath.Join(home, "file"), []byte("x"), 0o600)
	os.Symlink(outside, filepath.Join(home, "ln"))
	l, err := listLocal(t, home, "")
	if err != nil {
		t.Fatal(err)
	}
	resolved, _ := filepath.EvalSymlinks(home)
	if l.Path != resolved || l.Rel != "" || l.Skipped != 1 || l.Truncated {
		t.Errorf("home: %+v", l)
	}
	if want := []string{".hidden", "a", "B", "it's \"q\""}; !slices.Equal(l.Dirs, want) {
		t.Errorf("dirs %q, want %q", l.Dirs, want)
	}
	if l, err := listLocal(t, home, "a"); err != nil || l.Rel != "a" || !slices.Equal(l.Dirs, []string{"deep"}) {
		t.Errorf("a: %+v %v", l, err)
	}
	if l, err := listLocal(t, home, resolved+"/a/deep"); err != nil || l.Rel != "a/deep" || len(l.Dirs) != 0 {
		t.Errorf("absolute: %+v %v", l, err)
	}
}

// The picker stays inside the home: a symlink out, a path above it, a
// file and a missing folder are refused by the box.
func TestXferDirsRefusals(t *testing.T) {
	home, outside := t.TempDir(), t.TempDir()
	os.Symlink(outside, filepath.Join(home, "ln"))
	os.WriteFile(filepath.Join(home, "file"), []byte("x"), 0o600)
	for p, want := range map[string]string{
		"ln":                  "outside your home",
		filepath.Dir(home):    "outside your home",
		"/":                   "outside your home",
		"file":                "not a directory",
		"missing":             "no such directory",
		home + "/../" + "zzz": "no such directory",
	} {
		_, err := listLocal(t, home, p)
		var refused *xferRefusal
		if !errors.As(err, &refused) || !strings.Contains(refused.msg, want) {
			t.Errorf("%q: %v, want %q", p, err, want)
		}
	}
}

func TestXferDirsBounded(t *testing.T) {
	home := t.TempDir()
	for i := 0; i < xferDirsMax+50; i++ {
		os.Mkdir(filepath.Join(home, fmt.Sprintf("d%04d", i)), 0o700)
	}
	l, err := listLocal(t, home, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Dirs) != xferDirsMax || !l.Truncated {
		t.Errorf("%d dirs, truncated %v", len(l.Dirs), l.Truncated)
	}
}

func TestParseXferDirsRejectsGarbage(t *testing.T) {
	for _, out := range []string{"", "DIR /h\n", "junk\nmore\n"} {
		if _, err := parseXferDirs([]byte(out)); err == nil {
			t.Errorf("%q parsed", out)
		}
	}
}

func TestXferInputErrorIsInput(t *testing.T) {
	err := error(xferInputError("that folder is outside your home directory"))
	if !errors.Is(err, ErrTransferInput) || err.Error() != "that folder is outside your home directory" {
		t.Errorf("%v", err)
	}
}

// A run that fails after the headers (a lost connection, the deadline) is
// an error, never a complete or empty folder; the box's own refusal stays
// an input error; a clean run is the listing (fred on PR 143).
func TestXferDirsResultNeedsACompleteRun(t *testing.T) {
	partial := []byte("DIR /home/u\nHOME /home/u\na\x00b\x00")
	if l, err := xferDirsResult(partial, nil, errors.New("context deadline exceeded")); err == nil {
		t.Errorf("interrupted run accepted: %+v", l)
	}
	if l, err := xferDirsResult([]byte("DIR /home/u\nHOME /home/u\n"), nil, errors.New("connection lost")); err == nil {
		t.Errorf("interrupted empty run accepted: %+v", l)
	}
	if _, err := xferDirsResult([]byte("ERR no such directory\n"), nil, errors.New("exit status 3")); !errors.Is(err, ErrTransferInput) || err.Error() != "no such directory" {
		t.Errorf("refusal: %v", err)
	}
	if l, err := xferDirsResult(partial, nil, nil); err != nil || len(l.Dirs) != 2 {
		t.Errorf("clean run: %+v %v", l, err)
	}
}
