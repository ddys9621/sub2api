package kiro

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"sort"
)

// Minimal CBOR (RFC 8949) encoder/decoder used by Kiro's Smithy rpc-v2-cbor RPC endpoints.
//
// We implement only the subset required for ExchangeToken / RefreshToken /
// GetUserInfo / GetUserUsageAndLimits / GenerateSubscriptionManagementUrl:
//   - unsigned integers, negative integers (major types 0/1)
//   - byte strings, text strings (major types 2/3, definite length only)
//   - arrays, maps (major types 4/5, definite length only)
//   - tagged items (major type 6) are decoded transparently (tag value is discarded)
//   - simple/float values (major type 7): false, true, null, undefined, float16/32/64
//
// Indefinite-length strings/arrays/maps, floats with NaN/Inf payloads, and
// non-standard tags beyond transparent pass-through are not implemented.

// EncodeCBOR serialises a Go value to canonical CBOR.
//
// Supported Go types:
//   - nil                       -> null
//   - bool                      -> true/false
//   - int / int8..int64         -> integer
//   - uint / uint8..uint64      -> integer
//   - float32 / float64         -> float
//   - string                    -> text string
//   - []byte                    -> byte string
//   - []any                     -> array
//   - map[string]any            -> map (keys sorted lexicographically)
//   - anything implementing the CBORMarshaler interface
func EncodeCBOR(v any) ([]byte, error) {
	var buf []byte
	buf, err := encodeValue(buf, v)
	if err != nil {
		return nil, err
	}
	return buf, nil
}

// CBORMarshaler lets callers provide a custom CBOR representation.
type CBORMarshaler interface {
	MarshalCBOR() ([]byte, error)
}

func encodeValue(dst []byte, v any) ([]byte, error) {
	if v == nil {
		return append(dst, 0xf6), nil // null
	}

	if m, ok := v.(CBORMarshaler); ok {
		raw, err := m.MarshalCBOR()
		if err != nil {
			return nil, err
		}
		return append(dst, raw...), nil
	}

	switch x := v.(type) {
	case bool:
		if x {
			return append(dst, 0xf5), nil
		}
		return append(dst, 0xf4), nil
	case int:
		return encodeSignedInt(dst, int64(x)), nil
	case int8:
		return encodeSignedInt(dst, int64(x)), nil
	case int16:
		return encodeSignedInt(dst, int64(x)), nil
	case int32:
		return encodeSignedInt(dst, int64(x)), nil
	case int64:
		return encodeSignedInt(dst, x), nil
	case uint:
		return encodeUint(dst, 0, uint64(x)), nil
	case uint8:
		return encodeUint(dst, 0, uint64(x)), nil
	case uint16:
		return encodeUint(dst, 0, uint64(x)), nil
	case uint32:
		return encodeUint(dst, 0, uint64(x)), nil
	case uint64:
		return encodeUint(dst, 0, x), nil
	case float32:
		return encodeFloat64(dst, float64(x)), nil
	case float64:
		return encodeFloat64(dst, x), nil
	case string:
		dst = encodeUint(dst, 3, uint64(len(x)))
		return append(dst, x...), nil
	case []byte:
		dst = encodeUint(dst, 2, uint64(len(x)))
		return append(dst, x...), nil
	case []any:
		dst = encodeUint(dst, 4, uint64(len(x)))
		var err error
		for _, item := range x {
			dst, err = encodeValue(dst, item)
			if err != nil {
				return nil, err
			}
		}
		return dst, nil
	case map[string]any:
		// Sort keys for deterministic output (Canonical CBOR key ordering is
		// byte-wise shortest-first; for strings of equal length and ASCII-only
		// content used by Kiro RPCs, lexicographic ordering is sufficient).
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		dst = encodeUint(dst, 5, uint64(len(keys)))
		var err error
		for _, k := range keys {
			dst = encodeUint(dst, 3, uint64(len(k)))
			dst = append(dst, k...)
			dst, err = encodeValue(dst, x[k])
			if err != nil {
				return nil, err
			}
		}
		return dst, nil
	}

	return nil, fmt.Errorf("cbor: unsupported type %T", v)
}

func encodeUint(dst []byte, major byte, n uint64) []byte {
	mt := major << 5
	switch {
	case n < 24:
		return append(dst, mt|byte(n))
	case n < 1<<8:
		return append(dst, mt|24, byte(n))
	case n < 1<<16:
		return append(dst, mt|25, byte(n>>8), byte(n))
	case n < 1<<32:
		return append(dst, mt|26, byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	default:
		return append(dst, mt|27,
			byte(n>>56), byte(n>>48), byte(n>>40), byte(n>>32),
			byte(n>>24), byte(n>>16), byte(n>>8), byte(n))
	}
}

func encodeSignedInt(dst []byte, n int64) []byte {
	if n >= 0 {
		return encodeUint(dst, 0, uint64(n))
	}
	return encodeUint(dst, 1, uint64(-(n + 1)))
}

func encodeFloat64(dst []byte, f float64) []byte {
	bits := math.Float64bits(f)
	return append(dst, 0xfb,
		byte(bits>>56), byte(bits>>48), byte(bits>>40), byte(bits>>32),
		byte(bits>>24), byte(bits>>16), byte(bits>>8), byte(bits))
}

// ---------------------------------------------------------------------------
// Decoder
// ---------------------------------------------------------------------------

// DecodeCBOR parses a CBOR document into Go values using the same conventions
// as encoding/json for maps/arrays:
//   - CBOR integers  -> int64 (or uint64 when exceeding int64 range)
//   - CBOR floats    -> float64
//   - CBOR text/byte -> string / []byte
//   - CBOR arrays    -> []any
//   - CBOR maps      -> map[string]any (non-string keys are coerced with fmt.Sprint)
//   - CBOR tags      -> the tagged value is returned; the tag number is discarded
func DecodeCBOR(data []byte) (any, error) {
	d := &cborDecoder{buf: data}
	v, err := d.decode()
	if err != nil {
		return nil, err
	}
	if d.off != len(d.buf) {
		return nil, fmt.Errorf("cbor: trailing bytes after top-level value (%d unread)", len(d.buf)-d.off)
	}
	return v, nil
}

type cborDecoder struct {
	buf []byte
	off int
}

func (d *cborDecoder) readByte() (byte, error) {
	if d.off >= len(d.buf) {
		return 0, errors.New("cbor: unexpected end of data")
	}
	b := d.buf[d.off]
	d.off++
	return b, nil
}

func (d *cborDecoder) readN(n int) ([]byte, error) {
	if n < 0 {
		return nil, fmt.Errorf("cbor: negative length %d", n)
	}
	if d.off+n > len(d.buf) {
		return nil, fmt.Errorf("cbor: short read, need %d have %d", n, len(d.buf)-d.off)
	}
	b := d.buf[d.off : d.off+n]
	d.off += n
	return b, nil
}

// readHead returns (major, additional info decoded as uint64, isIndefinite).
func (d *cborDecoder) readHead() (byte, uint64, bool, error) {
	b, err := d.readByte()
	if err != nil {
		return 0, 0, false, err
	}
	major := b >> 5
	ai := b & 0x1f

	switch {
	case ai < 24:
		return major, uint64(ai), false, nil
	case ai == 24:
		x, err := d.readByte()
		if err != nil {
			return 0, 0, false, err
		}
		return major, uint64(x), false, nil
	case ai == 25:
		raw, err := d.readN(2)
		if err != nil {
			return 0, 0, false, err
		}
		return major, uint64(binary.BigEndian.Uint16(raw)), false, nil
	case ai == 26:
		raw, err := d.readN(4)
		if err != nil {
			return 0, 0, false, err
		}
		return major, uint64(binary.BigEndian.Uint32(raw)), false, nil
	case ai == 27:
		raw, err := d.readN(8)
		if err != nil {
			return 0, 0, false, err
		}
		return major, binary.BigEndian.Uint64(raw), false, nil
	case ai == 31:
		return major, 0, true, nil
	default:
		return 0, 0, false, fmt.Errorf("cbor: reserved additional info %d for major %d", ai, major)
	}
}

func (d *cborDecoder) decode() (any, error) {
	major, n, indef, err := d.readHead()
	if err != nil {
		return nil, err
	}
	switch major {
	case 0: // unsigned
		if n <= math.MaxInt64 {
			return int64(n), nil
		}
		return n, nil
	case 1: // negative
		if n <= math.MaxInt64 {
			v := -int64(n) - 1
			return v, nil
		}
		return nil, fmt.Errorf("cbor: negative integer out of int64 range")
	case 2: // bytes
		if indef {
			// Indefinite-length byte string: sequence of definite byte
			// strings terminated by a 0xff "break" stop code.
			var out []byte
			for {
				b, err := d.readByte()
				if err != nil {
					return nil, err
				}
				if b == 0xff {
					break
				}
				// Put the byte back and decode one definite byte string.
				d.off--
				chunkMajor, chunkLen, chunkIndef, cerr := d.readHead()
				if cerr != nil {
					return nil, cerr
				}
				if chunkIndef || chunkMajor != 2 {
					return nil, fmt.Errorf("cbor: invalid chunk in indefinite byte string (major=%d indef=%v)", chunkMajor, chunkIndef)
				}
				chunk, rerr := d.readN(int(chunkLen))
				if rerr != nil {
					return nil, rerr
				}
				out = append(out, chunk...)
			}
			return out, nil
		}
		return d.readN(int(n))
	case 3: // text
		if indef {
			var sb []byte
			for {
				b, err := d.readByte()
				if err != nil {
					return nil, err
				}
				if b == 0xff {
					break
				}
				d.off--
				chunkMajor, chunkLen, chunkIndef, cerr := d.readHead()
				if cerr != nil {
					return nil, cerr
				}
				if chunkIndef || chunkMajor != 3 {
					return nil, fmt.Errorf("cbor: invalid chunk in indefinite text string (major=%d indef=%v)", chunkMajor, chunkIndef)
				}
				chunk, rerr := d.readN(int(chunkLen))
				if rerr != nil {
					return nil, rerr
				}
				sb = append(sb, chunk...)
			}
			return string(sb), nil
		}
		raw, err := d.readN(int(n))
		if err != nil {
			return nil, err
		}
		return string(raw), nil
	case 4: // array
		if indef {
			arr := make([]any, 0, 8)
			for {
				b, err := d.readByte()
				if err != nil {
					return nil, err
				}
				if b == 0xff {
					break
				}
				d.off--
				item, derr := d.decode()
				if derr != nil {
					return nil, derr
				}
				arr = append(arr, item)
			}
			return arr, nil
		}
		arr := make([]any, 0, n)
		for i := uint64(0); i < n; i++ {
			item, err := d.decode()
			if err != nil {
				return nil, err
			}
			arr = append(arr, item)
		}
		return arr, nil
	case 5: // map
		if indef {
			m := make(map[string]any, 8)
			for {
				b, err := d.readByte()
				if err != nil {
					return nil, err
				}
				if b == 0xff {
					break
				}
				d.off--
				k, kerr := d.decode()
				if kerr != nil {
					return nil, kerr
				}
				v, verr := d.decode()
				if verr != nil {
					return nil, verr
				}
				var keyStr string
				if s, ok := k.(string); ok {
					keyStr = s
				} else {
					keyStr = fmt.Sprint(k)
				}
				m[keyStr] = v
			}
			return m, nil
		}
		m := make(map[string]any, n)
		for i := uint64(0); i < n; i++ {
			k, err := d.decode()
			if err != nil {
				return nil, err
			}
			v, err := d.decode()
			if err != nil {
				return nil, err
			}
			var keyStr string
			switch kx := k.(type) {
			case string:
				keyStr = kx
			default:
				keyStr = fmt.Sprint(kx)
			}
			m[keyStr] = v
		}
		return m, nil
	case 6: // tagged
		return d.decode()
	case 7: // simple/float
		switch n {
		case 20:
			return false, nil
		case 21:
			return true, nil
		case 22, 23:
			return nil, nil
		}
		// Floats: additional info 25 -> half, 26 -> single, 27 -> double.
		// We only ever see half/single in extremely compact payloads; emit float64 for all.
		switch {
		case n >= 0x10000: // float32 (ai=26 reconstituted) or float64 (ai=27)
			// We can't tell from n alone whether the original was f32 or f64.
			// The readHead consumed bytes already, so we approximate by bit width.
			if n>>32 != 0 {
				return math.Float64frombits(n), nil
			}
			return float64(math.Float32frombits(uint32(n))), nil
		default:
			// half-float or simple value <= 0xffff; best-effort float16 decoding
			return halfToFloat64(uint16(n)), nil
		}
	}
	return nil, fmt.Errorf("cbor: unknown major type %d", major)
}

func halfToFloat64(h uint16) float64 {
	sign := uint32(h>>15) & 0x01
	exp := uint32(h>>10) & 0x1f
	mant := uint32(h & 0x3ff)

	var f32bits uint32
	switch exp {
	case 0:
		if mant == 0 {
			f32bits = sign << 31
		} else {
			for mant&0x400 == 0 {
				mant <<= 1
				exp--
			}
			exp++
			mant &= 0x3ff
			f32bits = sign<<31 | (exp+127-15)<<23 | mant<<13
		}
	case 31:
		f32bits = sign<<31 | 0xff<<23 | mant<<13
	default:
		f32bits = sign<<31 | (exp+127-15)<<23 | mant<<13
	}
	return float64(math.Float32frombits(f32bits))
}

// AsString returns the string value from a decoded CBOR map entry, empty if missing.
func AsString(m map[string]any, key string) string {
	if v, ok := m[key]; ok {
		if s, ok := v.(string); ok {
			return s
		}
	}
	return ""
}

// AsInt64 returns the int64 value from a decoded CBOR map entry, 0 if missing.
func AsInt64(m map[string]any, key string) int64 {
	v, ok := m[key]
	if !ok {
		return 0
	}
	switch x := v.(type) {
	case int64:
		return x
	case uint64:
		if x <= math.MaxInt64 {
			return int64(x)
		}
		return math.MaxInt64
	case int:
		return int64(x)
	case float64:
		return int64(x)
	}
	return 0
}
