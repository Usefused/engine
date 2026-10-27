package engine

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
)

// TestLiveNumericArticleLookup verifies the reported resource against the real public provider only when explicitly enabled.
func TestLiveNumericArticleLookup(t *testing.T) {
	// Ordinary test runs must remain independent of public provider availability and article retention.
	if os.Getenv("FUSED_LIVE_NUMERIC_LOOKUP") != "1" {
		t.Skip("FUSED_LIVE_NUMERIC_LOOKUP=1 enables the public article lookup")
	}
	var input map[string]any
	// Use the same JSON decoding as a Unified App host instead of supplying a Go integer directly.
	if err := json.Unmarshal([]byte(`{"id":4917886,"Accept":"application/vnd.forem.api-v1+json"}`), &input); err != nil {
		t.Fatal(err)
	}
	operation := explicitAnonymousEndpoint(&models.IntegrationObject{
		Method: http.MethodGet, Path: "/api/articles/{id}",
		Parameters: models.Parameters{
			{Name: "id", In: "path", Type: "integer", Required: true},
			{Name: "Accept", In: "header", Type: "string", Required: true},
		},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	response := NewBoundedBufferStream(1 << 20)
	status, err := NewDispatcher().ExecuteStream(ctx, &models.Service{BaseURL: "https://dev.to"}, operation, input, nil, nil, response)
	// Confirm provider acceptance through the actual Engine dispatcher, not a separately constructed HTTP URL.
	if err != nil || status != http.StatusOK {
		t.Fatalf("numeric provider lookup: status=%d err=%v", status, err)
	}
	var article struct {
		ID int `json:"id"`
	}
	// A 200 response must identify the requested article rather than an unrelated fallback document.
	if err := json.Unmarshal(response.Bytes(), &article); err != nil || article.ID != 4917886 {
		t.Fatalf("unexpected article: id=%d err=%v", article.ID, err)
	}
}
