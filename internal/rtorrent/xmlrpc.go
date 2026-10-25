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

// Package rtorrent talks to one rTorrent instance over its SCGI XML-RPC
// endpoint, reached through a caller-supplied dialer (the SSH channel to
// the tenant's ~/.rtorrent.socket on a PMSS box, a TCP loopback port on
// other setups).
//
// Everything that comes back is treated as hostile: a compromised box can
// answer with anything. Responses are decoded as a stream behind byte,
// element, depth and entry caps, strings are clipped as they are read, and
// no value is retained before it passed its check.
package rtorrent

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"strconv"
	"strings"
	"unicode/utf8"
)

// Limits on what one response may hold. maxResponse bounds the bytes read
// from the socket for a single call; the decoder streams, so this is a
// bandwidth cap, not an allocation. maxElements and maxDepth stop a
// hostile peer from making the decoder walk forever or recurse; maxString
// is the longest string kept from any value (names are clipped tighter by
// the callers that know what they hold).
const (
	maxResponse = 64 << 20
	maxElements = 4_000_000
	maxDepth    = 16
	maxString   = 4096
	// maxValues bounds the Values retained from one response, whatever
	// their nesting (a byte limit is not an allocation limit: four million
	// empty <value/> entries in one inner array are 32 MiB on the wire and
	// hundreds of MiB retained); maxInner bounds any array below the
	// outermost one, where a torrent row has 21 entries and a multicall
	// member one.
	maxValues = 600_000
	maxInner  = 1024
)

// Value is one decoded XML-RPC value. Exactly one field is meaningful,
// chosen by Kind.
type Value struct {
	Kind   Kind
	Int    int64
	Str    string
	Bool   bool
	Float  float64
	Bytes  []byte
	Array  []Value
	Struct map[string]Value
}

// Kind tags the XML-RPC type of a Value.
type Kind uint8

// The XML-RPC value kinds rTorrent emits.
const (
	KindNil Kind = iota
	KindInt
	KindString
	KindBool
	KindFloat
	KindBytes
	KindArray
	KindStruct
)

// Fault is an XML-RPC fault returned by rTorrent (an unknown method, a bad
// hash, a refused command).
type Fault struct {
	Code   int64
	String string
}

func (f *Fault) Error() string { return fmt.Sprintf("rtorrent fault %d: %s", f.Code, f.String) }

var (
	// ErrTooLarge is a response past maxResponse, maxElements or maxDepth.
	ErrTooLarge = errors.New("rtorrent response too large")
	// ErrMalformed is a response that is not an XML-RPC methodResponse.
	ErrMalformed = errors.New("rtorrent response malformed")
)

// encodeCall renders one methodCall. Parameters may be string, int, int64,
// bool or []byte (base64); anything else is a programming error.
func encodeCall(method string, params ...any) ([]byte, error) {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?><methodCall><methodName>`)
	if err := xml.EscapeText(&b, []byte(method)); err != nil {
		return nil, err
	}
	b.WriteString(`</methodName><params>`)
	for _, p := range params {
		b.WriteString(`<param>`)
		if err := encodeValue(&b, p); err != nil {
			return nil, err
		}
		b.WriteString(`</param>`)
	}
	b.WriteString(`</params></methodCall>`)
	return []byte(b.String()), nil
}

func encodeValue(b *strings.Builder, p any) error {
	b.WriteString(`<value>`)
	switch v := p.(type) {
	case string:
		b.WriteString(`<string>`)
		if err := xml.EscapeText(b, []byte(v)); err != nil {
			return err
		}
		b.WriteString(`</string>`)
	case int:
		fmt.Fprintf(b, `<i8>%d</i8>`, v)
	case int64:
		fmt.Fprintf(b, `<i8>%d</i8>`, v)
	case bool:
		if v {
			b.WriteString(`<boolean>1</boolean>`)
		} else {
			b.WriteString(`<boolean>0</boolean>`)
		}
	case []byte:
		b.WriteString(`<base64>`)
		b.WriteString(base64.StdEncoding.EncodeToString(v))
		b.WriteString(`</base64>`)
	case []any:
		b.WriteString(`<array><data>`)
		for _, e := range v {
			if err := encodeValue(b, e); err != nil {
				return err
			}
		}
		b.WriteString(`</data></array>`)
	case map[string]any:
		// system.multicall takes an array of {methodName, params} structs.
		b.WriteString(`<struct>`)
		for _, k := range []string{"methodName", "params"} {
			e, ok := v[k]
			if !ok {
				continue
			}
			b.WriteString(`<member><name>`)
			if err := xml.EscapeText(b, []byte(k)); err != nil {
				return err
			}
			b.WriteString(`</name>`)
			if err := encodeValue(b, e); err != nil {
				return err
			}
			b.WriteString(`</member>`)
		}
		b.WriteString(`</struct>`)
	default:
		return fmt.Errorf("rtorrent: unsupported parameter type %T", p)
	}
	b.WriteString(`</value>`)
	return nil
}

// decoder walks one methodResponse as a token stream.
type decoder struct {
	d         *xml.Decoder
	elements  int
	values    int // Values retained so far, against maxValues
	maxRows   int // entries allowed in the outermost array; 0 = no cap
	maxValues int
}

// retain counts one more retained Value against the budget.
func (dec *decoder) retain() error {
	dec.values++
	if dec.values > dec.maxValues {
		return ErrTooLarge
	}
	return nil
}

// decodeResponse reads a methodResponse from r (already capped by the
// caller) and returns its single parameter, or the Fault it carried.
// maxRows caps the outermost array (the torrent list); more is an error,
// never a silently truncated view.
func decodeResponse(r io.Reader, maxRows int) (Value, error) {
	return decodeResponseBudget(r, maxRows, maxValues)
}

// decodeResponseBudget is decodeResponse with the retained-value budget
// given (tests shrink it).
func decodeResponseBudget(r io.Reader, maxRows, budget int) (Value, error) {
	d := xml.NewDecoder(newUTF8Reader(r))
	d.Strict = true
	// Entities other than the five predefined ones are refused by Strict
	// mode; encoding/xml never fetches external entities or DTDs.
	d.CharsetReader = func(charset string, in io.Reader) (io.Reader, error) {
		if !strings.EqualFold(charset, "utf-8") && !strings.EqualFold(charset, "us-ascii") {
			return nil, fmt.Errorf("unsupported charset %q", charset)
		}
		return in, nil
	}
	dec := &decoder{d: d, maxRows: maxRows, maxValues: budget}
	if err := dec.expectStart("methodResponse"); err != nil {
		return Value{}, err
	}
	name, err := dec.nextStart()
	if err != nil {
		return Value{}, err
	}
	switch name {
	case "params":
		if err := dec.expectStart("param"); err != nil {
			return Value{}, err
		}
		v, err := dec.value()
		if err != nil {
			return Value{}, err
		}
		for _, n := range []string{"param", "params", "methodResponse"} {
			if err := dec.expectEnd(n); err != nil {
				return Value{}, err
			}
		}
		if err := dec.eof(); err != nil {
			return Value{}, err
		}
		return v, nil
	case "fault":
		// A fault is a struct with a typed code and string, inside a
		// complete envelope: a truncated fault is a malformed response, not
		// a fault (it could otherwise flip the client's compatibility
		// switch, see list).
		v, err := dec.value()
		if err != nil {
			return Value{}, err
		}
		if v.Kind != KindStruct {
			return Value{}, ErrMalformed
		}
		code, str := v.Struct["faultCode"], v.Struct["faultString"]
		if code.Kind != KindInt || str.Kind != KindString {
			return Value{}, ErrMalformed
		}
		for _, n := range []string{"fault", "methodResponse"} {
			if err := dec.expectEnd(n); err != nil {
				return Value{}, err
			}
		}
		if err := dec.eof(); err != nil {
			return Value{}, err
		}
		return Value{}, &Fault{Code: code.Int, String: clip(str.Str, 300)}
	}
	return Value{}, ErrMalformed
}

// eof requires that nothing but whitespace, comments and processing
// instructions follow the envelope.
func (dec *decoder) eof() error {
	for {
		tok, err := dec.d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			var ne net.Error
			if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return err
			}
			return ErrMalformed
		}
		switch t := tok.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				return ErrMalformed
			}
		case xml.Comment, xml.ProcInst:
		default:
			return ErrMalformed // a second element after the envelope
		}
	}
}

// value decodes one <value>; the decoder is positioned before its start
// element.
func (dec *decoder) value() (Value, error) {
	return dec.valueAt(0)
}

func (dec *decoder) valueAt(depth int) (Value, error) {
	if depth > maxDepth {
		return Value{}, ErrTooLarge
	}
	if err := dec.expectStart("value"); err != nil {
		return Value{}, err
	}
	return dec.valueBody(depth)
}

// typed decodes the body of a typed element whose start was just read.
func (dec *decoder) typed(name string, depth int) (Value, error) {
	switch name {
	case "i4", "i8", "int": // a namespaced ex:i8 arrives as Local "i8"
		s, err := dec.text(name)
		if err != nil {
			return Value{}, err
		}
		n, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
		if err != nil {
			return Value{}, ErrMalformed
		}
		return Value{Kind: KindInt, Int: n}, nil
	case "string":
		s, err := dec.text(name)
		if err != nil {
			return Value{}, err
		}
		return Value{Kind: KindString, Str: clip(s, maxString)}, nil
	case "boolean":
		s, err := dec.text(name)
		if err != nil {
			return Value{}, err
		}
		switch strings.TrimSpace(s) {
		case "0", "false":
			return Value{Kind: KindBool}, nil
		case "1", "true":
			return Value{Kind: KindBool, Bool: true}, nil
		}
		return Value{}, ErrMalformed
	case "double":
		s, err := dec.text(name)
		if err != nil {
			return Value{}, err
		}
		f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			return Value{}, ErrMalformed
		}
		return Value{Kind: KindFloat, Float: f}, nil
	case "base64":
		s, err := dec.text(name)
		if err != nil {
			return Value{}, err
		}
		b, err := base64.StdEncoding.DecodeString(strings.Join(strings.Fields(s), ""))
		if err != nil {
			return Value{}, ErrMalformed
		}
		return Value{Kind: KindBytes, Bytes: b}, nil
	case "array":
		if err := dec.expectStart("data"); err != nil {
			return Value{}, err
		}
		v := Value{Kind: KindArray}
		for {
			tok, err := dec.token()
			if err != nil {
				return Value{}, err
			}
			switch t := tok.(type) {
			case xml.CharData:
				if strings.TrimSpace(string(t)) != "" {
					return Value{}, ErrMalformed
				}
			case xml.EndElement:
				if t.Name.Local != "data" {
					return Value{}, ErrMalformed
				}
				if err := dec.expectEnd("array"); err != nil {
					return Value{}, err
				}
				return v, nil
			case xml.StartElement:
				if t.Name.Local != "value" {
					return Value{}, ErrMalformed
				}
				if depth == 0 && dec.maxRows > 0 && len(v.Array) >= dec.maxRows {
					return Value{}, ErrTooLarge
				}
				if depth > 0 && len(v.Array) >= maxInner {
					return Value{}, ErrTooLarge
				}
				if err := dec.retain(); err != nil {
					return Value{}, err
				}
				e, err := dec.valueBody(depth + 1)
				if err != nil {
					return Value{}, err
				}
				v.Array = append(v.Array, e)
			default:
				return Value{}, ErrMalformed
			}
		}
	case "struct":
		v := Value{Kind: KindStruct, Struct: map[string]Value{}}
		for {
			tok, err := dec.token()
			if err != nil {
				return Value{}, err
			}
			switch t := tok.(type) {
			case xml.CharData:
				if strings.TrimSpace(string(t)) != "" {
					return Value{}, ErrMalformed
				}
			case xml.EndElement:
				if t.Name.Local != "struct" {
					return Value{}, ErrMalformed
				}
				return v, nil
			case xml.StartElement:
				if t.Name.Local != "member" {
					return Value{}, ErrMalformed
				}
				if err := dec.expectStart("name"); err != nil {
					return Value{}, err
				}
				name, err := dec.text("name")
				if err != nil {
					return Value{}, err
				}
				e, err := dec.valueAt(depth + 1)
				if err != nil {
					return Value{}, err
				}
				if err := dec.expectEnd("member"); err != nil {
					return Value{}, err
				}
				if len(v.Struct) < 256 {
					if err := dec.retain(); err != nil {
						return Value{}, err
					}
					v.Struct[clip(name, 64)] = e
				}
			default:
				return Value{}, ErrMalformed
			}
		}
	case "nil":
		if err := dec.expectEnd(name); err != nil {
			return Value{}, err
		}
		return Value{Kind: KindNil}, nil
	}
	return Value{}, ErrMalformed
}

// valueBody decodes a <value> whose start element was already consumed.
func (dec *decoder) valueBody(depth int) (Value, error) {
	if depth > maxDepth {
		return Value{}, ErrTooLarge
	}
	tok, err := dec.token()
	if err != nil {
		return Value{}, err
	}
	var v Value
	switch t := tok.(type) {
	case xml.CharData:
		text := string(t)
		nxt, err := dec.token()
		if err != nil {
			return Value{}, err
		}
		switch n := nxt.(type) {
		case xml.EndElement:
			if n.Name.Local != "value" {
				return Value{}, ErrMalformed
			}
			return Value{Kind: KindString, Str: clip(text, maxString)}, nil
		case xml.StartElement:
			if strings.TrimSpace(text) != "" {
				return Value{}, ErrMalformed
			}
			v, err = dec.typed(n.Name.Local, depth)
			if err != nil {
				return Value{}, err
			}
		default:
			return Value{}, ErrMalformed
		}
	case xml.StartElement:
		v, err = dec.typed(t.Name.Local, depth)
		if err != nil {
			return Value{}, err
		}
	case xml.EndElement:
		if t.Name.Local != "value" {
			return Value{}, ErrMalformed
		}
		return Value{Kind: KindString}, nil
	default:
		return Value{}, ErrMalformed
	}
	if err := dec.expectEnd("value"); err != nil {
		return Value{}, err
	}
	return v, nil
}

// text reads character data up to the end of element name, clipped so a
// hostile peer cannot make one string hold the whole response.
func (dec *decoder) text(name string) (string, error) {
	var b strings.Builder
	for {
		tok, err := dec.token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			// One token may be the whole string (encoding/xml returns a run
			// of character data as a single token), so each write is bounded
			// by the room left, not just gated on it: a 16 MiB name would
			// otherwise sit in the builder behind a 400-rune clip.
			if room := maxString*4 - b.Len(); room > 0 { // bytes; clipped to runes by the caller
				if len(t) > room {
					t = t[:room]
				}
				b.Write(t)
			}
		case xml.EndElement:
			if t.Name.Local != name {
				return "", ErrMalformed
			}
			return b.String(), nil
		default:
			return "", ErrMalformed
		}
	}
}

// token reads the next token, skipping comments and processing
// instructions, and counts elements against maxElements.
func (dec *decoder) token() (xml.Token, error) {
	for {
		tok, err := dec.d.Token()
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil, ErrMalformed
			}
			// Only the socket's own errors pass through (they are
			// attributed to the deadline by the caller); everything the
			// decoder itself objects to, a syntax error or a charset the
			// CharsetReader refused, is a malformed response.
			var ne net.Error
			if errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
				return nil, err
			}
			return nil, ErrMalformed
		}
		switch tok.(type) {
		case xml.Comment, xml.ProcInst, xml.Directive:
			dec.elements++
			if dec.elements > maxElements {
				return nil, ErrTooLarge
			}
			continue
		case xml.StartElement:
			dec.elements++
			if dec.elements > maxElements {
				return nil, ErrTooLarge
			}
		}
		return tok, nil
	}
}

// nextStart skips whitespace and returns the next start element's name.
func (dec *decoder) nextStart() (string, error) {
	for {
		tok, err := dec.token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				return "", ErrMalformed
			}
		case xml.StartElement:
			return t.Name.Local, nil
		default:
			return "", ErrMalformed
		}
	}
}

func (dec *decoder) expectStart(name string) error {
	got, err := dec.nextStart()
	if err != nil {
		return err
	}
	if got != name {
		return ErrMalformed
	}
	return nil
}

func (dec *decoder) expectEnd(name string) error {
	for {
		tok, err := dec.token()
		if err != nil {
			return err
		}
		switch t := tok.(type) {
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				return ErrMalformed
			}
		case xml.EndElement:
			if t.Name.Local != name {
				return ErrMalformed
			}
			return nil
		default:
			return ErrMalformed
		}
	}
}

// clip keeps at most n runes of s, with invalid UTF-8 already replaced by
// utf8Reader.
func clip(s string, n int) string {
	if len(s) <= n {
		return s
	}
	cut, runes := 0, 0
	for cut < len(s) && runes < n {
		_, w := utf8.DecodeRuneInString(s[cut:])
		cut += w
		runes++
	}
	return strings.Clone(s[:cut]) // its own allocation: the rest of s is garbage
}

// utf8Buffered replaces invalid UTF-8 with U+FFFD on the way in. rTorrent
// emits torrent names as the raw bytes it was given (the tinyxml2 backend
// validates nothing), and encoding/xml rejects a malformed sequence, which
// would hide a whole list behind one odd name. A rune split across two
// reads stays pending until the next read completes it.
type utf8Buffered struct {
	r       io.Reader
	pending []byte // bytes read but not yet validated (at most 3)
	out     []byte // sanitised bytes not yet handed out
	err     error
}

func newUTF8Reader(r io.Reader) io.Reader { return &utf8Buffered{r: r} }

func (b *utf8Buffered) Read(p []byte) (int, error) {
	for len(b.out) == 0 {
		if b.err != nil {
			if len(b.pending) > 0 { // a truncated rune at the very end
				b.out = append(b.out, "�"...)
				b.pending = nil
				break
			}
			return 0, b.err
		}
		buf := make([]byte, 4096)
		n, err := b.r.Read(buf)
		b.err = err
		b.pending = append(b.pending, buf[:n]...)
		b.sanitise(err != nil)
	}
	n := copy(p, b.out)
	b.out = b.out[n:]
	return n, nil
}

// sanitise moves complete runes from pending to out, replacing invalid
// bytes; unless final, a trailing partial rune stays pending.
func (b *utf8Buffered) sanitise(final bool) {
	i := 0
	for i < len(b.pending) {
		r, w := utf8.DecodeRune(b.pending[i:])
		if r == utf8.RuneError && w <= 1 {
			if !final && !utf8.FullRune(b.pending[i:]) && len(b.pending)-i < utf8.UTFMax {
				break // maybe the rest of this rune is in the next read
			}
			b.out = append(b.out, "�"...)
			i++
			continue
		}
		b.out = append(b.out, b.pending[i:i+w]...)
		i += w
	}
	b.pending = b.pending[i:]
}
