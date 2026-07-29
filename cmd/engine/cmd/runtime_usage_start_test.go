package cmd

import (
	"context"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

type runtimeUsageStartStore struct {
	store.Store
}

func (s *runtimeUsageStartStore) IncrementRuntimeUsageCounters(context.Context, []models.EngineUsageIncrement) error {
	return nil
}

func TestStartEngineUsageCounterSkipsWhenEntitlementDisablesAggregate(t *testing.T) {
	t.Cleanup(func() { sandbox.SetExecutionUsageRecorder(nil) })
	worker := startEngineUsageCounter(context.Background(), &runtimeUsageStartStore{}, models.RuntimeEntitlement{UsageReporting: "none"})
	if worker != nil {
		t.Fatal("expected usage counter worker to stay disabled")
	}
}

func TestStartEngineUsageCounterStartsForAggregateEntitlement(t *testing.T) {
	t.Cleanup(func() { sandbox.SetExecutionUsageRecorder(nil) })
	worker := startEngineUsageCounter(context.Background(), &runtimeUsageStartStore{}, models.DefaultRuntimeEntitlement())
	if worker == nil {
		t.Fatal("expected usage counter worker to start for aggregate reporting")
	}
	stopCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	worker.Stop(stopCtx)
}
