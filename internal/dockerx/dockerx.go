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

// Package dockerx is a minimal Docker Engine API client over any dialer, used
// to reach a tenant's rootless dockerd socket through SSH.
package dockerx

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Dialer opens a connection to the docker socket.
type Dialer func(ctx context.Context) (net.Conn, error)

// Client talks to one daemon.
type Client struct{ http *http.Client }

// New builds a client over dial.
func New(dial Dialer) *Client {
	tr := &http.Transport{
		DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		MaxIdleConns:    4,
		IdleConnTimeout: 60 * time.Second,
	}
	return &Client{http: &http.Client{Transport: tr, Timeout: 60 * time.Second}}
}

// Close releases idle connections.
func (c *Client) Close() {
	if tr, ok := c.http.Transport.(*http.Transport); ok {
		tr.CloseIdleConnections()
	}
}

func (c *Client) do(ctx context.Context, method, path string, out any) error {
	req, err := http.NewRequestWithContext(ctx, method, "http://docker"+path, nil)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(body))
		}
		if resp.StatusCode == http.StatusNotModified {
			return nil
		}
		return fmt.Errorf("docker: %s %s: http %d %s", method, path, resp.StatusCode, e.Message)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(body, out)
}

// Version is the daemon version block.
type Version struct {
	Version    string `json:"Version"`
	APIVersion string `json:"ApiVersion"`
	Os         string `json:"Os"`
	Arch       string `json:"Arch"`
}

// Version pings the daemon.
func (c *Client) Version(ctx context.Context) (*Version, error) {
	var v Version
	return &v, c.do(ctx, http.MethodGet, "/version", &v)
}

// Port is one published port mapping.
type Port struct {
	IP          string `json:"IP"`
	PrivatePort int    `json:"PrivatePort"`
	PublicPort  int    `json:"PublicPort"`
	Type        string `json:"Type"`
}

// Container is one row from /containers/json.
type Container struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`  // running, exited, created, paused, restarting, dead
	Status  string            `json:"Status"` // "Up 3 days", "Exited (1) 2 hours ago"
	Created int64             `json:"Created"`
	Ports   []Port            `json:"Ports"`
	Labels  map[string]string `json:"Labels"`
}

// Name returns the primary container name without the leading slash.
func (c Container) Name() string {
	if len(c.Names) == 0 {
		return c.ID[:min(12, len(c.ID))]
	}
	return strings.TrimPrefix(c.Names[0], "/")
}

// Containers lists all containers, including stopped ones.
func (c *Client) Containers(ctx context.Context) ([]Container, error) {
	var out []Container
	return out, c.do(ctx, http.MethodGet, "/containers/json?all=1", &out)
}

// Stats is a reduced one-shot resource sample for a container.
type Stats struct {
	CPUPct   float64 `json:"cpu_pct"`
	MemUsage int64   `json:"mem_usage"`
	MemLimit int64   `json:"mem_limit"`
	NetRx    int64   `json:"net_rx"`
	NetTx    int64   `json:"net_tx"`
}

type rawStats struct {
	CPUStats struct {
		CPUUsage struct {
			TotalUsage int64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemUsage int64 `json:"system_cpu_usage"`
		OnlineCPUs  int   `json:"online_cpus"`
	} `json:"cpu_stats"`
	PreCPUStats struct {
		CPUUsage struct {
			TotalUsage int64 `json:"total_usage"`
		} `json:"cpu_usage"`
		SystemUsage int64 `json:"system_cpu_usage"`
	} `json:"precpu_stats"`
	MemoryStats struct {
		Usage int64            `json:"usage"`
		Limit int64            `json:"limit"`
		Stats map[string]int64 `json:"stats"`
	} `json:"memory_stats"`
	Networks map[string]struct {
		RxBytes int64 `json:"rx_bytes"`
		TxBytes int64 `json:"tx_bytes"`
	} `json:"networks"`
}

// Stats takes one two-sample stats reading (about one second).
func (c *Client) Stats(ctx context.Context, id string) (*Stats, error) {
	var r rawStats
	if err := c.do(ctx, http.MethodGet, "/containers/"+id+"/stats?stream=false", &r); err != nil {
		return nil, err
	}
	s := &Stats{MemUsage: r.MemoryStats.Usage, MemLimit: r.MemoryStats.Limit}
	// Subtract page cache like `docker stats` does.
	if cache, ok := r.MemoryStats.Stats["inactive_file"]; ok && cache < s.MemUsage {
		s.MemUsage -= cache
	} else if cache, ok := r.MemoryStats.Stats["cache"]; ok && cache < s.MemUsage {
		s.MemUsage -= cache
	}
	cpuDelta := float64(r.CPUStats.CPUUsage.TotalUsage - r.PreCPUStats.CPUUsage.TotalUsage)
	sysDelta := float64(r.CPUStats.SystemUsage - r.PreCPUStats.SystemUsage)
	if cpuDelta > 0 && sysDelta > 0 {
		n := r.CPUStats.OnlineCPUs
		if n == 0 {
			n = 1
		}
		s.CPUPct = cpuDelta / sysDelta * float64(n) * 100
	}
	for _, nw := range r.Networks {
		s.NetRx += nw.RxBytes
		s.NetTx += nw.TxBytes
	}
	return s, nil
}

// StatsAll collects stats for the given running containers with bounded concurrency.
func (c *Client) StatsAll(ctx context.Context, ids []string, concurrency int) map[string]*Stats {
	out := make(map[string]*Stats, len(ids))
	var mu sync.Mutex
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, concurrency))
	for _, id := range ids {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if s, err := c.Stats(ctx, id); err == nil {
				mu.Lock()
				out[id] = s
				mu.Unlock()
			}
		}(id)
	}
	wg.Wait()
	return out
}

// Start starts a container.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+id+"/start", nil)
}

// Stop stops a container with a 10 second grace period.
func (c *Client) Stop(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+id+"/stop?t=10", nil)
}

// Restart restarts a container.
func (c *Client) Restart(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+id+"/restart?t=10", nil)
}
