package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
)

// importPreflightFixtureResponse builds one proof from the same candidate bytes
// Registry embeds, keeping hash-verification tests independent of providers.
func importPreflightFixtureResponse(t *testing.T, mutate func(*importPreflightResponse)) []byte {
	t.Helper()
	item, version := recoveryContractFixture()
	return importPreflightFixtureResponseForContract(t, item, version.ServiceVersionID.String(), mutate)
}

// importPreflightFixtureResponseForContract builds proof bytes for a specific
// runtime contract so feature-shaped fixtures still exercise the production decoder.
func importPreflightFixtureResponseForContract(t *testing.T, item runtimeContractBatchItem, operationID string, mutate func(*importPreflightResponse)) []byte {
	t.Helper()
	contract, err := json.Marshal(item)
	if err != nil {
		t.Fatalf("marshal candidate: %v", err)
	}
	digest := sha256.Sum256(contract)
	response := importPreflightResponse{
		OperationID: operationID, Phase: "engine_preflight", CommitState: "not_committed",
		ContractHash: "sha256:" + hex.EncodeToString(digest[:]), Contract: contract,
	}
	// Targeted mutations let each test break one proof invariant without rebuilding the fixture.
	if mutate != nil {
		mutate(&response)
	}
	payload, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal preflight response: %v", err)
	}
	return payload
}

// TestDecodeImportPreflightResponseAcceptsOptionalPaginationCollections proves
// Gmail-style omitted empty arrays survive strict decoding and runtime admission.
func TestDecodeImportPreflightResponseAcceptsOptionalPaginationCollections(t *testing.T) {
	item, version := recoveryContractFixture()
	item.RequiredCapabilities = []string{
		fusedobject.ExecutionCapabilityPaginationComposableV3,
		fusedobject.ExecutionCapabilityPaginationOptionalItemsV1,
	}
	item.Service.Pagination = &fusedobject.PaginationConfig{
		Version: paginationpolicy.Version,
		Request: []paginationpolicy.RequestStep{{
			State: "cursor", Target: paginationpolicy.RequestTarget{Location: paginationpolicy.RequestQuery, Name: "pageToken"},
			ValueType: paginationpolicy.ValueString, Apply: paginationpolicy.ApplySubsequent,
		}},
		Response: paginationpolicy.ResponsePlan{
			Items: paginationpolicy.ItemsSource{Path: "$.messages", MissingIsEmpty: true},
			Values: []paginationpolicy.ResponseValue{{
				Name: "next_cursor", Source: paginationpolicy.ValueSource{Location: paginationpolicy.SourceBody, Path: "$.nextPageToken", ValueType: paginationpolicy.ValueString},
			}},
		},
		Continuation: []paginationpolicy.ContinuationStep{{Kind: paginationpolicy.ContinuationToken, State: "cursor", ResponseValue: "next_cursor"}},
		Termination: paginationpolicy.Termination{
			StopOnEmptyItems: true, StopOnMissingValues: []string{"next_cursor"}, RepeatedValue: paginationpolicy.RepeatedError,
		},
		Limits: paginationpolicy.Limits{MaxPages: 100, MaxItems: 10_000, MaxBytes: 16_777_216, MaxDurationMs: 120_000},
	}

	result, err := decodeImportPreflightResponse(bytes.NewReader(importPreflightFixtureResponseForContract(t, item, version.ServiceVersionID.String(), nil)))
	// The optional collection capability must be admitted before Registry is allowed to publish the reviewed Gmail contract.
	if err != nil {
		t.Fatalf("decode optional pagination preflight: %v", err)
	}
	// Preserving the normalization flag prevents valid provider responses with omitted empty arrays from failing at execution time.
	if !result.Snapshot.ServiceMetadata.Pagination.Response.Items.MissingIsEmpty {
		t.Fatal("optional pagination collection was discarded")
	}
}

// addUnknownImportPreflightContractField rehashes a candidate containing an
// unsupported field so the test reaches strict runtime decoding, not hash rejection.
func addUnknownImportPreflightContractField(t *testing.T, response *importPreflightResponse) {
	t.Helper()
	var contract map[string]json.RawMessage
	// The shared fixture must remain a JSON object before it can be extended.
	if err := json.Unmarshal(response.Contract, &contract); err != nil {
		t.Fatal(err)
	}
	contract["catalog"] = json.RawMessage(`{"unsupported":true}`)
	payload, err := json.Marshal(contract)
	// A test-only raw field must still produce one canonical candidate document.
	if err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(payload)
	response.Contract = payload
	response.ContractHash = "sha256:" + hex.EncodeToString(digest[:])
}

// TestDecodeImportPreflightResponseRunsOrdinaryRuntimeAdmission proves a valid
// candidate receives the same computed Engine hash as normal snapshot fetches.
func TestDecodeImportPreflightResponseRunsOrdinaryRuntimeAdmission(t *testing.T) {
	result, err := decodeImportPreflightResponse(bytes.NewReader(importPreflightFixtureResponse(t, nil)))
	if err != nil {
		t.Fatalf("decodeImportPreflightResponse: %v", err)
	}
	// The Registry proof and Engine runtime hash have separate owners; both must be present.
	if result.OperationID.String() == "" || result.ContractHash == "" || result.Snapshot.ContractHash == "" {
		t.Fatalf("incomplete admitted preflight: %#v", result)
	}
}

// TestDecodeImportPreflightResponseRejectsProofAndTransportDefects ensures no
// malformed candidate can reach the Registry apply forwarding boundary.
func TestDecodeImportPreflightResponseRejectsProofAndTransportDefects(t *testing.T) {
	tests := []struct {
		name   string
		body   func(*testing.T) []byte
		needle string
	}{
		{name: "hash mismatch", body: func(t *testing.T) []byte {
			return importPreflightFixtureResponse(t, func(response *importPreflightResponse) {
				response.ContractHash = "sha256:" + strings.Repeat("0", 64)
			})
		}, needle: "hash does not match"},
		{name: "invalid phase", body: func(t *testing.T) []byte {
			return importPreflightFixtureResponse(t, func(response *importPreflightResponse) { response.Phase = "complete" })
		}, needle: "phase or commit state"},
		{name: "unknown field", body: func(t *testing.T) []byte {
			payload := importPreflightFixtureResponse(t, nil)
			return append(payload[:len(payload)-1], []byte(`,"legacy_contract":{}}`)...)
		}, needle: "invalid import preflight response"},
		{name: "unknown contract field", body: func(t *testing.T) []byte {
			return importPreflightFixtureResponse(t, func(response *importPreflightResponse) {
				addUnknownImportPreflightContractField(t, response)
			})
		}, needle: "invalid runtime contract"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := decodeImportPreflightResponse(bytes.NewReader(test.body(t)))
			// Stable bounded wording is sufficient; provider contract JSON must never enter the error.
			if err == nil || !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("error = %v, want %q", err, test.needle)
			}
		})
	}
}

// TestPreflightImportUsesLicensedIdentityAndPreservesTypedRejection covers the
// private Engine-to-Registry route and its bounded non-2xx passthrough.
func TestPreflightImportUsesLicensedIdentityAndPreservesTypedRejection(t *testing.T) {
	var authorization string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		authorization = r.Header.Get("Authorization")
		// The private route must remain distinct from public GraphQL and apply paths.
		if r.URL.Path != "/integrations/import/preflight" {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error":{"code":"IMPORT_REVIEW_MISMATCH","phase":"engine_preflight","commit_state":"not_committed"}}`))
	}))
	defer server.Close()
	client := &HTTPRegistryClient{endpoint: server.URL + "/graphql", licenseKey: "fsk_registry", httpClient: server.Client()}
	_, err := client.PreflightImport(context.Background(), []byte(`{"plan_id":"11111111-1111-4111-8111-111111111111","review_hash":"sha256:test"}`))
	var upstream *ImportPreflightHTTPError
	// Registry's structured admission error must survive without becoming a raw transport error.
	if !errors.As(err, &upstream) || upstream.StatusCode != http.StatusConflict || !bytes.Contains(upstream.Body, []byte("IMPORT_REVIEW_MISMATCH")) {
		t.Fatalf("preflight error = %#v", err)
	}
	if authorization != "Bearer fsk_registry" {
		t.Fatalf("authorization = %q", authorization)
	}
}

// TestReadBoundedImportPreflightBodyRejectsOverrun proves the shared response
// ceiling rejects the whole candidate rather than admitting a truncated prefix.
func TestReadBoundedImportPreflightBodyRejectsOverrun(t *testing.T) {
	_, err := readBoundedImportPreflightBody(strings.NewReader("12345"), 4)
	if !errors.Is(err, errImportPreflightResponseLimit) {
		t.Fatalf("overrun error = %v", err)
	}
}

// TestDoWithCallerDeadlineLetsImportOwnTimeout proves a finite composite
// workflow is not cut off by the ordinary short Registry client timeout.
func TestDoWithCallerDeadlineLetsImportOwnTimeout(t *testing.T) {
	client := &HTTPRegistryClient{
		licenseKey: "fsk_registry",
		httpClient: &http.Client{Timeout: time.Millisecond, Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			select {
			case <-time.After(20 * time.Millisecond):
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{}`)), Header: make(http.Header)}, nil
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		})},
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://registry.example/contract", nil)
	// Request construction is part of the finite caller-owned boundary.
	if err != nil {
		t.Fatal(err)
	}
	response, err := client.doWithCallerDeadline(request)
	// The one-second workflow budget, not the one-millisecond generic timeout, must win.
	if err != nil || response.StatusCode != http.StatusOK {
		t.Fatalf("response=%v error=%v", response, err)
	}
	_ = response.Body.Close()
}
