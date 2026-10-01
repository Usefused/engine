package cmd

import (
	"context"
	"log/slog"

	"github.com/Usefused/engine/internal/engine/api"
)

// startUnifiedAppWebhookExecution shares the REST executor and drains automatic invocations during Engine shutdown.
func startUnifiedAppWebhookExecution(ctx context.Context, server *api.EngineGRPCServer) *api.UnifiedAppWebhookWorker {
	worker, err := server.StartUnifiedAppWebhookWorker(ctx)
	// Missing durable delivery dependencies must be visible instead of silently disabling configured triggers.
	if err != nil {
		slog.ErrorContext(ctx, "Failed to start Unified App webhook execution")
		panic(err)
	}
	return worker
}
