package ratelimitcoordinator

import (
	"context"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
	server "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

func TestCoordinatorSharesFixedWindowAcrossInstances(t *testing.T) {
	kv := testKeyValue(t)
	first := testCoordinator(t, kv)
	second := testCoordinator(t, kv)
	request := fixedWindowRequest(25, time.Minute)

	var allowed atomic.Int64
	var failures atomic.Int64
	var group sync.WaitGroup
	for index := 0; index < 100; index++ {
		group.Add(1)
		go func(candidate *Coordinator) {
			defer group.Done()
			decision, err := candidate.AcquireProviderRateLimit(context.Background(), request)
			if err != nil {
				failures.Add(1)
				return
			}
			if decision.Allowed {
				allowed.Add(1)
			}
		}([]*Coordinator{first, second}[index%2])
	}
	group.Wait()

	if failures.Load() != 0 {
		t.Fatalf("acquisition failures = %d", failures.Load())
	}
	if allowed.Load() != 25 {
		t.Fatalf("allowed = %d, want 25", allowed.Load())
	}
}

func TestCoordinatorANDDoesNotPartiallyConsume(t *testing.T) {
	kv := testKeyValue(t)
	coordinator := testCoordinator(t, kv)
	request := fixedWindowRequest(1, time.Minute)
	request.Policies = append(request.Policies, resolvedFixedPolicy("wide", "service_version", request.ServiceVersionID, 2, time.Minute))

	first, err := coordinator.AcquireProviderRateLimit(context.Background(), request)
	if err != nil || !first.Allowed {
		t.Fatalf("first acquisition = %+v, %v", first, err)
	}
	second, err := coordinator.AcquireProviderRateLimit(context.Background(), request)
	if err != nil || second.Allowed {
		t.Fatalf("second acquisition = %+v, %v", second, err)
	}
	state := loadRequestState(t, kv, request)
	for _, policy := range state.Policies {
		if policy.FixedWindowUsed != 1 {
			t.Fatalf("policy %q used = %d, want 1", policy.Name, policy.FixedWindowUsed)
		}
	}
}

func TestCoordinatorRefillsTokenBucketAndResetsChangedConfig(t *testing.T) {
	kv := testKeyValue(t)
	coordinator := testCoordinator(t, kv)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	coordinator.now = func() time.Time { return now }
	request := tokenBucketRequest(2, 1, time.Second)

	assertAllowed(t, coordinator, request, true)
	assertAllowed(t, coordinator, request, true)
	assertAllowed(t, coordinator, request, false)
	now = now.Add(time.Second)
	assertAllowed(t, coordinator, request, true)

	request.Policies[0].Capacity = 1
	request.Policies[0].ConfigHash = "changed"
	assertAllowed(t, coordinator, request, true)
	assertAllowed(t, coordinator, request, false)
}

func TestCoordinatorSynchronizesClampAndCooldown(t *testing.T) {
	kv := testKeyValue(t)
	coordinator := testCoordinator(t, kv)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	coordinator.now = func() time.Time { return now }
	request := tokenBucketRequest(10, 1, time.Second)
	assertAllowed(t, coordinator, request, true)

	remaining := int64(0)
	cooldown := now.Add(2 * time.Second)
	err := coordinator.SyncProviderRateLimit(context.Background(), ratelimitpolicy.SyncRequest{
		AccountID: request.AccountID, ServiceVersionID: request.ServiceVersionID,
		CooldownUntil: &cooldown,
		Observations: []ratelimitpolicy.ResponseObservation{{
			PolicyName: "primary", ScopeKind: "connection",
			ScopeID: uuid.MustParse(request.Policies[0].ScopeID), Algorithm: "token_bucket",
			LocalLimit: 10, Remaining: &remaining,
		}},
	})
	if err != nil {
		t.Fatalf("synchronize: %v", err)
	}
	decision, err := coordinator.AcquireProviderRateLimit(context.Background(), request)
	if err != nil || decision.Allowed || decision.RetryAfter != 2*time.Second {
		t.Fatalf("cooldown decision = %+v, %v", decision, err)
	}
	now = cooldown
	assertAllowed(t, coordinator, request, true)
}

func TestCoordinatorInvalidatesRemoteLeaseOnControlEpoch(t *testing.T) {
	kv := testKeyValue(t)
	first := testCoordinator(t, kv)
	second := testCoordinator(t, kv)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	first.now = func() time.Time { return now }
	second.now = func() time.Time { return now }
	request := fixedWindowRequest(10, time.Minute)
	decision, err := first.AcquireProviderRateLimit(context.Background(), request)
	if err != nil || !decision.Allowed {
		t.Fatalf("reserve lease = %+v, %v", decision, err)
	}
	cooldown := now.Add(time.Minute)
	err = second.SyncProviderRateLimit(context.Background(), fixedWindowSync(request, &cooldown))
	if err != nil {
		t.Fatalf("remote synchronization: %v", err)
	}
	waitForLeaseInvalidation(t, first, request)
	decision, err = first.AcquireProviderRateLimit(context.Background(), request)
	if err != nil || decision.Allowed {
		t.Fatalf("post-clamp acquisition = %+v, %v", decision, err)
	}
}

func TestCoordinatorExpiresLeaseAtFixedWindowBoundary(t *testing.T) {
	kv := testKeyValue(t)
	coordinator := testCoordinator(t, kv)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	coordinator.now = func() time.Time { return now }
	request := fixedWindowRequest(100, time.Second)
	assertAllowed(t, coordinator, request, true)
	local, err := coordinator.AcquireProviderRateLimit(context.Background(), request)
	if err != nil || !local.LocalLease {
		t.Fatalf("second acquisition should use lease: %+v, %v", local, err)
	}
	now = now.Add(time.Second)
	refreshed, err := coordinator.AcquireProviderRateLimit(context.Background(), request)
	if err != nil || refreshed.LocalLease || refreshed.CoordinationAttempts == 0 {
		t.Fatalf("boundary acquisition should refresh centrally: %+v, %v", refreshed, err)
	}
}

func TestCoordinatorDisablesLeasesWhenEpochWatchStops(t *testing.T) {
	kv := testKeyValue(t)
	coordinator := testCoordinator(t, kv)
	request := fixedWindowRequest(100, time.Minute)
	assertAllowed(t, coordinator, request, true)
	if err := coordinator.Close(); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for coordinator.leaseEnabled.Load() && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	decision, err := coordinator.AcquireProviderRateLimit(context.Background(), request)
	if err != nil || decision.LocalLease || decision.CoordinationAttempts == 0 {
		t.Fatalf("watchless acquisition should use JetStream directly: %+v, %v", decision, err)
	}
}

func TestCoordinatorUsesConservativePostgresRecovery(t *testing.T) {
	kv := testKeyValue(t)
	request := fixedWindowRequest(2, time.Second)
	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	recovered := newState(request, now)
	coordinator, err := New(kv, staticRecovery{state: &recovered})
	if err != nil {
		t.Fatal(err)
	}
	coordinator.now = func() time.Time { return now }
	assertAllowed(t, coordinator, request, false)
	now = now.Add(2 * time.Second)
	assertAllowed(t, coordinator, request, true)
}

type staticRecovery struct {
	state *ratelimitpolicy.StateEnvelope
}

func fixedWindowSync(request ratelimitpolicy.AcquireRequest, cooldown *time.Time) ratelimitpolicy.SyncRequest {
	policy := request.Policies[0]
	return ratelimitpolicy.SyncRequest{
		AccountID: request.AccountID, ServiceVersionID: request.ServiceVersionID,
		CooldownUntil: cooldown,
		Observations: []ratelimitpolicy.ResponseObservation{{
			PolicyName: policy.Name, ScopeKind: policy.ScopeKind, ScopeID: uuid.MustParse(policy.ScopeID),
			Algorithm: policy.Algorithm, LocalLimit: policy.Limit, DurationMs: policy.DurationMs,
		}},
	}
}

func waitForLeaseInvalidation(t *testing.T, coordinator *Coordinator, request ratelimitpolicy.AcquireRequest) {
	t.Helper()
	_, key, err := validateAcquireRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		held := coordinator.leases.hold(key)
		active := held.entry.lease != nil
		held.release()
		if !active {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("remote control epoch did not invalidate the local lease")
}

func (s staticRecovery) LoadProviderRateLimitState(context.Context, ratelimitpolicy.AcquireRequest) (*ratelimitpolicy.StateEnvelope, error) {
	return s.state, nil
}

func assertAllowed(t *testing.T, coordinator *Coordinator, request ratelimitpolicy.AcquireRequest, want bool) {
	t.Helper()
	decision, err := coordinator.AcquireProviderRateLimit(context.Background(), request)
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if decision.Allowed != want {
		t.Fatalf("allowed = %v, want %v; decision=%+v", decision.Allowed, want, decision)
	}
}

func fixedWindowRequest(limit int64, duration time.Duration) ratelimitpolicy.AcquireRequest {
	versionID := uuid.New()
	return ratelimitpolicy.AcquireRequest{
		AccountID: uuid.New(), ServiceVersionID: versionID,
		Policies: []ratelimitpolicy.ResolvedPolicy{resolvedFixedPolicy("primary", "connection", uuid.New(), limit, duration)},
	}
}

func resolvedFixedPolicy(name, scope string, scopeID uuid.UUID, limit int64, duration time.Duration) ratelimitpolicy.ResolvedPolicy {
	return ratelimitpolicy.ResolvedPolicy{
		Name: name, Unit: "request", ScopeKind: scope, ScopeID: scopeID.String(), Cost: 1,
		Algorithm: "fixed_window", ConfigHash: name + "-config", Limit: limit, DurationMs: duration.Milliseconds(),
	}
}

func tokenBucketRequest(capacity, refill int64, interval time.Duration) ratelimitpolicy.AcquireRequest {
	return ratelimitpolicy.AcquireRequest{
		AccountID: uuid.New(), ServiceVersionID: uuid.New(),
		Policies: []ratelimitpolicy.ResolvedPolicy{{
			Name: "primary", Unit: "request", ScopeKind: "connection", ScopeID: uuid.NewString(), Cost: 1,
			Algorithm: "token_bucket", ConfigHash: "token-config", Capacity: capacity,
			RefillUnits: refill, RefillIntervalMs: interval.Milliseconds(),
		}},
	}
}

func loadRequestState(t *testing.T, kv nats.KeyValue, request ratelimitpolicy.AcquireRequest) ratelimitpolicy.StateEnvelope {
	t.Helper()
	policies, key, err := validateAcquireRequest(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = policies
	entry, err := kv.Get(key)
	if err != nil {
		t.Fatal(err)
	}
	state, err := decodeState(entry.Value())
	if err != nil {
		t.Fatal(err)
	}
	return state
}

func testCoordinator(t testing.TB, kv nats.KeyValue) *Coordinator {
	t.Helper()
	coordinator, err := New(kv, nil)
	if err != nil {
		t.Fatal(err)
	}
	return coordinator
}

func BenchmarkCoordinatorHotScope(b *testing.B) {
	kv := testKeyValue(b)
	coordinators := []*Coordinator{testCoordinator(b, kv), testCoordinator(b, kv)}
	request := fixedWindowRequest(1_000_000_000, time.Hour)
	var index atomic.Int64
	var attempts atomic.Int64
	var coordinated atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		coordinator := coordinators[index.Add(1)%int64(len(coordinators))]
		for pb.Next() {
			decision, err := coordinator.AcquireProviderRateLimit(context.Background(), request)
			if err != nil || !decision.Allowed {
				b.Fatalf("acquire = %+v, %v", decision, err)
			}
			attempts.Add(decision.CoordinationAttempts)
			if decision.CoordinationAttempts > 0 {
				coordinated.Add(1)
			}
		}
	})
	b.ReportMetric(float64(attempts.Load())/float64(b.N), "attempts/op")
	b.ReportMetric(float64(coordinated.Load())/float64(b.N), "coordinated/op")
}

func TestCoordinatorLatencyProfile(t *testing.T) {
	if os.Getenv("FUSED_RATE_LIMIT_PERF") != "1" {
		t.Skip("set FUSED_RATE_LIMIT_PERF=1 to run the local latency profile")
	}
	const sampleCount = 10_000
	const workers = 8
	kv := testKeyValue(t)
	coordinators := []*Coordinator{testCoordinator(t, kv), testCoordinator(t, kv)}
	request := fixedWindowRequest(1_000_000_000, time.Hour)
	durations := make([]time.Duration, sampleCount)
	var attempts atomic.Int64
	var coordinated atomic.Int64
	var group sync.WaitGroup
	start := make(chan struct{})
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go recordCoordinatorLatency(t, start, &group, coordinators[worker%2], request, durations, worker, workers, &attempts, &coordinated)
	}
	close(start)
	group.Wait()
	sort.Slice(durations, func(i, j int) bool { return durations[i] < durations[j] })
	t.Logf("samples=%d p50=%s p95=%s p99=%s coordinated/op=%.4f attempts/coordinated=%.4f",
		sampleCount, percentile(durations, 50), percentile(durations, 95), percentile(durations, 99),
		float64(coordinated.Load())/sampleCount, float64(attempts.Load())/float64(coordinated.Load()),
	)
}

func recordCoordinatorLatency(t *testing.T, start <-chan struct{}, group *sync.WaitGroup, coordinator *Coordinator, request ratelimitpolicy.AcquireRequest, durations []time.Duration, offset, stride int, attempts, coordinated *atomic.Int64) {
	defer group.Done()
	<-start
	for index := offset; index < len(durations); index += stride {
		started := time.Now()
		decision, err := coordinator.AcquireProviderRateLimit(context.Background(), request)
		durations[index] = time.Since(started)
		if err != nil || !decision.Allowed {
			t.Errorf("acquire = %+v, %v", decision, err)
			return
		}
		attempts.Add(decision.CoordinationAttempts)
		if decision.CoordinationAttempts > 0 {
			coordinated.Add(1)
		}
	}
}

func percentile(samples []time.Duration, percentile int) time.Duration {
	index := (len(samples)*percentile + 99) / 100
	if index > 0 {
		index--
	}
	return samples[index]
}

func testKeyValue(t testing.TB) nats.KeyValue {
	t.Helper()
	options := &server.Options{Host: "127.0.0.1", Port: -1, JetStream: true, StoreDir: t.TempDir()}
	natsServer, err := server.NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	go natsServer.Start()
	if !natsServer.ReadyForConnections(5 * time.Second) {
		t.Fatal("NATS server did not become ready")
	}
	t.Cleanup(natsServer.Shutdown)
	connection, err := nats.Connect(natsServer.ClientURL())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(connection.Close)
	jetStream, err := connection.JetStream()
	if err != nil {
		t.Fatal(err)
	}
	kv, err := jetStream.CreateKeyValue(&nats.KeyValueConfig{Bucket: "TEST_PROVIDER_LIMITS", History: 1})
	if err != nil {
		t.Fatal(err)
	}
	return kv
}
