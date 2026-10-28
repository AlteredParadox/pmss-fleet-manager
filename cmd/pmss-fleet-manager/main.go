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
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	legal "pmss-fleet-manager"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"pmss-fleet-manager/internal/alerts"
	"pmss-fleet-manager/internal/api"
	"pmss-fleet-manager/internal/collect"
	"pmss-fleet-manager/internal/secrets"
	"pmss-fleet-manager/internal/sshx"
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
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck(os.Args[2:]))
	}
	listen := flag.String("listen", envOr("PMSS_LISTEN", "127.0.0.1:8080"), "address(es) to listen on, comma-separated")
	allowedHosts := flag.String("allowed-hosts", os.Getenv("PMSS_ALLOWED_HOSTS"), "extra Host header names to serve (e.g. a reverse-proxy name), comma-separated; the listen addresses and localhost are always allowed")
	dataDir := flag.String("data", envOr("PMSS_DATA", "./data"), "directory for the SQLite database")
	auth := flag.String("auth", "", "optional HTTP basic auth as user:password (or set PMSS_AUTH, which -h never prints)")
	authFile := flag.String("auth-file", os.Getenv("PMSS_AUTH_FILE"), "file holding the HTTP basic auth login as user:password, e.g. a container secret (or set PMSS_AUTH_FILE); one trailing line ending is ignored")
	basePath := flag.String("base-path", os.Getenv("PMSS_BASE_PATH"), "URL path prefix when a reverse proxy serves the dashboard under a sub-path and forwards it unchanged (e.g. /fleet)")
	sshKey := flag.String("ssh-key", os.Getenv("PMSS_SSH_KEY"), "private key file tried for every box without a key path of its own, before ~/.ssh/id_* (or set PMSS_SSH_KEY), e.g. a container secret")
	logLevel := flag.String("log", envOr("PMSS_LOG", "info"), "log level: debug, info, warn, error")
	secretFile := flag.String("secret-file", os.Getenv("PMSS_SECRET_FILE"), "file holding the secret that encrypts stored credentials (or set PMSS_SECRET to the secret itself, which -h never prints); unset keeps them in plain text")
	previousSecretFile := flag.String("previous-secret-file", os.Getenv("PMSS_SECRET_PREVIOUS_FILE"), "when changing or removing the secret: file holding the one the credentials are encrypted with now (or set PMSS_SECRET_PREVIOUS); remove it after one successful start")
	monitorOnly := flag.Bool("monitor-only", os.Getenv("PMSS_MONITOR_ONLY") == "1", "refuse the web UI's actions on the boxes: torrent, client, discovery, container and transfer actions answer 403 and their controls are hidden; monitoring runs as usual (or set PMSS_MONITOR_ONLY=1); limits this interface, not the SSH accounts")
	forgetCreds := flag.Bool("forget-encrypted-credentials", os.Getenv("PMSS_FORGET_ENCRYPTED_CREDENTIALS") == "1", "after losing the secret: remove every stored credential that does not decrypt with the configured one, then start; the edit forms ask for them again")
	flag.Parse()
	if !flagGiven("auth") {
		// Read after parsing, not as the flag's default: -h and a flag
		// typo print defaults, and a credential must not land in a log.
		// An explicit -auth= still wins over the environment.
		*auth = os.Getenv("PMSS_AUTH")
	}
	login, authWarn, err := loginFrom(*auth, *authFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	*auth = login

	var lvl slog.Level
	if err := lvl.UnmarshalText([]byte(*logLevel)); err != nil {
		lvl = slog.LevelInfo
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
	if authWarn != "" {
		log.Warn(authWarn)
	}
	sshx.DefaultKey = *sshKey
	if w := sshKeyWarning(*sshKey); w != "" {
		log.Warn(w) // said once at startup; a box that needs the key names it again in its connection error
	}

	if *auth != "" {
		// Both halves are required: "user:" or ":" would pass a colon check
		// and silence the no-auth warning while guarding with an empty secret.
		if u, p, ok := strings.Cut(*auth, ":"); !ok || u == "" || p == "" {
			fmt.Fprintln(os.Stderr, "-auth must be user:password, with a non-empty user and password")
			os.Exit(2)
		}
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

	opts, encrypted, err := credentialOptions(*secretFile, *previousSecretFile, *forgetCreds, log)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	st, err := store.Open(filepath.Join(*dataDir, "pmss-fleet-manager.db"), opts...)
	if err != nil {
		if errors.Is(err, fs.ErrPermission) {
			// The usual cause is a bind-mounted data directory owned by
			// another user: the container runs as uid 65532 (nonroot) and
			// Unraid's appdata, for one, belongs to nobody:users (99:100).
			log.Error("data directory is not writable by this process",
				"dir", *dataDir, "uid", os.Getuid(), "gid", os.Getgid(), "err", err,
				"fix", fmt.Sprintf("make the directory writable for uid %d (chown %d:%d %s), or run the container with --user set to the directory's owner (Unraid: --user 99:100)", os.Getuid(), os.Getuid(), os.Getgid(), *dataDir))
			os.Exit(1)
		}
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()
	if n := st.ResealedOnOpen(); n > 0 {
		log.Info("rewrote stored credentials", "count", n, "encrypted", encrypted)
	}
	if n := st.ForgottenOnOpen(); n > 0 {
		log.Warn("removed stored credentials that did not decrypt; re-enter them in the edit forms", "count", n)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	mgr := collect.New(st, log)
	ev := alerts.New(st, mgr.Settings, log, func() { mgr.Publish(collect.Event{Type: "alerts"}) })
	mgr.OnPoll(ev.Evaluate)
	if err := mgr.Start(ctx); err != nil {
		log.Error("start collector", "err", err)
		os.Exit(1)
	}

	bp, err := api.NormalizeBasePath(*basePath)
	if err != nil {
		log.Error("invalid -base-path", "err", err)
		os.Exit(1)
	}
	if bp != "" {
		log.Info("serving under base path", "path", bp)
	}
	handler := api.New(st, mgr, *auth, bp)
	handler.SetLegal(legal.License, legal.ThirdPartyLicenses)
	handler.SetVersion(version)
	hosts, wildcard := allowedHostList(addrs, *allowedHosts)
	handler.SetAllowedHosts(hosts, wildcard)
	handler.SetMonitorOnly(*monitorOnly)
	if *monitorOnly {
		log.Info("monitor-only mode: requests that would change a box are refused")
	}
	if wildcard && *allowedHosts == "" {
		log.Warn("listening on a wildcard address without -allowed-hosts: requests by IP address or localhost are served, any other hostname is refused until it is listed in -allowed-hosts")
	}
	var servers []*http.Server
	var listenFailed atomic.Bool
	for _, a := range addrs {
		// WriteTimeout stays 0: /api/events is a long-lived SSE stream.
		srv := &http.Server{Addr: a, Handler: handler, ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute}
		// Shutdown waits for handlers, and the SSE stream only ends with its
		// request context: cancel that on shutdown or every stop takes the
		// full grace period while a tab is open.
		base, cancelBase := context.WithCancel(context.Background())
		srv.BaseContext = func(net.Listener) context.Context { return base }
		srv.RegisterOnShutdown(cancelBase)
		servers = append(servers, srv)
		go func(srv *http.Server) {
			log.Info("pmss-fleet-manager listening", "addr", "http://"+srv.Addr, "version", version)
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("http", "addr", srv.Addr, "err", err)
				listenFailed.Store(true)
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
	if listenFailed.Load() {
		// A bind failure is a failed start: close the store ourselves (a
		// deferred close would not run past os.Exit) and tell the supervisor.
		st.Close()
		os.Exit(1)
	}
}

// allowedHostList derives the Host header allow-list: every listen address's
// host part plus loopback names, plus the -allowed-hosts extras. wildcard
// reports a listen address with no specific host (0.0.0.0, ::, empty), which
// cannot be enumerated; the handler then also serves IP-literal Host headers.
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
		h = strings.TrimSpace(h)
		if hp, _, err := net.SplitHostPort(h); err == nil {
			h = hp // "dash.local:8443" means the name dash.local
		}
		if h = strings.Trim(h, "[]"); h != "" {
			hosts = append(hosts, h)
		}
	}
	return hosts, wildcard
}

// credentialOptions turns the credential-encryption configuration into
// store options and logs what it means for this start. Like PMSS_AUTH, the
// secret itself is only ever read from the environment, never a flag: a
// flag would be in `ps` and in -h output. encrypted reports whether the
// stored credentials are ciphertext from here on.
func credentialOptions(secretFile, previousFile string, forget bool, log *slog.Logger) (opts []store.Option, encrypted bool, err error) {
	creds, err := secrets.Resolve(os.Getenv("PMSS_SECRET"), secretFile, os.Getenv("PMSS_SECRET_PREVIOUS"), previousFile)
	if err != nil {
		return nil, false, err
	}
	for _, w := range creds.Warnings {
		log.Warn(w)
	}
	if creds.Current != nil {
		log.Info("stored credentials are encrypted", "secret", creds.Source)
	}
	if creds.Previous != nil {
		// Changing or removing the secret: credentials sealed under the
		// old one are rewritten under the new one (or in plain text) at
		// this start. The variable is meant to be removed afterwards.
		log.Warn("a previous secret is configured: credentials encrypted under it are being rewritten; remove it once this start succeeds")
	}
	opts = []store.Option{store.WithSealer(creds.Current), store.WithPreviousSealer(creds.Previous)}
	if forget {
		log.Warn("-forget-encrypted-credentials is set: stored credentials that do not decrypt are being removed; unset it once this start succeeds")
		opts = append(opts, store.WithForgetUnreadable())
	}
	return opts, creds.Current != nil, nil
}

// maxLoginFile bounds what loginFrom reads: a login is one short line.
const maxLoginFile = 4096

// loginFrom resolves the dashboard login from the -auth value (the flag, or
// PMSS_AUTH) or the file -auth-file / PMSS_AUTH_FILE names, for container
// secrets that should not sit in the environment. A file that is
// configured must yield a usable login: missing, unreadable, empty or not
// user:password stops the start rather than silently serving without
// authentication. Only one trailing line ending is removed, so a password
// that ends in a space keeps it. Errors name the path and the uid, never
// what the file holds. The warning, if any, is about the file's mode.
func loginFrom(value, file string) (login, warning string, err error) {
	if file == "" {
		return value, "", nil
	}
	if value != "" {
		return "", "", errors.New("set the login with -auth / PMSS_AUTH or with -auth-file / PMSS_AUTH_FILE, not both")
	}
	f, err := os.Open(file)
	if err != nil {
		return "", "", fmt.Errorf("login file %s cannot be read by uid %d: %w", file, os.Getuid(), errors.Unwrap(err))
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, maxLoginFile+1))
	if err != nil {
		return "", "", fmt.Errorf("login file %s cannot be read by uid %d: %w", file, os.Getuid(), errors.Unwrap(err))
	}
	if len(b) > maxLoginFile {
		return "", "", fmt.Errorf("login file %s is larger than %d bytes; it must hold one line, user:password", file, maxLoginFile)
	}
	s := string(b)
	if strings.HasSuffix(s, "\r\n") {
		s = s[:len(s)-2]
	} else if strings.HasSuffix(s, "\n") {
		s = s[:len(s)-1]
	}
	switch u, p, ok := strings.Cut(s, ":"); {
	case s == "":
		return "", "", fmt.Errorf("login file %s is empty; refusing to start without the login it should hold", file)
	case strings.ContainsAny(s, "\r\n\x00"):
		return "", "", fmt.Errorf("login file %s must hold one line, user:password", file)
	case !ok || u == "" || p == "":
		return "", "", fmt.Errorf("login file %s must hold user:password, with a non-empty user and password", file)
	}
	if fi, err := f.Stat(); err == nil && fi.Mode().Perm()&0o077 != 0 {
		warning = fmt.Sprintf("the login file %s is readable by other users (mode %04o); chmod 600 it", file, fi.Mode().Perm())
	}
	return s, warning, nil
}

// sshKeyWarning says, for the startup log, why the -ssh-key default cannot
// be read, or that other users can read it ("" when it is unset and
// private). Not fatal: boxes without a key path of their own fall back to
// ~/.ssh/id_* and the agent, and a mounted secret's mode may not be the
// operator's to set (as for the secret file).
func sshKeyWarning(path string) string {
	if path == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return fmt.Sprintf("the -ssh-key default %s cannot be read by uid %d (%v); boxes without a key path fall back to ~/.ssh/id_*", path, os.Getuid(), errors.Unwrap(err))
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Mode().Perm()&0o077 != 0 {
		return fmt.Sprintf("the -ssh-key default %s is readable by other users (mode %04o); chmod 600 it", path, fi.Mode().Perm())
	}
	return ""
}

// flagGiven reports whether name was passed on the command line.
func flagGiven(name string) bool {
	given := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			given = true
		}
	})
	return given
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
