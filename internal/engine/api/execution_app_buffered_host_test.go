package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
)

// TestBufferedCapabilityHostDefersDBWrites verifies local read-after-write and one final document.
func TestBufferedCapabilityHostDefersDBWrites(t *testing.T) {
	ctx := context.Background()
	base := &replayHostFixture{data: json.RawMessage(`{"stale":true}`)}
	buffer := NewBufferedCapabilityHost(base)
	initial, err := buffer.DBGet(ctx)
	if err != nil || string(initial) != "null" {
		t.Fatalf("expected fresh execution document, got %q error=%v", initial, err)
	}
	if err := buffer.DBSet(ctx, json.RawMessage(`{"customerId":"cus_123"}`)); err != nil {
		t.Fatalf("buffer data: %v", err)
	}
	read, err := buffer.DBGet(ctx)
	if err != nil || string(read) != `{"customerId":"cus_123"}` {
		t.Fatalf("expected local read-after-write, got %q error=%v", read, err)
	}
	if base.dbReads != 0 || base.dbWrites != 0 || string(base.data) != `{"stale":true}` {
		t.Fatalf("buffer touched durable base DB: %#v", base)
	}
	if string(buffer.Data()) != string(read) {
		t.Fatalf("terminal data differs from buffered document: %q", buffer.Data())
	}
}

// TestBufferedCapabilityHostEnforcesDocumentLimit rejects an oversized write before terminal commit.
func TestBufferedCapabilityHostEnforcesDocumentLimit(t *testing.T) {
	buffer := NewBufferedCapabilityHost(&replayHostFixture{})
	value := json.RawMessage(`{"value":"` + strings.Repeat("a", store.MaxExecutionDataBytes) + `"}`)
	if err := buffer.DBSet(context.Background(), value); !errors.Is(err, store.ErrExecutionResultDataTooLarge) {
		t.Fatalf("expected size rejection, got %v", err)
	}
	if string(buffer.Data()) != "null" {
		t.Fatalf("rejected write changed buffered document: %s", buffer.Data())
	}
}
