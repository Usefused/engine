package engine

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
)

// TestContractPoliciesResolveWithoutProviderBranches proves generic policy data, rather than provider names, drives quota coordination.
func TestContractPoliciesResolveWithoutProviderBranches(t *testing.T) {
	for name, contract := range quotaContractCases() {
		t.Run(name, func(t *testing.T) { assertQuotaFixtureResolves(t, contract.config, contract.stableKey) })
	}
}

// assertQuotaFixtureResolves also verifies that resolved identity material is hashed before crossing the coordinator boundary.
func assertQuotaFixtureResolves(t *testing.T, config ratelimitpolicy.Config, stableKey string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	ctx = WithProviderRateLimitIdentity(ctx, uuid.New(), uuid.New(), uuid.New())
	ctx = WithProviderQuotaBindings(ctx, map[string]string{
		"connection.project_id": "project-secret", "connection.dataset": "dataset-secret",
		"connection.tenant_id": "tenant-secret", "connection.site": "site-secret",
		"connection.rate_limit_resource": "resource-secret", "engine.egress_ip_class": "egress-secret",
	})
	service := &models.Service{ServiceVersionID: uuid.New(), RateLimit: &config}
	request, err := providerRateLimitRequest(ctx, service, &models.IntegrationObject{StableKey: stableKey})
	if err != nil || len(request.Policies) != len(config.Policies) {
		t.Fatalf("resolve = %d policies, %v", len(request.Policies), err)
	}
	for _, policy := range request.Policies {
		if strings.Contains(policy.ScopeID, "secret") {
			t.Fatal("raw quota identity crossed the coordinator boundary")
		}
	}
}

type quotaContractCase struct {
	config    ratelimitpolicy.Config
	stableKey string
}

// quotaContractCases keeps algorithm and identity coverage local to Engine;
// provider-specific acceptance mapping is deliberately outside this repo.
func quotaContractCases() map[string]quotaContractCase {
	return map[string]quotaContractCase{
		"fixed_service": {
			config:    quotaConfig(ratelimitpolicy.Policy{Name: "requests", Mode: ratelimitpolicy.ModeEnforce, Unit: ratelimitpolicy.UnitRequests, Identity: quotaIdentity(ratelimitpolicy.IdentityServiceVersion, ""), Cost: ratelimitpolicy.CostPlan{Default: 1}, Algorithm: ratelimitpolicy.AlgorithmFixedWindow, FixedWindow: &ratelimitpolicy.FixedWindow{Limit: 100, DurationMs: 60_000}}),
			stableKey: "rest:GET:/items",
		},
		"token_tenant": {
			config:    quotaConfig(ratelimitpolicy.Policy{Name: "points", Mode: ratelimitpolicy.ModeEnforce, Unit: ratelimitpolicy.UnitPoints, Identity: quotaIdentity(ratelimitpolicy.IdentityTenant, "connection.tenant_id"), Cost: ratelimitpolicy.CostPlan{Default: 1, Rules: []ratelimitpolicy.CostRule{{Operation: "rest:POST:/search", Cost: 5}}}, Algorithm: ratelimitpolicy.AlgorithmTokenBucket, TokenBucket: &ratelimitpolicy.TokenBucket{Capacity: 100, RefillUnits: 10, RefillIntervalMs: 1_000}}),
			stableKey: "rest:POST:/search",
		},
		"connection_concurrency": {
			config:    quotaConfig(ratelimitpolicy.Policy{Name: "concurrency", Mode: ratelimitpolicy.ModeEnforce, Unit: ratelimitpolicy.UnitRequests, Identity: quotaIdentity(ratelimitpolicy.IdentityConnection, ""), Cost: ratelimitpolicy.CostPlan{Default: 1}, Algorithm: ratelimitpolicy.AlgorithmConcurrency, Concurrency: &ratelimitpolicy.Concurrency{Limit: 4}}),
			stableKey: "rest:GET:/items",
		},
	}
}

// quotaConfig applies the canonical v3 envelope once for every local policy.
func quotaConfig(policy ratelimitpolicy.Policy) ratelimitpolicy.Config {
	return ratelimitpolicy.Config{Version: ratelimitpolicy.Version, Policies: []ratelimitpolicy.Policy{policy}}
}

// quotaIdentity keeps binding requirements explicit because Engine must hash
// resolved identity material before coordinator transport.
func quotaIdentity(kind ratelimitpolicy.IdentityKind, binding string) ratelimitpolicy.BucketIdentity {
	return ratelimitpolicy.BucketIdentity{Inputs: []ratelimitpolicy.IdentityInput{{Kind: kind, Binding: binding}}}
}
