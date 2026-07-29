package api

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

type RuntimeContractFetcher interface {
	FetchRuntimeContract(ctx context.Context, serviceID, serviceVersionID uuid.UUID, version, apiKey string) (*store.ServiceContractSnapshot, error)
}

type BatchRuntimeContractFetcher interface {
	FetchRuntimeContracts(ctx context.Context, versions []store.WorkspaceServiceVersion, apiKey string) ([]store.ServiceContractSnapshot, error)
}

type runtimeContractSnapshotWriter interface {
	UpsertServiceContractSnapshot(ctx context.Context, snapshot store.ServiceContractSnapshot) (*store.ServiceContractSnapshot, error)
}

func materializeRuntimeContractSnapshot(ctx context.Context, s store.Store, fetcher RuntimeContractFetcher, accountID, serviceID, serviceVersionID uuid.UUID, version, apiKey string) error {
	snapshotStore, ok := s.(runtimeContractSnapshotWriter)
	if !ok || fetcher == nil {
		// Why skip instead of failing: this helper is wired into handlers that
		// still use focused test doubles and transitional stores. Production
		// passes both capabilities; tests that do not exercise snapshotting keep
		// their old narrow contracts until their path is migrated deliberately.
		return nil
	}

	ctx, span := otel.Tracer("engine").Start(ctx, "engine.workspace.materialize_runtime_contract")
	defer span.End()
	span.SetAttributes(
		attribute.String("user_action", "workspace.materialize_runtime_contract"),
		attribute.String("account_id", accountID.String()),
		attribute.String("service_id", serviceID.String()),
		attribute.String("service_version_id", serviceVersionID.String()),
		attribute.String("service_version", version),
	)

	snapshot, err := fetcher.FetchRuntimeContract(ctx, serviceID, serviceVersionID, version, apiKey)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "fetch_failed"))
		slog.WarnContext(ctx, "runtime contract snapshot fetch failed",
			slog.String("service_id", serviceID.String()),
			slog.String("service_version_id", serviceVersionID.String()),
			slog.Any("error", err))
		return fmt.Errorf("failed to fetch runtime contract snapshot: %w", err)
	}
	if _, err := snapshotStore.UpsertServiceContractSnapshot(ctx, *snapshot); err != nil {
		span.SetAttributes(attribute.String("outcome", "write_failed"))
		slog.ErrorContext(ctx, "runtime contract snapshot write failed",
			slog.String("service_id", serviceID.String()),
			slog.String("service_version_id", serviceVersionID.String()),
			slog.Any("error", err))
		return fmt.Errorf("failed to store runtime contract snapshot: %w", err)
	}
	span.SetAttributes(attribute.String("outcome", "success"))
	return nil
}
