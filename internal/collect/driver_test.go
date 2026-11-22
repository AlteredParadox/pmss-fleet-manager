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
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sync"
	"testing"

	"pmss-fleet-manager/internal/dockerx"
)

// The driver is read on every poll, not once per daemon: a daemon restarted
// on another driver must be seen, and a daemon that stops answering /info
// must keep the last known value instead of blanking it (an empty driver
// would silently resolve the alert).
func TestRefreshDriverPicksUpSwitch(t *testing.T) {
	var mu sync.Mutex
	driver := "fuse-overlayfs"
	sock := filepath.Join(t.TempDir(), "docker.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		mu.Lock()
		defer mu.Unlock()
		fmt.Fprintf(w, `{"Driver":%q}`, driver)
	})}
	go srv.Serve(ln)
	closed := false
	defer func() {
		if !closed {
			srv.Close()
		}
	}()
	cli := dockerx.New(func(ctx context.Context) (net.Conn, error) { return net.Dial("unix", sock) })
	defer cli.Close()
	ctx := context.Background()

	ds := DockerState{}
	refreshDriver(ctx, cli, &ds)
	if ds.Driver != "fuse-overlayfs" {
		t.Fatalf("first read: %q", ds.Driver)
	}
	mu.Lock()
	driver = "overlay2"
	mu.Unlock()
	refreshDriver(ctx, cli, &ds)
	if ds.Driver != "overlay2" {
		t.Fatalf("switch not seen: %q", ds.Driver)
	}
	srv.Close()
	closed = true
	refreshDriver(ctx, cli, &ds)
	if ds.Driver != "overlay2" {
		t.Fatalf("unreadable /info blanked the driver: %q", ds.Driver)
	}
}
