package store

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/db"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

func TestServiceContractHashIgnoresEndpointAndWebhookOrder(t *testing.T) {
	serviceID := uuid.New()
	versionID := uuid.New()
	firstEndpoint := fusedobject.Endpoint{ID: uuid.New(), Name: "listWidgets", Method: "GET", Path: "/widgets"}
	secondEndpoint := fusedobject.Endpoint{ID: uuid.New(), Name: "createWidget", Method: "POST", Path: "/widgets"}
	firstWebhook := fusedobject.Webhook{ID: uuid.New(), Name: "widget.created", Method: "POST"}
	secondWebhook := fusedobject.Webhook{ID: uuid.New(), Name: "widget.deleted", Method: "POST"}

	base := ServiceContractSnapshot{
		ServiceID:        serviceID,
		ServiceVersionID: versionID,
		Version:          "2026-07-23",
		ServiceMetadata: fusedobject.ServiceMetadata{
			ID:               serviceID,
			ServiceVersionID: versionID,
			Name:             "Widgets",
		},
		Endpoints: []fusedobject.Endpoint{firstEndpoint, secondEndpoint},
		Webhooks:  []fusedobject.Webhook{firstWebhook, secondWebhook},
	}
	reordered := base
	reordered.Endpoints = []fusedobject.Endpoint{secondEndpoint, firstEndpoint}
	reordered.Webhooks = []fusedobject.Webhook{secondWebhook, firstWebhook}

	firstHash, err := serviceContractHash(base)
	if err != nil {
		t.Fatalf("serviceContractHash(base): %v", err)
	}
	secondHash, err := serviceContractHash(reordered)
	if err != nil {
		t.Fatalf("serviceContractHash(reordered): %v", err)
	}
	if firstHash != secondHash {
		t.Fatalf("expected stable hash independent of operation ordering, got %s and %s", firstHash, secondHash)
	}
}

func TestPostgresStoreServiceContractSnapshotRoundTrip(t *testing.T) {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not set")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	pool, err := db.InitEnginePostgres(ctx, dbURL)
	if err != nil {
		t.Fatalf("InitEnginePostgres: %v", err)
	}
	defer pool.Close()

	serviceID := uuid.New()
	versionID := uuid.New()
	defer pool.Exec(context.Background(), `DELETE FROM fused_service_contract_snapshots WHERE service_version_id = $1`, versionID) //nolint:errcheck

	s := NewPostgresStore(pool).(*postgresStore)
	query := `query ListWidgets { widgets { id } }`
	endpoint := fusedobject.Endpoint{
		ID: uuid.New(), Name: "listWidgets", Method: "POST", Path: "/graphql", NormalizedPath: "/graphql",
		GraphQLQuery: &query, ProviderProtocol: "graphql", OperationKind: "query",
	}
	webhook := fusedobject.Webhook{ID: uuid.New(), Name: "widget.created", Method: "POST"}
	snapshot := ServiceContractSnapshot{
		ServiceID:        serviceID,
		ServiceVersionID: versionID,
		Version:          "2026-07-23",
		ServiceMetadata: fusedobject.ServiceMetadata{
			ID:               serviceID,
			ServiceVersionID: versionID,
			Name:             "Widgets",
			BaseURL:          "https://api.example.com",
		},
		Endpoints: []fusedobject.Endpoint{endpoint},
		Webhooks:  []fusedobject.Webhook{webhook},
	}

	if _, err := s.UpsertServiceContractSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("UpsertServiceContractSnapshot: %v", err)
	}
	metadata, err := s.GetServiceContractMetadata(ctx, serviceID, versionID)
	if err != nil {
		t.Fatalf("GetServiceContractMetadata: %v", err)
	}
	if metadata.Name != "Widgets" || metadata.ServiceVersionID != versionID {
		t.Fatalf("unexpected metadata: %#v", metadata)
	}
	gotEndpoint, err := s.GetServiceContractEndpointByName(ctx, serviceID, versionID, "listWidgets")
	if err != nil {
		t.Fatalf("GetServiceContractEndpointByName: %v", err)
	}
	if gotEndpoint.ID != endpoint.ID || gotEndpoint.GraphQLQuery == nil || *gotEndpoint.GraphQLQuery != query || gotEndpoint.ProviderProtocol != "graphql" || gotEndpoint.OperationKind != "query" {
		t.Fatalf("unexpected endpoint: %#v", gotEndpoint)
	}

	snapshot.ServiceMetadata.Name = "Widgets v2"
	updatedEndpoint := fusedobject.Endpoint{ID: uuid.New(), Name: "createWidget", Method: "POST", Path: "/widgets"}
	snapshot.Endpoints = []fusedobject.Endpoint{updatedEndpoint}
	if _, err := s.UpsertServiceContractSnapshot(ctx, snapshot); err != nil {
		t.Fatalf("UpsertServiceContractSnapshot replacement: %v", err)
	}
	operations, err := s.ListServiceContractOperations(ctx, serviceID, versionID)
	if err != nil {
		t.Fatalf("ListServiceContractOperations: %v", err)
	}
	if len(operations) != 1 || operations[0].Name != "createWidget" {
		t.Fatalf("expected replacement operation only, got %#v", operations)
	}
	byName, err := s.ListServiceContractEndpointsByNames(ctx, serviceID, versionID, []string{"createWidget", "missingWidget"})
	if err != nil {
		t.Fatalf("ListServiceContractEndpointsByNames: %v", err)
	}
	if len(byName) != 1 || byName[0].ID != updatedEndpoint.ID {
		t.Fatalf("expected SQL-filtered name lookup, got %#v", byName)
	}
	secondServiceID := uuid.New()
	secondVersionID := uuid.New()
	defer pool.Exec(context.Background(), `DELETE FROM fused_service_contract_snapshots WHERE service_version_id = $1`, secondVersionID) //nolint:errcheck
	secondEndpoint := fusedobject.Endpoint{ID: uuid.New(), Name: "listGadgets", Method: "GET", Path: "/gadgets"}
	if _, err := s.UpsertServiceContractSnapshot(ctx, ServiceContractSnapshot{
		ServiceID: secondServiceID, ServiceVersionID: secondVersionID, Version: "2026-07-24",
		ServiceMetadata: fusedobject.ServiceMetadata{ID: secondServiceID, ServiceVersionID: secondVersionID, Name: "Gadgets"},
		Endpoints:       []fusedobject.Endpoint{secondEndpoint},
	}); err != nil {
		t.Fatalf("UpsertServiceContractSnapshot second selection: %v", err)
	}
	intersection, err := s.ListServiceContractEndpointsForSelections(ctx, []ServiceContractEndpointSelection{
		{SelectionIndex: 0, ServiceID: serviceID, ServiceVersionID: versionID, EndpointIDs: []uuid.UUID{updatedEndpoint.ID}},
		{SelectionIndex: 1, ServiceID: secondServiceID, ServiceVersionID: secondVersionID, SelectAll: true},
	}, []string{"createWidget", "listGadgets", "missingWidget"})
	if err != nil {
		t.Fatalf("ListServiceContractEndpointsForSelections: %v", err)
	}
	if len(intersection) != 2 || intersection[0].SelectionIndex != 0 || intersection[0].Endpoint.ID != updatedEndpoint.ID || intersection[1].SelectionIndex != 1 || intersection[1].Endpoint.ID != secondEndpoint.ID {
		t.Fatalf("batched name/app-scope intersection returned %#v", intersection)
	}
	_, err = s.ListServiceContractEndpointsForSelections(ctx, []ServiceContractEndpointSelection{{
		SelectionIndex: 0, ServiceID: uuid.New(), ServiceVersionID: uuid.New(), SelectAll: true,
	}}, []string{"createWidget"})
	if !errors.Is(err, ErrServiceContractSnapshotNotFound) {
		t.Fatalf("missing strict snapshot error = %v, want %v", err, ErrServiceContractSnapshotNotFound)
	}
	byID, err := s.ListServiceContractEndpointsByIDs(ctx, serviceID, versionID, []uuid.UUID{updatedEndpoint.ID, uuid.New()})
	if err != nil {
		t.Fatalf("ListServiceContractEndpointsByIDs: %v", err)
	}
	if len(byID) != 1 || byID[0].Name != "createWidget" {
		t.Fatalf("expected SQL-filtered ID lookup, got %#v", byID)
	}
	if _, err := s.GetServiceContractEndpointByName(ctx, serviceID, versionID, "listWidgets"); err != ErrServiceContractEndpointNotFound {
		t.Fatalf("expected old endpoint removed, got %v", err)
	}
}
