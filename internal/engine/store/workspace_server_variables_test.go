package store

import "testing"

func TestDecodeWorkspaceExecutionPolicyServerVariables(t *testing.T) {
	var override WorkspaceExecutionPolicyOverride
	if err := decodeWorkspaceExecutionPolicyJSON(&override, nil, nil, nil, nil, []byte(`{"region":"eu"}`)); err != nil {
		t.Fatalf("decodeWorkspaceExecutionPolicyJSON: %v", err)
	}
	if override.ServerVariables["region"] != "eu" {
		t.Fatalf("server variables = %#v", override.ServerVariables)
	}
}
