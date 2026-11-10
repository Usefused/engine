package worker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type webhookDiagnosticRetentionStub struct {
	retentionStoreStub
	batches []int64
	cutoffs []time.Time
}

// DeleteExpiredWebhookDiagnostics records bounded cleanup passes independently of ordinary receipt cleanup.
func (s *webhookDiagnosticRetentionStub) DeleteExpiredWebhookDiagnostics(_ context.Context, cutoff time.Time, _ int) (int64, error) {
	s.cutoffs = append(s.cutoffs, cutoff)
	result := s.batches[0]
	s.batches = s.batches[1:]
	return result, nil
}

// TestWebhookDiagnosticsRetentionAlwaysRuns proves expiry still runs with unlimited receipts and drains full batches.
func TestWebhookDiagnosticsRetentionAlwaysRuns(t *testing.T) {
	now := time.Now()
	unlimited := &webhookDiagnosticRetentionStub{batches: []int64{2, 1}}
	cleanupExecutionRetentionPass(context.Background(), unlimited, now, 0, 2)
	require.Len(t, unlimited.cutoffs, 2)
	require.True(t, unlimited.cutoffs[0].IsZero())
	require.Zero(t, unlimited.calls)
	short := &webhookDiagnosticRetentionStub{batches: []int64{0}, retentionStoreStub: retentionStoreStub{results: []int64{0}}}
	cleanupExecutionRetentionPass(context.Background(), short, now, 1, 2)
	require.Equal(t, now.Add(-24*time.Hour), short.cutoffs[0])
	require.Equal(t, 1, short.calls)
}
