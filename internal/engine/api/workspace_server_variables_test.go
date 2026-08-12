package api

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestWorkspaceExecutionPolicyServerVariablesStayLocal(t *testing.T) {
	var policy workspaceExecutionPolicy
	if err := json.Unmarshal([]byte(`{"server_variables":{"region":"eu"}}`), &policy); err != nil {
		t.Fatalf("decode policy: %v", err)
	}
	override := workspaceExecutionPolicyOverride(uuid.New(), nil, &policy)
	if override.ServerVariables["region"] != "eu" {
		t.Fatalf("override variables = %#v", override.ServerVariables)
	}
	published := publicExecutionPolicy(&policy)
	payload, err := json.Marshal(published)
	if err != nil {
		t.Fatalf("marshal public policy: %v", err)
	}
	if strings.Contains(string(payload), "server_variables") || policy.ServerVariables["region"] != "eu" {
		t.Fatalf("public projection leaked or mutated local server variables: %s", payload)
	}
}

func TestPublicExecutionPolicyExcludesWorkspaceControls(t *testing.T) {
	public := true
	retry := canonicalWorkspaceRetryTest(t)
	policy := workspaceExecutionPolicy{
		Public:      &public,
		RetryConfig: &retry,
		Reset:       true,
		ServerVariables: map[string]string{
			"region": "eu",
		},
	}
	payload, err := json.Marshal(publicExecutionPolicy(&policy))
	if err != nil {
		t.Fatalf("marshal public policy: %v", err)
	}
	for _, forbidden := range []string{"public", "retry_config", "reset", "server_variables"} {
		if strings.Contains(string(payload), `"`+forbidden+`"`) {
			t.Fatalf("Registry payload leaked %q: %s", forbidden, payload)
		}
	}
	var published struct {
		Retry *retryConfig `json:"retry"`
	}
	if err := json.Unmarshal(payload, &published); err != nil {
		t.Fatalf("decode Registry payload: %v", err)
	}
	if published.Retry == nil || len(published.Retry.Rules) != len(retry.Rules) {
		t.Fatalf("Registry payload did not canonicalize retry_config: %#v", published.Retry)
	}
}

func TestExecutionPolicyPublishActionsUseRegistryProjection(t *testing.T) {
	serviceID := uuid.New()
	policy := &workspaceExecutionPolicy{Reset: true, ServerVariables: map[string]string{"region": "eu"}}
	desired := workspaceDesiredState{Services: map[uuid.UUID]workspaceDesiredService{
		serviceID: {
			ServiceID:       serviceID,
			ExecutionPolicy: policy,
			VersionPolicies: []workspaceDesiredVersionPolicy{{Version: "v1", ExecutionPolicy: policy}},
		},
	}}
	updater := &mockRegistryClient{}
	action := workspacePlanAction{ServiceID: serviceID.String()}
	if err := applyWorkspaceExecutionPolicyPublishAction(context.Background(), updater, "key", desired, action); err != nil {
		t.Fatalf("publish service policy: %v", err)
	}
	action.Version = "v1"
	if err := applyWorkspaceVersionExecutionPolicyPublishAction(context.Background(), updater, "key", desired, action); err != nil {
		t.Fatalf("publish version policy: %v", err)
	}
	if len(updater.configPolicies) != 1 || len(updater.versionConfigUpdates) != 1 {
		t.Fatalf("publish calls = service %d, version %d", len(updater.configPolicies), len(updater.versionConfigUpdates))
	}
	if _, ok := updater.configPolicies[0].(*registryExecutionPolicy); !ok {
		t.Fatalf("service publish type = %T", updater.configPolicies[0])
	}
	if _, ok := updater.versionConfigUpdates[0].Policy.(*registryExecutionPolicy); !ok {
		t.Fatalf("version publish type = %T", updater.versionConfigUpdates[0].Policy)
	}
}

func TestValidateWorkspaceServerVariablesRejectsUnsafeValues(t *testing.T) {
	policy := &workspaceExecutionPolicy{ServerVariables: map[string]string{"region": "eu\nunsafe"}}
	if err := validateWorkspaceExecutionPolicy("service", "v1", policy); err == nil {
		t.Fatal("expected unsafe server variable rejection")
	}
}
