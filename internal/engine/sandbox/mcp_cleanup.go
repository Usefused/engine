package sandbox

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const mcpCleanupBatchSize = 100

// StartMCPCleanupWorker runs a background goroutine that checks for MCP servers
// inactive/idle more than 7 days and deletes their sandbox directory files.
func StartMCPCleanupWorker(ctx context.Context, database *pgxpool.Pool) {
	ticker := time.NewTicker(24 * time.Hour)
	go func() {
		for {
			select {
			case <-ctx.Done():
				ticker.Stop()
				return
			case <-ticker.C:
				runCleanup(ctx, database)
			}
		}
	}()

	// Run an initial cleanup shortly after startup
	go func() {
		time.Sleep(5 * time.Minute)
		runCleanup(ctx, database)
	}()
}

func runCleanup(ctx context.Context, database *pgxpool.Pool) {
	slog.InfoContext(ctx, "Running Engine MCP retention cleanup worker")

	threshold := time.Now().Add(-7 * 24 * time.Hour)
	result, err := cleanupExpiredMCPBatches(ctx, threshold,
		func(ctx context.Context, before time.Time, after uuid.UUID) ([]uuid.UUID, error) {
			return listExpiredMCPs(ctx, database, before, after, mcpCleanupBatchSize)
		}, os.RemoveAll)
	if err != nil {
		slog.ErrorContext(ctx, "Engine MCP retention cleanup failed", slog.Any("error", err))
		return
	}
	slog.InfoContext(ctx, "Engine MCP retention cleanup complete",
		slog.Int("cleaned_up", result.cleaned), slog.Int("failed", result.failed))
}

type mcpCleanupResult struct {
	cleaned int
	failed  int
}

type expiredMCPBatchLister func(context.Context, time.Time, uuid.UUID) ([]uuid.UUID, error)

func cleanupExpiredMCPBatches(
	ctx context.Context,
	before time.Time,
	list expiredMCPBatchLister,
	remove func(string) error,
) (mcpCleanupResult, error) {
	var result mcpCleanupResult
	after := uuid.Nil
	for {
		appIDs, err := list(ctx, before, after)
		if err != nil {
			return result, err
		}
		if len(appIDs) == 0 {
			return result, nil
		}
		for _, appID := range appIDs {
			after = appID
			if err := remove(sandboxDirFor(appID.String())); err != nil {
				result.failed++
				slog.ErrorContext(ctx, "Failed to remove MCP sandbox directory",
					slog.Any("error", err), slog.String("app.id", appID.String()))
				continue
			}
			result.cleaned++
		}
	}
}

func listExpiredMCPs(ctx context.Context, database *pgxpool.Pool, before time.Time, after uuid.UUID, limit int) ([]uuid.UUID, error) {
	query := `
		SELECT DISTINCT app.app_id FROM fused_apps app
		JOIN fused_app_families family ON family.app_family_id = app.app_family_id
		LEFT JOIN (
			SELECT app_id,
				   COUNT(*) FILTER (WHERE ended_at IS NULL) as active_sessions,
				   MAX(ended_at) as last_ended_at
			FROM fused_mcp_sessions
			GROUP BY app_id
		) s ON app.app_id = s.app_id
		WHERE family.kind = 'mcp'
		  AND app.status IN ('active', 'deprecated')
		  AND COALESCE(s.active_sessions, 0) = 0
		  AND COALESCE(s.last_ended_at, app.created_at) < $1
		  AND ($2 = '00000000-0000-0000-0000-000000000000'::uuid OR app.app_id > $2)
		ORDER BY app.app_id
		LIMIT $3
	`
	rows, err := database.Query(ctx, query, before, after, limit)
	if err != nil {
		return nil, fmt.Errorf("query expired MCP apps: %w", err)
	}
	defer rows.Close()

	var appIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan expired MCP app: %w", err)
		}
		appIDs = append(appIDs, id)
	}
	return appIDs, rows.Err()
}
