package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	entitlementpkg "github.com/Usefused/engine/internal/engine/entitlement"
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
	entitlementpkg.LiveEntitlement.Store(models.RuntimeEntitlement{PublicServiceInsightsEnabled: true})
	defer entitlementpkg.LiveEntitlement.Reset()
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

func TestPublicInsightReaderFollowsLiveEntitlementChanges(t *testing.T) {
	defer entitlementpkg.LiveEntitlement.Reset()
	client := &publicInsightReaderClientStub{value: models.PublicServiceInsights{TotalCalls: 12}}
	reader := &publicInsightReader{client: client, cache: make(map[string]publicInsightCacheEntry)}
	query := models.PublicServiceInsightsQuery{ServiceID: uuid.New()}

	entitlementpkg.LiveEntitlement.Store(models.RuntimeEntitlement{PublicServiceInsightsEnabled: false})
	if _, err := reader.Fetch(context.Background(), query); err == nil {
		t.Fatal("disabled plan was allowed to read cross-engine insights")
	}
	if client.calls != 0 {
		t.Fatalf("disabled plan reached Registry %d times", client.calls)
	}

	entitlementpkg.LiveEntitlement.Store(models.RuntimeEntitlement{PublicServiceInsightsEnabled: true})
	if _, err := reader.Fetch(context.Background(), query); err != nil {
		t.Fatalf("enabled plan could not read cross-engine insights: %v", err)
	}
	if client.calls != 1 {
		t.Fatalf("enabled plan reached Registry %d times, want 1", client.calls)
	}

	entitlementpkg.LiveEntitlement.Store(models.RuntimeEntitlement{PublicServiceInsightsEnabled: false})
	if _, err := reader.Fetch(context.Background(), query); err == nil {
		t.Fatal("downgraded plan was served cached cross-engine insights")
	}
	if client.calls != 1 {
		t.Fatalf("downgraded plan reached Registry %d times, want no additional call", client.calls)
	}
}
