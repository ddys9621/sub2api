//go:build unit

package kiro

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"io"
	"testing"
)

// buildEventStreamFrame constructs a valid EventStream frame with the given
// headers (simple string-valued) and JSON payload, for round-trip tests.
func buildEventStreamFrame(headers map[string]string, payload []byte) []byte {
	// Headers
	var hbuf bytes.Buffer
	for name, value := range headers {
		hbuf.WriteByte(byte(len(name)))
		hbuf.WriteString(name)
		hbuf.WriteByte(0x07) // string
		var lb [2]byte
		binary.BigEndian.PutUint16(lb[:], uint16(len(value)))
		hbuf.Write(lb[:])
		hbuf.WriteString(value)
	}

	hraw := hbuf.Bytes()
	totalLen := uint32(12 + len(hraw) + len(payload) + 4)
	headersLen := uint32(len(hraw))

	// Prelude (8 bytes) + prelude CRC (4 bytes)
	var prelude [8]byte
	binary.BigEndian.PutUint32(prelude[0:4], totalLen)
	binary.BigEndian.PutUint32(prelude[4:8], headersLen)
	preludeCRC := crc32.ChecksumIEEE(prelude[:])

	var out bytes.Buffer
	out.Write(prelude[:])
	binary.Write(&out, binary.BigEndian, preludeCRC)
	out.Write(hraw)
	out.Write(payload)

	// Message CRC over everything except the trailing 4 bytes.
	msgCRC := crc32.ChecksumIEEE(out.Bytes())
	binary.Write(&out, binary.BigEndian, msgCRC)
	return out.Bytes()
}

func TestEventStreamReader_DecodeSingleFrame(t *testing.T) {
	payload := []byte(`{"hello":"world"}`)
	frame := buildEventStreamFrame(map[string]string{
		":message-type": "event",
		":event-type":   "assistantResponseEvent",
		":content-type": "application/json",
	}, payload)

	r := NewEventStreamReader(bytes.NewReader(frame))
	msg, err := r.Next()
	if err != nil {
		t.Fatalf("Next error: %v", err)
	}
	if msg.EventType() != "assistantResponseEvent" {
		t.Errorf(":event-type = %q", msg.EventType())
	}
	if msg.MessageType() != "event" {
		t.Errorf(":message-type = %q", msg.MessageType())
	}
	if !bytes.Equal(msg.Payload, payload) {
		t.Errorf("payload mismatch: got %q", msg.Payload)
	}
	if _, err := r.Next(); err != io.EOF {
		t.Errorf("expected io.EOF after last frame, got %v", err)
	}
}

func TestEventStreamReader_MultipleFrames(t *testing.T) {
	var buf bytes.Buffer
	for i, text := range []string{"alpha", "beta", "gamma"} {
		buf.Write(buildEventStreamFrame(map[string]string{
			":event-type": "assistantResponseEvent",
		}, []byte("payload-"+text)))
		_ = i
	}

	r := NewEventStreamReader(&buf)
	got := 0
	for {
		_, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("frame %d: %v", got, err)
		}
		got++
	}
	if got != 3 {
		t.Errorf("read %d frames, want 3", got)
	}
}
