package sandbox

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

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
	artifactIDs, err := listExpiredMCPs(ctx, database, threshold)
	if err != nil {
		slog.ErrorContext(ctx, "Cleanup worker failed to list expired MCPs", slog.Any("error", err))
		return
	}

	count := 0
	for _, artifactID := range artifactIDs {
		slog.InfoContext(ctx, "MCP sandbox older than 7 days, triggering cleanup", slog.String("artifact_id", artifactID.String()))

		// Remove the directory and all contents.
		if err := os.RemoveAll(sandboxDirFor(artifactID.String())); err != nil {
			slog.ErrorContext(ctx, "Failed to remove sandbox directory", slog.Any("error", err), slog.String("sandbox_id", artifactID.String()))
		} else {
			count++
			slog.InfoContext(ctx, "Sandbox directory cleaned up", slog.String("sandbox_id", artifactID.String()))
		}
	}

	slog.InfoContext(ctx, "Engine MCP retention cleanup complete", slog.Int("cleaned_up", count))
}

func listExpiredMCPs(ctx context.Context, database *pgxpool.Pool, before time.Time) ([]uuid.UUID, error) {
	query := `
		SELECT DISTINCT sc.artifact_id FROM fused_artifact_scopes sc
		LEFT JOIN (
			SELECT artifact_id, 
				   COUNT(*) FILTER (WHERE ended_at IS NULL) as active_sessions,
				   MAX(ended_at) as last_ended_at
			FROM fused_mcp_sessions
			GROUP BY artifact_id
		) s ON sc.artifact_id = s.artifact_id
		WHERE COALESCE(s.active_sessions, 0) = 0
		  AND COALESCE(s.last_ended_at, sc.created_at) < $1
	`
	rows, err := database.Query(ctx, query, before)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var artifactIDs []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err == nil {
			artifactIDs = append(artifactIDs, id)
		}
	}
	return artifactIDs, nil
}
