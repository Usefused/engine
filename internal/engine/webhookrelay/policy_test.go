package webhookrelay

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/testcontract"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestRelayRejectsAmbiguousProofSelectors keeps routing paths from becoming wildcard, fallback or coerced claims.
func TestRelayRejectsAmbiguousProofSelectors(t *testing.T) {
	policy := Routing{AuthName: "oauth", TokenResourcePath: "team.id", TokenAppPath: "app_id", EventResourcePath: "team_id", EventAppPath: "api_app_id", EventIDPath: "event_id"}
	require.NoError(t, policy.Validate())
	// Literal paths alone may select provider-owned identity claims.
	for _, path := range []string{"", "team.*", "team.#", "team.id|other", "team[0].id", "$.team"} {
		copy := policy
		copy.EventResourcePath = path
		require.Error(t, copy.Validate())
	}
	// Non-string claims must not acquire identity through implicit JSON coercion.
	for _, raw := range []string{`{}`, `{"id":null}`, `{"id":true}`, `{"id":123}`, `{"id":[]}`, `{"id":""}`} {
		_, err := StringClaim([]byte(raw), "id")
		require.Error(t, err)
	}
	_, err := Decode([]byte(`{"source":{"bucket":"test","connection_id":"` + uuid.NewString() + `","registration_id":"` + uuid.NewString() + `","team_id":"forged"}}`))
	require.Error(t, err)
}

// TestRelayRequiresFreshBodyAuthentication rejects policy shapes which cannot attest to complete resource ownership.
func TestRelayRequiresFreshBodyAuthentication(t *testing.T) {
	policy := testcontract.AuthenticatedSignature()
	require.NoError(t, ValidateExportVerification(&policy))
	policy.Rules[1].Verification.Signature.Components = nil
	require.Error(t, ValidateExportVerification(&policy))
	policy = testcontract.AuthenticatedSignature()
	policy.Rules[0].Verification.Signature.Timestamp = nil
	require.Error(t, ValidateExportVerification(&policy))
	require.Error(t, ValidateExportVerification(nil))
}

// TestRelayReceiptsAreAudienceBound prevents a valid receiver from acknowledging a sibling's stream or arbitrary NATS subject.
func TestRelayReceiptsAreAudienceBound(t *testing.T) {
	broker := Broker{Key: bytes.Repeat([]byte{1}, 32)}
	id := uuid.New()
	reply := "$JS.ACK.WEBHOOKS.remote-" + id.String() + ".1.2.3.4.5"
	receipt := broker.signReceipt(id, reply)
	got, err := broker.receiptReply(id, receipt)
	require.NoError(t, err)
	require.Equal(t, reply, got)
	_, err = broker.receiptReply(uuid.New(), receipt)
	require.Error(t, err)
	_, err = broker.receiptReply(id, receipt+"tampered")
	require.Error(t, err)
	_, err = broker.receiptReply(id, broker.signReceipt(id, "arbitrary.subject"))
	require.Error(t, err)
}

// TestDelegatedPayloadWithholdsTransportCredentials proves remote envelopes carry event data without provider signature or callback metadata.
func TestDelegatedPayloadWithholdsTransportCredentials(t *testing.T) {
	raw := []byte(`{"body":{"type":"event"},"headers":{"Authorization":"private","X-Signature":"signed"},"query":{"secret":"private"},"path":{"urlSlug":"private"}}`)
	out := delegatedPayload(raw, "event")
	require.True(t, json.Valid(out))
	require.NotContains(t, string(out), "private")
	require.NotContains(t, string(out), "signed")
	require.Contains(t, string(out), `"type":"event"`)
}

type accessFixture struct{}

// AccessToken supplies only a non-sensitive sentinel used to verify redirect isolation.
func (accessFixture) AccessToken(context.Context) (string, error) { return "sentinel", nil }

// TestRelayClientNeverForwardsCredentialsThroughRedirect ensures even a same-host redirect cannot move broker bearer or provider tokens.
func TestRelayClientNeverForwardsCredentialsThroughRedirect(t *testing.T) {
	called := false
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { called = true; w.WriteHeader(204) }))
	t.Cleanup(destination.Close)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, destination.URL, 307) }))
	t.Cleanup(origin.Close)
	client, err := NewClient(origin.URL, accessFixture{}, nil)
	require.NoError(t, err)
	_, err = client.Subscribe(t.Context(), uuid.New(), uuid.New(), "provider-sentinel")
	require.Error(t, err)
	require.False(t, called)
	_, err = NewClient("http://example.com", accessFixture{}, nil)
	require.Error(t, err)
	_, err = NewClient("https://user:password@example.com", accessFixture{}, nil)
	require.Error(t, err)
	require.False(t, strings.Contains(err.Error(), "password"))
}
