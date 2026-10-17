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

// Command pmss-fleet-manager serves the fleet dashboard.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	legal "pmss-fleet-manager"
	"strings"
	"syscall"
	"time"

	"pmss-fleet-manager/internal/alerts"
	"pmss-fleet-manager/internal/api"
	"pmss-fleet-manager/internal/collect"
	"pmss-fleet-manager/internal/store"
)

// version is stamped at build time by `make build` and the release workflow
// via -ldflags "-X main.version=vX.Y.Z". Plain `go build` reports "dev".
var version = "dev"

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "version" || os.Args[1] == "--version") {
		fmt.Println("pmss-fleet-manager", version)
		return
	}
	listen := flag.String("listen", envOr("PMSS_LISTEN", "127.0.0.1:8080"), "address(es) to listen on, comma-separated")
	allowedHosts := flag.String("allowed-hosts", os.Getenv("PMSS_ALLOWED_HOSTS"), "extra Host header names to serve (e.g. a reverse-proxy name), comma-separated; the listen addresses and localhost are always allowed")
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
	handler.SetLegal(legal.License, legal.ThirdPartyLicenses)
	handler.SetVersion(version)
	hosts, wildcard := allowedHostList(addrs, *allowedHosts)
	if wildcard && *allowedHosts == "" {
		log.Warn("listening on a wildcard address without -allowed-hosts: Host header validation is off, so a DNS-rebinding page could reach the API; set -allowed-hosts")
	} else {
		handler.SetAllowedHosts(hosts)
	}
	var servers []*http.Server
	for _, a := range addrs {
		// WriteTimeout stays 0: /api/events is a long-lived SSE stream.
		srv := &http.Server{Addr: a, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
		servers = append(servers, srv)
		go func(srv *http.Server) {
			log.Info("pmss-fleet-manager listening", "addr", "http://"+srv.Addr, "version", version)
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

// allowedHostList derives the Host header allow-list: every listen address's
// host part plus loopback names, plus the -allowed-hosts extras. wildcard
// reports a listen address with no specific host (0.0.0.0, ::, empty), which
// cannot be enumerated.
func allowedHostList(addrs []string, extra string) (hosts []string, wildcard bool) {
	hosts = []string{"localhost", "127.0.0.1", "::1"}
	for _, a := range addrs {
		h, _, err := net.SplitHostPort(a)
		if err != nil {
			h = a
		}
		h = strings.Trim(h, "[]")
		if h == "" || h == "0.0.0.0" || h == "::" {
			wildcard = true
			continue
		}
		hosts = append(hosts, h)
	}
	for _, h := range strings.Split(extra, ",") {
		if h = strings.TrimSpace(h); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts, wildcard
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
