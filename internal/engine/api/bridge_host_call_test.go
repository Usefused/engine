package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/Usefused/engine/internal/engine/bridgehost"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

type bridgeHostFailureFixture struct {
	*bridgeSnapshotFixture
	failure error
}

// Invoke injects a classified Bridge failure at the real capability host boundary.
func (f *bridgeHostFailureFixture) Invoke(context.Context, string, bridgehost.Pin, []byte) ([]byte, error) {
	return nil, f.failure
}

// TestBridgeSnapshotFailureClassification proves known rejection stays failed while uncertain effects still cancel the attempt.
func TestBridgeSnapshotFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name         string
		failure      error
		wantCanceled bool
	}{
		{"offline", &bridgehost.InvocationError{Outcome: "not_started", Code: "unavailable"}, false},
		{"wrapped_rejection", fmt.Errorf("dispatch: %w", &bridgehost.InvocationError{Outcome: "not_started", Code: "rejected"}), false},
		{"unknown_effect", &bridgehost.InvocationError{Outcome: "unknown", Code: "connection_lost"}, true},
		{"unconfirmed_commit", errors.New("snapshot commit failed"), true},
	} {
		// Each classification gets fresh context and snapshot storage to prevent cross-case cancellation or cache reuse.
		t.Run(tc.name, func(t *testing.T) {
			fixture, server, identity := newBridgeSnapshotFixture()
			server.bridgeHost = &bridgeHostFailureFixture{fixture, tc.failure}
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			host := &executionCapabilityHost{apps: server, identity: identity, automaticSnapshot: true, snapshotRunID: uuid.New(), cancelAttempt: cancel, called: true}
			_, err := host.callBridge(ctx, json.RawMessage(`{"bridge":"volume","input":20}`))
			require.Error(t, err)
			require.Equal(t, tc.wantCanceled, ctx.Err() != nil)
			// Earlier provider work must not make a definite not-started result uncertain merely because snapshots are enabled.
			status, code := capabilityCompletion(err, ctx.Err(), true)
			expectedStatus, expectedCode := "failed", "execution_failed"
			// Only genuinely uncertain effects retain the conservative interrupted classification.
			if tc.wantCanceled {
				expectedStatus, expectedCode = "indeterminate", "execution_interrupted"
			}
			require.Equal(t, expectedStatus, status)
			require.Equal(t, expectedCode, code)
			require.Empty(t, fixture.rows)
		})
	}
}
