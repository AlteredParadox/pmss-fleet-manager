// Package phpserial decodes the subset of PHP serialize() output that PMSS
// writes to tenant-readable stats files (arrays of scalars, nested).
package phpserial

import (
	"errors"
	"fmt"
	"strconv"
)

// Array is a PHP array: ordered keys (string or int64) with arbitrary values.
type Array struct {
	Keys   []any
	Values map[any]any
}

// Get returns the value for a string or int key.
func (a *Array) Get(key any) (any, bool) {
	if a == nil {
		return nil, false
	}
	if k, ok := key.(int); ok {
		key = int64(k)
	}
	v, ok := a.Values[key]
	return v, ok
}

// Float returns the value at key coerced to float64.
func (a *Array) Float(key any) (float64, bool) {
	v, ok := a.Get(key)
	if !ok {
		return 0, false
	}
	return ToFloat(v)
}

// Array returns the nested array at key, or nil.
func (a *Array) Array(key any) *Array {
	v, ok := a.Get(key)
	if !ok {
		return nil
	}
	sub, _ := v.(*Array)
	return sub
}

// ToFloat coerces a decoded scalar to float64.
func ToFloat(v any) (float64, bool) {
	switch t := v.(type) {
	case int64:
		return float64(t), true
	case float64:
		return t, true
	case string:
		f, err := strconv.ParseFloat(t, 64)
		return f, err == nil
	case bool:
		if t {
			return 1, true
		}
		return 0, true
	}
	return 0, false
}

type decoder struct {
	b   []byte
	pos int
}

// Unserialize decodes a PHP serialized value. Supported types: i, d, s, b, N, a.
func Unserialize(b []byte) (any, error) {
	d := &decoder{b: b}
	return d.value()
}

func (d *decoder) expect(c byte) error {
	if d.pos >= len(d.b) || d.b[d.pos] != c {
		return fmt.Errorf("phpserial: expected %q at offset %d", c, d.pos)
	}
	d.pos++
	return nil
}

func (d *decoder) readUntil(c byte) (string, error) {
	start := d.pos
	for d.pos < len(d.b) {
		if d.b[d.pos] == c {
			s := string(d.b[start:d.pos])
			d.pos++
			return s, nil
		}
		d.pos++
	}
	return "", errors.New("phpserial: unexpected end of input")
}

func (d *decoder) value() (any, error) {
	if d.pos >= len(d.b) {
		return nil, errors.New("phpserial: unexpected end of input")
	}
	typ := d.b[d.pos]
	d.pos++
	switch typ {
	case 'N':
		return nil, d.expect(';')
	case 'b', 'i', 'd':
		if err := d.expect(':'); err != nil {
			return nil, err
		}
		raw, err := d.readUntil(';')
		if err != nil {
			return nil, err
		}
		switch typ {
		case 'b':
			return raw == "1", nil
		case 'i':
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil {
				return nil, fmt.Errorf("phpserial: bad int %q", raw)
			}
			return n, nil
		default:
			switch raw {
			case "INF", "-INF", "NAN":
				return 0.0, nil
			}
			f, err := strconv.ParseFloat(raw, 64)
			if err != nil {
				return nil, fmt.Errorf("phpserial: bad float %q", raw)
			}
			return f, nil
		}
	case 's':
		if err := d.expect(':'); err != nil {
			return nil, err
		}
		lenStr, err := d.readUntil(':')
		if err != nil {
			return nil, err
		}
		n, err := strconv.Atoi(lenStr)
		if err != nil || n < 0 {
			return nil, fmt.Errorf("phpserial: bad string length %q", lenStr)
		}
		if err := d.expect('"'); err != nil {
			return nil, err
		}
		if d.pos+n > len(d.b) {
			return nil, errors.New("phpserial: string overruns input")
		}
		s := string(d.b[d.pos : d.pos+n])
		d.pos += n
		if err := d.expect('"'); err != nil {
			return nil, err
		}
		return s, d.expect(';')
	case 'a':
		if err := d.expect(':'); err != nil {
			return nil, err
		}
		cntStr, err := d.readUntil(':')
		if err != nil {
			return nil, err
		}
		cnt, err := strconv.Atoi(cntStr)
		if err != nil || cnt < 0 {
			return nil, fmt.Errorf("phpserial: bad array length %q", cntStr)
		}
		if err := d.expect('{'); err != nil {
			return nil, err
		}
		arr := &Array{Values: make(map[any]any, cnt)}
		for i := 0; i < cnt; i++ {
			k, err := d.value()
			if err != nil {
				return nil, err
			}
			switch k.(type) {
			case int64, string:
			default:
				return nil, fmt.Errorf("phpserial: unsupported array key %T", k)
			}
			v, err := d.value()
			if err != nil {
				return nil, err
			}
			arr.Keys = append(arr.Keys, k)
			arr.Values[k] = v
		}
		return arr, d.expect('}')
	}
	return nil, fmt.Errorf("phpserial: unsupported type %q at offset %d", typ, d.pos-1)
}
