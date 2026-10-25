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
// reader positioned at the response body, capped at maxResponse bytes.
// The connection is closed when ctx ends or when the caller is done with
// the returned closer.
func roundTrip(ctx context.Context, dial Dialer, timeout time.Duration, maxBytes int64, body []byte) (io.Reader, func(), error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	conn, err := dial(ctx)
	if err != nil {
		cancel()
		return nil, nil, err
	}
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
		done()
		return nil, nil, err
	}
	// rTorrent reads to CONTENT_LENGTH, so no half-close is needed; some
	// servers wait for one, and a unix socket supports it.
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
	br := bufio.NewReaderSize(io.LimitReader(conn, maxBytes+1), 64<<10)
	if err := skipHeaders(br); err != nil {
		err = deadlineErr(ctx, err)
		done()
		return nil, nil, err
	}
	return br, done, nil
}

// skipHeaders consumes the CGI-style header block ("Status: 200 OK",
// "Content-Type: text/xml", ...) up to the blank line. A response that does
// not start with a header line is refused rather than guessed at.
func skipHeaders(br *bufio.Reader) error {
	for n := 0; n < 32; n++ {
		line, err := br.ReadString('\n')
		if err != nil {
			if errors.Is(err, io.EOF) {
				return ErrSCGI // closed without a header block
			}
			return err // the read itself failed (deadline, reset)
		}
		if len(line) > 8192 {
			return ErrSCGI
		}
		line = strings.TrimRight(line, "\r\n")
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
