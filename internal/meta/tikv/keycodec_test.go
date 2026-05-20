package tikv

import (
	"bytes"
	"testing"

	"github.com/google/uuid"
)

func TestPackKeyEmptySegments(t *testing.T) {
	got := PackKey("p/")
	if string(got) != "p/" {
		t.Fatalf("empty segments: got %q want %q", got, "p/")
	}
}

func TestPackKeyStringSegments(t *testing.T) {
	got := PackKey("p/", "a", "b")
	want := append([]byte("p/"), 'a', 0x00, 0x00, 'b', 0x00, 0x00)
	if !bytes.Equal(got, want) {
		t.Fatalf("string segments: got %x want %x", got, want)
	}
}

func TestPackKeyStuffsNullBytes(t *testing.T) {
	got := PackKey("p/", "a\x00b")
	want := append([]byte("p/"), 'a', 0x00, 0xFF, 'b', 0x00, 0x00)
	if !bytes.Equal(got, want) {
		t.Fatalf("stuffed null: got %x want %x", got, want)
	}
	// Round-trip via readEscaped (keys.go) so we know the inverse works.
	decoded, rest, err := readEscaped(got[len("p/"):])
	if err != nil {
		t.Fatalf("readEscaped: %v", err)
	}
	if decoded != "a\x00b" {
		t.Fatalf("decoded: %q", decoded)
	}
	if len(rest) != 0 {
		t.Fatalf("trailing bytes: %x", rest)
	}
}

func TestPackKeyBytesSegment(t *testing.T) {
	got := PackKey("p/", []byte("xy"))
	want := append([]byte("p/"), 'x', 'y', 0x00, 0x00)
	if !bytes.Equal(got, want) {
		t.Fatalf("[]byte segment: got %x want %x", got, want)
	}
}

func TestPackKeyUUIDSegment(t *testing.T) {
	id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	got := PackKey("p/", id)
	want := append([]byte("p/"), id[:]...)
	if !bytes.Equal(got, want) {
		t.Fatalf("uuid segment: got %x want %x", got, want)
	}
}

func TestPackKeyMixedSegments(t *testing.T) {
	id := uuid.MustParse("01234567-89ab-cdef-0123-456789abcdef")
	got := PackKey("p/", "head", id, "tail")
	want := append([]byte("p/"), 'h', 'e', 'a', 'd', 0x00, 0x00)
	want = append(want, id[:]...)
	want = append(want, 't', 'a', 'i', 'l', 0x00, 0x00)
	if !bytes.Equal(got, want) {
		t.Fatalf("mixed: got %x want %x", got, want)
	}
}

func TestPackKeyPanicsOnUnsupportedType(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatalf("expected panic on int segment")
		}
	}()
	_ = PackKey("p/", 42)
}

func TestMarshalUnmarshalBlobRoundTrip(t *testing.T) {
	type payload struct {
		A string            `json:"a"`
		B map[string]string `json:"b,omitempty"`
	}
	in := payload{A: "x", B: map[string]string{"k": "v"}}
	raw, err := MarshalBlob(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out payload
	if err := UnmarshalBlob(raw, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if out.A != "x" || out.B["k"] != "v" {
		t.Fatalf("round-trip: %+v", out)
	}
}

func TestUnmarshalBlobEmptyIsNoOp(t *testing.T) {
	var out map[string]string
	if err := UnmarshalBlob(nil, &out); err != nil {
		t.Fatalf("nil input: %v", err)
	}
	if out != nil {
		t.Fatalf("nil input mutated dest: %v", out)
	}
	if err := UnmarshalBlob([]byte{}, &out); err != nil {
		t.Fatalf("empty input: %v", err)
	}
}
