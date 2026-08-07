package cmd

import (
	"context"
	"errors"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/worker"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

var errBackgroundStoreCapability = errors.New("background database capability is unavailable")

// backgroundDatabaseGate prevents independently scheduled maintenance jobs
// from opening multiple connections at the same instant. Foreground request
// paths deliberately keep the original store so traffic can expand the pool.
type backgroundDatabaseGate struct {
	available chan struct{}
}

func newBackgroundDatabaseGate() *backgroundDatabaseGate {
	available := make(chan struct{}, 1)
	available <- struct{}{}
	return &backgroundDatabaseGate{available: available}
}

func (g *backgroundDatabaseGate) run(ctx context.Context, operation func() error) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-g.available:
	}
	defer func() { g.available <- struct{}{} }()
	return operation()
}

func backgroundDatabaseValue[T any](ctx context.Context, gate *backgroundDatabaseGate, operation func() (T, error)) (T, error) {
	var result T
	err := gate.run(ctx, func() error {
		var err error
		result, err = operation()
		return err
	})
	return result, err
}

// serializedBackgroundStore gates only short scheduled probes. Once a worker
// finds real work, its mutations and Registry calls remain unconstrained so a
// slow maintenance pass cannot delay authorization refresh or foreground I/O.
type serializedBackgroundStore struct {
	store.Store
	gate           *backgroundDatabaseGate
	revisionLoader accesscontrol.AuthorizationRevisionLoader
	usageReports   worker.RuntimeUsageReportStore
	packageLeases  worker.SDKPackageLeaseStore
}

func newSerializedBackgroundStore(source store.Store) *serializedBackgroundStore {
	revisionLoader, _ := source.(accesscontrol.AuthorizationRevisionLoader)
	usageReports, _ := source.(worker.RuntimeUsageReportStore)
	packageLeases, _ := source.(worker.SDKPackageLeaseStore)
	return &serializedBackgroundStore{
		Store: source, gate: newBackgroundDatabaseGate(), revisionLoader: revisionLoader,
		usageReports: usageReports, packageLeases: packageLeases,
	}
}

func (s *serializedBackgroundStore) LoadAuthorizationRevision(ctx context.Context) (int64, error) {
	if s.revisionLoader == nil {
		return 0, errBackgroundStoreCapability
	}
	return backgroundDatabaseValue(ctx, s.gate, func() (int64, error) {
		return s.revisionLoader.LoadAuthorizationRevision(ctx)
	})
}

func (s *serializedBackgroundStore) ListPendingRuntimeUsageReports(ctx context.Context, limit int) ([]models.EngineUsageReport, error) {
	if s.usageReports == nil {
		return nil, errBackgroundStoreCapability
	}
	return backgroundDatabaseValue(ctx, s.gate, func() ([]models.EngineUsageReport, error) {
		return s.usageReports.ListPendingRuntimeUsageReports(ctx, limit)
	})
}

func (s *serializedBackgroundStore) MarkRuntimeUsageReportsFlushed(ctx context.Context, reportIDs []uuid.UUID, flushedAt time.Time) error {
	if s.usageReports == nil {
		return errBackgroundStoreCapability
	}
	return s.usageReports.MarkRuntimeUsageReportsFlushed(ctx, reportIDs, flushedAt)
}

func (s *serializedBackgroundStore) ListSDKPackageLeaseRenewals(ctx context.Context, after uuid.UUID, limit int) ([]models.SDKPackageLeaseRenewal, error) {
	if s.packageLeases == nil {
		return nil, errBackgroundStoreCapability
	}
	return backgroundDatabaseValue(ctx, s.gate, func() ([]models.SDKPackageLeaseRenewal, error) {
		return s.packageLeases.ListSDKPackageLeaseRenewals(ctx, after, limit)
	})
}

func (s *serializedBackgroundStore) ListUnprojectedPublicInsightServiceIDs(ctx context.Context, before time.Time, limit int) ([]uuid.UUID, error) {
	return backgroundDatabaseValue(ctx, s.gate, func() ([]uuid.UUID, error) {
		return s.Store.ListUnprojectedPublicInsightServiceIDs(ctx, before, limit)
	})
}

func (s *serializedBackgroundStore) ListPendingPublicServiceInsightReports(ctx context.Context, limit int, now time.Time) ([]models.PublicServiceInsightReport, error) {
	return backgroundDatabaseValue(ctx, s.gate, func() ([]models.PublicServiceInsightReport, error) {
		return s.Store.ListPendingPublicServiceInsightReports(ctx, limit, now)
	})
}
