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
	"bufio"
	"bytes"
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
	// One byte over the cap tells a body that was cut off from one that
	// fits: a truncated list would otherwise fail as malformed JSON.
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return err
	}
	if len(body) > maxBody {
		return fmt.Errorf("docker: response from %s is larger than %d MiB", path, maxBody>>20)
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
	if cs, ok := out.(*[]Container); ok {
		*cs, err = decodeContainers(body, MaxContainers)
		return err
	}
	return json.Unmarshal(body, out)
}

// maxBody bounds any one response; a few hundred containers is well under
// 1 MiB. A larger response is an error, never silently truncated.
const maxBody = 16 << 20

// MaxContainers bounds the container list the dashboard will decode; the
// stream stops there before materialising more.
const MaxContainers = 2000

// decodeContainers reads the /containers/json array element by element,
// stopping with an error once it exceeds maxEntries.
func decodeContainers(body []byte, maxEntries int) ([]Container, error) {
	dec := json.NewDecoder(bytes.NewReader(body))
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	if tok == nil {
		return nil, nil
	}
	if tok != json.Delim('[') {
		return nil, fmt.Errorf("docker: container list is not an array")
	}
	var out []Container
	for dec.More() {
		if len(out) >= maxEntries {
			return nil, fmt.Errorf("docker: more than %d containers; not supported", maxEntries)
		}
		var c Container
		if err := dec.Decode(&c); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
		return nil, fmt.Errorf("docker: malformed container list")
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("docker: trailing data after the container list")
	}
	return out, nil
}

// Version is the daemon version block.
type Version struct {
	Version string `json:"Version"`
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
	ID     string   `json:"Id"`
	Names  []string `json:"Names"`
	Image  string   `json:"Image"`
	State  string   `json:"State"`  // running, exited, created, paused, restarting, dead
	Status string   `json:"Status"` // "Up 3 days", "Exited (1) 2 hours ago"
	Ports  []Port   `json:"Ports"`
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
	return out, c.do(ctx, http.MethodGet, containersPath+"json?all=1", &out)
}

// Stats is a reduced one-shot resource sample for a container.
type Stats struct {
	CPUPct   float64
	MemUsage int64
}

const containersPath = "/containers/"

// rawCPUUsage is the Engine API's cpu_usage block.
type rawCPUUsage struct {
	TotalUsage int64 `json:"total_usage"`
}

// rawCPUStats is one cpu_stats / precpu_stats sample.
type rawCPUStats struct {
	CPUUsage    rawCPUUsage `json:"cpu_usage"`
	SystemUsage int64       `json:"system_cpu_usage"`
	OnlineCPUs  int         `json:"online_cpus"`
}

// rawMemoryStats is the memory_stats block.
type rawMemoryStats struct {
	Usage int64            `json:"usage"`
	Stats map[string]int64 `json:"stats"`
}

type rawStats struct {
	CPUStats    rawCPUStats    `json:"cpu_stats"`
	PreCPUStats rawCPUStats    `json:"precpu_stats"`
	MemoryStats rawMemoryStats `json:"memory_stats"`
}

// Stats takes one two-sample stats reading (about one second).
func (c *Client) Stats(ctx context.Context, id string) (*Stats, error) {
	var r rawStats
	if err := c.do(ctx, http.MethodGet, containersPath+id+"/stats?stream=false", &r); err != nil {
		return nil, err
	}
	s := &Stats{MemUsage: r.MemoryStats.Usage}
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

// maxLogBytes is how much of a container's output Logs keeps: the newest
// bytes, since the call is a tail.
const maxLogBytes = 1 << 20

// Logs returns the last tail lines of a container's stdout and stderr,
// interleaved in the order the daemon kept them, the newest maxLogBytes
// of them.
func (c *Client) Logs(ctx context.Context, id string, tail int) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, fmt.Sprintf("http://docker%s%s/logs?stdout=1&stderr=1&tail=%d", containersPath, id, tail), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		var e struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Message == "" {
			e.Message = strings.TrimSpace(string(body))
		}
		return nil, fmt.Errorf("docker: logs %s: http %d %s", id, resp.StatusCode, e.Message)
	}
	return demuxTail(resp.Body, maxLogBytes)
}

// tailBuf keeps the last keep bytes written to it.
type tailBuf struct {
	keep int
	b    []byte
}

func (t *tailBuf) Write(p []byte) (int, error) {
	t.b = append(t.b, p...)
	if len(t.b) > 2*t.keep { // trim rarely, not on every write
		t.b = append(t.b[:0:0], t.b[len(t.b)-t.keep:]...)
	}
	return len(p), nil
}

func (t *tailBuf) bytes() []byte {
	if len(t.b) > t.keep {
		return t.b[len(t.b)-t.keep:]
	}
	return t.b
}

// demuxTail streams a container's log output into a buffer that retains
// the newest keep bytes, stripping the stream framing the daemon uses for
// containers without a TTY (an 8-byte header per frame: stream type, three
// zero bytes, big-endian payload length) as it goes, so a cut never falls
// inside a header. A TTY container's output has no framing and is copied
// as is; so is a stream whose first header does not parse. A frame cut
// short by the daemon keeps the bytes that arrived.
func demuxTail(r io.Reader, keep int) ([]byte, error) {
	br := bufio.NewReader(r)
	out := &tailBuf{keep: keep}
	head, err := br.Peek(8)
	if err != nil || !looksFramed(head) {
		if _, cerr := io.Copy(out, br); cerr != nil {
			return out.bytes(), cerr
		}
		return out.bytes(), nil
	}
	var hdr [8]byte
	for {
		if _, err := io.ReadFull(br, hdr[:]); err != nil {
			if err == io.EOF || err == io.ErrUnexpectedEOF {
				return out.bytes(), nil // the end, or a partial trailing header: nothing more to show
			}
			return out.bytes(), err // a cut connection: the caller must not present a silent truncation
		}
		n := int64(hdr[4])<<24 | int64(hdr[5])<<16 | int64(hdr[6])<<8 | int64(hdr[7])
		if _, err := io.CopyN(out, br, n); err != nil {
			if err == io.EOF {
				return out.bytes(), nil // frame cut short by the daemon: keep what came
			}
			return out.bytes(), err
		}
	}
}

// looksFramed checks the first header: stream type 0..2 and three zero
// bytes, which printable log text never starts with.
func looksFramed(b []byte) bool {
	return len(b) >= 8 && b[0] <= 2 && b[1] == 0 && b[2] == 0 && b[3] == 0
}

// Start starts a container.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, containersPath+id+"/start", nil)
}

// Stop stops a container with a 10 second grace period.
func (c *Client) Stop(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, containersPath+id+"/stop?t=10", nil)
}

// Restart restarts a container.
func (c *Client) Restart(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, containersPath+id+"/restart?t=10", nil)
}
