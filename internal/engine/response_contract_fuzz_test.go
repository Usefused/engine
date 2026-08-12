package engine

import (
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

func FuzzResponseMediaSelectionStaysBounded(f *testing.F) {
	f.Add(201, "application/json; charset=utf-8")
	f.Add(204, "text/event-stream")
	f.Add(503, "application/octet-stream")
	f.Add(-1, "not a media type")
	responses := models.Responses{
		"201":     {Representations: []models.ResponseRepresentation{{MediaType: "application/json"}}},
		"2XX":     {Representations: []models.ResponseRepresentation{{MediaType: "text/event-stream"}}},
		"default": {Representations: []models.ResponseRepresentation{{MediaType: "application/octet-stream"}}},
	}
	f.Fuzz(func(t *testing.T, status int, contentType string) {
		family := boundedResponseMediaFamily(contentType)
		outcome := responseMediaSelectionOutcome(responses, status, contentType)
		assertBoundedFuzzValue(t, family, "sse", "json", "xml", "text", "binary", "other", "unknown")
		assertBoundedFuzzValue(t, outcome, "matched", "mismatched", "missing_or_invalid", "no_content", "undocumented")
	})
}

func FuzzLinkHeaderSplittingPreservesInput(f *testing.F) {
	f.Add(`<https://provider.test/items?page=2>; rel="next", <https://provider.test/items?page=9>; rel="last"`)
	f.Add(`<https://provider.test/items?q=a,b>; rel="next"; title="a,b"`)
	f.Add("")
	f.Fuzz(func(t *testing.T, header string) {
		parts := splitLinkHeader(header)
		if len(parts) == 0 {
			t.Fatal("splitLinkHeader returned no segments")
		}
		if reconstructed := strings.Join(parts, ","); reconstructed != header {
			t.Fatalf("Link header changed during splitting: %q", reconstructed)
		}
	})
}

func assertBoundedFuzzValue(t *testing.T, actual string, allowed ...string) {
	t.Helper()
	for _, value := range allowed {
		if actual == value {
			return
		}
	}
	t.Fatalf("unbounded value %q", actual)
}
