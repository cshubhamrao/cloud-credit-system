package compress_test

import (
	"bytes"
	"testing"

	"github.com/cshubhamrao/cloud-credit-system/internal/compress"
)

func TestName(t *testing.T) {
	if compress.Name != "zstd" {
		t.Errorf("Name = %q, want %q", compress.Name, "zstd")
	}
}

func TestNewCompressor_NotNil(t *testing.T) {
	enc := compress.NewCompressor()
	if enc == nil {
		t.Fatal("NewCompressor returned nil")
	}
}

func TestNewDecompressor_NotNil(t *testing.T) {
	dec := compress.NewDecompressor()
	if dec == nil {
		t.Fatal("NewDecompressor returned nil")
	}
}

func TestDecompressor_CloseReturnsNil(t *testing.T) {
	dec := compress.NewDecompressor()
	if err := dec.Close(); err != nil {
		t.Errorf("Close() = %v, want nil", err)
	}
}

func TestDecompressor_CloseMultipleTimes(t *testing.T) {
	dec := compress.NewDecompressor()
	// Must not panic on multiple Close calls
	if err := dec.Close(); err != nil {
		t.Errorf("first Close() = %v, want nil", err)
	}
	if err := dec.Close(); err != nil {
		t.Errorf("second Close() = %v, want nil", err)
	}
}

func TestRoundTrip(t *testing.T) {
	src := []byte("hello from the cloud credit system — testing zstd round-trip compression")

	enc := compress.NewCompressor()
	compressed := enc.EncodeAll(src, nil)

	if len(compressed) == 0 {
		t.Fatal("EncodeAll returned empty output")
	}

	dec := compress.NewDecompressor()
	got, err := dec.DecodeAll(compressed, nil)
	if err != nil {
		t.Fatalf("DecodeAll error: %v", err)
	}

	if !bytes.Equal(got, src) {
		t.Errorf("round-trip mismatch: got %q, want %q", got, src)
	}
}

func TestRoundTrip_EmptyInput(t *testing.T) {
	enc := compress.NewCompressor()
	compressed := enc.EncodeAll([]byte{}, nil)

	dec := compress.NewDecompressor()
	got, err := dec.DecodeAll(compressed, nil)
	if err != nil {
		t.Fatalf("DecodeAll error on empty input: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty output, got %q", got)
	}
}

func TestRoundTrip_LargerPayload(t *testing.T) {
	// Simulate a realistic protobuf-sized payload
	src := bytes.Repeat([]byte("quota heartbeat data "), 100)

	enc := compress.NewCompressor()
	compressed := enc.EncodeAll(src, nil)

	dec := compress.NewDecompressor()
	got, err := dec.DecodeAll(compressed, nil)
	if err != nil {
		t.Fatalf("DecodeAll error: %v", err)
	}
	if !bytes.Equal(got, src) {
		t.Error("round-trip mismatch for larger payload")
	}
}
