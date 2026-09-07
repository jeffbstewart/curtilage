// SPDX-License-Identifier: Apache-2.0

package webauthn

import (
	"errors"
	"fmt"
	"math"
)

// A minimal CBOR reader: exactly the subset CTAP2 canonical encoding
// uses in attestation objects and COSE keys -- definite-length maps,
// arrays, byte and text strings, and integers.  Nothing streams,
// nothing is indefinite, and unknown simple values are errors.  A
// full CBOR library would be the only reason for a new dependency in
// this package; the subset is a page.

var errCBOR = errors.New("webauthn: malformed CBOR")

type cborReader struct {
	b   []byte
	pos int
}

// head reads one item header: major type and its integer argument.
func (r *cborReader) head() (major byte, arg uint64, err error) {
	if r.pos >= len(r.b) {
		return 0, 0, errCBOR
	}
	first := r.b[r.pos]
	r.pos++
	major = first >> 5
	info := first & 0x1f
	switch {
	case info < 24:
		return major, uint64(info), nil
	case info == 24, info == 25, info == 26, info == 27:
		n := 1 << (info - 24)
		if r.pos+n > len(r.b) {
			return 0, 0, errCBOR
		}
		for i := 0; i < n; i++ {
			arg = arg<<8 | uint64(r.b[r.pos+i])
		}
		r.pos += n
		return major, arg, nil
	}
	return 0, 0, fmt.Errorf("%w: indefinite or reserved length", errCBOR)
}

// value reads any supported item as one of: int64, []byte, string,
// []any, map[int64]any (COSE and attestation maps key by int or
// text; text keys land in the same map as strings via mapKey).
func (r *cborReader) value(depth int) (any, error) {
	if depth > 8 {
		return nil, fmt.Errorf("%w: nesting", errCBOR)
	}
	major, arg, err := r.head()
	if err != nil {
		return nil, err
	}
	switch major {
	case 0: // unsigned
		if arg > math.MaxInt64 {
			return nil, errCBOR
		}
		return int64(arg), nil
	case 1: // negative: -1 - arg
		if arg > math.MaxInt64-1 {
			return nil, errCBOR
		}
		return -1 - int64(arg), nil
	case 2: // byte string
		return r.take(arg)
	case 3: // text string
		b, err := r.take(arg)
		if err != nil {
			return nil, err
		}
		return string(b), nil
	case 4: // array
		if arg > 64 {
			return nil, errCBOR
		}
		out := make([]any, 0, arg)
		for i := uint64(0); i < arg; i++ {
			v, err := r.value(depth + 1)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		}
		return out, nil
	case 5: // map
		if arg > 64 {
			return nil, errCBOR
		}
		out := make(map[any]any, arg)
		for i := uint64(0); i < arg; i++ {
			k, err := r.value(depth + 1)
			if err != nil {
				return nil, err
			}
			switch k.(type) {
			case int64, string:
			default:
				return nil, fmt.Errorf("%w: map key type", errCBOR)
			}
			v, err := r.value(depth + 1)
			if err != nil {
				return nil, err
			}
			out[k] = v
		}
		return out, nil
	}
	return nil, fmt.Errorf("%w: major type %d", errCBOR, major)
}

func (r *cborReader) take(n uint64) ([]byte, error) {
	if n > uint64(len(r.b)-r.pos) {
		return nil, errCBOR
	}
	out := r.b[r.pos : r.pos+int(n)]
	r.pos += int(n)
	return out, nil
}

// decodeCBOR reads one item and requires nothing to follow it.
func decodeCBOR(b []byte) (any, error) {
	r := &cborReader{b: b}
	v, err := r.value(0)
	if err != nil {
		return nil, err
	}
	if r.pos != len(r.b) {
		return nil, fmt.Errorf("%w: trailing bytes", errCBOR)
	}
	return v, nil
}

// decodeCBORPrefix reads one item and returns it with the number of
// bytes it spanned -- COSE keys inside authenticator data are not
// length-prefixed, the CBOR itself is the delimiter.
func decodeCBORPrefix(b []byte) (any, int, error) {
	r := &cborReader{b: b}
	v, err := r.value(0)
	if err != nil {
		return nil, 0, err
	}
	return v, r.pos, nil
}
