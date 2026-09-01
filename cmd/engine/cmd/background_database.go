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

var _ worker.SDKGenerationBuildStore = (*serializedBackgroundStore)(nil)

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
	sdkGenerations worker.SDKGenerationBuildStore
	authRefresh    worker.ConnectedAuthRefreshStore
}

// newSerializedBackgroundStore discovers optional worker capabilities once and
// applies one shared gate only to their short scheduled database probes.
func newSerializedBackgroundStore(source store.Store) *serializedBackgroundStore {
	revisionLoader, _ := source.(accesscontrol.AuthorizationRevisionLoader)
	usageReports, _ := source.(worker.RuntimeUsageReportStore)
	packageLeases, _ := source.(worker.SDKPackageLeaseStore)
	sdkGenerations, _ := source.(worker.SDKGenerationBuildStore)
	authRefresh, _ := source.(worker.ConnectedAuthRefreshStore)
	return &serializedBackgroundStore{
		Store: source, gate: newBackgroundDatabaseGate(), revisionLoader: revisionLoader,
		usageReports: usageReports, packageLeases: packageLeases, sdkGenerations: sdkGenerations, authRefresh: authRefresh,
	}
}

// connectedAuthRefreshCapability returns the gated claim view only when the
// wrapped Engine store can actually persist cross-replica refresh leases.
func (s *serializedBackgroundStore) connectedAuthRefreshCapability() (worker.ConnectedAuthRefreshStore, error) {
	if s == nil || s.authRefresh == nil {
		// Why: returning the wrapper itself would defer a missing capability to
		// hourly runtime failures and silently let OAuth grants expire.
		return nil, errBackgroundStoreCapability
	}
	return s, nil
}

// sdkGenerationCapability returns the gated discovery view only when the
// wrapped store can durably finalize building SDK versions.
func (s *serializedBackgroundStore) sdkGenerationCapability() (worker.SDKGenerationBuildStore, error) {
	// Starting without this capability would strand every accepted asynchronous SDK build.
	if s == nil || s.sdkGenerations == nil {
		return nil, errBackgroundStoreCapability
	}
	return s, nil
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

// ListPendingSDKGenerationBuilds serializes only pending-row discovery; the
// worker's Registry calls and compare-and-swap mutations bypass the gate.
func (s *serializedBackgroundStore) ListPendingSDKGenerationBuilds(ctx context.Context, after uuid.UUID, limit int) ([]store.SDKGenerationBuild, error) {
	// A missing production capability must fail the pass rather than appear as an empty queue.
	if s.sdkGenerations == nil {
		return nil, errBackgroundStoreCapability
	}
	return backgroundDatabaseValue(ctx, s.gate, func() ([]store.SDKGenerationBuild, error) {
		return s.sdkGenerations.ListPendingSDKGenerationBuilds(ctx, after, limit)
	})
}

// CompleteSDKGeneration forwards the activation CAS outside the shared probe
// gate so Registry latency cannot hold unrelated background database reads.
func (s *serializedBackgroundStore) CompleteSDKGeneration(ctx context.Context, appID uuid.UUID, jobID, idempotencyKey string) (bool, error) {
	// Capability absence leaves the durable build pending for a later healthy process.
	if s.sdkGenerations == nil {
		return false, errBackgroundStoreCapability
	}
	return s.sdkGenerations.CompleteSDKGeneration(ctx, appID, jobID, idempotencyKey)
}

// FailSDKGeneration forwards a confirmed terminal Registry outcome outside the probe gate.
func (s *serializedBackgroundStore) FailSDKGeneration(ctx context.Context, appID uuid.UUID, jobID, idempotencyKey string) (bool, error) {
	// Capability absence leaves the durable build pending rather than inventing failure state.
	if s.sdkGenerations == nil {
		return false, errBackgroundStoreCapability
	}
	return s.sdkGenerations.FailSDKGeneration(ctx, appID, jobID, idempotencyKey)
}

// ClaimAuthConnectionsForRefresh serializes only the due-row discovery and
// lease claim; token-provider calls and completion writes bypass this gate.
func (s *serializedBackgroundStore) ClaimAuthConnectionsForRefresh(ctx context.Context, cutoff, passStartedAt, now, leaseExpiresAt time.Time, limit int) ([]store.AuthConnectionRefreshClaim, error) {
	if s.authRefresh == nil {
		return nil, errBackgroundStoreCapability
	}
	return backgroundDatabaseValue(ctx, s.gate, func() ([]store.AuthConnectionRefreshClaim, error) {
		return s.authRefresh.ClaimAuthConnectionsForRefresh(ctx, cutoff, passStartedAt, now, leaseExpiresAt, limit)
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
