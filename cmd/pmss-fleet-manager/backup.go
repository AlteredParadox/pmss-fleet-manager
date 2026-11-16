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
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"pmss-fleet-manager/internal/secrets"
	"pmss-fleet-manager/internal/store"
)

const (
	dbName   = "pmss-fleet-manager.db"
	lockName = "pmss-fleet-manager.lock"
)

// Message prefixes of the two commands.
const (
	backupTag  = "backup:"
	restoreTag = "restore:"
)

var errDataDirInUse = errors.New("another pmss-fleet-manager process is using this data directory")

// backupCmd is `pmss-fleet-manager backup -out FILE`: a consistent copy
// of the database, taken while the dashboard runs or not. -out - writes
// it to stdout (for `docker exec`, where the container cannot see the
// host's paths), staged in a private directory under the data directory.
func backupCmd(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", envOr("PMSS_DATA", "./data"), "the server's -data directory")
	out := fs.String("out", "", "file to write the backup to (must not exist), or - for stdout")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *out == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: pmss-fleet-manager backup [-data DIR] -out FILE|-")
		return 2
	}
	src := filepath.Join(*dataDir, dbName)
	ctx := context.Background()
	if *out != "-" {
		sum, err := store.Backup(ctx, src, *out)
		if err != nil {
			fmt.Fprintln(stderr, backupTag, err)
			return 1
		}
		fmt.Fprintf(stderr, "backup: wrote %s (%s)\n", *out, describe(sum))
		return 0
	}
	tmp, err := os.MkdirTemp(*dataDir, ".backup-")
	if err != nil {
		fmt.Fprintln(stderr, backupTag, err)
		return 1
	}
	defer os.RemoveAll(tmp)
	staged := filepath.Join(tmp, dbName)
	sum, err := store.Backup(ctx, src, staged)
	if err == nil {
		err = streamFile(staged, stdout)
	}
	if err != nil {
		fmt.Fprintln(stderr, backupTag, err)
		return 1
	}
	fmt.Fprintf(stderr, "backup: wrote %d bytes to stdout (%s)\n", sum.Bytes, describe(sum))
	return 0
}

func streamFile(path string, w io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// restoreCmd is `pmss-fleet-manager restore -in FILE`: it replaces the
// database with a backup while the dashboard is stopped, after checking
// that the dashboard would start with it and the configured secret, and
// keeps the database it replaces beside it.
func restoreCmd(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("restore", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data", envOr("PMSS_DATA", "./data"), "the server's -data directory")
	in := fs.String("in", "", "the backup file to restore")
	secretFile := fs.String("secret-file", os.Getenv("PMSS_SECRET_FILE"), "the secret the server will start with (or set PMSS_SECRET); needed when the backup's credentials are encrypted")
	skipCheck := fs.Bool("skip-secret-check", false, "restore without checking that the configured secret decrypts the backup's credentials (the secret is lost: start once with -forget-encrypted-credentials afterwards)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *in == "" || fs.NArg() > 0 {
		fmt.Fprintln(stderr, "usage: pmss-fleet-manager restore [-data DIR] [-secret-file FILE] -in FILE")
		return 2
	}
	creds, err := secrets.Resolve(os.Getenv("PMSS_SECRET"), *secretFile, "", "")
	if err != nil {
		fmt.Fprintln(stderr, restoreTag, err)
		return 2
	}
	for _, w := range creds.Warnings {
		fmt.Fprintln(stderr, "restore: warning:", w)
	}
	release, err := lockDataDir(*dataDir)
	if err != nil {
		if errors.Is(err, errDataDirInUse) {
			err = fmt.Errorf("%w (%s): stop the dashboard first", err, *dataDir)
		}
		fmt.Fprintln(stderr, restoreTag, err)
		return 1
	}
	defer release()
	res, err := store.Restore(context.Background(), filepath.Join(*dataDir, dbName), *in, !*skipCheck, store.WithSealer(creds.Current))
	if err != nil {
		fmt.Fprintln(stderr, restoreTag, err)
		if errors.Is(err, secrets.ErrNoKey) || errors.Is(err, secrets.ErrWrongKey) {
			fmt.Fprintln(stderr, "restore: the backup's credentials are encrypted: give the secret it was made with (-secret-file or PMSS_SECRET); if that secret is lost, -skip-secret-check restores it anyway, and the first start then needs -forget-encrypted-credentials")
		}
		return 1
	}
	fmt.Fprintf(stderr, "restore: restored %s into %s (%s)\n", *in, *dataDir, describe(res.Summary))
	if res.Previous != "" {
		fmt.Fprintf(stderr, "restore: the database it replaced is saved as %s\n", res.Previous)
	}
	switch {
	case *skipCheck && res.Encrypted:
		fmt.Fprintln(stderr, "restore: the secret was not checked: start with the backup's secret, or once with -forget-encrypted-credentials")
	case res.Encrypted:
		fmt.Fprintln(stderr, "restore: start the dashboard with the same secret")
	}
	return 0
}

func describe(s store.Summary) string {
	enc := "credentials in plain text: keep the file private"
	if s.Encrypted {
		enc = "credentials encrypted: restoring needs the same secret"
	}
	return fmt.Sprintf("%d boxes, %s", s.Boxes, enc)
}
