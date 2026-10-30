package api

import (
	"context"
	"errors"
	"net/http"
	"strconv"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type mcpOAuthServiceChoice struct {
	Connections []store.OAuthMCPConnectionChoice
	Index       int
	Name        string
	Connected   bool
	Operations  []string
}
type mcpOAuthConsentPage struct {
	Name, Resource, AppID string
	Services              []mcpOAuthServiceChoice
}

// consentPage joins immutable operation provenance with one authorized service-summary batch.
func (h *mcpOAuthHandler) consentPage(ctx context.Context, actor accesscontrol.Actor, resource string) (mcpOAuthConsentPage, error) {
	item, err := h.loadApp(ctx, actor, resource)
	// No catalogue is exposed until exact server delegation has been authorized.
	if err != nil {
		return mcpOAuthConsentPage{}, err
	}
	catalogue, err := loadMCPAppOperationCatalogue(ctx, h.store, *item)
	// Partial catalogues cannot establish a safe membership check for submitted actions.
	if err != nil {
		return mcpOAuthConsentPage{}, err
	}
	repository := h.store.(store.AppCatalogRepository)
	summaries, err := repository.ListAuthorizedAppServiceSummaries(ctx, actor.AccountID, item.AppID, accesscontrol.AuthorizedScope{IDs: []uuid.UUID{item.AppFamilyID}})
	// Service labels and credential-routing slugs must come from the authorized store.
	if err != nil {
		return mcpOAuthConsentPage{}, err
	}
	page := mcpOAuthConsentPage{Name: item.Name, Resource: resource, AppID: item.AppID.String()}
	byService := make(map[uuid.UUID]int, len(summaries))
	// Stable service ordering also binds each connected-account field to its service.
	for i, summary := range summaries {
		byService[summary.ServiceID] = i
		page.Services = append(page.Services, mcpOAuthServiceChoice{Index: i, Name: summary.ServiceName})
	}
	// Operations retain exact service provenance, including native imported MCP grants.
	for _, operation := range catalogue.Operations {
		i, found := byService[operation.ServiceID]
		// Corrupt service provenance must fail the whole consent view.
		if !found {
			return mcpOAuthConsentPage{}, errors.New("missing service provenance")
		}
		page.Services[i].Operations = append(page.Services[i].Operations, operation.OperationID)
	}
	// Authentication requirements are projected separately from operation membership.
	if err := markMCPConnectedServices(&page, byService, item.Selections); err != nil {
		return mcpOAuthConsentPage{}, err
	}
	choices, err := h.connectionChoices(ctx, item.AppID)
	// Provider-account lookup is bounded and shares the already-authorized immutable version.
	if err != nil {
		return mcpOAuthConsentPage{}, err
	}
	attachMCPConnectionChoices(&page, byService, choices)
	return page, nil
}

// attachMCPConnectionChoices groups an already SQL-authorized account catalogue without per-service queries.
func attachMCPConnectionChoices(page *mcpOAuthConsentPage, byService map[uuid.UUID]int, choices []store.OAuthMCPConnectionChoice) {
	// Membership was established by the exact-version SQL catalogue.
	for _, choice := range choices {
		// A stale or corrupt choice must not attach to a different service index.
		if i, ok := byService[choice.ServiceID]; ok {
			page.Services[i].Connections = append(page.Services[i].Connections, choice)
		}
	}
}

// markMCPConnectedServices requires account consent whenever the version exposes a connected authentication alternative.
func markMCPConnectedServices(page *mcpOAuthConsentPage, byService map[uuid.UUID]int, selections []models.SDKSelection) error {
	// Connected provider credentials are selected once at consent, then fixed on the runtime token.
	for _, selection := range selections {
		i, found := byService[selection.ServiceID]
		// A selection without a matching summary cannot safely route credentials.
		if !found {
			return errors.New("missing service summary")
		}
		page.Services[i].Connected = mcpSelectionNeedsAccount(selection)
	}
	return nil
}

// mcpSelectionNeedsAccount respects a pinned static scheme while requiring consent for unpinned connected alternatives.
func mcpSelectionNeedsAccount(selection models.SDKSelection) bool {
	// An explicitly selected scheme is authoritative over other contract alternatives.
	if selection.AuthName != "" {
		return selection.AuthType == "oauth" || selection.AuthType == "oidc"
	}
	// Unpinned schemes still need an account when any callable alternative uses connected authentication.
	for _, required := range selection.RequiredAuth {
		// Only connected authentication alternatives require a user-account selection.
		if required.AuthType == "oauth" || required.AuthType == "oidc" {
			return true
		}
	}
	return false
}

// submittedGrant checks exact action membership and pins connected-user bindings before code issuance.
func (h *mcpOAuthHandler) submittedGrant(r *http.Request, actor accesscontrol.Actor) (store.OAuthMCPGrant, error) {
	page, err := h.consentPage(r.Context(), actor, r.PostForm.Get("resource"))
	// A promotion during consent requires a fresh review of the new server version.
	if err != nil || page.AppID != r.PostForm.Get("app_id") {
		return store.OAuthMCPGrant{}, errors.New("server changed")
	}
	id, _ := h.resourceID(page.Resource)
	target, err := h.store.ResolveMCPRoute(r.Context(), id)
	// A concurrent deactivation or promotion cannot issue against an unseen version.
	if err != nil || target == nil || target.AppID.String() != page.AppID {
		return store.OAuthMCPGrant{}, errors.New("server changed")
	}
	grant := store.OAuthMCPGrant{Resource: page.Resource, AppID: target.AppID, AppFamilyID: target.AppFamilyID}
	selected, err := selectedMCPOperations(r.PostForm["operation"], page.Services)
	// Empty, duplicate, unknown, and oversized selections are all rejected.
	if err != nil {
		return grant, err
	}
	grant.Operations = r.PostForm["operation"]
	grant.Connections, err = submittedMCPBindings(r, page.Services, selected)
	return grant, err
}

// submittedMCPBindings collects only reviewed accounts for services whose actions were selected.
func submittedMCPBindings(r *http.Request, services []mcpOAuthServiceChoice, selected map[int]bool) ([]store.OAuthMCPConnectionBinding, error) {
	bindings := make([]store.OAuthMCPConnectionBinding, 0)
	// Only services with selected actions contribute credential bindings.
	for _, service := range services {
		// Unselected services must not contribute any provider-account authority.
		if !selected[service.Index] || !service.Connected {
			continue
		}
		binding, err := submittedMCPBinding(r, service)
		// No connected service may silently fall back to caller-supplied account selectors.
		if err != nil {
			return nil, err
		}
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

// selectedMCPOperations produces the exact service set implied by the submitted explicit allowlist.
func selectedMCPOperations(operations []string, services []mcpOAuthServiceChoice) (map[int]bool, error) {
	// A missing selection must never acquire the manual token API's wildcard default.
	if len(operations) == 0 || len(operations) > 1000 {
		return nil, errors.New("choose between 1 and 1000 actions")
	}
	available := make(map[string]int)
	// Build membership from the immutable catalogue, never from form-provided service IDs.
	for _, service := range services {
		// The immutable catalogue defines every selectable invocation name.
		for _, operation := range service.Operations {
			available[operation] = service.Index
		}
	}
	selected := make(map[int]bool)
	seen := make(map[string]bool)
	// Reject injection of an unlisted action, including another server's action names.
	for _, operation := range operations {
		i, found := available[operation]
		// Unknown and repeated names cannot turn a submitted list into broader authority.
		if !found || seen[operation] {
			return nil, errors.New("invalid action selection")
		}
		seen[operation] = true
		selected[i] = true
	}
	return selected, nil
}

// submittedMCPBinding identifies one existing provider connection without accepting provider secrets.
func submittedMCPBinding(r *http.Request, service mcpOAuthServiceChoice) (store.OAuthMCPConnectionBinding, error) {
	value := r.PostForm.Get("account_" + strconv.Itoa(service.Index))
	// Choices come from a fresh authorized lookup, so a removed or foreign account cannot be injected.
	for _, choice := range service.Connections {
		// Only a freshly authorized opaque choice can supply connection identities.
		if value == choice.Value {
			return store.OAuthMCPConnectionBinding{ServiceID: choice.ServiceID, AuthName: choice.AuthName, ConnectionID: choice.ConnectionID, ResourceID: choice.ResourceID}, nil
		}
	}
	return store.OAuthMCPConnectionBinding{}, errors.New("choose a connected account")
}

// connectionChoices requires the bounded connection catalogue rather than exposing credential storage.
func (h *mcpOAuthHandler) connectionChoices(ctx context.Context, appID uuid.UUID) ([]store.OAuthMCPConnectionChoice, error) {
	repository, ok := h.store.(interface {
		OAuthMCPConnections(context.Context, uuid.UUID) ([]store.OAuthMCPConnectionChoice, error)
	})
	// Missing storage support cannot produce a consent form that accepts arbitrary references.
	if !ok {
		return nil, errors.New("MCP connection catalogue unavailable")
	}
	return repository.OAuthMCPConnections(ctx, appID)
}
