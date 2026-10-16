// Command pmss-fleet-manager serves the fleet dashboard.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"pmss-fleet-manager/internal/alerts"
	"pmss-fleet-manager/internal/api"
	"pmss-fleet-manager/internal/collect"
	"pmss-fleet-manager/internal/store"
)

func main() {
	listen := flag.String("listen", envOr("PMSS_LISTEN", "127.0.0.1:8080"), "address(es) to listen on, comma-separated")
	dataDir := flag.String("data", envOr("PMSS_DATA", "./data"), "directory for the SQLite database")
	auth := flag.String("auth", os.Getenv("PMSS_AUTH"), "optional HTTP basic auth as user:password")
	logLevel := flag.String("log", envOr("PMSS_LOG", "info"), "log level: debug, info, warn, error")
	flag.Parse()

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))

	if *auth != "" && !strings.Contains(*auth, ":") {
		fmt.Fprintln(os.Stderr, "-auth must be user:password")
		os.Exit(2)
	}
	var addrs []string
	for _, a := range strings.Split(*listen, ",") {
		if a = strings.TrimSpace(a); a != "" {
			addrs = append(addrs, a)
		}
	}
	if len(addrs) == 0 {
		fmt.Fprintln(os.Stderr, "-listen needs at least one address")
		os.Exit(2)
	}
	for _, a := range addrs {
		if *auth == "" && !isLoopback(a) {
			log.Warn("listening on a non-loopback address without -auth; anyone who can reach this port can read box credentials and control your boxes", "addr", a)
		}
	}

	st, err := store.Open(filepath.Join(*dataDir, "pmss-fleet-manager.db"))
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr := collect.New(st, log)
	ev := alerts.New(st, mgr.Settings, log, func() { mgr.Publish(collect.Event{Type: "alerts"}) })
	mgr.OnPoll(ev.Evaluate)
	if err := mgr.Start(ctx); err != nil {
		log.Error("start collector", "err", err)
		os.Exit(1)
	}

	handler := api.New(st, mgr, log, *auth)
	var servers []*http.Server
	for _, a := range addrs {
		srv := &http.Server{Addr: a, Handler: handler, ReadHeaderTimeout: 10 * time.Second}
		servers = append(servers, srv)
		go func(srv *http.Server) {
			log.Info("pmss-fleet-manager listening", "addr", "http://"+srv.Addr)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("http", "addr", srv.Addr, "err", err)
				stop()
			}
		}(srv)
	}

	<-ctx.Done()
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Shutdown(sctx)
	}
	mgr.Stop()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func isLoopback(addr string) bool {
	host := addr
	if i := strings.LastIndex(addr, ":"); i >= 0 {
		host = addr[:i]
	}
	host = strings.Trim(host, "[]")
	return host == "127.0.0.1" || host == "localhost" || host == "::1"
}
