package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// validateDirectAPIOpenAPIPlan runs the production export projection against the prospective immutable scope before any API plan is stored.
func validateDirectAPIOpenAPIPlan(ctx context.Context, s store.Store, doc sdkConfigDocument, existingAppID uuid.UUID, selections []models.SDKSelection) error {
	// Unified Apps publish their authored manifest; only package-free SDKs need this physical OpenAPI projection.
	if doc.Kind == store.AppKindUnifiedApp.String() || sdkConfigGeneratesPackage(doc) {
		return nil
	}
	contracts, ok := s.(appOpenAPIContractStore)
	// A direct API cannot be published when its Engine cannot read the same immutable schemas used by export.
	if !ok {
		return workspaceConfigHTTPError{
			status: http.StatusServiceUnavailable, code: "app_openapi_schema_unavailable", category: "dependency",
			message:     "Immutable operation schemas are unavailable for this REST API.",
			remediation: "Restore Engine immutable service-contract storage, then create a new API plan.",
		}
	}
	encodedSelections, err := json.Marshal(selections)
	// Selection serialization must complete before the real export projection is allowed to inspect it.
	if err != nil {
		return workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to encode direct API selections"}
	}
	appID := directAPIOpenAPIPlanID(existingAppID, doc)
	app := &store.App{
		AppID: appID, Version: doc.Version, ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: encodedSelections,
		Status: store.AppStatusActive,
	}
	family := &store.AppFamily{Kind: store.AppKindSDK, DisplayName: doc.Name}
	bridges, err := directAPIPlanBridgeBindings(ctx, s, doc)
	// Exact local schemas must resolve before a direct API can claim exportable scope.
	if err != nil {
		return err
	}
	_, err = buildAppOpenAPIDocument(ctx, contracts, app, family, "", bridges...)
	// The exporter already classifies schema absence and inconsistency with a stable repairable error code.
	if err != nil {
		return err
	}
	return nil
}

// directAPIOpenAPIPlanID uses the real immutable identity when it exists and a deterministic placeholder before a new plan allocates one.
func directAPIOpenAPIPlanID(existingAppID uuid.UUID, doc sdkConfigDocument) uuid.UUID {
	// Existing versions must validate the literal namespaces, route enum, and setup command that export will publish.
	if existingAppID != uuid.Nil {
		return existingAppID
	}
	// New app identity is allocated during apply, while schema validity remains independent of the deterministic placeholder value.
	return uuid.NewSHA1(uuid.NameSpaceOID, []byte("direct-api-openapi-plan\x00"+doc.Name+"\x00"+doc.Version))
}

// directAPIPlanBridgeBindings reuses contract admission so OpenAPI never trusts client-supplied local schemas.
func directAPIPlanBridgeBindings(ctx context.Context, s store.Store, doc sdkConfigDocument) ([]models.BridgeBinding, error) {
	// Existing provider plans need no new control actor or Bridge store dependency.
	if len(doc.Bridges) == 0 {
		return nil, nil
	}
	actor, ok := accesscontrol.ActorFromContext(ctx)
	// Bridge schema discovery requires the same authenticated workspace authority as publication.
	if !ok {
		return nil, accesscontrol.ErrAuthenticationRequired
	}
	return resolveBridgeAttachments(ctx, s, actor.AccountID, doc)
}
