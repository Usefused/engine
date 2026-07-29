package engine

import "testing"

// TestBufferStream_Accumulates verifies the shared buffering ResponseStream
// accumulates chunks in order and exposes them as bytes and string.
func TestBufferStream_Accumulates(t *testing.T) {
	b := NewBufferStream()

	if b.String() != "" {
		t.Errorf("expected empty buffer, got %q", b.String())
	}

	if err := b.Send([]byte("hello ")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := b.Send([]byte("world")); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := b.String(); got != "hello world" {
		t.Errorf("expected 'hello world', got %q", got)
	}
	if got := string(b.Bytes()); got != "hello world" {
		t.Errorf("Bytes() mismatch, got %q", got)
	}
}

// TestBufferStream_SatisfiesResponseStream is a compile-time assertion that
// BufferStream implements the ResponseStream interface used by the dispatcher.
func TestBufferStream_SatisfiesResponseStream(t *testing.T) {
	var _ ResponseStream = NewBufferStream()
}
