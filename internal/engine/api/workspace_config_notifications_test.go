package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/store"
)

// TestWorkspaceNotificationMatches_ServiceAndDeclaredVersion is the core
// positive case: a notification scoped to a service+version this workspace
// config actually declares is a match, mirroring sdkNotificationMatches'
// own service+version branch (sdk_config_handlers.go) -- workspace has no
// single config_key of its own to fast-path against the way SDK does, so
// this is the whole check.
func TestWorkspaceNotificationMatches_ServiceAndDeclaredVersion(t *testing.T) {
	serviceID := uuid.New()
	serviceVersions := map[uuid.UUID][]string{serviceID: {"2026-01-01", "2026-06-01"}}
	notification := store.WorkspaceNotification{ServiceID: &serviceID, Version: "2026-06-01"}
	if !workspaceNotificationMatches(notification, serviceVersions) {
		t.Fatalf("expected match for a declared service+version")
	}
}

func TestWorkspaceNotificationMatches_ServiceWideVersionAlwaysMatches(t *testing.T) {
	serviceID := uuid.New()
	serviceVersions := map[uuid.UUID][]string{serviceID: {"2026-01-01"}}
	notification := store.WorkspaceNotification{ServiceID: &serviceID, Version: ""}
	if !workspaceNotificationMatches(notification, serviceVersions) {
		t.Fatalf("expected a service-wide (Version==\"\") notification to match regardless of declared versions")
	}
}

func TestWorkspaceNotificationMatches_VersionNotDeclared_NoMatch(t *testing.T) {
	serviceID := uuid.New()
	serviceVersions := map[uuid.UUID][]string{serviceID: {"2026-01-01"}}
	notification := store.WorkspaceNotification{ServiceID: &serviceID, Version: "2099-01-01"}
	if workspaceNotificationMatches(notification, serviceVersions) {
		t.Fatalf("expected no match for a version this workspace config never declared")
	}
}

func TestWorkspaceNotificationMatches_UnknownService_NoMatch(t *testing.T) {
	serviceVersions := map[uuid.UUID][]string{uuid.New(): {"2026-01-01"}}
	other := uuid.New()
	notification := store.WorkspaceNotification{ServiceID: &other, Version: "2026-01-01"}
	if workspaceNotificationMatches(notification, serviceVersions) {
		t.Fatalf("expected no match for a service this workspace config doesn't declare at all")
	}
}

func TestWorkspaceNotificationMatches_NilServiceID_NoMatch(t *testing.T) {
	notification := store.WorkspaceNotification{ServiceID: nil}
	if workspaceNotificationMatches(notification, map[uuid.UUID][]string{}) {
		t.Fatalf("expected no match for a notification with no service_id at all")
	}
}

func TestWorkspaceServiceVersionsMap(t *testing.T) {
	svcA, svcB := uuid.New(), uuid.New()
	desired := workspaceDesiredState{
		Services: map[uuid.UUID]workspaceDesiredService{
			svcA: {ServiceID: svcA, Versions: []string{"v1", "v2"}},
			svcB: {ServiceID: svcB, Versions: []string{"v1"}},
		},
	}
	got := workspaceServiceVersionsMap(desired)
	if len(got[svcA]) != 2 || got[svcA][0] != "v1" || got[svcA][1] != "v2" {
		t.Fatalf("expected svcA versions [v1 v2], got %#v", got[svcA])
	}
	if len(got[svcB]) != 1 || got[svcB][0] != "v1" {
		t.Fatalf("expected svcB versions [v1], got %#v", got[svcB])
	}
}

func TestFilterWorkspaceEngineNotifications(t *testing.T) {
	matchingService := uuid.New()
	otherService := uuid.New()
	serviceVersions := map[uuid.UUID][]string{matchingService: {"2026-01-01"}}
	notifications := []store.WorkspaceNotification{
		{ID: uuid.New(), Type: store.WorkspaceNotificationTypeRegistryVersionAdded, ServiceID: &matchingService, Version: "2026-01-01", Message: "matches"},
		{ID: uuid.New(), Type: store.WorkspaceNotificationTypeRegistryVersionAdded, ServiceID: &otherService, Version: "2026-01-01", Message: "wrong service"},
		{ID: uuid.New(), Type: store.WorkspaceNotificationTypeRegistryVersionAdded, ServiceID: &matchingService, Version: "2099-01-01", Message: "wrong version"},
	}
	items := filterWorkspaceEngineNotifications(notifications, serviceVersions)
	if len(items) != 1 || items[0].Message != "matches" {
		t.Fatalf("expected exactly the one matching notification, got %#v", items)
	}
}

// mockNotificationOnlyConfigStore is a narrow test double for
// collectWorkspacePlanNotifications -- only ListWorkspaceNotifications is
// exercised, everything else panics on the embedded nil interface if ever
// called, making an accidental extra call fail loudly.
type mockNotificationOnlyConfigStore struct {
	store.ConfigRepository
	notifications []store.WorkspaceNotification
	listErr       error
}

func (m *mockNotificationOnlyConfigStore) ListWorkspaceNotifications(ctx context.Context, status store.WorkspaceNotificationStatus) ([]store.WorkspaceNotification, error) {
	return m.notifications, m.listErr
}

func TestCollectWorkspacePlanNotifications_StoreError_ReturnsWarningNotError(t *testing.T) {
	configStore := &mockNotificationOnlyConfigStore{listErr: errors.New("db unavailable")}
	inbox := collectWorkspacePlanNotifications(context.Background(), configStore, map[uuid.UUID][]string{})
	if len(inbox.Items) != 0 {
		t.Fatalf("expected no items on a store error, got %#v", inbox.Items)
	}
	if len(inbox.Warnings) != 1 || inbox.Warnings[0] != "engine_notifications_unavailable" {
		t.Fatalf("expected engine_notifications_unavailable warning, got %#v", inbox.Warnings)
	}
}

func TestCollectWorkspacePlanNotifications_FiltersToRelevantServices(t *testing.T) {
	serviceID := uuid.New()
	configStore := &mockNotificationOnlyConfigStore{notifications: []store.WorkspaceNotification{
		{ID: uuid.New(), Type: store.WorkspaceNotificationTypeRegistryVersionAdded, ServiceID: &serviceID, Version: "v1", Message: "relevant"},
		{ID: uuid.New(), Type: store.WorkspaceNotificationTypeRegistryVersionAdded, ServiceID: nil, Message: "irrelevant"},
	}}
	inbox := collectWorkspacePlanNotifications(context.Background(), configStore, map[uuid.UUID][]string{serviceID: {"v1"}})
	if len(inbox.Warnings) != 0 {
		t.Fatalf("expected no warnings, got %#v", inbox.Warnings)
	}
	if len(inbox.Items) != 1 || inbox.Items[0].Message != "relevant" {
		t.Fatalf("expected only the relevant notification, got %#v", inbox.Items)
	}
}

// TestUnresolvedWorkspaceNotifications_KeepsPendingAndAcknowledged_DropsDismissed
// is workspaceNotificationInbox's own filtering rule in isolation: pending
// and acknowledged both survive (the UI's two-tier read/dismiss model needs
// acknowledged rows to stay visible, just de-emphasized), only dismissed is
// dropped. This is the exact behavior that regressed when
// workspaceNotificationInbox used to call ListWorkspaceNotifications with
// WorkspaceNotificationStatusPending directly -- see its doc comment.
func TestUnresolvedWorkspaceNotifications_KeepsPendingAndAcknowledged_DropsDismissed(t *testing.T) {
	notifications := []store.WorkspaceNotification{
		{ID: uuid.New(), Status: store.WorkspaceNotificationStatusPending, Message: "pending"},
		{ID: uuid.New(), Status: store.WorkspaceNotificationStatusAcknowledged, Message: "acknowledged"},
		{ID: uuid.New(), Status: store.WorkspaceNotificationStatusDismissed, Message: "dismissed"},
	}
	got := unresolvedWorkspaceNotifications(notifications)
	if len(got) != 2 {
		t.Fatalf("expected 2 unresolved notifications, got %#v", got)
	}
	for _, n := range got {
		if n.Status == store.WorkspaceNotificationStatusDismissed {
			t.Fatalf("dismissed notification leaked through: %#v", n)
		}
	}
}

// TestWorkspaceNotificationsE2E_QueryAcknowledgeDismissRoundTrip is the
// smaller end-to-end test for "GraphQL fetching the notifications" +
// "marking notifications as read/dismissed": one notification goes through
// query (pending, visible) -> mutate acknowledged -> query (still visible,
// now acknowledged -- this is the regression unresolvedWorkspaceNotifications
// fixes) -> mutate dismissed -> query (gone). No DATABASE_URL needed: this
// exercises the real GraphQL schema/resolvers end-to-end against
// mockConfigStore, same as the other GraphQL tests in this file.
func TestWorkspaceNotificationsE2E_QueryAcknowledgeDismissRoundTrip(t *testing.T) {
	accountID := uuid.New()
	noteID := uuid.New()
	svcID := uuid.New()
	configStore := &mockConfigStore{notifications: []store.WorkspaceNotification{
		{
			ID:        noteID,
			Type:      store.WorkspaceNotificationTypeRegistryVersionDeprecated,
			Severity:  store.WorkspaceNotificationSeverityNonBreaking,
			Status:    store.WorkspaceNotificationStatusPending,
			ServiceID: &svcID,
			Version:   "2026-07-01",
			Message:   "version 2026-07-01 deprecated",
		},
	}}
	s := &workspaceTestStore{accountID: accountID}
	h := buildNotificationGraphQLHandler(t, s, configStore)

	// 1. Fresh query: the pending notification is present.
	inbox := workspaceNotificationsGraphQLData(t, h)
	items := inbox["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected 1 item before any resolution, got %#v", items)
	}
	first := items[0].(map[string]any)
	if first["status"] != "pending" || first["id"] != "engine:"+noteID.String() {
		t.Fatalf("unexpected initial notification state: %#v", first)
	}

	// 2. Acknowledge it via the mutation.
	ackData := doMCPGraphQLRequest(t, h, `mutation { updateWorkspaceNotificationStatus(id: "`+noteID.String()+`", status: "acknowledged") { id status } }`)
	ackResult := ackData["updateWorkspaceNotificationStatus"].(map[string]any)
	if ackResult["status"] != "acknowledged" {
		t.Fatalf("expected mutation to return status acknowledged, got %#v", ackResult)
	}

	// 3. Re-query: the notification must still be visible, now acknowledged.
	// This is the exact case unresolvedWorkspaceNotifications exists for --
	// a query that filtered by status="pending" would drop this row here.
	inbox = workspaceNotificationsGraphQLData(t, h)
	items = inbox["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("expected the acknowledged notification to remain visible, got %#v", items)
	}
	if items[0].(map[string]any)["status"] != "acknowledged" {
		t.Fatalf("expected status acknowledged after re-query, got %#v", items[0])
	}

	// 4. Dismiss it via the mutation.
	dismissData := doMCPGraphQLRequest(t, h, `mutation { updateWorkspaceNotificationStatus(id: "`+noteID.String()+`", status: "dismissed") { id status } }`)
	dismissResult := dismissData["updateWorkspaceNotificationStatus"].(map[string]any)
	if dismissResult["status"] != "dismissed" {
		t.Fatalf("expected mutation to return status dismissed, got %#v", dismissResult)
	}

	// 5. Re-query: dismissed is the two-tier model's terminal, hidden state.
	inbox = workspaceNotificationsGraphQLData(t, h)
	items = inbox["items"].([]any)
	if len(items) != 0 {
		t.Fatalf("expected the dismissed notification to be hidden, got %#v", items)
	}
}

// ─── updateWorkspaceNotificationStatus GraphQL mutation ─────────────────────

// buildNotificationGraphQLHandler is mountMCPGraphQLTestHandler's shape but
// with a caller-supplied configStore -- that helper hardcodes a fresh empty
// &mockConfigStore{}, which can't carry the pre-seeded notification these
// tests need to act on.
func buildNotificationGraphQLHandler(t *testing.T, s store.Store, configStore store.ConfigRepository) http.HandlerFunc {
	t.Helper()
	schema, err := newMCPGraphQLSchema(configStore, s, &mockVerifier{}, &mockRegistryClient{}, []byte("12345678901234567890123456789012"))
	if err != nil {
		t.Fatalf("newMCPGraphQLSchema() error = %v", err)
	}
	return mcpGraphQLHandler(schema, s)
}

func TestUpdateWorkspaceNotificationStatusMutation_MarksAcknowledged(t *testing.T) {
	accountID := uuid.New()
	noteID := uuid.New()
	configStore := &mockConfigStore{notifications: []store.WorkspaceNotification{
		{ID: noteID, Type: store.WorkspaceNotificationTypeRegistryVersionAdded, Severity: store.WorkspaceNotificationSeverityNonBreaking, Status: store.WorkspaceNotificationStatusPending, Message: "a new version"},
	}}
	s := &workspaceTestStore{accountID: accountID}
	h := buildNotificationGraphQLHandler(t, s, configStore)

	data := doMCPGraphQLRequest(t, h, `mutation { updateWorkspaceNotificationStatus(id: "`+noteID.String()+`", status: "acknowledged") { id status } }`)

	result, ok := data["updateWorkspaceNotificationStatus"].(map[string]any)
	if !ok {
		t.Fatalf("expected updateWorkspaceNotificationStatus object, got %#v", data)
	}
	if result["status"] != "acknowledged" {
		t.Fatalf("expected status acknowledged, got %#v", result["status"])
	}
	if configStore.notifications[0].Status != store.WorkspaceNotificationStatusAcknowledged {
		t.Fatalf("expected underlying store row updated to acknowledged, got %s", configStore.notifications[0].Status)
	}
	if configStore.notifications[0].ResolvedBy == nil || *configStore.notifications[0].ResolvedBy != accountID {
		t.Fatalf("expected resolved_by set to the calling account %s, got %v", accountID, configStore.notifications[0].ResolvedBy)
	}
}

func TestUpdateWorkspaceNotificationStatusMutation_RejectsPendingTarget(t *testing.T) {
	accountID := uuid.New()
	noteID := uuid.New()
	configStore := &mockConfigStore{notifications: []store.WorkspaceNotification{
		{ID: noteID, Status: store.WorkspaceNotificationStatusPending},
	}}
	s := &workspaceTestStore{accountID: accountID}
	h := buildNotificationGraphQLHandler(t, s, configStore)

	body, _ := json.Marshal(map[string]string{"query": `mutation { updateWorkspaceNotificationStatus(id: "` + noteID.String() + `", status: "pending") { id } }`})
	req := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, hasErrors := resp["errors"]; !hasErrors {
		t.Fatalf("expected a graphql error for target status=pending, got %s", rr.Body.String())
	}
	if configStore.notifications[0].Status != store.WorkspaceNotificationStatusPending {
		t.Fatalf("expected the row untouched after a rejected transition, got %s", configStore.notifications[0].Status)
	}
}

func TestUpdateWorkspaceNotificationStatusMutation_DismissedIsTerminal(t *testing.T) {
	accountID := uuid.New()
	noteID := uuid.New()
	configStore := &mockConfigStore{notifications: []store.WorkspaceNotification{
		{ID: noteID, Status: store.WorkspaceNotificationStatusDismissed},
	}}
	s := &workspaceTestStore{accountID: accountID}
	h := buildNotificationGraphQLHandler(t, s, configStore)

	body, _ := json.Marshal(map[string]string{"query": `mutation { updateWorkspaceNotificationStatus(id: "` + noteID.String() + `", status: "acknowledged") { id } }`})
	req := httptest.NewRequest(http.MethodPost, "/engine/graphql", strings.NewReader(string(body)))
	req.Header.Set("X-API-Key", "fsk_test")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	var resp map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, hasErrors := resp["errors"]; !hasErrors {
		t.Fatalf("expected a graphql error for an already-dismissed notification, got %s", rr.Body.String())
	}
}
