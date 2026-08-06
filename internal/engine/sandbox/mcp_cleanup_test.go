package sandbox

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestCleanupExpiredMCPBatchesUsesCursorAndContinuesAfterRemoveFailure(t *testing.T) {
	first := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	second := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	third := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	cursors := make([]uuid.UUID, 0, 3)

	list := func(_ context.Context, _ time.Time, after uuid.UUID) ([]uuid.UUID, error) {
		cursors = append(cursors, after)
		switch after {
		case uuid.Nil:
			return []uuid.UUID{first, second}, nil
		case second:
			return []uuid.UUID{third}, nil
		default:
			return nil, nil
		}
	}
	remove := func(path string) error {
		if path == sandboxDirFor(second.String()) {
			return errors.New("remove failed")
		}
		return nil
	}

	result, err := cleanupExpiredMCPBatches(context.Background(), time.Now(), list, remove)
	require.NoError(t, err)
	assert.Equal(t, mcpCleanupResult{cleaned: 2, failed: 1}, result)
	assert.Equal(t, []uuid.UUID{uuid.Nil, second, third}, cursors)
}

func TestCleanupExpiredMCPBatchesStopsOnListFailure(t *testing.T) {
	wantErr := errors.New("query failed")
	result, err := cleanupExpiredMCPBatches(
		context.Background(), time.Now(),
		func(context.Context, time.Time, uuid.UUID) ([]uuid.UUID, error) {
			return nil, wantErr
		},
		func(string) error { return nil },
	)

	assert.ErrorIs(t, err, wantErr)
	assert.Equal(t, mcpCleanupResult{}, result)
}
