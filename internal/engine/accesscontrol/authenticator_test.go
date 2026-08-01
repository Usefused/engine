package accesscontrol

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

type principalLoaderStub struct {
	mu        sync.Mutex
	principal ControlPrincipal
	err       error
	loads     int
}

type blockingPrincipalLoader struct {
	mu        sync.Mutex
	principal ControlPrincipal
	err       error
	loads     int
	started   chan struct{}
	release   chan struct{}
	once      sync.Once
}

func (s *blockingPrincipalLoader) LoadControlPrincipal(context.Context, string) (ControlPrincipal, error) {
	s.mu.Lock()
	s.loads++
	principal, err := s.principal, s.err
	s.mu.Unlock()
	s.once.Do(func() { close(s.started) })
	<-s.release
	return principal, err
}

func (s *blockingPrincipalLoader) setResult(principal ControlPrincipal, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.principal, s.err = principal, err
}

func (s *blockingPrincipalLoader) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

func (s *principalLoaderStub) LoadControlPrincipal(context.Context, string) (ControlPrincipal, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.loads++
	return s.principal, s.err
}

func (s *principalLoaderStub) loadCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.loads
}

func TestAuthenticatorCachesCompleteActorSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	loader := &principalLoaderStub{principal: testPrincipal(4)}
	authenticator := mustAuthenticator(t, loader, 4, AuthenticatorOptions{
		CacheEntries: 4,
		CacheTTL:     30 * time.Second,
		Now:          func() time.Time { return now },
	})

	first, err := authenticator.AuthenticateControlCredential(context.Background(), "secret")
	if err != nil {
		t.Fatalf("first AuthenticateControlCredential: %v", err)
	}
	second, err := authenticator.AuthenticateControlCredential(context.Background(), "secret")
	if err != nil {
		t.Fatalf("second AuthenticateControlCredential: %v", err)
	}
	if loader.loads != 1 {
		t.Fatalf("loader calls = %d, want 1", loader.loads)
	}
	if first.SubjectID != second.SubjectID || second.Authorization.Revision != 4 {
		t.Fatalf("cached actor mismatch: first=%#v second=%#v", first, second)
	}
}

func TestAuthenticatorShortNegativeCacheBoundsInvalidCredentialLoads(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	loader := &principalLoaderStub{err: ErrAuthenticationRequired}
	authenticator := mustAuthenticator(t, loader, 1, AuthenticatorOptions{
		CacheEntries: 2, NegativeCacheTTL: time.Second, Now: func() time.Time { return now },
	})
	for range 5 {
		if _, err := authenticator.AuthenticateControlCredential(context.Background(), "invalid"); !errors.Is(err, ErrAuthenticationRequired) {
			t.Fatalf("authentication error = %v", err)
		}
	}
	if loader.loadCount() != 1 {
		t.Fatalf("loader calls = %d, want 1", loader.loadCount())
	}
	now = now.Add(2 * time.Second)
	_, _ = authenticator.AuthenticateControlCredential(context.Background(), "invalid")
	if loader.loadCount() != 2 {
		t.Fatalf("loader calls after expiry = %d, want 2", loader.loadCount())
	}
}

func TestAuthenticatorCoalescesConcurrentInvalidCredentialLoads(t *testing.T) {
	loader := &principalLoaderStub{err: ErrAuthenticationRequired}
	authenticator := mustAuthenticator(t, loader, 1, AuthenticatorOptions{})
	var wait sync.WaitGroup
	for range 20 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			_, _ = authenticator.AuthenticateControlCredential(context.Background(), "same-invalid-key")
		}()
	}
	wait.Wait()
	if loader.loadCount() != 1 {
		t.Fatalf("loader calls = %d, want 1", loader.loadCount())
	}
}

func TestAuthenticatorCoalescesConcurrentValidCredentialLoads(t *testing.T) {
	loader := &blockingPrincipalLoader{
		principal: testPrincipal(1), started: make(chan struct{}), release: make(chan struct{}),
	}
	authenticator := mustAuthenticator(t, loader, 1, AuthenticatorOptions{})
	const callers = 50
	start := make(chan struct{})
	errorsByCaller := make(chan error, callers)
	var ready, done sync.WaitGroup
	ready.Add(callers)
	done.Add(callers)
	for range callers {
		go func() {
			defer done.Done()
			ready.Done()
			<-start
			_, err := authenticator.AuthenticateControlCredential(context.Background(), "same-valid-key")
			errorsByCaller <- err
		}()
	}
	ready.Wait()
	close(start)
	<-loader.started
	close(loader.release)
	done.Wait()
	close(errorsByCaller)
	for err := range errorsByCaller {
		if err != nil {
			t.Fatalf("AuthenticateControlCredential: %v", err)
		}
	}
	if loader.loadCount() != 1 {
		t.Fatalf("loader calls = %d, want 1", loader.loadCount())
	}
}

func TestAuthenticatorDoesNotPublishDenialAcrossRevisionChange(t *testing.T) {
	loader := &blockingPrincipalLoader{
		err: ErrAuthenticationRequired, started: make(chan struct{}), release: make(chan struct{}),
	}
	authenticator := mustAuthenticator(t, loader, 1, AuthenticatorOptions{})
	firstResult := make(chan error, 1)
	go func() {
		_, err := authenticator.AuthenticateControlCredential(context.Background(), "newly-created-key")
		firstResult <- err
	}()
	<-loader.started
	loader.setResult(testPrincipal(2), nil)
	if !authenticator.SetRevision(2) {
		t.Fatal("SetRevision did not advance")
	}
	close(loader.release)
	if err := <-firstResult; !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("in-flight authentication error = %v", err)
	}
	if _, err := authenticator.AuthenticateControlCredential(context.Background(), "newly-created-key"); err != nil {
		t.Fatalf("new credential remained negatively cached: %v", err)
	}
	if loader.loadCount() != 2 {
		t.Fatalf("loader calls = %d, want 2", loader.loadCount())
	}
}

func TestAuthenticatorRevisionClearsNegativeCache(t *testing.T) {
	loader := &principalLoaderStub{err: ErrAuthenticationRequired}
	authenticator := mustAuthenticator(t, loader, 1, AuthenticatorOptions{})
	_, _ = authenticator.AuthenticateControlCredential(context.Background(), "new-key")
	loader.mu.Lock()
	loader.err = nil
	loader.principal = testPrincipal(2)
	loader.mu.Unlock()
	authenticator.SetRevision(2)
	if _, err := authenticator.AuthenticateControlCredential(context.Background(), "new-key"); err != nil {
		t.Fatalf("valid credential remained negatively cached: %v", err)
	}
}

func TestAuthenticatorBoundsNegativeCredentialCache(t *testing.T) {
	loader := &principalLoaderStub{err: ErrAuthenticationRequired}
	authenticator := mustAuthenticator(t, loader, 1, AuthenticatorOptions{CacheEntries: 1})
	for _, credential := range []string{"first-invalid", "second-invalid", "first-invalid"} {
		if _, err := authenticator.AuthenticateControlCredential(context.Background(), credential); !errors.Is(err, ErrAuthenticationRequired) {
			t.Fatalf("authentication error = %v", err)
		}
	}
	if loader.loadCount() != 3 {
		t.Fatalf("loader calls = %d, want 3 after negative-cache LRU eviction", loader.loadCount())
	}
}

func TestAuthenticatorRecordsNegativeCacheMetrics(t *testing.T) {
	reader := sdkmetric.NewManualReader()
	provider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))
	counter, err := provider.Meter("engine.accesscontrol.test").Int64Counter("engine.authentication.negative_cache.requests")
	if err != nil {
		t.Fatalf("create negative-cache counter: %v", err)
	}
	previousCounter := negativeCacheRequests
	negativeCacheRequests = counter
	defer func() {
		negativeCacheRequests = previousCounter
		_ = provider.Shutdown(context.Background())
	}()

	loader := &principalLoaderStub{err: ErrAuthenticationRequired}
	authenticator := mustAuthenticator(t, loader, 1, AuthenticatorOptions{})
	_, _ = authenticator.AuthenticateControlCredential(context.Background(), "metric-invalid")
	_, _ = authenticator.AuthenticateControlCredential(context.Background(), "metric-invalid")
	counts := collectNegativeCacheMetricCounts(t, reader)
	for result, want := range map[string]int64{"miss": 1, "load_denied": 1, "hit": 1} {
		if counts[result] != want {
			t.Fatalf("negative-cache metric %q = %d, want %d (all=%#v)", result, counts[result], want, counts)
		}
	}
}

func TestAuthenticatorMarksNegativeCacheHitSpanDenied(t *testing.T) {
	previousProvider := otel.GetTracerProvider()
	recorder := tracetest.NewSpanRecorder()
	provider := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder))
	otel.SetTracerProvider(provider)
	defer func() {
		_ = provider.Shutdown(context.Background())
		otel.SetTracerProvider(previousProvider)
	}()
	loader := &principalLoaderStub{err: ErrAuthenticationRequired}
	authenticator := mustAuthenticator(t, loader, 1, AuthenticatorOptions{})
	_, _ = authenticator.AuthenticateControlCredential(context.Background(), "traced-invalid")
	_, _ = authenticator.AuthenticateControlCredential(context.Background(), "traced-invalid")
	for _, span := range recorder.Ended() {
		if span.Name() == "engine.authn.control" && span.Status().Code == codes.Error && spanHasBoolAttribute(span.Attributes(), "engine.authn.negative_cache", true) {
			return
		}
	}
	t.Fatal("negative-cache authentication span was not marked as denied")
}

func spanHasBoolAttribute(attributes []attribute.KeyValue, key string, want bool) bool {
	for _, item := range attributes {
		if string(item.Key) == key && item.Value.Type() == attribute.BOOL && item.Value.AsBool() == want {
			return true
		}
	}
	return false
}

func collectNegativeCacheMetricCounts(t *testing.T, reader *sdkmetric.ManualReader) map[string]int64 {
	t.Helper()
	var resourceMetrics metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &resourceMetrics); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	counts := make(map[string]int64)
	for _, scope := range resourceMetrics.ScopeMetrics {
		for _, measurement := range scope.Metrics {
			sum, ok := measurement.Data.(metricdata.Sum[int64])
			if measurement.Name != "engine.authentication.negative_cache.requests" || !ok {
				continue
			}
			for _, point := range sum.DataPoints {
				value, ok := point.Attributes.Value(attribute.Key("result"))
				if ok && value.Type() == attribute.STRING {
					counts[value.AsString()] += point.Value
				}
			}
		}
	}
	return counts
}

func TestAuthenticatorReloadsAfterTTLAndRevisionChange(t *testing.T) {
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	loader := &principalLoaderStub{principal: testPrincipal(2)}
	authenticator := mustAuthenticator(t, loader, 2, AuthenticatorOptions{
		CacheEntries: 4,
		CacheTTL:     time.Second,
		Now:          func() time.Time { return now },
	})

	mustAuthenticate(t, authenticator, "secret")
	now = now.Add(2 * time.Second)
	mustAuthenticate(t, authenticator, "secret")
	loader.principal.Revision = 3
	if !authenticator.SetRevision(3) {
		t.Fatal("SetRevision did not invalidate newer revision")
	}
	mustAuthenticate(t, authenticator, "secret")
	if loader.loads != 3 {
		t.Fatalf("loader calls = %d, want 3", loader.loads)
	}
}

func TestAuthenticatorFailsClosedOnStaleDatabaseRevision(t *testing.T) {
	loader := &principalLoaderStub{principal: testPrincipal(2)}
	authenticator := mustAuthenticator(t, loader, 3, AuthenticatorOptions{})
	_, err := authenticator.AuthenticateControlCredential(context.Background(), "secret")
	if !errors.Is(err, ErrStaleAuthorizationRevision) {
		t.Fatalf("error = %v, want ErrStaleAuthorizationRevision", err)
	}
}

func TestAuthenticatorRevisionNeverMovesBackwardConcurrently(t *testing.T) {
	authenticator := mustAuthenticator(t, &principalLoaderStub{}, 1, AuthenticatorOptions{})
	var wait sync.WaitGroup
	for revision := int64(2); revision <= 100; revision++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			authenticator.SetRevision(revision)
		}()
	}
	wait.Wait()
	if got := authenticator.CurrentRevision(); got != 100 {
		t.Fatalf("revision = %d, want 100", got)
	}
}

func TestAuthenticatorRejectsMissingAndExpiredCredentials(t *testing.T) {
	now := time.Now().UTC()
	expiresAt := now.Add(-time.Second)
	loader := &principalLoaderStub{principal: testPrincipal(1)}
	loader.principal.ExpiresAt = &expiresAt
	authenticator := mustAuthenticator(t, loader, 1, AuthenticatorOptions{Now: func() time.Time { return now }})

	if _, err := authenticator.AuthenticateControlCredential(context.Background(), ""); !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("missing credential error = %v", err)
	}
	if loader.loads != 0 {
		t.Fatalf("missing credential queried loader %d time(s)", loader.loads)
	}
	if _, err := authenticator.AuthenticateControlCredential(context.Background(), "expired"); !errors.Is(err, ErrAuthenticationRequired) {
		t.Fatalf("expired credential error = %v", err)
	}
}

func TestAuthenticatorBoundsCredentialCache(t *testing.T) {
	loader := &principalLoaderStub{principal: testPrincipal(1)}
	authenticator := mustAuthenticator(t, loader, 1, AuthenticatorOptions{CacheEntries: 1})
	mustAuthenticate(t, authenticator, "first")
	mustAuthenticate(t, authenticator, "second")
	mustAuthenticate(t, authenticator, "first")
	if loader.loads != 3 {
		t.Fatalf("loader calls = %d, want 3 after LRU eviction", loader.loads)
	}
}

func testPrincipal(revision int64) ControlPrincipal {
	workspaceID := uuid.New()
	return ControlPrincipal{
		AccountID:    uuid.New(),
		WorkspaceID:  workspaceID,
		SubjectID:    uuid.New(),
		CredentialID: uuid.New(),
		Kind:         SubjectUser,
		Revision:     revision,
		EffectiveGrants: []Grant{{
			Permission: PermissionWorkspaceRead,
			Resource:   ResourceRef{Type: ResourceWorkspace, ID: workspaceID},
		}},
	}
}

func mustAuthenticator(t *testing.T, loader PrincipalLoader, revision int64, options AuthenticatorOptions) *Authenticator {
	t.Helper()
	authenticator, err := NewAuthenticator(loader, revision, options)
	if err != nil {
		t.Fatalf("NewAuthenticator: %v", err)
	}
	return authenticator
}

func mustAuthenticate(t *testing.T, authenticator *Authenticator, credential string) Actor {
	t.Helper()
	actor, err := authenticator.AuthenticateControlCredential(context.Background(), credential)
	if err != nil {
		t.Fatalf("AuthenticateControlCredential: %v", err)
	}
	return actor
}
