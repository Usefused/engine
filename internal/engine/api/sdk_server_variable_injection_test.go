package api

import (
	"strings"
	"testing"
)

// TestValidateAppServerVariableInjection covers the routing-specific grammar
// without changing established header/query/path/body injection behavior.
func TestValidateAppServerVariableInjection(t *testing.T) {
	tests := []struct {
		name      string
		injection InjectionConfig
		wantError string
	}{
		{name: "bucket value", injection: InjectionConfig{Location: "server_variable", Name: "app_id", Value: "${bucket.values.SENDBIRD_APP_ID}", Mode: "force"}},
		{name: "bucket env default mode", injection: InjectionConfig{Location: "server_variable", Name: "app_id", Value: "${bucket.env.SENDBIRD_APP_ID}"}},
		{name: "secret", injection: InjectionConfig{Location: "server_variable", Name: "app_id", Value: "${bucket.secrets.SENDBIRD_APP_ID}"}, wantError: "requires ${bucket.env.*} or ${bucket.values.*}"},
		{name: "unsafe name", injection: InjectionConfig{Location: "server_variable", Name: "app/id", Value: "${bucket.values.SENDBIRD_APP_ID}"}, wantError: "name is invalid"},
		{name: "unknown mode", injection: InjectionConfig{Location: "server_variable", Name: "app_id", Value: "${bucket.values.SENDBIRD_APP_ID}", Mode: "replace"}, wantError: "mode must be force or default"},
	}
	// Each case passes through the same service-level validator used by both SDK
	// and MCP planning.
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateAppServiceDoc("sendbird", sdkConfigServiceDoc{SelectAll: true, Injections: []InjectionConfig{test.injection}})
			if test.wantError == "" && err != nil {
				t.Fatalf("validateAppServiceDoc: %v", err)
			}
			if test.wantError != "" && (err == nil || !strings.Contains(err.Error(), test.wantError)) {
				t.Fatalf("validateAppServiceDoc error = %v, want %q", err, test.wantError)
			}
		})
	}
}

// TestCanonicalAppServerVariableInjectionDefaultsToForce proves omitted mode
// has one deterministic immutable meaning before hashing and persistence.
func TestCanonicalAppServerVariableInjectionDefaultsToForce(t *testing.T) {
	doc := canonicalAppDocument(sdkConfigDocument{Services: map[string]sdkConfigServiceDoc{
		" sendbird ": {Injections: []InjectionConfig{{Location: " SERVER_VARIABLE ", Name: " app_id ", Value: " ${bucket.values.SENDBIRD_APP_ID} "}}},
	}})
	injection := doc.Services["sendbird"].Injections[0]
	if injection.Location != appServerVariableLocation || injection.Name != "app_id" || injection.Mode != "force" || injection.Value != "${bucket.values.SENDBIRD_APP_ID}" {
		t.Fatalf("canonical injection = %#v", injection)
	}
}

// TestCanonicalAppInjectionsPreservesExistingTransportFields prevents routing
// normalization from changing case- or whitespace-sensitive request bindings.
func TestCanonicalAppInjectionsPreservesExistingTransportFields(t *testing.T) {
	want := InjectionConfig{Location: "BODY", Name: " CaseSensitive ", Value: " padded value ", Mode: "replace"}
	got := canonicalAppInjections([]InjectionConfig{want})[0]
	if got != want {
		t.Fatalf("ordinary injection = %#v, want %#v", got, want)
	}
}
