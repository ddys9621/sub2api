//go:build unit

package kiro

import (
	"bytes"
	"reflect"
	"testing"
)

func TestCBORRoundTripSimple(t *testing.T) {
	cases := []any{
		nil,
		true,
		false,
		int64(0),
		int64(1),
		int64(23),
		int64(24),
		int64(255),
		int64(256),
		int64(1<<31 - 1),
		int64(1 << 32),
		int64(-1),
		int64(-24),
		int64(-25),
		int64(-256),
		"",
		"hello",
		"长字符串测试",
		[]byte{1, 2, 3, 4},
		[]any{int64(1), "two", true},
		map[string]any{
			"code":         "abc",
			"codeVerifier": "def",
			"num":          int64(42),
		},
	}

	for _, want := range cases {
		enc, err := EncodeCBOR(want)
		if err != nil {
			t.Fatalf("EncodeCBOR(%v) error: %v", want, err)
		}
		got, err := DecodeCBOR(enc)
		if err != nil {
			t.Fatalf("DecodeCBOR(%x) error: %v", enc, err)
		}
		// byte slice equality
		if wantBytes, ok := want.([]byte); ok {
			if gotBytes, ok := got.([]byte); !ok || !bytes.Equal(wantBytes, gotBytes) {
				t.Errorf("roundtrip []byte: want %v got %v", wantBytes, got)
			}
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("roundtrip: want %#v got %#v", want, got)
		}
	}
}

func TestCBORDecodeKnownVector(t *testing.T) {
	// From RFC 8949 Appendix A: encoding of {"a":1,"b":[2,3]}
	raw := []byte{0xa2, 0x61, 0x61, 0x01, 0x61, 0x62, 0x82, 0x02, 0x03}
	got, err := DecodeCBOR(raw)
	if err != nil {
		t.Fatalf("DecodeCBOR err: %v", err)
	}
	want := map[string]any{
		"a": int64(1),
		"b": []any{int64(2), int64(3)},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("known vector: got %#v want %#v", got, want)
	}
}

func TestAsHelpers(t *testing.T) {
	m := map[string]any{"s": "x", "n": int64(42)}
	if AsString(m, "s") != "x" {
		t.Errorf("AsString")
	}
	if AsString(m, "missing") != "" {
		t.Errorf("AsString missing")
	}
	if AsInt64(m, "n") != 42 {
		t.Errorf("AsInt64")
	}
	if AsInt64(m, "missing") != 0 {
		t.Errorf("AsInt64 missing")
	}
}
