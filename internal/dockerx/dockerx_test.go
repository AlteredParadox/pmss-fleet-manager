package dockerx

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
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
	srv := httptest.NewServer(mux)
	defer srv.Close()
	addr := srv.Listener.Addr().String()
	c := New(func(ctx context.Context) (net.Conn, error) { return net.Dial("tcp", addr) })
	ctx := context.Background()
	cs, err := c.Containers(ctx)
	if err != nil || len(cs) != 1 || cs[0].Name() != "jellyfin" || cs[0].Ports[0].PublicPort != 8096 {
		t.Fatalf("%+v %v", cs, err)
	}
	st := c.StatsAll(ctx, []string{cs[0].ID}, 2)
	s := st[cs[0].ID]
	if s == nil || s.MemUsage != 800 || s.NetTx != 7 || s.CPUPct < 39.9 || s.CPUPct > 40.1 {
		t.Fatalf("%+v", s)
	}
	if err := c.Start(ctx, cs[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := c.Start(ctx, "nope"); err == nil {
		t.Fatal("want error")
	}
}
