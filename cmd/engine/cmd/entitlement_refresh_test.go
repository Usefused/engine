package cmd

import (
	"context"
	"errors"
	"testing"

	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/shared/models"
)

type heartbeatEntitlementStoreStub struct {
	saved models.RuntimeEntitlement
	err   error
}

func (s *heartbeatEntitlementStoreStub) SaveRuntimeEntitlement(_ context.Context, value models.RuntimeEntitlement) error {
	if s.err != nil {
		return s.err
	}
	s.saved = value
	return nil
}

func (s *heartbeatEntitlementStoreStub) GetRuntimeEntitlement(context.Context) (models.RuntimeEntitlement, error) {
	return models.RuntimeEntitlement{}, nil
}

func TestPersistHeartbeatEntitlementStoresAfterPersistence(t *testing.T) {
	entitlement.LiveEntitlement.Reset()
	t.Cleanup(entitlement.LiveEntitlement.Reset)
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{Plan: "dev", EntitlementRevision: "dev-revision", ExecutionRetentionDays: models.IntPtr(7)})
	store := &heartbeatEntitlementStoreStub{}
	next := models.RuntimeEntitlement{Plan: "scale-up", EntitlementRevision: "scale-revision", ExecutionRetentionDays: models.IntPtr(30)}

	if err := persistHeartbeatEntitlement(context.Background(), store, next); err != nil {
		t.Fatalf("persist heartbeat entitlement: %v", err)
	}
	if store.saved.Plan != "scale-up" || entitlement.LiveEntitlement.Load().Plan != "scale-up" || entitlement.LiveEntitlement.Load().EntitlementRevision != "scale-revision" {
		t.Fatalf("entitlement was not persisted and published: saved=%#v live=%#v", store.saved, entitlement.LiveEntitlement.Load())
	}
}

func TestPersistHeartbeatEntitlementDoesNotPublishFailedWrite(t *testing.T) {
	entitlement.LiveEntitlement.Reset()
	t.Cleanup(entitlement.LiveEntitlement.Reset)
	entitlement.LiveEntitlement.Store(models.RuntimeEntitlement{Plan: "dev", EntitlementRevision: "dev-revision", ExecutionRetentionDays: models.IntPtr(7)})
	store := &heartbeatEntitlementStoreStub{err: errors.New("database unavailable")}

	err := persistHeartbeatEntitlement(context.Background(), store, models.RuntimeEntitlement{Plan: "scale-up", EntitlementRevision: "scale-revision", ExecutionRetentionDays: models.IntPtr(30)})
	if err == nil {
		t.Fatal("expected persistence failure")
	}
	if got := entitlement.LiveEntitlement.Load().Plan; got != "dev" {
		t.Fatalf("failed persistence changed live plan to %q", got)
	}
	if got := entitlement.LiveEntitlement.Load().EntitlementRevision; got != "dev-revision" {
		t.Fatalf("failed persistence acknowledged revision %q", got)
	}
}

func TestEngineExecutionRetentionDaysTracksPlanChanges(t *testing.T) {
	if got := engineExecutionRetentionDays(models.RuntimeEntitlement{}, 14); got != 14 {
		t.Fatalf("fallback retention = %d, want 14", got)
	}
	if got := engineExecutionRetentionDays(models.RuntimeEntitlement{ExecutionRetentionDays: models.IntPtr(7)}, 14); got != 7 {
		t.Fatalf("dev retention = %d, want 7", got)
	}
	if got := engineExecutionRetentionDays(models.RuntimeEntitlement{ExecutionRetentionDays: models.IntPtr(30)}, 14); got != 30 {
		t.Fatalf("updated retention = %d, want 30", got)
	}
}
