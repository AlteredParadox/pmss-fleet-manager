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

package dockerx

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestContainersAndStats(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/containers/json", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`[{"Id":"abcdef123456789","Names":["/jellyfin"],"Image":"lscr.io/linuxserver/jellyfin:latest","State":"running","Status":"Up 3 days","Created":1700000000,"Ports":[{"IP":"127.0.0.1","PrivatePort":8096,"PublicPort":8096,"Type":"tcp"}]}]`))
	})
	mux.HandleFunc("/containers/abcdef123456789/stats", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"cpu_stats":{"cpu_usage":{"total_usage":2000},"system_cpu_usage":100000,"online_cpus":4},"precpu_stats":{"cpu_usage":{"total_usage":1000},"system_cpu_usage":90000},"memory_stats":{"usage":1000,"limit":8000,"stats":{"inactive_file":200}},"networks":{"eth0":{"rx_bytes":5,"tx_bytes":7}}}`))
	})
	mux.HandleFunc("/containers/abcdef123456789/start", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(304) })
	mux.HandleFunc("/containers/nope/start", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"No such container: nope"}`))
	})
	mux.HandleFunc("/containers/abcdef123456789/logs", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tail") != "50" {
			w.WriteHeader(400)
			return
		}
		// Two multiplexed frames (stdout, stderr) and a truncated third.
		w.Write([]byte("\x01\x00\x00\x00\x00\x00\x00\x06hello\n\x02\x00\x00\x00\x00\x00\x00\x04oops\x01\x00\x00\x00\x00\x00\x00\x10cut"))
	})
	mux.HandleFunc("/containers/tty/logs", func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("raw tty output\n")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) })
	ctx := context.Background()
	if out, err := c.Logs(ctx, "abcdef123456789", 50); err != nil || string(out) != "hello\noopscut" {
		t.Fatalf("logs: %q %v", out, err)
	}
	if out, err := c.Logs(ctx, "tty", 50); err != nil || string(out) != "raw tty output\n" {
		t.Fatalf("tty logs: %q %v", out, err)
	}
	if _, err := c.Logs(ctx, "nope", 50); err == nil {
		t.Fatal("missing container: want error")
	}
	// The byte cap keeps the newest output, not the oldest, and never cuts
	// inside a frame header.
	var framed []byte
	for i := 0; i < 100; i++ {
		line := []byte(fmt.Sprintf("line %03d of a long container log\n", i))
		framed = append(framed, 1, 0, 0, 0, 0, 0, byte(len(line)>>8), byte(len(line)))
		framed = append(framed, line...)
	}
	got, err := demuxTail(bytes.NewReader(framed), 200)
	if err != nil || len(got) != 200 || !bytes.HasSuffix(got, []byte("line 099 of a long container log\n")) || bytes.Contains(got, []byte{1, 0, 0, 0}) {
		t.Fatalf("tail cap: %v %q", err, got)
	}
	if got, _ := demuxTail(strings.NewReader("raw tty output that is longer than the cap\n"), 10); string(got) != "n the cap\n" {
		t.Fatalf("raw tail cap: %q", got)
	}
	cs, err := c.Containers(ctx)
	if err != nil || len(cs) != 1 || cs[0].Name() != "jellyfin" || cs[0].Ports[0].PublicPort != 8096 {
		t.Fatalf("%+v %v", cs, err)
	}
	st := c.StatsAll(ctx, []string{cs[0].ID}, 2)
	s := st[cs[0].ID]
	if s == nil || s.MemUsage != 800 || s.CPUPct < 39.9 || s.CPUPct > 40.1 {
		t.Fatalf("%+v", s)
	}
	if err := c.Start(ctx, cs[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx, "nope"); err == nil {
		t.Fatal("want error")
	}
}
