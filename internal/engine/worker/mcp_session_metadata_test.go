package worker

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/mcpsession"
	"github.com/google/uuid"
)

// TestDecodeMCPSessionInitializedRetainsProvenance admits the SSE metadata transition through the existing worker.
func TestDecodeMCPSessionInitializedRetainsProvenance(t *testing.T) {
	data := mcpSessionEventData{AppID: uuid.NewString(), SessionID: "synthetic-session", Type: "initialized", Timestamp: time.Now().UTC(), Metadata: mcpsession.Metadata{ClientName: "Example Agent", ClientVersion: "1", InitialClientIP: "192.0.2.3"}}
	raw, _ := json.Marshal(data)
	session, err := decodeMCPSession(raw, data.Timestamp.Add(time.Minute))
	// Initialization enriches one still-open session using the producer's timestamp.
	if err != nil || session.EndedAt != nil || !session.StartedAt.Equal(data.Timestamp) {
		t.Fatalf("initialized session = %#v, err %v", session, err)
	}
	// Display provenance survives the canonical worker without acquiring execution identity semantics.
	if session.ClientName != data.ClientName || session.ClientVersion != data.ClientVersion || session.InitialClientIP != data.InitialClientIP {
		t.Fatal("metadata was not preserved")
	}
}

// TestDecodeMCPSessionRejectsInvalidProvenance makes poison metadata terminal rather than endlessly retryable.
func TestDecodeMCPSessionRejectsInvalidProvenance(t *testing.T) {
	for _, metadata := range []mcpsession.Metadata{{ClientName: strings.Repeat("x", 129)}, {ClientVersion: "bad\nvalue"}, {InitialClientIP: "invalid"}} {
		data := mcpSessionEventData{AppID: uuid.NewString(), SessionID: "synthetic-session", Type: "initialized", Metadata: metadata}
		raw, _ := json.Marshal(data)
		// The worker must enforce bounds independently of the HTTP producer.
		if _, err := decodeMCPSession(raw, time.Now()); err == nil {
			t.Fatal("invalid metadata accepted")
		}
	}
}
