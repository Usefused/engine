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
