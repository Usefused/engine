package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/google/uuid"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestPromptClassifierGroundsLicensedSelection covers the Registry wire contract, exact-ID bypass, failure, and scope enforcement.
func TestPromptClassifierGroundsLicensedSelection(t *testing.T) {
	t.Setenv("FUSED_ENV", "development")
	for _, tc := range []struct {
		name, intent, selected string
		status, count          int
		want                   string
		wantError              bool
	}{
		{name: "semantic", intent: "show bills", selected: "listInvoices", status: 200, count: 2, want: "listInvoices"},
		{name: "exact bypass", intent: "listInvoices", status: 502, count: 2, want: "listInvoices"},
		{name: "no match", intent: "book flights", status: 200, count: 2},
		{name: "out of scope", intent: "show bills", selected: "deleteEverything", status: 200, count: 2, wantError: true},
		{name: "upstream failure", intent: "show bills", status: 502, count: 2, wantError: true},
		{name: "oversized catalogue", intent: "show bills", status: 200, count: 2049, wantError: true},
	} {
		// Every case owns its transport so expected failures cannot contaminate another selection.
		t.Run(tc.name, func(t *testing.T) {
			serviceID := uuid.New()
			calls := 0
			// The fake Registry verifies both catalogue admission and the existing licensed classifier endpoint.
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				// Caller control credentials must never replace Engine's license at either boundary.
				if r.Header.Get("Authorization") != "Bearer engine-license" {
					t.Error("missing Engine license")
					http.Error(w, "unauthorized", 401)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				// Discovery reads must request only names/descriptions on the exact resolved service version.
				if r.URL.Path == "/graphql" {
					var input struct {
						Query     string            `json:"query"`
						Variables map[string]string `json:"variables"`
					}
					_ = json.NewDecoder(r.Body).Decode(&input)
					if input.Variables["serviceId"] != serviceID.String() || input.Variables["version"] != "2026-09" || strings.Contains(input.Query, "parameters") || strings.Contains(input.Query, "searchEndpoints") {
						t.Error("wrong catalogue identity or fields")
					}
					ops := make([]map[string]string, tc.count)
					for i := range ops {
						ops[i] = map[string]string{"name": fmt.Sprintf("op%d", i), "description": strings.Repeat("界", 200)}
					}
					ops[0]["name"] = "listInvoices"
					_ = json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"serviceOperations": ops}})
					return
				}
				if r.URL.Path != "/api/engine/fused-intelligent-classifier" {
					t.Error("wrong classifier endpoint")
				} // Prompt and MCP must share one paid boundary.
				calls++
				var input struct {
					Intent     string                `json:"intent"`
					Operations []ClassifierOperation `json:"operations"`
				}
				_ = json.NewDecoder(r.Body).Decode(&input)
				if input.Intent != tc.intent || len(input.Operations) != tc.count {
					t.Error("incomplete catalogue or changed intent")
				} // Ranking must not prefilter candidates.
				for _, op := range input.Operations {
					if len(op.Description) > 512 || !utf8.ValidString(op.Description) {
						t.Error("unbounded or broken UTF-8 disclosure")
					} // Cap prose without splitting characters.
				}
				w.WriteHeader(tc.status)
				_ = json.NewEncoder(w).Encode(map[string]string{"provider": "fused-intelligent-classifier", "operationName": tc.selected})
			}))
			defer server.Close()
			client := NewHTTPRegistryClient(server.URL+"/graphql", "engine-license")
			got, err := client.ClassifyServiceOperation(context.Background(), serviceID, "2026-09", tc.intent)
			if (err != nil) != tc.wantError || got != tc.want {
				t.Fatalf("result=%q err=%v", got, err)
			} // Every selection and failure must preserve the expected scope.
			wantCalls := 1
			if tc.intent == "listInvoices" || tc.count > 2048 {
				wantCalls = 0
			} // Exact identifiers and over-limit requests bypass paid inference.
			if calls != wantCalls {
				t.Fatalf("classifier calls=%d want=%d", calls, wantCalls)
			}
		})
	}
}

// TestPromptClassifierRejectsUnavailableCatalogue proves failed visibility/version resolution cannot reach paid inference.
func TestPromptClassifierRejectsUnavailableCatalogue(t *testing.T) {
	t.Setenv("FUSED_ENV", "development")
	// A rejected exact version must stop at the catalogue boundary.
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/graphql" {
			t.Error("failed catalogue reached classifier")
		}
		_, _ = w.Write([]byte(`{"errors":[{"message":"version unavailable"}],"data":{"serviceOperations":null}}`))
	}))
	defer server.Close()
	client := NewHTTPRegistryClient(server.URL+"/graphql", "engine-license")
	_, err := client.ClassifyServiceOperation(context.Background(), uuid.New(), "v1", "read bills")
	if err == nil {
		t.Fatal("failed version was admitted")
	} // Absence of a complete visible version is not no-match.
}
