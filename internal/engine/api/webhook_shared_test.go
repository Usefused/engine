package api

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
)

// authEventValidationCountingStore records mutable workspace lookups while serving one exact immutable SDK runtime.
type authEventValidationCountingStore struct {
	store.Store
	runtime            *store.AppRuntime
	runtimeCalls       int
	mutableLookupCalls int
}

// GetAppRuntime returns the exact SDK version used to authorize implicit event subjects.
func (s *authEventValidationCountingStore) GetAppRuntime(context.Context, uuid.UUID) (*store.AppRuntime, error) {
	s.runtimeCalls++
	return s.runtime, nil
}

// IsWorkspaceServiceEnabled counts mutable validation calls that reserved events must bypass.
func (s *authEventValidationCountingStore) IsWorkspaceServiceEnabled(context.Context, uuid.UUID) (bool, error) {
	s.mutableLookupCalls++
	return true, nil
}

// ListWorkspaceWebhooks counts fallback mutable validation calls that reserved events must bypass.
func (s *authEventValidationCountingStore) ListWorkspaceWebhooks(context.Context, uuid.UUID) ([]store.WorkspaceWebhook, error) {
	s.mutableLookupCalls++
	return nil, nil
}

// TestResolveWebhookAttachmentLabel_ReturnsAttachmentFromConfigState is the
// core isolation-fix assertion: the label comes entirely from the
// connecting SDK/MCP's own applied config (via fused_apps.config_key),
// never from anything the client reports itself -- see the function's doc
// comment and plans/plan-webhook-kind.md's subject-filter section.
func TestResolveWebhookAttachmentLabel_ReturnsAttachmentFromConfigState(t *testing.T) {
	appID := uuid.New()
	configKey := "sdk:jira-sdk:1.0.0"
	s := &workspaceTestStore{
		mockScopes: map[uuid.UUID]*store.AppRuntime{
			appID: {AppID: appID, ConfigKey: configKey},
		},
	}
	configStore := &mockConfigStore{state: &store.ConfigState{
		ConfigKey:    configKey,
		DesiredState: []byte(`{"apiVersion":"fused/v1","kind":"sdk","name":"jira-sdk","webhook_attachment":"team-x-webhooks","services":{}}`),
	}}

	label, err := resolveWebhookAttachmentLabel(context.Background(), configStore, s, appID)
	if err != nil {
		t.Fatalf("resolveWebhookAttachmentLabel: %v", err)
	}
	if label != "team-x-webhooks" {
		t.Fatalf("expected label %q, got %q", "team-x-webhooks", label)
	}
}

func TestResolveWebhookAttachmentLabel_NoScopeReturnsEmptyNotError(t *testing.T) {
	appID := uuid.New()
	s := &workspaceTestStore{mockScopes: map[uuid.UUID]*store.AppRuntime{}}
	configStore := &mockConfigStore{}

	label, err := resolveWebhookAttachmentLabel(context.Background(), configStore, s, appID)
	if err != nil {
		t.Fatalf("expected no error for an unknown sdk id, got %v", err)
	}
	if label != "" {
		t.Fatalf("expected empty label for an unknown sdk id, got %q", label)
	}
}

func TestResolveWebhookAttachmentLabel_ScopeWithNoConfigKeyReturnsEmpty(t *testing.T) {
	appID := uuid.New()
	s := &workspaceTestStore{
		mockScopes: map[uuid.UUID]*store.AppRuntime{appID: {AppID: appID}},
	}
	configStore := &mockConfigStore{}

	label, err := resolveWebhookAttachmentLabel(context.Background(), configStore, s, appID)
	if err != nil {
		t.Fatalf("expected no error for a scope with no config_key, got %v", err)
	}
	if label != "" {
		t.Fatalf("expected empty label for a scope with no config_key, got %q", label)
	}
}

func TestResolveWebhookAttachmentLabel_NoConfigStateReturnsEmpty(t *testing.T) {
	appID := uuid.New()
	s := &workspaceTestStore{
		mockScopes: map[uuid.UUID]*store.AppRuntime{appID: {AppID: appID, ConfigKey: "sdk:reader:1.0.0"}},
	}
	// mockConfigStore.state is nil with a nil error -- GetConfigState's real
	// "not found" shape (config_repository.go's scanConfigState), not an
	// error condition.
	configStore := &mockConfigStore{}

	label, err := resolveWebhookAttachmentLabel(context.Background(), configStore, s, appID)
	if err != nil {
		t.Fatalf("expected no error when the config state is missing, got %v", err)
	}
	if label != "" {
		t.Fatalf("expected empty label when the config state is missing, got %q", label)
	}
}

func TestResolveWebhookAttachmentLabel_NoAttachmentReturnsEmpty(t *testing.T) {
	appID := uuid.New()
	s := &workspaceTestStore{
		mockScopes: map[uuid.UUID]*store.AppRuntime{appID: {AppID: appID, ConfigKey: "sdk:reader:1.0.0"}},
	}
	configStore := &mockConfigStore{state: &store.ConfigState{
		DesiredState: []byte(`{"apiVersion":"fused/v1","kind":"sdk","name":"reader","services":{}}`),
	}}

	label, err := resolveWebhookAttachmentLabel(context.Background(), configStore, s, appID)
	if err != nil {
		t.Fatalf("expected no error for a config with no webhook_attachment, got %v", err)
	}
	if label != "" {
		t.Fatalf("expected empty label for a config with no webhook_attachment, got %q", label)
	}
}

func TestResolveWebhookAttachmentLabel_ConfigStateLookupErrorPropagates(t *testing.T) {
	appID := uuid.New()
	s := &workspaceTestStore{
		mockScopes: map[uuid.UUID]*store.AppRuntime{appID: {AppID: appID, ConfigKey: "sdk:reader:1.0.0"}},
	}
	configStore := &mockConfigStore{err: errors.New("db unavailable")}

	if _, err := resolveWebhookAttachmentLabel(context.Background(), configStore, s, appID); err == nil {
		t.Fatal("expected a config state lookup failure to propagate")
	}
}

// subjectSafeLabel is duplicated verbatim in sandbox/webhook.go (see that
// package's test for the equivalent case) -- both must apply the exact same
// substitution or a label containing "." would match on the publish side
// but not the consumer-filter side, silently dropping every delivery.
func TestSubjectSafeLabel_ReplacesDots(t *testing.T) {
	if got, want := subjectSafeLabel("team.x"), "team-x"; got != want {
		t.Fatalf("subjectSafeLabel(%q) = %q, want %q", "team.x", got, want)
	}
}

// TestBuildAuthEventFilterSubjectsScopesExactFamilyAndService proves implicit events cannot use a provider attachment label or another family.
func TestBuildAuthEventFilterSubjectsScopesExactFamilyAndService(t *testing.T) {
	accountID := uuid.New()
	familyID := uuid.New()
	serviceID := uuid.New()
	requested := serviceID.String() + ".fused.auth.connection.completed"

	filters, err := buildAuthEventFilterSubjects(accountID, familyID, map[uuid.UUID]struct{}{serviceID: {}}, []string{requested})
	if err != nil {
		t.Fatalf("buildAuthEventFilterSubjects: %v", err)
	}
	if len(filters) != 1 {
		t.Fatalf("filters = %#v", filters)
	}
	if !strings.Contains(filters[0], ".fused-auth."+familyID.String()+"."+serviceID.String()+".") {
		t.Fatalf("filter %q is not family/service scoped", filters[0])
	}
}

// TestBuildAuthEventFilterSubjectsRejectsUnselectedService prevents a shared bucket from granting event visibility.
func TestBuildAuthEventFilterSubjectsRejectsUnselectedService(t *testing.T) {
	requested := uuid.NewString() + ".fused.auth.token.refresh_failed"
	_, err := buildAuthEventFilterSubjects(uuid.New(), uuid.New(), map[uuid.UUID]struct{}{}, []string{requested})
	// Absence from this exact SDK version must fail the entire reserved subscription rather than silently broadening it.
	if err == nil {
		t.Fatal("unselected connected-auth service was accepted")
	}
}

// TestConnectedAuthSelectionServicesRequiresOAuthPath separates OAuth capability from merely selecting a service.
func TestConnectedAuthSelectionServicesRequiresOAuthPath(t *testing.T) {
	oauthServiceID := uuid.New()
	staticServiceID := uuid.New()
	services := connectedAuthSelectionServices([]models.SDKSelection{
		{ServiceID: oauthServiceID, AuthType: "openIdConnect"},
		{ServiceID: staticServiceID, AuthType: "apiKey"},
	})
	if _, ok := services[oauthServiceID]; !ok {
		t.Fatal("OIDC selection was not registered for implicit auth events")
	}
	if _, ok := services[staticServiceID]; ok {
		t.Fatal("static-auth selection was registered for implicit auth events")
	}
}

// TestConnectedAuthSelectionServicesAdmitsRequiredMixedAuthButNotUnusedOAuth proves only persisted selected auth members grant the implicit surface.
func TestConnectedAuthSelectionServicesAdmitsRequiredMixedAuthButNotUnusedOAuth(t *testing.T) {
	mixedServiceID := uuid.New()
	unusedAlternativeServiceID := uuid.New()
	services := connectedAuthSelectionServices([]models.SDKSelection{
		{
			ServiceID: mixedServiceID,
			RequiredAuth: []models.SDKRequiredAuth{
				{AuthType: "oauth2", AuthName: "user"},
				{AuthType: "mutualTLS", AuthName: "client"},
			},
		},
		{
			ServiceID: unusedAlternativeServiceID,
			AuthType:  "apiKey",
			RequiredAuth: []models.SDKRequiredAuth{
				{AuthType: "apiKey", AuthName: "selected-key"},
			},
		},
	})
	if _, ok := services[mixedServiceID]; !ok {
		t.Fatal("selected OAuth plus mTLS requirement was not admitted")
	}
	// OAuth declared elsewhere by the service is absent from the persisted selected members and cannot grant delivery.
	if _, ok := services[unusedAlternativeServiceID]; ok {
		t.Fatal("unused OAuth alternative granted implicit auth events")
	}
}

// TestBuildFilterSubjectsReservesFusedAuthNames proves a user-created provider attachment cannot reproduce a system subscription.
func TestBuildFilterSubjectsReservesFusedAuthNames(t *testing.T) {
	accountID := uuid.New()
	serviceID := uuid.New()
	filters := buildFilterSubjects(accountID, "fused-auth", []string{serviceID.String() + ".fused.auth.connection.completed"})
	// Reserved semantic names never become provider subjects even when the user label resembles the system marker.
	if len(filters) != 0 {
		t.Fatalf("provider filters = %#v", filters)
	}
}

// TestFusedAuthSubjectsNeverPublishProviderAnalytics preserves one transport without merging provider and Engine-owned accounting.
func TestFusedAuthSubjectsNeverPublishProviderAnalytics(t *testing.T) {
	accountID := uuid.New()
	familyID := uuid.New()
	serviceID := uuid.New()
	authSubject := messaging.FusedAuthWebhookSubject(accountID, familyID, serviceID, "fused.auth.token.refresh_failed")
	if shouldPublishWebhookAnalytics(authSubject) {
		t.Fatal("Fused auth subject was admitted to provider analytics")
	}
	providerSubject := "webhooks." + accountID.String() + "." + serviceID.String() + ".jira.issue.created"
	if !shouldPublishWebhookAnalytics(providerSubject) {
		t.Fatal("provider webhook subject was excluded from provider analytics")
	}
}

// TestValidateRequestedEventsUsesExactWebhookSelections proves provider validation is bounded by immutable app data rather than per-event queries.
func TestValidateRequestedEventsUsesExactWebhookSelections(t *testing.T) {
	serviceID := uuid.New()
	otherServiceID := uuid.New()
	selections, err := json.Marshal([]models.SDKSelection{{
		SchemaVersion: models.AppSelectionSchemaVersion, ServiceID: serviceID, ServiceVersionID: uuid.New(),
		WebhookNames: []string{"issue.created", "issue.updated"},
	}})
	if err != nil {
		t.Fatalf("marshal selections: %v", err)
	}
	runtime := &store.AppRuntime{ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: selections}
	events, err := validateRequestedEvents(runtime, []string{
		serviceID.String() + ".issue.created",
		serviceID.String() + ".issue.updated",
		serviceID.String() + ".issue.deleted",
		otherServiceID.String() + ".issue.created",
	})
	if err != nil {
		t.Fatalf("validateRequestedEvents: %v", err)
	}
	if len(events) != 2 || events[0] != serviceID.String()+".issue.created" || events[1] != serviceID.String()+".issue.updated" {
		t.Fatalf("validated provider events = %#v", events)
	}
}

// TestResolveAuthEventFilterSubjectsUsesExactSDKVersion proves sibling versions cannot borrow each other's OAuth service selection.
func TestResolveAuthEventFilterSubjectsUsesExactSDKVersion(t *testing.T) {
	appID := uuid.New()
	accountID := uuid.New()
	familyID := uuid.New()
	oauthServiceID := uuid.New()
	staticServiceID := uuid.New()
	selections, err := json.Marshal([]models.SDKSelection{
		{SchemaVersion: models.AppSelectionSchemaVersion, ServiceID: oauthServiceID, ServiceVersionID: uuid.New(), AuthType: "oauth2", EndpointIDs: []uuid.UUID{uuid.New()}},
		{SchemaVersion: models.AppSelectionSchemaVersion, ServiceID: staticServiceID, ServiceVersionID: uuid.New(), AuthType: "apiKey", EndpointIDs: []uuid.UUID{uuid.New()}},
	})
	if err != nil {
		t.Fatalf("marshal selections: %v", err)
	}
	s := &workspaceTestStore{mockScopes: map[uuid.UUID]*store.AppRuntime{
		appID: {
			AppID: appID, AppFamilyID: familyID, AccountID: accountID, Kind: store.AppKindSDK,
			ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: selections,
		},
	}}
	allowed := oauthServiceID.String() + ".fused.auth.connection.reconnect_required"
	filters, err := resolveAuthEventFilterSubjects(context.Background(), s, appID, accountID, familyID, []string{allowed})
	if err != nil || len(filters) != 1 {
		t.Fatalf("OAuth filters = %#v, %v", filters, err)
	}
	denied := staticServiceID.String() + ".fused.auth.connection.reconnect_required"
	_, err = resolveAuthEventFilterSubjects(context.Background(), s, appID, accountID, familyID, []string{denied})
	// Static auth in this exact version must remain denied even if another sibling version selects OAuth for the same service.
	if err == nil {
		t.Fatal("static-auth exact version received an implicit OAuth event subject")
	}
}

// TestMultipleAuthEventRequestsUseOneExactRuntimeRead proves implicit registration has no per-event workspace-query growth.
func TestMultipleAuthEventRequestsUseOneExactRuntimeRead(t *testing.T) {
	appID := uuid.New()
	accountID := uuid.New()
	familyID := uuid.New()
	serviceID := uuid.New()
	selections, err := json.Marshal([]models.SDKSelection{{
		SchemaVersion: models.AppSelectionSchemaVersion, ServiceID: serviceID, ServiceVersionID: uuid.New(),
		AuthType: "oauth2", EndpointIDs: []uuid.UUID{uuid.New()},
	}})
	if err != nil {
		t.Fatalf("marshal selections: %v", err)
	}
	s := &authEventValidationCountingStore{runtime: &store.AppRuntime{
		AppID: appID, AppFamilyID: familyID, AccountID: accountID, Kind: store.AppKindSDK,
		ScopeSchemaVersion: models.AppScopeSchemaVersion, Selections: selections,
	}}
	requests := []string{
		serviceID.String() + ".fused.auth.connection.completed",
		serviceID.String() + ".fused.auth.token.refreshed",
		serviceID.String() + ".fused.auth.token.refresh_failed",
		serviceID.String() + ".fused.auth.connection.reconnect_required",
	}
	providerRequests, authRequests := partitionWebhookEventRequests(requests)
	runtime, err := s.GetAppRuntime(context.Background(), appID)
	if err != nil {
		t.Fatalf("GetAppRuntime: %v", err)
	}
	_, err = validateRequestedEvents(runtime, providerRequests)
	if err != nil {
		t.Fatalf("validateRequestedEvents: %v", err)
	}
	filters, err := authEventFilterSubjectsForRuntime(runtime, accountID, familyID, authRequests)
	if err != nil || len(filters) != len(requests) {
		t.Fatalf("auth filters = %#v, %v", filters, err)
	}
	if s.runtimeCalls != 1 || s.mutableLookupCalls != 0 {
		t.Fatalf("runtime calls = %d, mutable lookup calls = %d", s.runtimeCalls, s.mutableLookupCalls)
	}
}
