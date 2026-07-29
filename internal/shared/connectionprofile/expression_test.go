package connectionprofile

import "testing"

func TestParseExpression(t *testing.T) {
	tests := []struct {
		name       string
		value      string
		kind       SourceKind
		envName    string
		sourcePath string
		wantErr    bool
	}{
		{name: "literal", value: "2026-01", kind: SourceLiteral},
		{name: "environment", value: "$SHOPIFY_API_VERSION", kind: SourceEnvironment, envName: "SHOPIFY_API_VERSION"},
		{name: "provider ID", value: "${resource.provider_resource_id}", kind: SourceConnectionResource, sourcePath: "provider_resource_id"},
		{name: "base URL", value: "${resource.base_url}", kind: SourceConnectionResource, sourcePath: "base_url"},
		{name: "metadata", value: "${resource.metadata.account_id}", kind: SourceConnectionResource, sourcePath: "metadata.account_id"},
		{name: "mixed", value: "prefix-${resource.provider_resource_id}", wantErr: true},
		{name: "unknown namespace", value: "${connection.token}", wantErr: true},
		{name: "unknown resource field", value: "${resource.token}", wantErr: true},
		{name: "malformed environment", value: "$BAD-NAME", wantErr: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			expression, err := ParseExpression(test.value)
			if (err != nil) != test.wantErr {
				t.Fatalf("ParseExpression error = %v, wantErr %v", err, test.wantErr)
			}
			if expression.Kind != test.kind || expression.EnvName != test.envName || expression.SourcePath != test.sourcePath {
				t.Fatalf("ParseExpression = %#v", expression)
			}
		})
	}
}

func TestParseExpressionErrorDoesNotLeakValue(t *testing.T) {
	value := "prefix-${resource.metadata.customer-secret-value}"
	_, err := ParseExpression(value)
	if err == nil {
		t.Fatal("expected invalid mixed expression")
	}
	if err.Error() == value {
		t.Fatal("validation error leaked the supplied value")
	}
}
