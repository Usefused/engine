package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/google/uuid"
)

func TestWorkspaceRetryV3SurvivesPublishAndLocalOverride(t *testing.T) {
	payload := readEngineWorkspaceRetryFixture(t)
	var retry retryConfig
	if err := json.Unmarshal(payload, &retry); err != nil {
		t.Fatalf("decode workspace retry: %v", err)
	}
	policy := &workspaceExecutionPolicy{Public: boolPtrEngineTest(true), Retry: &retry}
	if err := validateWorkspaceExecutionPolicy("payments", "v1", policy); err != nil {
		t.Fatalf("validate canonical retry v3: %v", err)
	}

	encoded, err := json.Marshal(publicExecutionPolicy(policy))
	if err != nil {
		t.Fatalf("encode published retry: %v", err)
	}
	assertEngineWorkspaceRetryJSON(t, encodedPolicyRetry(t, encoded), payload)

	override := workspaceExecutionPolicyOverride(uuid.New(), nil, policy)
	if override.RetryConfig == nil || !reflect.DeepEqual(*override.RetryConfig, retry) {
		t.Fatalf("local override changed retry v3: %#v", override.RetryConfig)
	}
}

func TestWorkspaceRetryV3RejectsIncompleteContract(t *testing.T) {
	policy := &workspaceExecutionPolicy{Retry: &retryConfig{Version: 3}}
	err := validateWorkspaceExecutionPolicy("payments", "v1", policy)
	if err == nil {
		t.Fatal("incomplete retry v3 was accepted")
	}
}

func TestWorkspaceExecutionPolicyRejectsRetryAliasesAndResetConflicts(t *testing.T) {
	if err := validateWorkspaceExecutionPolicy("payments", "v1", &workspaceExecutionPolicy{Reset: true}); err != nil {
		t.Fatalf("standalone reset was rejected: %v", err)
	}
	retry := canonicalWorkspaceRetryTest(t)
	aliases := &workspaceExecutionPolicy{Retry: &retry, RetryConfig: &retry}
	if err := validateWorkspaceExecutionPolicy("payments", "v1", aliases); err == nil {
		t.Fatal("retry and retry_config aliases were accepted together")
	}

	conflicts := workspaceResetConflictCases()
	for _, conflict := range conflicts {
		t.Run(conflict.name, func(t *testing.T) {
			policy := &workspaceExecutionPolicy{Reset: true}
			conflict.configure(policy)
			if err := validateWorkspaceExecutionPolicy("payments", "v1", policy); err == nil {
				t.Fatalf("reset with %s was accepted", conflict.name)
			}
		})
	}
}

func canonicalWorkspaceRetryTest(t *testing.T) retryConfig {
	t.Helper()
	var retry retryConfig
	if err := json.Unmarshal(readEngineWorkspaceRetryFixture(t), &retry); err != nil {
		t.Fatalf("decode canonical retry fixture: %v", err)
	}
	return retry
}

type workspaceResetConflictCase struct {
	name      string
	configure func(*workspaceExecutionPolicy)
}

func workspaceResetConflictCases() []workspaceResetConflictCase {
	return []workspaceResetConflictCase{
		{name: "public", configure: func(policy *workspaceExecutionPolicy) { policy.Public = boolPtrEngineTest(false) }},
		{name: "rate_limit", configure: func(policy *workspaceExecutionPolicy) { policy.RateLimit = &rateLimitConfig{} }},
		{name: "retry", configure: func(policy *workspaceExecutionPolicy) { policy.Retry = &retryConfig{} }},
		{name: "retry_config", configure: func(policy *workspaceExecutionPolicy) { policy.RetryConfig = &retryConfig{} }},
		{name: "timeout_ms", configure: func(policy *workspaceExecutionPolicy) { value := 1; policy.TimeoutMs = &value }},
		{name: "pagination", configure: func(policy *workspaceExecutionPolicy) { policy.Pagination = &paginationConfig{} }},
		{name: "base_url", configure: func(policy *workspaceExecutionPolicy) { value := "https://provider.test"; policy.BaseURL = &value }},
		{name: "server_variables", configure: func(policy *workspaceExecutionPolicy) { policy.ServerVariables = map[string]string{"region": "eu"} }},
		{name: "event_extraction_path", configure: func(policy *workspaceExecutionPolicy) { value := "$.data"; policy.EventExtractionPath = &value }},
		{name: "incoming_webhook_config", configure: func(policy *workspaceExecutionPolicy) { policy.IncomingWebhookConfig = &webhookVerifyConfig{} }},
	}
}

func readEngineWorkspaceRetryFixture(t *testing.T) []byte {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("..", "..", "..", "..", "contract-fixtures", "retry", "v3_idempotency_predicates.json"))
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func encodedPolicyRetry(t *testing.T, policy []byte) []byte {
	t.Helper()
	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(policy, &envelope); err != nil {
		t.Fatal(err)
	}
	return envelope["retry"]
}

func assertEngineWorkspaceRetryJSON(t *testing.T, gotPayload, wantPayload []byte) {
	t.Helper()
	var got, want any
	if err := json.Unmarshal(gotPayload, &got); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(wantPayload, &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Engine workspace changed retry v3\ngot:  %s\nwant: %s", gotPayload, wantPayload)
	}
}
