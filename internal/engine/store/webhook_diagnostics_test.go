package store

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

// TestWebhookDiagnosticsPostgres proves evidence is encrypted, immutable, receipt-bound, tenant-isolated and expiring.
func TestWebhookDiagnosticsPostgres(t *testing.T) {
	f := newExecutionActivityFixture(t)
	event := webhookActivityReceipt(f.event, uuid.New(), "rejected", 0)
	scope := WebhookDiagnosticScope{AccountID: event.AccountID, ServiceID: event.ServiceID, RegistrationID: event.WebhookID, EventID: event.ID}
	repository := &cachedStore{Store: f.repository}
	key := bytes.Repeat([]byte{7}, 32)
	payload := json.RawMessage(`{"body":"private sentinel","encoding":"utf-8"}`)
	require.NoError(t, repository.SaveWebhookDiagnostics(f.ctx, scope, payload, key))
	// An orphaned or not-yet-persisted execution can never be revealed.
	_, err := repository.GetWebhookDiagnostics(f.ctx, scope, key)
	require.ErrorIs(t, err, ErrExecutionResultNotFound)
	require.NoError(t, f.repository.BatchCreateEngineExecutionEvents(f.ctx, []models.EngineExecutionEvent{event}))
	var encrypted string
	require.NoError(t, f.pool.QueryRow(f.ctx, `SELECT diagnostic_payload FROM fused_webhook_diagnostics WHERE event_id=$1`, event.ID).Scan(&encrypted))
	require.NotContains(t, encrypted, "private sentinel")
	opened, err := repository.GetWebhookDiagnostics(f.ctx, scope, key)
	require.NoError(t, err)
	require.JSONEq(t, string(payload), string(opened))
	for _, foreign := range []WebhookDiagnosticScope{
		{AccountID: uuid.New(), ServiceID: scope.ServiceID, RegistrationID: scope.RegistrationID, EventID: scope.EventID},
		{AccountID: scope.AccountID, ServiceID: uuid.New(), RegistrationID: scope.RegistrationID, EventID: scope.EventID},
		{AccountID: scope.AccountID, ServiceID: scope.ServiceID, RegistrationID: uuid.New(), EventID: scope.EventID},
		{AccountID: scope.AccountID, ServiceID: scope.ServiceID, RegistrationID: scope.RegistrationID, EventID: uuid.New()},
	} {
		// Knowledge of any subset of identifiers cannot substitute for all four exact identities.
		_, err = repository.GetWebhookDiagnostics(f.ctx, foreign, key)
		require.ErrorIs(t, err, ErrExecutionResultNotFound)
	}
	_, err = repository.GetWebhookDiagnostics(f.ctx, scope, bytes.Repeat([]byte{8}, 32))
	require.Error(t, err)
	require.ErrorIs(t, repository.SaveWebhookDiagnostics(f.ctx, scope, payload, key), ErrExecutionResultNotFound)
	require.ErrorIs(t, repository.SaveWebhookDiagnostics(f.ctx, WebhookDiagnosticScope{}, payload, key), ErrExecutionResultInvalid)
	// Ordinary receipt projection contains correlation and failure metadata but never private message bytes.
	rows, _, err := repository.ListWebhookActivity(f.ctx, WebhookActivityFilter{AccountID: scope.AccountID, ServiceID: scope.ServiceID, RegistrationID: &scope.RegistrationID, Limit: 20})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	require.Equal(t, scope.RegistrationID, *rows[0].RegistrationID)
	require.Equal(t, event.ServiceVersionID, rows[0].ServiceVersionID)
	require.Equal(t, event.EndedAt.UnixMilli(), rows[0].EndedAt.UnixMilli())
	ordinary, _ := json.Marshal(rows)
	require.NotContains(t, string(ordinary), "private sentinel")
	_, err = f.pool.Exec(f.ctx, `UPDATE fused_webhook_diagnostics SET expires_at=NOW()-INTERVAL '1 second' WHERE event_id=$1`, scope.EventID)
	require.NoError(t, err)
	_, err = repository.GetWebhookDiagnostics(f.ctx, scope, key)
	require.ErrorIs(t, err, ErrExecutionResultNotFound)
	deleted, err := repository.DeleteExpiredWebhookDiagnostics(f.ctx, time.Time{}, 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
	// A shorter canonical retention window also deletes otherwise unexpired private evidence.
	require.NoError(t, repository.SaveWebhookDiagnostics(f.ctx, scope, payload, key))
	deleted, err = repository.DeleteExpiredWebhookDiagnostics(f.ctx, time.Now().Add(time.Hour), 10)
	require.NoError(t, err)
	require.EqualValues(t, 1, deleted)
}
