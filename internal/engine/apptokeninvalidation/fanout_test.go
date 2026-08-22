package apptokeninvalidation

import (
	"testing"

	"github.com/google/uuid"
)

type fanoutTarget struct {
	wantToken uuid.UUID
	result    int
	calls     int
}

func (target *fanoutTarget) InvalidateToken(tokenID uuid.UUID) int {
	if tokenID == target.wantToken {
		target.calls++
	}
	return target.result
}

func TestFanoutInvalidatorInvalidatesEveryRuntimeConsumer(t *testing.T) {
	tokenID := uuid.New()
	cache := &fanoutTarget{wantToken: tokenID, result: 2}
	sessions := &fanoutTarget{wantToken: tokenID, result: 1}
	invalidator := NewFanoutInvalidator(nil, cache, sessions)

	if got := invalidator.InvalidateToken(tokenID); got != 3 {
		t.Fatalf("invalidated entries = %d, want 3", got)
	}
	if cache.calls != 1 || sessions.calls != 1 {
		t.Fatalf("target calls = %d/%d, want 1/1", cache.calls, sessions.calls)
	}
}

func TestNilFanoutInvalidatorIsSafe(t *testing.T) {
	var invalidator *FanoutInvalidator
	if got := invalidator.InvalidateToken(uuid.New()); got != 0 {
		t.Fatalf("nil invalidator result = %d, want 0", got)
	}
}
