package engine

import (
	"fmt"
	"testing"

	"github.com/Usefused/engine/internal/shared/models"
)

var responseSelectionResult *models.ResponseRepresentation

func TestResponseContractSelectionAllocationBound(t *testing.T) {
	responses := responseSelectionPerformanceFixture()
	allocations := testing.AllocsPerRun(1_000, func() {
		selected, ok := responseRepresentationForStatusAndMedia(responses, 206, "application/vnd.fused.result+json; charset=utf-8")
		if !ok {
			panic("response selection fixture did not match")
		}
		responseSelectionResult = selected
	})
	// This gate measures only Engine contract selection; provider I/O and body
	// decoding stay outside the sample so regressions remain attributable.
	if allocations > 12 {
		t.Fatalf("response contract selection allocations/run = %.2f, want <= 12", allocations)
	}
	t.Logf("response contract selection allocations/run=%.2f", allocations)
}

func BenchmarkResponseContractSelection(b *testing.B) {
	responses := responseSelectionPerformanceFixture()
	tests := []struct {
		name        string
		status      int
		contentType string
	}{
		{name: "exact_vendor_json", status: 206, contentType: "application/vnd.fused.result+json; charset=utf-8"},
		{name: "range_sse", status: 202, contentType: "text/event-stream"},
		{name: "default_binary", status: 418, contentType: "application/octet-stream"},
	}
	for _, test := range tests {
		b.Run(test.name, func(b *testing.B) {
			b.ReportAllocs()
			for range b.N {
				responseSelectionResult, _ = responseRepresentationForStatusAndMedia(responses, test.status, test.contentType)
			}
		})
	}
}

func responseSelectionPerformanceFixture() models.Responses {
	representations := make([]models.ResponseRepresentation, 0, 8)
	for index := range 7 {
		representations = append(representations, models.ResponseRepresentation{MediaType: fmt.Sprintf("application/vnd.fused.%d+json", index)})
	}
	representations = append(representations, models.ResponseRepresentation{MediaType: "application/vnd.fused.result+json"})
	return models.Responses{
		"206":     {Representations: representations},
		"2XX":     {Representations: []models.ResponseRepresentation{{MediaType: "text/event-stream"}}},
		"default": {Representations: []models.ResponseRepresentation{{MediaType: "application/octet-stream"}}},
	}
}
