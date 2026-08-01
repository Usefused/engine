package sandbox

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestFetchServiceChangelogSinceSendsRFC3339SinceAndParsesRows verifies both
// halves of the wire contract with graph/service_changelog.go: the `since`
// variable must be RFC3339 text (that resolver's own time.Parse expects
// exactly that), and every nullable field (version, plan_id, created_by)
// must round-trip correctly whether present or null.
func TestFetchServiceChangelogSinceSendsRFC3339SinceAndParsesRows(t *testing.T) {
	serviceID := uuid.New()
	entryID := uuid.New()
	planID := uuid.New()
	createdBy := uuid.New()
	since := time.Date(2026, 7, 20, 12, 30, 0, 0, time.UTC)

	var requestBody graphqlQuery
	client := &HTTPRegistryClient{
		endpoint:   "https://registry.example/graphql",
		licenseKey: "engine-license-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			if err := json.NewDecoder(request.Body).Decode(&requestBody); err != nil {
				t.Fatalf("decode request: %v", err)
			}
			body := `{"data":{"serviceChangelogSince":[
				{"id":"` + entryID.String() + `","service_id":"` + serviceID.String() + `","version":"1.0.0",
				 "config_type":"version","changelog_type":"new","diff":{"added":["GET /x"]},
				 "plan_id":"` + planID.String() + `","config_key":"import:1.0.0","created_by":"` + createdBy.String() + `",
				 "created_at":"2026-07-21T00:00:00Z"},
				{"id":"` + uuid.New().String() + `","service_id":"` + serviceID.String() + `","version":null,
				 "config_type":"execution_policy","changelog_type":"changed","diff":null,
				 "plan_id":null,"config_key":null,"created_by":null,"created_at":"2026-07-22T00:00:00Z"}
			]}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}

	entries, err := client.FetchServiceChangelogSince(context.Background(), serviceID, since, "fsk_test")
	if err != nil {
		t.Fatalf("FetchServiceChangelogSince() error = %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(entries))
	}

	if sinceArg, _ := requestBody.Variables["since"].(string); sinceArg != "2026-07-20T12:30:00Z" {
		t.Fatalf("expected RFC3339 since variable, got %q", sinceArg)
	}

	first := entries[0]
	if first.ID != entryID || first.ServiceID != serviceID {
		t.Fatalf("unexpected first entry ids: %#v", first)
	}
	if first.Version == nil || *first.Version != "1.0.0" {
		t.Fatalf("expected version=1.0.0, got %+v", first.Version)
	}
	if first.PlanID == nil || *first.PlanID != planID {
		t.Fatalf("expected plan_id=%s, got %+v", planID, first.PlanID)
	}
	if first.CreatedBy == nil || *first.CreatedBy != createdBy {
		t.Fatalf("expected created_by=%s, got %+v", createdBy, first.CreatedBy)
	}
	if len(first.Diff) == 0 {
		t.Fatalf("expected a non-empty diff payload for the first row")
	}

	second := entries[1]
	if second.Version != nil || second.PlanID != nil || second.CreatedBy != nil {
		t.Fatalf("expected every nullable field nil on the second row, got %+v", second)
	}
	if !second.CreatedAt.After(first.CreatedAt) {
		t.Fatalf("expected second row's created_at after the first's")
	}
}

// TestFetchServiceChangelogSinceRejectsMalformedID guards against silently
// returning uuid.Nil for a malformed id -- toModel must surface a clear
// error instead.
func TestFetchServiceChangelogSinceRejectsMalformedID(t *testing.T) {
	client := &HTTPRegistryClient{
		endpoint:   "https://registry.example/graphql",
		licenseKey: "engine-license-key",
		httpClient: &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
			body := `{"data":{"serviceChangelogSince":[{"id":"not-a-uuid","service_id":"` + uuid.New().String() + `",
				"config_type":"version","changelog_type":"new","created_at":"2026-07-21T00:00:00Z"}]}}`
			return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
		})},
	}

	if _, err := client.FetchServiceChangelogSince(context.Background(), uuid.New(), time.Now(), ""); err == nil {
		t.Fatal("expected an error for a malformed changelog id, got nil")
	}
}
