package engine

import (
	"bytes"
	"errors"
	"net/http"
	"testing"
)

func TestDeferredResponseStreamBoundsDiscardableAttemptBody(t *testing.T) {
	inner := &mockStream{}
	stream := newDeferredResponseStream(inner, func(int) bool { return true })
	if err := stream.SendResponseContract(http.StatusServiceUnavailable, "json"); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(bytes.Repeat([]byte("x"), maxDeferredResponseBytes+1)); !errors.Is(err, errDeferredResponseTooLarge) {
		t.Fatalf("oversized deferred body error=%v", err)
	}
	if err := stream.Commit(); !errors.Is(err, errDeferredResponseTooLarge) {
		t.Fatalf("oversized deferred commit error=%v", err)
	}
	if len(inner.contracts) != 0 || len(inner.chunks) != 0 {
		t.Fatalf("oversized discarded attempt escaped: contracts=%#v chunks=%d", inner.contracts, len(inner.chunks))
	}
}

func TestDeferredResponseStreamDoesNotCapFinalAttempt(t *testing.T) {
	inner := &mockStream{}
	stream := newDeferredResponseStream(inner, func(int) bool { return false })
	payload := bytes.Repeat([]byte("x"), maxDeferredResponseBytes+1)
	if err := stream.SendResponseContract(http.StatusServiceUnavailable, "json"); err != nil {
		t.Fatal(err)
	}
	if err := stream.Send(payload); err != nil {
		t.Fatal(err)
	}
	if len(inner.contracts) != 1 || !bytes.Equal(bytes.Join(inner.chunks, nil), payload) {
		t.Fatal("final attempt did not stream directly")
	}
}

// TestBoundedBufferStreamAcceptsExactBudget proves the hard limit is inclusive
// and preserves chunked response assembly below that boundary.
func TestBoundedBufferStreamAcceptsExactBudget(t *testing.T) {
	stream := NewBoundedBufferStream(6)
	// Multiple chunks prove the budget applies to the aggregate rather than each write.
	for _, chunk := range [][]byte{[]byte("abc"), []byte("def")} {
		// Every admitted chunk must remain observable in its original order.
		if err := stream.Send(chunk); err != nil {
			t.Fatalf("Send() error = %v", err)
		}
	}
	// The exact boundary must not be rejected as an off-by-one overflow.
	if got := stream.String(); got != "abcdef" {
		t.Fatalf("String() = %q, want %q", got, "abcdef")
	}
}

// TestBoundedBufferStreamRejectsOverflowWithoutPartialResult proves an
// oversized provider result cannot be consumed or resumed after rejection.
func TestBoundedBufferStreamRejectsOverflowWithoutPartialResult(t *testing.T) {
	stream := NewBoundedBufferStream(5)
	// The first chunk establishes that overflow also clears already-buffered output.
	if err := stream.Send([]byte("1234")); err != nil {
		t.Fatalf("first Send() error = %v", err)
	}
	// The second chunk crosses the aggregate budget even though each chunk fits alone.
	if err := stream.Send([]byte("56")); !errors.Is(err, ErrBufferStreamLimitExceeded) {
		t.Fatalf("overflow Send() error = %v, want ErrBufferStreamLimitExceeded", err)
	}
	// No partial provider document or backing allocation may survive a hard-limit failure.
	if got := stream.Bytes(); len(got) != 0 || stream.buf.Cap() != 0 {
		t.Fatalf("buffer retained len/cap %d/%d after overflow", len(got), stream.buf.Cap())
	}
	// The failure stays sticky so a later small chunk cannot manufacture a valid-looking suffix.
	if err := stream.Send([]byte("1")); !errors.Is(err, ErrBufferStreamLimitExceeded) {
		t.Fatalf("Send() after overflow error = %v, want ErrBufferStreamLimitExceeded", err)
	}
}

// TestBoundedBufferStreamTreatsNegativeBudgetAsZero ensures invalid policy
// configuration fails closed instead of silently selecting the unbounded mode.
func TestBoundedBufferStreamTreatsNegativeBudgetAsZero(t *testing.T) {
	stream := NewBoundedBufferStream(-1)
	// Any nonempty result must exceed the normalized zero-byte budget.
	if err := stream.Send([]byte("x")); !errors.Is(err, ErrBufferStreamLimitExceeded) {
		t.Fatalf("Send() error = %v, want ErrBufferStreamLimitExceeded", err)
	}
}
