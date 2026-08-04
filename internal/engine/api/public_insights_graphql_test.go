package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/shared/models"
)

type publicInsightReaderClientStub struct {
	value models.PublicServiceInsights
	err   error
	calls int
}

func (c *publicInsightReaderClientStub) FetchPublicServiceInsights(context.Context, models.PublicServiceInsightsQuery) (models.PublicServiceInsights, error) {
	c.calls++
	return c.value, c.err
}

func TestPublicInsightReaderCachesAndLabelsStaleFallback(t *testing.T) {
	client := &publicInsightReaderClientStub{value: models.PublicServiceInsights{TotalCalls: 12}}
	reader := &publicInsightReader{client: client, cache: make(map[string]publicInsightCacheEntry)}
	query := models.PublicServiceInsightsQuery{
		ServiceID: uuid.New(), StartDate: time.Now().Add(-time.Hour), EndDate: time.Now(), Granularity: "hour",
	}

	first, err := reader.Fetch(context.Background(), query)
	if err != nil || first.TotalCalls != 12 {
		t.Fatalf("first fetch = %#v, %v", first, err)
	}
	client.err = errors.New("registry unavailable")
	second, err := reader.Fetch(context.Background(), query)
	if err != nil || second.PartialData || client.calls != 1 {
		t.Fatalf("cached fetch = %#v, %v calls=%d", second, err, client.calls)
	}

	reader.mu.Lock()
	for key, entry := range reader.cache {
		entry.createdAt = time.Now().Add(-publicInsightCacheTTL - time.Second)
		reader.cache[key] = entry
	}
	reader.mu.Unlock()
	stale, err := reader.Fetch(context.Background(), query)
	if err != nil || !stale.PartialData || client.calls != 2 {
		t.Fatalf("stale fallback = %#v, %v calls=%d", stale, err, client.calls)
	}
}
