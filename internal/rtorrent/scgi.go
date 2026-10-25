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

package rtorrent

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

// Dialer opens a connection to the SCGI endpoint: the unix socket named in
// ~/.rtorrent.rc through the SSH channel on a PMSS box.
type Dialer func(ctx context.Context) (net.Conn, error)

// callTimeout bounds one request/response round trip. rTorrent serves RPC
// on its single main loop and PMSS's watchdog restarts an instance whose
// socket does not answer within 120 s, so a slow answer is a sick client,
// not one to wait for.
const callTimeout = 20 * time.Second

// ErrSCGI is a response that is not an SCGI/CGI answer at all.
var ErrSCGI = errors.New("rtorrent: not an SCGI response")

// maxHeaderLine bounds one response header line; rTorrent's are tens of
// bytes.
const maxHeaderLine = 8192

// scgiRequest frames body as one SCGI request: a netstring of NUL-separated
// headers (CONTENT_LENGTH first, as the protocol requires) then the body.
func scgiRequest(body []byte) []byte {
	hdr := fmt.Sprintf("CONTENT_LENGTH\x00%d\x00SCGI\x001\x00", len(body))
	var b bytes.Buffer
	b.Grow(len(hdr) + len(body) + 16)
	b.WriteString(strconv.Itoa(len(hdr)))
	b.WriteByte(':')
	b.WriteString(hdr)
	b.WriteByte(',')
	b.Write(body)
	return b.Bytes()
}

// roundTrip sends one framed request over a fresh connection (rTorrent
// answers exactly one request per connection and closes) and returns a
// reader positioned at the response body, capped at maxBytes, with the
// context the round trip runs under (the caller's, bounded by timeout) so
// a decode error can be attributed to that deadline. The connection is
// closed when the context ends or when the caller calls the returned
// closer.
func roundTrip(ctx context.Context, dial Dialer, timeout time.Duration, maxBytes int64, body []byte) (*responseBody, context.Context, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	conn, err := dial(ctx)
	if err != nil {
		err = deadlineErr(ctx, err) // before cancel(), or every dial failure would read as Canceled
		cancel()
		return nil, nil, nil, err
	}
	// A plain socket honours the deadline; a stream forwarded over SSH
	// supports none (x/crypto/ssh channels refuse SetDeadline), and then
	// the dialer's Close is what must end a stalled read. That is the
	// contract a Dialer must meet for this package's timeouts to hold: the
	// collector's SSH dialer meets it by wedging the transport after a
	// grace when a stream is closed with I/O in flight (sshx.boundedConn),
	// so the AfterFunc below is sufficient there; this package itself has
	// no production dialer.
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	stop := context.AfterFunc(ctx, func() { conn.Close() })
	done := func() {
		stop()
		conn.Close()
		cancel()
	}
	if _, err := conn.Write(scgiRequest(body)); err != nil {
		err = deadlineErr(ctx, err)
		done()
		return nil, nil, nil, err
	}
	// rTorrent reads to CONTENT_LENGTH, so no half-close is needed; some
	// servers wait for one, and a unix socket supports it.
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	// The buffer is exactly one header line: ReadSlice refuses a longer
	// line with ErrBufferFull the moment it is full, so a peer sending a
	// header without a newline is cut at maxHeaderLine, not read toward the
	// response cap (daybreak blue re-review of PR 122). Body bytes already
	// buffered after the blank line stay in the reader for the decoder.
	cr := &countingReader{r: io.LimitReader(conn, maxBytes+1)}
	br := bufio.NewReaderSize(cr, maxHeaderLine+1)
	if err := skipHeaders(br); err != nil {
		err = deadlineErr(ctx, err)
		done()
		return nil, nil, nil, err
	}
	return &responseBody{Reader: br, cr: cr, max: maxBytes}, ctx, done, nil
}

// countingReader counts the bytes read through it.
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += int64(n)
	return n, err
}

// responseBody is the response reader with its byte count, so the caller
// can tell a response that stayed within the cap from one that reached the
// sentinel byte past it, even when the decoder was content.
type responseBody struct {
	*bufio.Reader
	cr  *countingReader
	max int64
}

// over reports whether more than the cap was read (headers included).
func (b *responseBody) over() bool { return b.cr.n > b.max }

// skipHeaders consumes the CGI-style header block ("Status: 200 OK",
// "Content-Type: text/xml", ...) up to the blank line. A response that does
// not start with a header line is refused rather than guessed at.
func skipHeaders(br *bufio.Reader) error {
	for n := 0; n < 32; n++ {
		raw, err := br.ReadSlice('\n')
		if err != nil {
			if errors.Is(err, bufio.ErrBufferFull) || errors.Is(err, io.EOF) {
				return ErrSCGI // a line past maxHeaderLine, or closed without a header block
			}
			return err // the read itself failed (deadline, reset)
		}
		line := strings.TrimRight(string(raw), "\r\n")
		if line == "" {
			if n == 0 {
				return ErrSCGI
			}
			return nil
		}
		if n == 0 {
			if _, _, ok := strings.Cut(line, ":"); !ok || strings.HasPrefix(line, "<") {
				return ErrSCGI
			}
		}
	}
	return ErrSCGI
}

// deadlineErr reports the deadline as the cause when the connection's own
// deadline (set to the context's) cut a read, whichever timer fired first.
func deadlineErr(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		return cerr
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return context.DeadlineExceeded
	}
	return err
}
