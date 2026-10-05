package api

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// appAvailableAuthCandidate retains the complete AND requirement set for one compatible choice.
type appAvailableAuthCandidate struct {
	index     int
	selection models.SDKSelection
}

// resolveAvailableAppAuth chooses only a unique ready policy, using metadata batched by the actual selected bucket.
func resolveAvailableAppAuth(ctx context.Context, s store.Store, doc, previous sdkConfigDocument, services []sdkResolvedService, selections []models.SDKSelection, contracts map[string]sandbox.ServiceVersionExecutionAuthContract, buckets appBucketSet) error {
	grouped := make(map[uuid.UUID][]appAvailableAuthCandidate)
	for index, service := range services {
		// Only a new automatic selection may inspect credentials to change the provider default.
		if !canInferAppAuth(doc, previous, service) {
			continue
		}
		contract := contracts[executionAuthContractKey(service.ServiceID, service.Version, selections[index].OperationNames, selections[index].SelectAll)]
		candidates, err := availableAppAuthCandidates(selections[index], contract)
		// Invalid provider definitions must never be mistaken for missing credentials.
		if err != nil {
			return err
		}
		bucket := buckets.forService(service.ServiceID)
		for _, candidate := range candidates {
			grouped[bucket.ID] = append(grouped[bucket.ID], appAvailableAuthCandidate{index: index, selection: candidate})
		}
	}
	for bucketID, candidates := range grouped {
		// One presence query covers all candidate schemes for every service using this bucket.
		if err := chooseAvailableAppAuth(ctx, s, bucketID, candidates, services, selections); err != nil {
			return err
		}
	}
	return nil
}

// canInferAppAuth protects authored choices and the provider-order policy of previously published configs.
func canInferAppAuth(doc, previous sdkConfigDocument, service sdkResolvedService) bool {
	auth := doc.Services[service.PublicTarget].Auth
	// Explicit selectors and references remain authoritative even when another scheme has credentials.
	if auth != nil && (auth.Type != "" || auth.Name != "" || auth.Ref != "") {
		return false
	}
	_, published := previous.Services[service.ServiceName]
	return !published
}

// availableAppAuthCandidates reuses contract validation so credentials cannot authorize unsupported operations or partial AND requirements.
func availableAppAuthCandidates(selection models.SDKSelection, contract sandbox.ServiceVersionExecutionAuthContract) ([]models.SDKSelection, error) {
	secured := securedOperationSummaries(contract.Operations)
	// Anonymous and webhook-only selections must not gain an authentication requirement from stored secrets.
	if len(secured) == 0 {
		return nil, nil
	}
	choices := appAuthsAcceptingScopes(compatibleAppAuths(contract.AuthConfigs, secured, "", ""), selection.ConnectScopes)
	candidates := make([]models.SDKSelection, 0, len(choices))
	seen := make(map[string]bool)
	for _, auth := range choices {
		candidate := selection
		candidate.AuthType, candidate.AuthName = sandbox.CanonicalFusedAuthType(auth), sandbox.AuthCredentialName(auth)
		// The shared resolver expands each choice to all required credentials before checking availability.
		if err := resolveSelectionAuthPolicy(&candidate, contract, &sdkAuthResolutionTelemetry{}); err != nil {
			return nil, err
		}
		key, _ := json.Marshal(candidate.RequiredAuth)
		// Two names within the same AND branch represent one policy, not competing auth choices.
		if seen[string(key)] {
			continue
		}
		seen[string(key)] = true
		candidates = append(candidates, candidate)
	}
	return candidates, nil
}

// chooseAvailableAppAuth preserves provider ordering when none are ready and rejects ambiguity rather than silently picking credentials.
func chooseAvailableAppAuth(ctx context.Context, s store.Store, bucketID uuid.UUID, candidates []appAvailableAuthCandidate, services []sdkResolvedService, selections []models.SDKSelection) error {
	requested := make([]models.SDKSelection, 0, len(candidates))
	for _, candidate := range candidates {
		requested = append(requested, candidate.selection)
	}
	ready, keys, err := loadAppBucketMaterial(ctx, s, bucketID, requested)
	// An unavailable store cannot prove absence and must not cause fallback to provider ordering.
	if err != nil {
		return err
	}
	matches := make(map[int][]models.SDKSelection)
	for _, candidate := range candidates {
		// All fields of every required scheme must exist in this exact service and bucket.
		if len(missingAppBucketMaterial(candidate.selection, nil, ready, keys)) == 0 {
			matches[candidate.index] = append(matches[candidate.index], candidate.selection)
		}
	}
	for index, available := range matches {
		// More than one complete alternative needs an explicit, reviewable auth selector.
		if len(available) > 1 {
			labels := make([]string, 0, len(available))
			for _, candidate := range available {
				labels = append(labels, appAuthSchemeLabel(candidate.AuthName, candidate.AuthType))
			}
			err := appServiceValidationError{serviceID: selections[index].ServiceID, code: "app_auth_selection_required",
				reason: "has credentials for multiple compatible auth schemes; set auth.type and auth.name",
				detail: "Available schemes: " + strings.Join(labels, ", "), remedy: "Choose a service authentication scheme in the app editor or set services.<service>.auth.type and auth.name in config, then plan again."}
			return err.httpError(services[index])
		}
		selections[index] = available[0]
	}
	return nil
}

// retainPlannedAppAuth keeps a repeated plan for the same immutable version independent of later credential additions.
func retainPlannedAppAuth(service, previous sdkConfigServiceDoc) sdkConfigServiceDoc {
	// Any authored selector overrides inheritance and is still checked by immutable-version admission.
	if service.Auth == nil && previous.Auth != nil {
		service.Auth = previous.Auth
	}
	return service
}

// pinAppAuthReview saves the resolved scheme with desired state and exposes it in the existing plan summary.
func pinAppAuthReview(state *sdkConfigDocument, previous sdkConfigDocument, services []sdkResolvedService, selections []models.SDKSelection, summary []map[string]any) {
	for index, service := range services {
		selection := selections[index]
		// Anonymous and heterogeneous multi-scheme selections have no single selector to serialize.
		if selection.AuthType == "" || selection.AuthName == "" {
			continue
		}
		doc := state.Services[service.ServiceName]
		ref := ""
		// Preserve explicit managed or reusable credential references alongside the resolved selector.
		if doc.Auth != nil {
			ref = doc.Auth.Ref
		}
		resolved := &sdkAppAuthDoc{Type: selection.AuthType, Name: selection.AuthName, Ref: ref}
		_, published := previous.Services[service.ServiceName]
		// Pin new automatic choices without rewriting authored selectors or older immutable desired state.
		if doc.Auth == nil && !published {
			doc.Auth = resolved
			state.Services[service.ServiceName] = doc
		}
		summary[index]["auth"] = resolved
	}
}
