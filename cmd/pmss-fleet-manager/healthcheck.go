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
	"net"
	"net/http"
	"os"
	"strings"
	"time"
)

// healthcheck is `pmss-fleet-manager healthcheck`: one GET of /healthz on
// the local listener, exit 0 when it answers 200, 1 otherwise. It is the
// container healthcheck for the distroless image, which has no shell or
// curl. It runs as its own process and so cannot see the flags the server
// was started with: the address comes from PMSS_LISTEN (which the image
// sets) or its own -listen, and the server answers /healthz at the root
// whatever its base path, so a -base-path flag needs nothing here. A
// server given a different -listen flag needs PMSS_LISTEN set the same, or
// -healthcheck-url / PMSS_HEALTHCHECK_URL.
// It opens no database and needs neither the login nor the credential
// secret.
func healthcheck(args []string) int {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	listen := fs.String("listen", envOr("PMSS_LISTEN", "127.0.0.1:8080"), "the server's -listen")
	target := fs.String("healthcheck-url", os.Getenv("PMSS_HEALTHCHECK_URL"), "URL to check instead of one derived from -listen")
	timeout := fs.Duration("timeout", 4*time.Second, "how long to wait for the answer")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	u := *target
	if u == "" {
		var err error
		if u, err = healthURL(*listen); err != nil {
			fmt.Fprintln(os.Stderr, "healthcheck:", err)
			return 1
		}
	}
	if err := probeHealth(u, *timeout); err != nil {
		fmt.Fprintln(os.Stderr, "healthcheck:", err)
		return 1
	}
	return 0
}

// healthURL is the /healthz URL for the server's listen setting: the first
// loopback or wildcard address of the list (a wildcard is reached on
// loopback: 0.0.0.0 as 127.0.0.1, :: as ::1), else the first address.
func healthURL(listen string) (string, error) {
	var first, local string
	for _, a := range strings.Split(listen, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		host, port, err := net.SplitHostPort(a)
		if err != nil {
			return "", fmt.Errorf("cannot read the listen address %q: %v", a, err)
		}
		switch ip := net.ParseIP(host); {
		case host == "" || host == "0.0.0.0":
			host = "127.0.0.1"
		case host == "::":
			host = "::1"
		case strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback()):
		default:
			if first == "" {
				first = net.JoinHostPort(host, port)
			}
			continue
		}
		local = net.JoinHostPort(host, port)
		break
	}
	hp := local
	if hp == "" {
		hp = first
	}
	if hp == "" {
		return "", errors.New("no listen address to check")
	}
	return "http://" + hp + "/healthz", nil
}

// probeHealth GETs u and wants a 200 within timeout.
func probeHealth(u string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	cli := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := cli.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s answered %s", u, resp.Status)
	}
	return nil
}
