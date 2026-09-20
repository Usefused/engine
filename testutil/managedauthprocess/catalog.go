package main

import (
	"context"
	"encoding/json"
	"log"
	"os"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/jackc/pgx/v5/pgxpool"
)

// installRuntimeCatalog extends only an existing disposable consumer snapshot for real SDK and MCP acceptance.
func installRuntimeCatalog(ctx context.Context, pool *pgxpool.Pool) {
	var input struct {
		Metadata  fusedobject.ServiceMetadata `json:"metadata"`
		Endpoints []fusedobject.Endpoint      `json:"endpoints"`
		Webhooks  []fusedobject.Webhook       `json:"webhooks"`
	}
	raw, err := os.ReadFile(os.Getenv("CATALOG_FILE"))
	must(err)
	must(json.Unmarshal(raw, &input))
	contracts := store.NewPostgresStore(pool).(store.ServiceContractSnapshotStore)
	metadata, err := contracts.GetServiceContractMetadata(ctx, input.Metadata.ID, input.Metadata.ServiceVersionID)
	must(err)
	// Preserve the existing auth contract and identity; this helper cannot provision a new service.
	_, err = contracts.UpsertServiceContractSnapshot(ctx, store.ServiceContractSnapshot{
		ExecutionContractEnvelope: metadata.ExecutionContractEnvelope,
		ServiceID:                 metadata.ID, ServiceVersionID: metadata.ServiceVersionID, Version: "1.0.0",
		ServiceMetadata: *metadata, Endpoints: input.Endpoints, Webhooks: input.Webhooks,
	})
	must(err)
	log.Print("reviewed runtime catalogue stored; credentials and connections preserved")
}
