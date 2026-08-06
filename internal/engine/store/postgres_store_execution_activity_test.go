package store

import (
	"context"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

func TestEngineExecutionWhereClauseScopesAppFamilyAndVersionInSQL(t *testing.T) {
	accountID := uuid.New()
	appFamilyID := uuid.New()
	appID := uuid.New()
	whereClause, args := engineExecutionWhereClause(EngineExecutionFilter{
		AccountID: accountID, AppFamilyID: appFamilyID, AppID: appID, Transport: "sdk", Status: "failed",
	})

	if !strings.HasPrefix(whereClause, "WHERE account_id = $1 AND app_family_id = $2 AND app_id = $3") {
		t.Fatalf("where clause does not enforce account, family, and app scope: %s", whereClause)
	}
	wantArgs := []any{accountID, appFamilyID, appID, "sdk", "failed"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", args, wantArgs)
	}
}

func TestEngineExecutionWhereClauseScopesWholeAppFamilyInSQL(t *testing.T) {
	accountID := uuid.New()
	appFamilyID := uuid.New()
	whereClause, args := engineExecutionWhereClause(EngineExecutionFilter{
		AccountID: accountID, AppFamilyID: appFamilyID,
	})

	if whereClause != "WHERE account_id = $1 AND app_family_id = $2" {
		t.Fatalf("where clause = %s, want family scope", whereClause)
	}
	if !reflect.DeepEqual(args, []any{accountID, appFamilyID}) {
		t.Fatalf("args = %#v", args)
	}
}

func TestEngineExecutionWhereClauseDefaultsToServiceScope(t *testing.T) {
	serviceID := uuid.New()
	whereClause, args := engineExecutionWhereClause(EngineExecutionFilter{
		AccountID: uuid.New(), ServiceID: serviceID,
	})

	if !strings.Contains(whereClause, "service_id = $2") || strings.Contains(whereClause, "app_family_id") {
		t.Fatalf("where clause = %s, want service scope", whereClause)
	}
	if args[1] != serviceID {
		t.Fatalf("scope arg = %v, want %s", args[1], serviceID)
	}
}

func TestValidateExecutionEventIdentity(t *testing.T) {
	valid := models.EngineExecutionEvent{
		Transport:   models.EngineExecutionTransportSDK,
		AppFamilyID: uuid.New(), AppID: uuid.New(), AppVersion: "1.2.0",
	}
	if err := validateExecutionEventIdentity(valid); err != nil {
		t.Fatalf("valid SDK identity rejected: %v", err)
	}

	missing := valid
	missing.AppFamilyID = uuid.Nil
	if err := validateExecutionEventIdentity(missing); err == nil {
		t.Fatal("SDK event without family identity was accepted")
	}

	webhook := models.EngineExecutionEvent{Transport: models.EngineExecutionTransportWebhook}
	if err := validateExecutionEventIdentity(webhook); err != nil {
		t.Fatalf("webhook without app identity rejected: %v", err)
	}
}

func TestEngineExecutionEventPersistenceUsesAppIdentity(t *testing.T) {
	databaseURL := os.Getenv("DATABASE_URL")
	if databaseURL == "" {
		t.Skip("DATABASE_URL not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	pool := isolatedBootstrapPool(t, ctx, databaseURL)
	defer pool.Close()
	repository := NewPostgresStore(pool).(*postgresStore)

	accountID, familyID, appID := uuid.New(), uuid.New(), uuid.New()
	now := time.Now().UTC()
	event := models.EngineExecutionEvent{
		ID: uuid.New(), AccountID: accountID, AppFamilyID: familyID, AppID: appID,
		AppVersion: "2.0.0", Transport: models.EngineExecutionTransportMCP,
		ProviderProtocol: "graphql", Direction: models.EngineExecutionDirectionOutbound,
		ServiceID: uuid.New(), EndpointName: "issues.list", Status: models.EngineExecutionStatusSuccess,
		LatencyMs: 12, AttemptCount: 1, StartedAt: now, EndedAt: now, CreatedAt: now,
	}
	if err := repository.BatchCreateEngineExecutionEvents(ctx, []models.EngineExecutionEvent{event}); err != nil {
		t.Fatalf("persist app execution event: %v", err)
	}

	rows, total, err := repository.ListEngineExecutionEventsByApp(ctx, EngineExecutionFilter{
		AccountID: accountID, AppFamilyID: familyID, AppID: appID, Limit: 10,
	})
	if err != nil || total != 1 || len(rows) != 1 {
		t.Fatalf("app execution events = %#v, total %d, error %v", rows, total, err)
	}
	if rows[0].AppVersion != "2.0.0" || rows[0].ProviderProtocol != "graphql" {
		t.Fatalf("persisted identity = %#v", rows[0])
	}

	_, total, err = repository.ListEngineExecutionEventsByApp(ctx, EngineExecutionFilter{
		AccountID: accountID, AppFamilyID: familyID, AppID: uuid.New(), Limit: 10,
	})
	if err != nil || total != 0 {
		t.Fatalf("exact app filter total = %d, error %v", total, err)
	}

	webhook := event
	webhook.ID, webhook.Transport = uuid.New(), models.EngineExecutionTransportWebhook
	webhook.AppFamilyID, webhook.AppID, webhook.AppVersion = uuid.Nil, uuid.Nil, ""
	if err := repository.BatchCreateEngineExecutionEvents(ctx, []models.EngineExecutionEvent{webhook}); err != nil {
		t.Fatalf("persist app-independent webhook event: %v", err)
	}
}
