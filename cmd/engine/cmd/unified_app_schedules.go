package cmd

import (
	"context"
	"log/slog"

	"github.com/Usefused/engine/internal/engine/api"
)

// startUnifiedAppScheduleExecution shares the REST executor and drains automatic invocations during Engine shutdown.
func startUnifiedAppScheduleExecution(ctx context.Context, server *api.EngineGRPCServer) *api.UnifiedAppScheduleWorker {
	worker, err := server.StartUnifiedAppScheduleWorker(ctx)
	// Missing durable schedule dependencies must be visible instead of silently disabling configured triggers.
	if err != nil {
		slog.ErrorContext(ctx, "Failed to start Unified App schedule execution")
		panic(err)
	}
	return worker
}
