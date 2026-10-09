package api

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"sort"
)

// resolveImportedMCPSelections pins actor-visible approved catalogs into the ordinary immutable app scope.
func resolveImportedMCPSelections(ctx context.Context, s store.Store, call sdkPlanCall, selections []models.SDKSelection, services []sdkResolvedService, buckets appBucketSet) ([]models.SDKSelection, error) {
	for i := range selections {
		request := importedMCPRequest(call.document, services, selections[i].ServiceID)
		// Physical-only services need no catalog authority.
		if request == nil {
			continue
		}
		binding, err := resolveImportedMCPBinding(ctx, s, call.actor.SubjectID, selections[i], request, buckets.forService(selections[i].ServiceID))
		// One invalid imported selection invalidates the complete plan.
		if err != nil {
			return nil, err
		}
		selections[i].ImportedMCP = binding
	}
	return selections, nil
}

// importedMCPRequest matches the authored service key to its already-resolved identity.
func importedMCPRequest(doc sdkConfigDocument, services []sdkResolvedService, id uuid.UUID) *models.ImportedMCPSelection {
	for _, service := range services {
		// Resolve aliases once; a provider display name is not another service's identity.
		if service.ServiceID == id {
			return doc.Services[service.PublicTarget].MCP
		}
	}
	return nil
}

// resolveImportedMCPBinding copies metadata only after exact revision and credential-bucket admission.
func resolveImportedMCPBinding(ctx context.Context, s store.Store, subject uuid.UUID, selection models.SDKSelection, request *models.ImportedMCPSelection, bucket store.Bucket) (*models.ImportedMCPBinding, error) {
	repository, ok := s.(store.MCPCatalogStore)
	// A missing repository cannot turn the authored request into unchecked metadata.
	if !ok {
		return nil, store.ErrMCPCatalogUnavailable
	}
	snapshot, err := repository.GetMCPCatalog(ctx, store.MCPCatalogScope{ServiceID: selection.ServiceID, VersionID: selection.ServiceVersionID, SubjectID: subject})
	// The browser selects a revision, never supplies its definitions or upstream URL.
	if err != nil || snapshot == nil || snapshot.ID != request.RevisionID {
		return nil, errors.New("imported MCP catalog changed or is unavailable; select its capabilities again")
	}
	// Credential-derived catalogs cannot silently delegate a different bucket's identity.
	if snapshot.SecretName != "" && snapshot.BucketID != bucket.ID {
		return nil, errors.New("imported MCP requires the service's app bucket to match its approved connection bucket")
	}
	// Current bucket-use rights remain required when metadata was discovered with credentials.
	if err = authorizeMCPCatalogBucket(ctx, snapshot.BucketID); err != nil {
		return nil, err
	}
	return selectImportedMCPBinding(snapshot, request)
}

// selectImportedMCPBinding preserves exact definitions and rejects empty or unknown selections.
func selectImportedMCPBinding(snapshot *store.MCPCatalogSnapshot, request *models.ImportedMCPSelection) (*models.ImportedMCPBinding, error) {
	result := &models.ImportedMCPBinding{RevisionID: snapshot.ID, URL: snapshot.URL, SecretName: snapshot.SecretName}
	groups := []struct {
		wanted []string
		source []json.RawMessage
		target *[]json.RawMessage
		key    string
	}{
		{request.Tools, snapshot.Catalog.Tools, &result.Tools, "name"}, {request.Prompts, snapshot.Catalog.Prompts, &result.Prompts, "name"},
		{request.Resources, snapshot.Catalog.Resources, &result.Resources, "uri"}, {request.ResourceTemplates, snapshot.Catalog.ResourceTemplates, &result.ResourceTemplates, "uriTemplate"},
	}
	count := 0
	for _, group := range groups {
		values, err := selectImportedMCPItems(group.source, group.wanted, group.key)
		// Selection admission is all-or-nothing across protocol namespaces.
		if err != nil {
			return nil, err
		}
		*group.target = values
		count += len(values)
	}
	// An explicit MCP object must grant at least one capability.
	if count == 0 {
		return nil, errors.New("select at least one imported MCP capability")
	}
	return result, nil
}

// selectImportedMCPItems rejects stale, duplicate and unknown identifiers without accepting caller-authored schemas.
func selectImportedMCPItems(source []json.RawMessage, wanted []string, key string) ([]json.RawMessage, error) {
	available := map[string]json.RawMessage{}
	for _, raw := range source {
		var item map[string]json.RawMessage
		_ = json.Unmarshal(raw, &item)
		var name string
		_ = json.Unmarshal(item[key], &name)
		available[name] = raw
	}
	result := make([]json.RawMessage, 0, len(wanted))
	for _, name := range wanted {
		raw, ok := available[name]
		// Deleting consumed identities also rejects duplicate requested grants.
		if !ok {
			return nil, errors.New("unknown or duplicate imported MCP capability")
		}
		result = append(result, raw)
		delete(available, name)
	}
	return result, nil
}

// hasAppServiceCapabilities allows imported-only services without granting any implicit endpoint scope.
func hasAppServiceCapabilities(service sdkConfigServiceDoc) bool {
	return len(service.Operations) > 0 || service.SelectAll || len(service.Webhooks) > 0 || service.WebhooksSelectAll || service.MCP != nil
}

// canonicalImportedMCPSelection stabilizes immutable source comparisons without trimming provider-owned names.
func canonicalImportedMCPSelection(selection *models.ImportedMCPSelection) *models.ImportedMCPSelection {
	// Absent imported selections retain their historical document shape.
	if selection == nil {
		return nil
	}
	result := *selection
	result.Tools = append([]string(nil), selection.Tools...)
	sort.Strings(result.Tools)
	result.Prompts = append([]string(nil), selection.Prompts...)
	sort.Strings(result.Prompts)
	result.Resources = append([]string(nil), selection.Resources...)
	sort.Strings(result.Resources)
	result.ResourceTemplates = append([]string(nil), selection.ResourceTemplates...)
	sort.Strings(result.ResourceTemplates)
	return &result
}
