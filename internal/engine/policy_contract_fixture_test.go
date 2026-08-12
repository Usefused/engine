package engine

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
)

// TestContractPoliciesResolveWithoutProviderBranches proves generic policy data, rather than provider names, drives quota coordination.
func TestContractPoliciesResolveWithoutProviderBranches(t *testing.T) {
	root := filepath.Join("..", "..", "..", "contract-fixtures", "rate-limit")
	fixtures := map[string]string{
		"v3_shared_credential.json": "rest:GET:/items", "v3_method_concurrency.json": "rest:GET:/api/v2/invoices",
		"v3_simultaneous_windows.json": "rest:GET:/items", "v3_weighted_burst_tenant.json": "rest:POST:/wiki/api/v2/search",
		"v3_dynamic_headers.json": "rest:GET:/items", "v3_quota_units.json": "rest:POST:/v1/resources/send",
		"v3_complexity.json": "graphql:query:Items", "v3_composite_identity.json": "rest:GET:/items", "v3_concurrency.json": "rest:GET:/items",
	}
	for name, stableKey := range fixtures {
		t.Run(name, func(t *testing.T) { assertQuotaFixtureResolves(t, filepath.Join(root, name), stableKey) })
	}
}

// assertQuotaFixtureResolves also verifies that resolved identity material is hashed before crossing the coordinator boundary.
func assertQuotaFixtureResolves(t *testing.T, path, stableKey string) {
	t.Helper()
	var config ratelimitpolicy.Config
	decodeFixtureFile(t, path, &config)
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

// decodeFixtureFile keeps fixture decoding strict enough that malformed policy JSON fails at the test boundary.
func decodeFixtureFile(t *testing.T, path string, target any) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(payload, target); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
}
