package store

import (
	"context"
	"encoding/json"
	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"testing"
)

// TestImportedOnlyAppSummariesAcceptNullPhysicalLists reproduces the CLI-created app's nil endpoint serialization against PostgreSQL.
func TestImportedOnlyAppSummariesAcceptNullPhysicalLists(t *testing.T) {
	fixture := newAppTokenPolicyFixture(t)
	serviceID := uuid.New()
	_, err := fixture.pool.Exec(fixture.ctx, `INSERT INTO fused_workspace_services (service_id,service_slug,service_name) VALUES ($1,$2,'Imported-only fixture')`, serviceID, "mcp-null-"+serviceID.String())
	// A real workspace row is required to exercise the production summary join.
	if err != nil {
		t.Fatal(err)
	}
	// Cleanup is restricted to this test's generated service identity.
	t.Cleanup(func() {
		_, _ = fixture.pool.Exec(context.Background(), `DELETE FROM fused_workspace_services WHERE service_id=$1`, serviceID)
	})
	selection := models.SDKSelection{ServiceID: serviceID, ServiceVersionID: uuid.New(), ImportedMCP: &models.ImportedMCPBinding{RevisionID: uuid.New(), Tools: []json.RawMessage{json.RawMessage(`{"name":"echo","inputSchema":{"type":"object"}}`)}}}
	raw, err := json.Marshal([]models.SDKSelection{selection})
	// Marshaling nil endpoint IDs deliberately produces the JSON null that previously crashed SQL counting.
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.pool.Exec(fixture.ctx, `UPDATE fused_apps SET selections=$2::jsonb WHERE app_id=$1`, fixture.appID, raw)
	// Persist the fixture through the same jsonb selection column used by app apply.
	if err != nil {
		t.Fatal(err)
	}
	var accountID uuid.UUID
	err = fixture.pool.QueryRow(fixture.ctx, `SELECT account_id FROM fused_apps WHERE app_id=$1`, fixture.appID).Scan(&accountID)
	// Account identity is owned by the isolated fixture, not a shared local workspace.
	if err != nil {
		t.Fatal(err)
	}
	scope := accesscontrol.AuthorizedScope{All: true}
	services, err := fixture.repository.ListAuthorizedAppServiceSummaries(fixture.ctx, accountID, fixture.appID, scope)
	// Imported scope remains visible while both physical counters stay at zero.
	if err != nil || len(services) != 1 || services[0].EndpointCount != 0 || services[0].WebhookCount != 0 {
		t.Fatalf("services=%+v err=%v", services, err)
	}
	consumers, err := fixture.repository.ListServiceConsumers(fixture.ctx, accountID, scope, serviceID)
	// Dependency checks must retain the imported-only app instead of failing or allowing unsafe service removal.
	if err != nil || len(consumers) != 1 || consumers[0].AppID != fixture.appID {
		t.Fatalf("consumers=%+v err=%v", consumers, err)
	}
}
