package auth

import (
	"context"
	"errors"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Advance(duration time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(duration)
	c.mu.Unlock()
}

func tokenProjection(appID, tokenID uuid.UUID, expiresAt *time.Time) *store.AuthProjection {
	return &store.AuthProjection{
		AccountID: uuid.New(), AppFamilyID: uuid.New(), AppID: appID, TokenID: tokenID,
		Version: "1.0.0", Kind: store.AppKindSDK, AppStatus: store.AppStatusActive,
		TokenPolicy: store.AppTokenPolicy{AllowAll: true, ExpiresAt: expiresAt},
	}
}

func TestTokenValidatorCachesSuccessfulAuthorization(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	appID, tokenID := uuid.New(), uuid.New()
	var calls atomic.Int32
	validator := newTokenValidator(&mockStore{authorizeAppFn: func(_ context.Context, gotAppID uuid.UUID, _ string) (*store.AuthProjection, error) {
		calls.Add(1)
		return tokenProjection(gotAppID, tokenID, nil), nil
	}}, clock.Now)

	first, err := validator.Validate(context.Background(), appID, "token")
	if err != nil {
		t.Fatalf("first validation failed: %v", err)
	}
	second, err := validator.Validate(context.Background(), appID, "token")
	if err != nil {
		t.Fatalf("cached validation failed: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("AuthorizeApp calls = %d, want 1", calls.Load())
	}
	if first.AppID != second.AppID {
		t.Fatalf("cached identity app = %s, want %s", second.AppID, first.AppID)
	}
}

func TestTokenValidatorBoundsCacheByTokenExpiry(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	appID, tokenID := uuid.New(), uuid.New()
	expiresAt := clock.Now().Add(5 * time.Second)
	var calls atomic.Int32
	validator := newTokenValidator(&mockStore{authorizeAppFn: func(_ context.Context, _ uuid.UUID, _ string) (*store.AuthProjection, error) {
		if calls.Add(1) > 1 {
			return nil, errors.New("expired")
		}
		return tokenProjection(appID, tokenID, &expiresAt), nil
	}}, clock.Now)

	if _, err := validator.Validate(context.Background(), appID, "token"); err != nil {
		t.Fatalf("initial validation failed: %v", err)
	}
	clock.Advance(4 * time.Second)
	if _, err := validator.Validate(context.Background(), appID, "token"); err != nil {
		t.Fatalf("validation before expiry failed: %v", err)
	}
	clock.Advance(2 * time.Second)
	if _, err := validator.Validate(context.Background(), appID, "token"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("validation after expiry error = %v, want unauthorized", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("AuthorizeApp calls = %d, want 2", calls.Load())
	}
	validator.mu.Lock()
	entries, reverseIndexes := len(validator.entries), len(validator.keysByToken)
	validator.mu.Unlock()
	if entries != 0 || reverseIndexes != 0 {
		t.Fatalf("expired cache state = (%d entries, %d indexes), want empty", entries, reverseIndexes)
	}
}

func TestTokenValidatorRefreshesAfterMaximumTTL(t *testing.T) {
	clock := &testClock{now: time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)}
	appID, tokenID := uuid.New(), uuid.New()
	var calls atomic.Int32
	validator := newTokenValidator(&mockStore{authorizeAppFn: func(_ context.Context, _ uuid.UUID, _ string) (*store.AuthProjection, error) {
		calls.Add(1)
		return tokenProjection(appID, tokenID, nil), nil
	}}, clock.Now)

	if _, err := validator.Validate(context.Background(), appID, "token"); err != nil {
		t.Fatalf("initial validation failed: %v", err)
	}
	clock.Advance(tokenCacheTTL + time.Millisecond)
	if _, err := validator.Validate(context.Background(), appID, "token"); err != nil {
		t.Fatalf("refreshed validation failed: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("AuthorizeApp calls = %d, want 2", calls.Load())
	}
}

func TestTokenValidatorDoesNotCacheFailures(t *testing.T) {
	appID, tokenID := uuid.New(), uuid.New()
	var calls atomic.Int32
	validator := NewTokenValidator(&mockStore{authorizeAppFn: func(_ context.Context, _ uuid.UUID, _ string) (*store.AuthProjection, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("not found")
		}
		return tokenProjection(appID, tokenID, nil), nil
	}})

	if _, err := validator.Validate(context.Background(), appID, "token"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("first validation error = %v, want unauthorized", err)
	}
	if _, err := validator.Validate(context.Background(), appID, "token"); err != nil {
		t.Fatalf("second validation failed: %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("AuthorizeApp calls = %d, want 2", calls.Load())
	}
}

func TestTokenValidatorInvalidatesOneTokenAcrossAppVersions(t *testing.T) {
	appA, appB := uuid.New(), uuid.New()
	tokenAID, tokenBID := uuid.New(), uuid.New()
	tokenAHash, tokenBHash := HashToken("token-a"), HashToken("token-b")
	var calls atomic.Int32
	validator := NewTokenValidator(&mockStore{authorizeAppFn: func(_ context.Context, appID uuid.UUID, tokenHash string) (*store.AuthProjection, error) {
		calls.Add(1)
		switch tokenHash {
		case tokenAHash:
			return tokenProjection(appID, tokenAID, nil), nil
		case tokenBHash:
			return tokenProjection(appID, tokenBID, nil), nil
		default:
			return nil, errors.New("not found")
		}
	}})

	for _, request := range []struct {
		appID uuid.UUID
		token string
	}{{appA, "token-a"}, {appB, "token-a"}, {appA, "token-b"}} {
		if _, err := validator.Validate(context.Background(), request.appID, request.token); err != nil {
			t.Fatalf("prime cache: %v", err)
		}
	}
	if got := validator.InvalidateToken(tokenAID); got != 2 {
		t.Fatalf("invalidated entries = %d, want 2", got)
	}
	for _, request := range []struct {
		appID uuid.UUID
		token string
	}{{appA, "token-a"}, {appB, "token-a"}, {appA, "token-b"}} {
		if _, err := validator.Validate(context.Background(), request.appID, request.token); err != nil {
			t.Fatalf("validate after invalidation: %v", err)
		}
	}
	if calls.Load() != 5 {
		t.Fatalf("AuthorizeApp calls = %d, want 5; unrelated token should remain cached", calls.Load())
	}
}

func TestTokenValidatorCoalescesConcurrentMisses(t *testing.T) {
	appID, tokenID := uuid.New(), uuid.New()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	validator := NewTokenValidator(&mockStore{authorizeAppFn: func(_ context.Context, _ uuid.UUID, _ string) (*store.AuthProjection, error) {
		if calls.Add(1) == 1 {
			close(started)
		}
		<-release
		return tokenProjection(appID, tokenID, nil), nil
	}})

	leaderResult := make(chan error, 1)
	go func() {
		_, err := validator.Validate(context.Background(), appID, "token")
		leaderResult <- err
	}()
	<-started
	key := tokenCacheKey{appID: appID, tokenDigest: digestToken("token")}
	const followers = 15
	loads := make([]*tokenLoad, 0, followers)
	for i := 0; i < followers; i++ {
		lookup := validator.acquire(key)
		if lookup.leader || lookup.load == nil {
			t.Fatal("concurrent miss did not join the shared load")
		}
		loads = append(loads, lookup.load)
	}
	close(release)
	if err := <-leaderResult; err != nil {
		t.Fatalf("leader validation failed: %v", err)
	}
	for _, load := range loads {
		if _, _, err := waitForTokenLoad(context.Background(), load, tokenCacheResultCoalesced); err != nil {
			t.Fatalf("coalesced validation failed: %v", err)
		}
	}
	if calls.Load() != 1 {
		t.Fatalf("AuthorizeApp calls = %d, want 1", calls.Load())
	}
}

func TestTokenValidatorCallerCancellationDoesNotFailCoalescedWaiter(t *testing.T) {
	appID, tokenID := uuid.New(), uuid.New()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	validator := NewTokenValidator(&mockStore{authorizeAppFn: func(ctx context.Context, _ uuid.UUID, _ string) (*store.AuthProjection, error) {
		calls.Add(1)
		close(started)
		select {
		case <-release:
			return tokenProjection(appID, tokenID, nil), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderResult := make(chan error, 1)
	go func() {
		_, err := validator.Validate(leaderCtx, appID, "token")
		leaderResult <- err
	}()
	<-started
	followerResult := make(chan error, 1)
	go func() {
		_, err := validator.Validate(context.Background(), appID, "token")
		followerResult <- err
	}()
	cancelLeader()
	if err := <-leaderResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v, want context cancellation", err)
	}
	close(release)
	if err := <-followerResult; err != nil {
		t.Fatalf("coalesced follower inherited leader cancellation: %v", err)
	}
	if calls.Load() != 1 {
		t.Fatalf("AuthorizeApp calls = %d, want 1", calls.Load())
	}
}

func TestTokenInvalidationPreventsInflightLoadFromRepopulatingCache(t *testing.T) {
	appID, tokenID := uuid.New(), uuid.New()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	validator := NewTokenValidator(&mockStore{authorizeAppFn: func(_ context.Context, _ uuid.UUID, _ string) (*store.AuthProjection, error) {
		if calls.Add(1) > 1 {
			return nil, errors.New("revoked")
		}
		close(started)
		<-release
		return tokenProjection(appID, tokenID, nil), nil
	}})

	firstResult := make(chan error, 1)
	go func() {
		_, err := validator.Validate(context.Background(), appID, "token")
		firstResult <- err
	}()
	<-started
	validator.InvalidateToken(tokenID)
	key := tokenCacheKey{appID: appID, tokenDigest: digestToken("token")}
	joinedAfterRevocation := validator.acquire(key)
	if joinedAfterRevocation.leader || joinedAfterRevocation.load == nil {
		t.Fatal("post-revocation validation did not join the in-flight lookup")
	}
	close(release)
	if err := <-firstResult; !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("in-flight validation error = %v, want unauthorized", err)
	}
	if _, _, err := waitForTokenLoad(context.Background(), joinedAfterRevocation.load, tokenCacheResultCoalesced); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("post-revocation waiter error = %v, want unauthorized", err)
	}
	if _, err := validator.Validate(context.Background(), appID, "token"); !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("post-invalidation validation error = %v, want unauthorized", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("AuthorizeApp calls = %d, want 2", calls.Load())
	}
}

func TestUnrelatedInvalidationDoesNotPoisonInflightLoad(t *testing.T) {
	appID, tokenID := uuid.New(), uuid.New()
	started, release := make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	validator := NewTokenValidator(&mockStore{authorizeAppFn: func(_ context.Context, _ uuid.UUID, _ string) (*store.AuthProjection, error) {
		calls.Add(1)
		close(started)
		<-release
		return tokenProjection(appID, tokenID, nil), nil
	}})
	result := make(chan error, 1)
	go func() {
		_, err := validator.Validate(context.Background(), appID, "token")
		result <- err
	}()
	<-started
	validator.InvalidateToken(uuid.New())
	close(release)
	if err := <-result; err != nil {
		t.Fatalf("unrelated revocation poisoned authorization: %v", err)
	}
	if _, err := validator.Validate(context.Background(), appID, "token"); err != nil || calls.Load() != 1 {
		t.Fatalf("valid result was not cached: error=%v calls=%d", err, calls.Load())
	}
}

func TestTokenValidatorTracesOnlySafeCacheResult(t *testing.T) {
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	previousProvider := otel.GetTracerProvider()
	otel.SetTracerProvider(provider)
	t.Cleanup(func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previousProvider)
	})

	appID := uuid.New()
	validator := NewTokenValidator(&mockStore{authorizeAppFn: func(_ context.Context, _ uuid.UUID, _ string) (*store.AuthProjection, error) {
		return tokenProjection(appID, uuid.New(), nil), nil
	}})
	for i := 0; i < 2; i++ {
		if _, err := validator.Validate(context.Background(), appID, "secret-token"); err != nil {
			t.Fatalf("validation %d failed: %v", i, err)
		}
	}

	results := cacheResultsFromSpans(t, recorder.Ended(), "secret-token", HashToken("secret-token"))
	if !results[string(tokenCacheResultMiss)] || !results[string(tokenCacheResultHit)] {
		t.Fatalf("cache trace results = %v, want miss and hit", results)
	}
}

func cacheResultsFromSpans(t *testing.T, spans []sdktrace.ReadOnlySpan, prohibited ...string) map[string]bool {
	t.Helper()
	results := make(map[string]bool)
	for _, span := range spans {
		if span.Name() != "engine.auth.app_token.validate" {
			continue
		}
		for _, attr := range span.Attributes() {
			key := string(attr.Key)
			value := attr.Value.Emit()
			if strings.Contains(key, "token") || strings.Contains(key, "hash") {
				t.Fatalf("unsafe authorization cache attribute %q", key)
			}
			for _, secret := range prohibited {
				if strings.Contains(value, secret) {
					t.Fatalf("authorization cache attribute %q exposed prohibited value", key)
				}
			}
			if key == tokenCacheResultAttribute {
				results[attr.Value.AsString()] = true
			}
		}
	}
	return results
}
