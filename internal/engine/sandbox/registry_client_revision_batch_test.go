package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// revisionBatchFixture includes sibling versions and all provenance fields so batching cannot collapse identities or alter contracts.
func revisionBatchFixture(count int) ([]ServiceVersionRef, []ServiceVersionRevision) {
	refs := make([]ServiceVersionRef, count)
	rows := make([]ServiceVersionRevision, count)
	serviceID := uuid.New()
	for i := range refs {
		refs[i] = ServiceVersionRef{ServiceID: serviceID, Version: fmt.Sprintf("v%d", i)}
		rows[i] = ServiceVersionRevision{ServiceID: serviceID, Version: refs[i].Version, ServiceVersionID: uuid.New(), Revision: i + 1, SourceHash: fmt.Sprintf("source-%d", i), GenerationContractHash: fmt.Sprintf("generation-%d", i), RuntimeContractHash: fmt.Sprintf("runtime-%d", i), IsPublic: i%2 == 0}
	}
	return refs, rows
}

// revisionBatchResponse serializes real wire DTOs without dropping hashes or visibility metadata.
func revisionBatchResponse(t *testing.T, rows []ServiceVersionRevision) *http.Response {
	t.Helper()
	body, err := json.Marshal(serviceVersionRevisionBatchResponse{Versions: rows})
	// Broken fixture serialization must fail before the client under test runs.
	if err != nil {
		t.Fatal(err)
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}
}

// TestFetchServiceVersionRevisionsBatches verifies the Registry boundary, exact metadata, repeated references, and empty-input behavior.
func TestFetchServiceVersionRevisionsBatches(t *testing.T) {
	for _, count := range []int{0, 1, 50, 51, 115, 205} {
		// Each boundary case uses its own transport and request accounting.
		t.Run(fmt.Sprint(count), func(t *testing.T) {
			refs, expected := revisionBatchFixture(count)
			// A repeat spanning batches must retain multiplicity rather than consume a new identity.
			if count > 50 {
				refs[50], expected[50] = refs[0], expected[0]
			}
			offset, calls := 0, 0
			installationID, runtimeID := uuid.New(), uuid.New()
			client := &HTTPRegistryClient{endpoint: "https://registry.example/graphql", licenseKey: "engine-license", installationID: installationID, runtimeInstanceID: runtimeID,
				// The transport enforces the real endpoint limit and licensed identity for every batch.
				httpClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					// Batching must retain the original endpoint and credential boundary.
					if r.Method != http.MethodPost || r.URL.Path != "/integrations/versions/revisions" || r.Header.Get("X-API-Key") != "engine-license" || r.Header.Get("Authorization") != "Bearer engine-license" || r.Header.Get("X-Fused-Installation-ID") != installationID.String() || r.Header.Get("X-Fused-Runtime-Instance-ID") != runtimeID.String() {
						t.Fatal("unexpected request identity")
					}
					var body struct {
						Versions []ServiceVersionRef `json:"versions"`
					}
					// Wire decoding catches incorrect envelope or reference serialization.
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Fatal(err)
					}
					size := len(body.Versions)
					// Registry rejects requests above 50 regardless of total workspace size.
					if size == 0 || size > 50 {
						t.Fatalf("invalid batch size %d", size)
					}
					// Every reference must occur exactly where the caller supplied it.
					if !reflect.DeepEqual(body.Versions, refs[offset:offset+size]) {
						t.Fatal("references changed")
					}
					result := revisionBatchResponse(t, expected[offset:offset+size])
					offset += size
					return result, nil
				})}}
			got, err := client.FetchServiceVersionRevisions(context.Background(), refs, "caller-secret")
			// Successful aggregation must cover all references with the minimum bounded requests.
			if err != nil || offset != count || calls != (count+49)/50 {
				t.Fatalf("offset=%d calls=%d err=%v", offset, calls, err)
			}
			// Empty input preserves the existing nil result contract.
			if count == 0 {
				expected = nil
			}
			// Exact DTO equality proves metadata was neither regenerated nor normalized.
			if !reflect.DeepEqual(got, expected) {
				t.Fatal("revision metadata changed")
			}
		})
	}
}

// TestFetchServiceVersionRevisionsDiscardsPartialResults prevents later failures or malformed identities from becoming a usable plan snapshot.
func TestFetchServiceVersionRevisionsDiscardsPartialResults(t *testing.T) {
	for _, mode := range []string{"http", "decode", "transport", "cancel", "foreign", "duplicate", "unpinned"} {
		// Each failure is injected after one successful batch to exercise atomic return behavior.
		t.Run(mode, func(t *testing.T) {
			refs, rows := revisionBatchFixture(115)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			calls := 0
			client := &HTTPRegistryClient{endpoint: "https://registry.example/graphql", licenseKey: "engine-license",
				// The second response models dependency and protocol failures without any real Registry mutation.
				httpClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
					calls++
					// Only the first batch succeeds; cancellation must prevent another request entirely.
					if calls == 1 {
						// Expiration after a response must be checked before the next dispatch.
						if mode == "cancel" {
							cancel()
						}
						return revisionBatchResponse(t, rows[:50]), nil
					}
					// Failure modes distinguish transport, status, decoding, and identity validation.
					switch mode {
					case "http":
						return &http.Response{StatusCode: http.StatusForbidden, Body: io.NopCloser(strings.NewReader("denied"))}, nil
					case "decode":
						return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"versions":[`))}, nil
					case "transport":
						return nil, errors.New("connection lost")
					case "foreign":
						rows[50].ServiceID = uuid.New()
					case "duplicate":
						rows[51] = rows[50]
					case "unpinned":
						rows[50].ServiceVersionID = uuid.Nil
					}
					return revisionBatchResponse(t, rows[50:100]), nil
				})}}
			got, err := client.FetchServiceVersionRevisions(ctx, refs, "caller-secret")
			// No earlier rows may escape on a later batch failure.
			if err == nil || got != nil || calls > 2 {
				t.Fatalf("got=%v calls=%d err=%v", got, calls, err)
			}
			// Cancellation retains its identity and stops between batches.
			if mode == "cancel" && (!errors.Is(err, context.Canceled) || calls != 1) {
				t.Fatalf("cancellation lost: calls=%d err=%v", calls, err)
			}
		})
	}
}

// TestFetchServiceVersionRevisionsPreservesVisibilityOmissions keeps missing versions absent for caller-specific planning rejection.
func TestFetchServiceVersionRevisionsPreservesVisibilityOmissions(t *testing.T) {
	refs, rows := revisionBatchFixture(51)
	calls := 0
	client := &HTTPRegistryClient{endpoint: "https://registry.example/graphql", licenseKey: "engine-license",
		// Registry legitimately omits references the licensed caller cannot see.
		httpClient: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			calls++
			// Leave the first reference unresolved rather than inventing a replacement row.
			if calls == 1 {
				return revisionBatchResponse(t, rows[1:50]), nil
			}
			return revisionBatchResponse(t, rows[50:]), nil
		})}}
	got, err := client.FetchServiceVersionRevisions(context.Background(), refs, "caller-secret")
	// The client must preserve the omission and all visible records across batches.
	if err != nil || !reflect.DeepEqual(got, rows[1:]) {
		t.Fatalf("visibility result changed: %v", err)
	}
}
