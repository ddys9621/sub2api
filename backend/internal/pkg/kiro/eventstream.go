package kiro

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
)

// AWS EventStream (vnd.amazon.eventstream) binary frame parser.
//
// Frame layout (all big-endian):
//
//	+------------------+------------------+---------------------+
//	| TotalLength (4)  | HeadersLength(4) |  PreludeCRC (4)     |
//	+------------------+------------------+---------------------+
//	| Headers (HeadersLength bytes)                              |
//	+------------------------------------------------------------+
//	| Payload (TotalLength - HeadersLength - 16 bytes)           |
//	+------------------------------------------------------------+
//	| MessageCRC (4)                                             |
//	+------------------------------------------------------------+
//
// Header layout (repeated):
//
//	+-------------+--------------------+-------------+-----------+
//	| NameLen (1) | Name (NameLen ASCII) | Type (1)   | Value ... |
//	+-------------+--------------------+-------------+-----------+
//
// Type codes used by CodeWhisperer:
//
//	0x00 boolTrue, 0x01 boolFalse, 0x02 byte, 0x03 int16, 0x04 int32,
//	0x05 int64, 0x06 bytes (uint16 len), 0x07 string (uint16 len),
//	0x08 timestamp (int64 ms), 0x09 uuid (16 bytes)

// EventStreamMessage is one decoded frame.
type EventStreamMessage struct {
	Headers map[string]EventStreamHeader
	Payload []byte
}

// EventStreamHeader is one decoded header value.
type EventStreamHeader struct {
	Type  byte
	Value any // bool / int32 / int64 / string / []byte
}

// StringValue returns the header value as string (empty if absent or not a string).
func (m EventStreamMessage) StringValue(name string) string {
	h, ok := m.Headers[name]
	if !ok {
		return ""
	}
	if s, ok := h.Value.(string); ok {
		return s
	}
	return ""
}

// EventType returns the conventional ":event-type" header value.
func (m EventStreamMessage) EventType() string {
	return m.StringValue(":event-type")
}

// MessageType returns the ":message-type" header (usually "event" or "exception").
func (m EventStreamMessage) MessageType() string {
	return m.StringValue(":message-type")
}

// ErrShortFrame is returned when the stream ends in the middle of a frame.
var ErrShortFrame = errors.New("eventstream: unexpected EOF in frame")

// EventStreamReader decodes a sequence of EventStream frames from an io.Reader.
type EventStreamReader struct {
	r *bufio.Reader
}

// NewEventStreamReader wraps r with a buffered reader.
func NewEventStreamReader(r io.Reader) *EventStreamReader {
	return &EventStreamReader{r: bufio.NewReaderSize(r, 64*1024)}
}

// Next reads and decodes the next frame. Returns io.EOF when the stream ends
// cleanly on a frame boundary.
func (esr *EventStreamReader) Next() (*EventStreamMessage, error) {
	var prelude [12]byte
	if _, err := io.ReadFull(esr.r, prelude[:]); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, io.EOF
		}
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return nil, ErrShortFrame
		}
		return nil, err
	}

	totalLen := binary.BigEndian.Uint32(prelude[0:4])
	headersLen := binary.BigEndian.Uint32(prelude[4:8])
	preludeCRC := binary.BigEndian.Uint32(prelude[8:12])

	// Validate prelude CRC (CRC32/IEEE over the first 8 bytes).
	if crc32.ChecksumIEEE(prelude[:8]) != preludeCRC {
		return nil, fmt.Errorf("eventstream: prelude CRC mismatch")
	}

	if totalLen < 16 || headersLen > totalLen-16 {
		return nil, fmt.Errorf("eventstream: invalid lengths (total=%d headers=%d)", totalLen, headersLen)
	}

	remaining := int(totalLen) - 12
	body := make([]byte, remaining)
	if _, err := io.ReadFull(esr.r, body); err != nil {
		return nil, ErrShortFrame
	}

	// Verify message CRC (over the first (totalLen-4) bytes, i.e. prelude + headers + payload).
	msgCRC := binary.BigEndian.Uint32(body[remaining-4:])
	combined := make([]byte, 0, 12+remaining-4)
	combined = append(combined, prelude[:]...)
	combined = append(combined, body[:remaining-4]...)
	if crc32.ChecksumIEEE(combined) != msgCRC {
		return nil, fmt.Errorf("eventstream: message CRC mismatch")
	}

	headersRaw := body[:headersLen]
	payload := body[headersLen : remaining-4]

	headers, err := parseHeaders(headersRaw)
	if err != nil {
		return nil, err
	}

	return &EventStreamMessage{
		Headers: headers,
		Payload: append([]byte(nil), payload...),
	}, nil
}

func parseHeaders(raw []byte) (map[string]EventStreamHeader, error) {
	out := make(map[string]EventStreamHeader)
	for i := 0; i < len(raw); {
		if i+1 > len(raw) {
			return nil, fmt.Errorf("eventstream: truncated header name length")
		}
		nameLen := int(raw[i])
		i++
		if i+nameLen > len(raw) {
			return nil, fmt.Errorf("eventstream: truncated header name")
		}
		name := string(raw[i : i+nameLen])
		i += nameLen

		if i+1 > len(raw) {
			return nil, fmt.Errorf("eventstream: missing header type for %q", name)
		}
		typ := raw[i]
		i++

		var value any
		switch typ {
		case 0x00: // true
			value = true
		case 0x01: // false
			value = false
		case 0x02: // byte
			if i+1 > len(raw) {
				return nil, fmt.Errorf("eventstream: truncated byte header %q", name)
			}
			value = int8(raw[i])
			i++
		case 0x03: // int16
			if i+2 > len(raw) {
				return nil, fmt.Errorf("eventstream: truncated int16 header %q", name)
			}
			value = int16(binary.BigEndian.Uint16(raw[i : i+2]))
			i += 2
		case 0x04: // int32
			if i+4 > len(raw) {
				return nil, fmt.Errorf("eventstream: truncated int32 header %q", name)
			}
			value = int32(binary.BigEndian.Uint32(raw[i : i+4]))
			i += 4
		case 0x05: // int64
			if i+8 > len(raw) {
				return nil, fmt.Errorf("eventstream: truncated int64 header %q", name)
			}
			value = int64(binary.BigEndian.Uint64(raw[i : i+8]))
			i += 8
		case 0x06: // byte array
			if i+2 > len(raw) {
				return nil, fmt.Errorf("eventstream: truncated byte array header %q", name)
			}
			bl := int(binary.BigEndian.Uint16(raw[i : i+2]))
			i += 2
			if i+bl > len(raw) {
				return nil, fmt.Errorf("eventstream: truncated byte array data %q", name)
			}
			value = append([]byte(nil), raw[i:i+bl]...)
			i += bl
		case 0x07: // string
			if i+2 > len(raw) {
				return nil, fmt.Errorf("eventstream: truncated string header %q", name)
			}
			sl := int(binary.BigEndian.Uint16(raw[i : i+2]))
			i += 2
			if i+sl > len(raw) {
				return nil, fmt.Errorf("eventstream: truncated string data %q", name)
			}
			value = string(raw[i : i+sl])
			i += sl
		case 0x08: // timestamp (int64 millis)
			if i+8 > len(raw) {
				return nil, fmt.Errorf("eventstream: truncated timestamp header %q", name)
			}
			value = int64(binary.BigEndian.Uint64(raw[i : i+8]))
			i += 8
		case 0x09: // uuid
			if i+16 > len(raw) {
				return nil, fmt.Errorf("eventstream: truncated uuid header %q", name)
			}
			value = append([]byte(nil), raw[i:i+16]...)
			i += 16
		default:
			return nil, fmt.Errorf("eventstream: unknown header type 0x%02x for %q", typ, name)
		}
		out[name] = EventStreamHeader{Type: typ, Value: value}
	}
	return out, nil
}
