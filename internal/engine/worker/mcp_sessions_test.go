package worker

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestDecodeMCPSessionRetainsTokenProtocolAndEndReason(t *testing.T) {
	appID, tokenID, occurredAt := uuid.New(), uuid.New(), time.Now().UTC()
	producerAt := occurredAt.Add(-time.Minute)
	lastActivityAt := producerAt.Add(-time.Second)
	payload := []byte(`{
		"app_id":"` + appID.String() + `","app_token_id":"` + tokenID.String() + `",
		"session_id":"session-1","protocol_version":"2025-06-18",
		"type":"ended","end_reason":"client_terminated","timestamp":"` + producerAt.Format(time.RFC3339Nano) + `",
		"last_activity_at":"` + lastActivityAt.Format(time.RFC3339Nano) + `"
	}`)

	session, err := decodeMCPSession(payload, occurredAt)

	if err != nil {
		t.Fatalf("decode session: %v", err)
	}
	if session.AppID != appID || session.AppTokenID != tokenID || session.ProtocolVersion != "2025-06-18" {
		t.Fatalf("session identity = %#v, want app/token/protocol attribution", session)
	}
	if session.EndedAt == nil || session.EndReason != "client_terminated" || !session.LastActivityAt.Equal(lastActivityAt) {
		t.Fatalf("session termination = %#v, want timestamp/reason/activity", session)
	}
}

func TestDecodeMCPSessionSupportsLegacyEventWithoutTokenID(t *testing.T) {
	payload := []byte(`{"app_id":"` + uuid.NewString() + `","session_id":"legacy","type":"started"}`)

	session, err := decodeMCPSession(payload, time.Now().UTC())

	if err != nil || session.AppTokenID != uuid.Nil || session.ProtocolVersion != "2024-11-05" {
		t.Fatalf("legacy session = %#v/%v, want empty token and legacy protocol", session, err)
	}
}

func TestDecodeMCPSessionRejectsAmbiguousEvents(t *testing.T) {
	appID := uuid.NewString()
	tests := [][]byte{
		[]byte(`{"app_id":"` + appID + `","session_id":"one","type":"unknown"}`),
		[]byte(`{"app_id":"` + appID + `","session_id":"one","type":"ended"}`),
		[]byte(`{"app_id":"` + appID + `","session_id":"one","type":"started","end_reason":"idle_timeout"}`),
		[]byte(`{"app_id":"` + appID + `","session_id":"one","type":"started","extra":true}`),
		[]byte(`{"app_id":"` + appID + `","session_id":"one","type":"started"} {}`),
		[]byte(`{"app_id":"` + appID + `","app_token_id":"not-a-uuid","session_id":"one","type":"started"}`),
		[]byte(`{"app_id":"` + appID + `","session_id":"one","type":"activity"}`),
		[]byte(`{"app_id":"` + appID + `","session_id":"one","type":"started","protocol_version":"invalid version"}`),
		[]byte(`{"app_id":"` + appID + `","session_id":"one","type":"started","timestamp":"2026-08-22T10:00:00Z","last_activity_at":"2026-08-22T10:00:01Z"}`),
	}
	for _, payload := range tests {
		if _, err := decodeMCPSession(payload, time.Now().UTC()); err == nil {
			t.Fatalf("accepted invalid event: %s", payload)
		}
	}
}

func TestDecodeMCPSessionAcceptsToolCallTimeout(t *testing.T) {
	payload := []byte(`{"app_id":"` + uuid.NewString() + `","session_id":"timed-out","type":"ended","end_reason":"tool_call_timeout"}`)
	session, err := decodeMCPSession(payload, time.Now().UTC())
	if err != nil || session.EndReason != "tool_call_timeout" {
		t.Fatalf("timeout event = %#v/%v", session, err)
	}
}
