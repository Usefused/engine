package api

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/executionevent"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"github.com/stretchr/testify/require"
)

// recordingWebhookConsumptionStore captures per-consumer webhook writes for API handler tests.
type recordingWebhookConsumptionStore struct {
	store.Store
	mu      sync.Mutex
	records []models.WebhookConsumption
}

// RecordWebhookConsumption appends each recorded consumption under a lock for deterministic assertions.
func (s *recordingWebhookConsumptionStore) RecordWebhookConsumption(_ context.Context, record models.WebhookConsumption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
	return nil
}

// snapshot returns a copy of recorded consumptions so assertions do not race concurrent writers.
func (s *recordingWebhookConsumptionStore) snapshot() []models.WebhookConsumption {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]models.WebhookConsumption, len(s.records))
	copy(out, s.records)
	return out
}

// TestSDKWebhookAckRecordsConsumption verifies provider ACKs persist SDK consumer identity and time while ignoring Fused auth events.
func TestSDKWebhookAckRecordsConsumption(t *testing.T) {
	recorder := &recordingWebhookConsumptionStore{Store: &workspaceTestStore{}}
	accountID, serviceID, appFamilyID, appID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	scope := webhookSubscriptionScope{
		accountID: accountID, appFamilyID: appFamilyID, appID: appID, receiverName: "billing-worker",
	}
	msgMap := newPendingWebhookMsgs()

	providerMsgID := uuid.NewString()
	providerMsg := nats.NewMsg("webhooks." + accountID.String() + "." + serviceID.String() + ".stripe-events.checkout.session.completed")
	providerMsg.Header.Set("X-Webhook-Msg-ID", providerMsgID)
	msgMap.Store(providerMsgID, providerMsg)

	authMsgID := uuid.NewString()
	authMsg := nats.NewMsg(messaging.FusedAuthWebhookSubject(accountID, appFamilyID, serviceID, "fused.auth.connection.completed"))
	authMsg.Header.Set("X-Webhook-Msg-ID", authMsgID)
	msgMap.Store(authMsgID, authMsg)

	// Acknowledge both a provider webhook and a Fused auth lifecycle event.
	applyWebhookClientMessage(context.Background(), recorder, &enginev1.WebhookClientMessage{
		Payload: &enginev1.WebhookClientMessage_Ack{Ack: &enginev1.WebhookAck{EventId: providerMsgID}},
	}, msgMap, scope)
	applyWebhookClientMessage(context.Background(), recorder, &enginev1.WebhookClientMessage{
		Payload: &enginev1.WebhookClientMessage_Ack{Ack: &enginev1.WebhookAck{EventId: authMsgID}},
	}, msgMap, scope)

	recorded := recorder.snapshot()
	require.Len(t, recorded, 1)
	require.Equal(t, executionevent.WebhookEventID(providerMsgID), recorded[0].EventID)
	require.Equal(t, accountID, recorded[0].AccountID)
	require.Equal(t, "sdk", recorded[0].ConsumerKind)
	require.Equal(t, appID.String()+":billing-worker", recorded[0].ConsumerKey)
	require.NotNil(t, recorded[0].AppFamilyID)
	require.Equal(t, appFamilyID, *recorded[0].AppFamilyID)
	require.NotNil(t, recorded[0].AppID)
	require.Equal(t, appID, *recorded[0].AppID)
	require.Equal(t, "billing-worker", recorded[0].ReceiverName)
	require.False(t, recorded[0].ConsumedAt.IsZero())
}

// unifiedWebhookConsumptionTestStore embeds the full Unified App test store while recording webhook consumptions.
type unifiedWebhookConsumptionTestStore struct {
	*unifiedWebhookTestStore
	mu      sync.Mutex
	records []models.WebhookConsumption
}

// RecordWebhookConsumption records Unified App webhook consumptions without hiding optional store interfaces.
func (s *unifiedWebhookConsumptionTestStore) RecordWebhookConsumption(_ context.Context, record models.WebhookConsumption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, record)
	return nil
}

// snapshot returns a copy of recorded Unified App consumptions.
func (s *unifiedWebhookConsumptionTestStore) snapshot() []models.WebhookConsumption {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]models.WebhookConsumption, len(s.records))
	copy(out, s.records)
	return out
}

// TestUnifiedAppWebhookExecutionRecordsConsumption proves Unified App webhook execution records its consumer metadata and timestamp.
func TestUnifiedAppWebhookExecutionRecordsConsumption(t *testing.T) {
	server, repository, message := newUnifiedWebhookFixture(t)
	recorder := &unifiedWebhookConsumptionTestStore{unifiedWebhookTestStore: repository}
	server.store = recorder
	msgID := message.Header.Get("X-Webhook-Msg-ID")

	err := server.executeUnifiedWebhook(context.Background(), recorder, repository.target.FamilyID, message)
	require.NoError(t, err)

	recorded := recorder.snapshot()
	require.Len(t, recorded, 1)
	require.Equal(t, executionevent.WebhookEventID(msgID), recorded[0].EventID)
	require.Equal(t, repository.target.AccountID, recorded[0].AccountID)
	require.Equal(t, "unified_app", recorded[0].ConsumerKind)
	require.Equal(t, repository.target.FamilyID.String(), recorded[0].ConsumerKey)
	require.NotNil(t, recorded[0].AppFamilyID)
	require.Equal(t, repository.target.FamilyID, *recorded[0].AppFamilyID)
	require.NotNil(t, recorded[0].AppID)
	require.Equal(t, repository.target.AppID, *recorded[0].AppID)
	require.Equal(t, repository.target.Version, recorded[0].AppVersion)
	require.False(t, recorded[0].ConsumedAt.IsZero())
}

// webhookConsumptionGraphQLStore serves one webhook event populated with multiple consumer entries.
type webhookConsumptionGraphQLStore struct {
	*webhookPageTestStore
	events []models.WebhookEvent
}

// ListWebhookActivity returns the fixture's pre-populated events with consumptions attached.
func (s *webhookConsumptionGraphQLStore) ListWebhookActivity(_ context.Context, _ store.WebhookActivityFilter) ([]models.WebhookEvent, int64, error) {
	return s.events, int64(len(s.events)), nil
}

// GetWebhookActivityAnalytics returns an empty summary because this test exercises event projection only.
func (s *webhookConsumptionGraphQLStore) GetWebhookActivityAnalytics(_ context.Context, _ store.WebhookActivityFilter) (models.WebhookAnalytics, error) {
	return models.WebhookAnalytics{}, nil
}

// TestGraphQLWebhookEventsProjectsConsumptions verifies GraphQL exposes consumer kinds, names, versions, receivers, and timestamps.
func TestGraphQLWebhookEventsProjectsConsumptions(t *testing.T) {
	account, workspace, service, registration := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	sdkFamily, sdkApp := uuid.New(), uuid.New()
	mcpFamily, mcpApp := uuid.New(), uuid.New()
	consumedAt := time.Date(2026, 11, 20, 16, 46, 20, 0, time.UTC)
	repository := &webhookConsumptionGraphQLStore{
		webhookPageTestStore: &webhookPageTestStore{
			workspaceTestStore: &workspaceTestStore{
				accountID: account, workspaceID: workspace,
				workspaceServices:        []store.WorkspaceService{{ServiceID: service, Version: "1.0.0"}},
				workspaceServiceVersions: map[uuid.UUID][]store.WorkspaceServiceVersion{service: {{ServiceID: service, Version: "1.0.0"}}},
			},
		},
		events: []models.WebhookEvent{{
			ID: uuid.New(), AccountID: account, ServiceID: service, RegistrationID: &registration,
			MsgID: "msg-1", EventType: "checkout.session.completed", DeliveryStatus: "success", VerificationStatus: "verified",
			CreatedAt: consumedAt.Add(-time.Minute), EndedAt: consumedAt, RecordedAt: consumedAt.Add(-time.Minute),
			Consumptions: []models.WebhookConsumption{
				{
					ConsumerKind: "sdk", ConsumerName: "Billing SDK", AppFamilyID: &sdkFamily, AppID: &sdkApp,
					AppVersion: "2.1.0", ReceiverName: "billing-worker", ConsumedAt: consumedAt,
				},
				{
					ConsumerKind: "mcp", ConsumerName: "Ops MCP", AppFamilyID: &mcpFamily, AppID: &mcpApp,
					AppVersion: "1.0.0", ConsumedAt: consumedAt.Add(5 * time.Second),
				},
			},
		}},
	}
	schema, err := newMCPGraphQLSchema(&mockConfigStore{}, repository, &mockVerifier{}, &mockRegistryClient{}, nil, nil, nil)
	require.NoError(t, err)
	actor := actorWithResourcePermissions(t, workspace,
		accesscontrol.Grant{Permission: accesscontrol.PermissionServiceRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: service}},
		accesscontrol.Grant{Permission: accesscontrol.PermissionAuditRead, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspace}},
	)
	actor.AccountID = account
	query := `query { webhookEvents(service_id:"` + service.String() + `", registration_id:"` + registration.String() + `") { total items { id consumptions { consumer_kind consumer_name app_family_id app_id app_version receiver_name consumed_at } } } }`
	response := webhookRegistrationRequest(t, mcpGraphQLHandler(schema), actor, query)
	require.Equal(t, http.StatusOK, response.Code, response.Body.String())

	var body struct {
		Data struct {
			WebhookEvents struct {
				Total int `json:"total"`
				Items []struct {
					Consumptions []struct {
						ConsumerKind string `json:"consumer_kind"`
						ConsumerName string `json:"consumer_name"`
						AppFamilyID  string `json:"app_family_id"`
						AppID        string `json:"app_id"`
						AppVersion   string `json:"app_version"`
						ReceiverName string `json:"receiver_name"`
						ConsumedAt   string `json:"consumed_at"`
					} `json:"consumptions"`
				} `json:"items"`
			} `json:"webhookEvents"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(response.Body.Bytes(), &body))
	require.Equal(t, 1, body.Data.WebhookEvents.Total)
	require.Len(t, body.Data.WebhookEvents.Items, 1)
	consumptions := body.Data.WebhookEvents.Items[0].Consumptions
	require.Len(t, consumptions, 2)
	require.Equal(t, "sdk", consumptions[0].ConsumerKind)
	require.Equal(t, "Billing SDK", consumptions[0].ConsumerName)
	require.Equal(t, sdkFamily.String(), consumptions[0].AppFamilyID)
	require.Equal(t, sdkApp.String(), consumptions[0].AppID)
	require.Equal(t, "2.1.0", consumptions[0].AppVersion)
	require.Equal(t, "billing-worker", consumptions[0].ReceiverName)
	require.Equal(t, consumedAt.Format(time.RFC3339), consumptions[0].ConsumedAt)
	require.Equal(t, "mcp", consumptions[1].ConsumerKind)
	require.Equal(t, "Ops MCP", consumptions[1].ConsumerName)
}
