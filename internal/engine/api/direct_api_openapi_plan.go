package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/unified"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// validateDirectAPIOpenAPIPlan runs the production export projection against the prospective immutable scope before any API plan is stored.
func validateDirectAPIOpenAPIPlan(ctx context.Context, s store.Store, doc sdkConfigDocument, existingAppID uuid.UUID, selections []models.SDKSelection, compilation sdkUnifiedCompilation) error {
	// Generated SDKs retain their package contract and do not promise a package-free REST API export during this plan path.
	if sdkConfigGeneratesPackage(doc) {
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
		UnifiedDefinitionSchemaVersion: unified.DefinitionSchemaVersion, UnifiedDefinitions: compilation.DefinitionJSON,
		UnifiedDefinitionHash: compilation.DefinitionHash, UnifiedCodegenDescriptorHash: compilation.CodegenDescriptorHash,
		Status: store.AppStatusActive,
	}
	family := &store.AppFamily{Kind: store.AppKindSDK, DisplayName: doc.Name}
	_, err = buildAppOpenAPIDocument(ctx, contracts, app, family, "")
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
