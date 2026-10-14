package cmd

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/google/uuid"
)

// TestSecretRevealAuditWithholdsPlaintext checks final-audit failure, overflow, denial, and canonical identity attribution.
func TestSecretRevealAuditWithholdsPlaintext(t *testing.T) {
	bucketID, secretID := uuid.New(), uuid.New()
	for _, test := range []struct {
		name                   string
		permission             accesscontrol.Permission
		failFrom, size, status int
	}{
		{"allowed", accesscontrol.PermissionCredentialsReveal, 0, 30, 200},
		{"preflight unavailable", accesscontrol.PermissionCredentialsReveal, 1, 30, 503},
		{"final unavailable", accesscontrol.PermissionCredentialsReveal, 2, 30, 503},
		{"response too large", accesscontrol.PermissionCredentialsReveal, 0, maxAuditResponseBytes + 1, 503},
		{"manage is not reveal", accesscontrol.PermissionCredentialsManage, 0, 30, 403},
	} {
		// Each case gets isolated audit state so a previous allowed read cannot mask missing records.
		t.Run(test.name, func(t *testing.T) {
			actor := actorWithGrants(t, uuid.New(), accesscontrol.Grant{Permission: test.permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}})
			recorder := &controlAuditRecorderStub{failFromCall: test.failFrom}
			handler := controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(
				// The synthetic value must remain buffered even if the handler attempts to flush.
				func(w http.ResponseWriter, _ *http.Request) {
					w.Header().Set("Cache-Control", "no-store")
					_, _ = w.Write([]byte(strings.Repeat("x", test.size)))
					w.(http.Flusher).Flush()
				}))
			response := httptest.NewRecorder()
			handler.ServeHTTP(response, requestWithActor(t, "POST", "/workspace/buckets/"+bucketID.String()+"/secrets/"+secretID.String()+"/reveal", actor))
			// Failed authorization or audit must suppress every byte of the synthetic secret.
			if response.Code != test.status || test.status != 200 && strings.Contains(response.Body.String(), strings.Repeat("x", 30)) {
				t.Fatalf("status=%d", response.Code)
			}
			assertSecretAuditMetadata(t, recorder, secretID)
			// A successful disclosure requires both durable attempt and final outcome records.
			if test.status == 200 && (len(recorder.events) != 2 || recorder.events[1].Outcome != accesscontrol.AuditSucceeded) {
				t.Fatal("missing successful reveal audit")
			}
		})
	}
}

// assertSecretAuditMetadata verifies opaque row attribution without persisting any response material.
func assertSecretAuditMetadata(t *testing.T, recorder *controlAuditRecorderStub, secretID uuid.UUID) {
	t.Helper()
	for _, event := range recorder.events {
		encoded, _ := json.Marshal(event)
		// Audit records identify the requested row, never the disclosed material or response envelope.
		if event.Metadata["secret_id"] != secretID.String() || strings.Contains(string(encoded), strings.Repeat("x", 30)) {
			t.Fatal("invalid reveal audit metadata")
		}
	}
}

// TestSecretExportPolicyAuditRecordsBoolean verifies policy changes reuse the canonical audit without request bodies.
func TestSecretExportPolicyAuditRecordsBoolean(t *testing.T) {
	bucketID, secretID := uuid.New(), uuid.New()
	actor := actorWithGrants(t, uuid.New(), accesscontrol.Grant{Permission: accesscontrol.PermissionCredentialsManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: bucketID}})
	recorder := &controlAuditRecorderStub{}
	handler := controlAuthorizationMiddlewareWithAudit(accesscontrol.SnapshotAuthorizer{}, nil, recorder)(http.HandlerFunc(
		// Simulate a committed boolean-only policy update at the existing mutation boundary.
		func(w http.ResponseWriter, r *http.Request) {
			accesscontrol.MarkSecretExportPolicy(r.Context(), true)
			w.WriteHeader(204)
		}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, requestWithActor(t, "PUT", "/workspace/buckets/"+bucketID.String()+"/secrets/"+secretID.String()+"/exportable", actor))
	// Both attempts and final outcomes remain on the same stream, with the result carrying only fixed policy evidence.
	if response.Code != 204 || len(recorder.events) != 2 || recorder.events[1].Metadata["exportable"] != true {
		t.Fatal("missing policy audit evidence")
	}
	assertSecretAuditMetadata(t, recorder, secretID)
}
