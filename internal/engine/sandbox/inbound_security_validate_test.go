package sandbox

import (
	"encoding/json"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

// inboundSecurityFixture makes a valid passive operation so tests isolate security admission.
func inboundSecurityFixture(name string, scheme fusedobject.InboundSecurityScheme) fusedobject.Webhook {
	return fusedobject.Webhook{Name: "created", Method: "POST", Contract: &fusedobject.InboundOperationContract{
		Kind: "webhook", Path: "created", Tags: []string{}, Parameters: fusedobject.Parameters{}, Responses: fusedobject.Responses{},
		SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{{Scheme: name, Scopes: []string{}}}}},
		SecuritySchemes:      map[string]fusedobject.InboundSecurityScheme{name: scheme},
	}}
}

// TestInboundSecurityAcceptsDocumentaryDefinitions covers the reported schemes without minting verifier policy.
func TestInboundSecurityAcceptsDocumentaryDefinitions(t *testing.T) {
	cases := map[string]fusedobject.InboundSecurityScheme{
		"webhookBasicAuth":        {Type: "http", Scheme: "basic"},
		"TrelloWebhookSignature":  {Type: "apiKey", In: "header", Name: "X-Trello-Webhook"},
		"StripeWebhookSignature":  {Type: "apiKey", In: "header", Name: "Stripe-Signature"},
		"pubSubOIDC":              {Type: "http", Scheme: "bearer", BearerFormat: "JWT"},
		"channelToken":            {Type: "apiKey", In: "header", Name: "X-Goog-Channel-Token"},
		"AtlassianConnectJWT":     {Type: "apiKey", In: "header", Name: "Authorization"},
		"documentaryCustomScheme": {Type: "http", Scheme: "custom-auth"},
		"clientCertificate":       {Type: "mutualTLS"},
	}
	for name, scheme := range cases {
		// Each provider definition must work without any outbound auth catalogue.
		t.Run(name, func(t *testing.T) {
			metadata := &fusedobject.ServiceMetadata{}
			webhook := inboundSecurityFixture(name, scheme)
			// Valid documentary security must not trigger an executable-strategy requirement.
			if err := validateTransportContract(metadata, nil, []fusedobject.Webhook{webhook}); err != nil {
				t.Fatalf("validate documentary scheme: %v", err)
			}
			// Validation is inert: it must not invent provider credentials or a webhook verifier.
			if metadata.IncomingWebhookConfig != nil || len(metadata.AuthConfigs) != 0 {
				t.Fatalf("documentary definition acquired execution authority: %#v", metadata)
			}
		})
	}
}

// TestInboundSecurityRejectsMissingDefinitions proves outbound names never act as an inbound fallback.
func TestInboundSecurityRejectsMissingDefinitions(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{AuthConfigs: fusedobject.AuthConfigs{{Name: "shared", Type: "http", Scheme: "basic"}}}
	webhook := inboundSecurityFixture("shared", fusedobject.InboundSecurityScheme{Type: "http", Scheme: "bearer"})
	webhook.Contract.SecuritySchemes = nil
	// The same outbound name cannot repair a missing inbound definition.
	if err := validateTransportContract(metadata, nil, []fusedobject.Webhook{webhook}); err == nil || !strings.Contains(err.Error(), "unknown scheme") {
		t.Fatalf("missing inbound definition error = %v", err)
	}
	webhook.Contract.SecuritySchemes = map[string]fusedobject.InboundSecurityScheme{"shared": {Type: "http", Scheme: "bearer"}}
	// Independent same-named definitions remain valid when both are actually present.
	if err := validateTransportContract(metadata, nil, []fusedobject.Webhook{webhook}); err != nil {
		t.Fatalf("independent same-name definitions: %v", err)
	}
	endpoint := fusedobject.Endpoint{Name: "list", Method: "GET", Path: "/items", ProviderProtocol: "rest", SecurityRequirements: webhook.Contract.SecurityRequirements}
	// Inbound definitions must not repair an endpoint's missing outbound credential either.
	if err := validateTransportContract(&fusedobject.ServiceMetadata{}, []fusedobject.Endpoint{endpoint}, []fusedobject.Webhook{webhook}); err == nil || !strings.Contains(err.Error(), "unknown scheme") {
		t.Fatalf("inbound definition leaked into outbound namespace: %v", err)
	}
}

// TestInboundSecurityAnonymousAndLegacyContracts keeps supported legacy and anonymous forms explicit.
func TestInboundSecurityAnonymousAndLegacyContracts(t *testing.T) {
	webhook := inboundSecurityFixture("unused", fusedobject.InboundSecurityScheme{Type: "http", Scheme: "basic"})
	webhook.Contract.SecuritySchemes = nil
	webhook.Contract.SecurityRequirements = authrouting.Requirements{{Schemes: []authrouting.Requirement{}}}
	legacy := fusedobject.Webhook{Name: "legacy", Method: "POST"}
	// Anonymous alternatives need no definitions, and nil legacy contracts need no standard namespace.
	if err := validateTransportContract(&fusedobject.ServiceMetadata{}, nil, []fusedobject.Webhook{webhook, legacy}); err != nil {
		t.Fatalf("anonymous and legacy contracts: %v", err)
	}
	webhook.Contract.Kind = "callback"
	webhook.Contract.RuntimeExpression = "{$request.body#/callbackUrl}"
	webhook.Contract.Parent = &fusedobject.CallbackParent{OperationID: "create", Method: "POST", Path: "/items", CallbackName: "notify"}
	// Callback operations inherit the same explicit anonymous security semantics.
	if err := validateTransportContract(&fusedobject.ServiceMetadata{}, nil, []fusedobject.Webhook{webhook}); err != nil {
		t.Fatalf("anonymous callback contract: %v", err)
	}
	webhook.Contract.SecurityRequirements = nil
	// A present contract with absent canonical requirements must not become silently anonymous.
	if err := validateTransportContract(&fusedobject.ServiceMetadata{}, nil, []fusedobject.Webhook{webhook}); err == nil {
		t.Fatal("missing canonical requirements accepted")
	}
}

// TestInboundSecurityRejectsInvalidDefinitions checks malformed and unresolved definitions before admission.
func TestInboundSecurityRejectsInvalidDefinitions(t *testing.T) {
	cases := map[string]fusedobject.InboundSecurityScheme{
		"unresolved reference": {},
		"unknown type":         {Type: "magic"},
		"invalid API location": {Type: "apiKey", In: "body", Name: "key"},
		"missing API name":     {Type: "apiKey", In: "header"},
		"missing HTTP scheme":  {Type: "http"},
		"flow on HTTP":         {Type: "http", Scheme: "basic", Flows: map[string]fusedobject.InboundOAuthFlow{"password": {Scopes: map[string]string{}}}},
		"unknown OAuth flow":   {Type: "oauth2", Flows: map[string]fusedobject.InboundOAuthFlow{"magic": {TokenURL: "https://provider.test/token", Scopes: map[string]string{}}}},
		"missing flow scopes":  {Type: "oauth2", Flows: map[string]fusedobject.InboundOAuthFlow{"clientCredentials": {TokenURL: "https://provider.test/token"}}},
		"missing token URL":    {Type: "oauth2", Flows: map[string]fusedobject.InboundOAuthFlow{"clientCredentials": {Scopes: map[string]string{}}}},
		"bad metadata URL":     {Type: "oauth2", OAuth2MetadataURL: "file:///metadata"},
	}
	for name, scheme := range cases {
		// Every malformed shape must fail even when its map key satisfies the requirement name.
		t.Run(name, func(t *testing.T) {
			webhook := inboundSecurityFixture("signature", scheme)
			// Type and field validation cannot be replaced by a mere map-membership check.
			if err := validateTransportContract(&fusedobject.ServiceMetadata{}, nil, []fusedobject.Webhook{webhook}); err == nil {
				t.Fatal("malformed inbound definition accepted")
			}
		})
	}
	for _, name := range []string{"", " signature ", "bad\nname", strings.Repeat("x", 129)} {
		webhook := inboundSecurityFixture(name, fusedobject.InboundSecurityScheme{Type: "http", Scheme: "basic"})
		// Definition identity must remain exact rather than normalized into an alias.
		if err := validateTransportContract(&fusedobject.ServiceMetadata{}, nil, []fusedobject.Webhook{webhook}); err == nil {
			t.Fatalf("invalid inbound name accepted: %q", name)
		}
	}
}

// TestInboundSecurityScopeValidation uses local scheme types and declarations rather than outbound credentials.
func TestInboundSecurityScopeValidation(t *testing.T) {
	flow := fusedobject.InboundOAuthFlow{TokenURL: "https://provider.test/token", Scopes: map[string]string{"events:read": "Read events"}}
	webhook := inboundSecurityFixture("shared", fusedobject.InboundSecurityScheme{Type: "oauth2", Flows: map[string]fusedobject.InboundOAuthFlow{"clientCredentials": flow}})
	webhook.Contract.SecurityRequirements[0].Schemes[0].Scopes = []string{"events:read"}
	metadata := &fusedobject.ServiceMetadata{AuthConfigs: fusedobject.AuthConfigs{{Name: "shared", Type: "http", Scheme: "basic"}}}
	// Inbound OAuth scopes must not be checked against the unrelated outbound Basic type.
	if err := validateTransportContract(metadata, nil, []fusedobject.Webhook{webhook}); err != nil {
		t.Fatalf("local OAuth declaration: %v", err)
	}
	for _, scopes := range [][]string{{"undeclared"}, {"events:read", "events:read"}, {""}} {
		webhook.Contract.SecurityRequirements[0].Schemes[0].Scopes = scopes
		// Unknown, duplicate, and empty scopes each violate the admitted canonical contract.
		if err := validateTransportContract(metadata, nil, []fusedobject.Webhook{webhook}); err == nil {
			t.Fatalf("invalid scope set accepted: %#v", scopes)
		}
	}
	webhook.Contract.SecurityRequirements[0].Schemes[0].Scopes = []string{"events:read"}
	for _, scheme := range []string{"bearer", "oauth", "oidc"} {
		webhook.Contract.SecuritySchemes["shared"] = fusedobject.InboundSecurityScheme{Type: "http", Scheme: scheme}
		// Neither bearer formatting nor an OAuth-like HTTP scheme name can authorize scopes.
		if err := validateTransportContract(metadata, nil, []fusedobject.Webhook{webhook}); err == nil {
			t.Fatalf("scoped HTTP inbound scheme accepted: %q", scheme)
		}
	}
	webhook.Contract.SecuritySchemes["shared"] = fusedobject.InboundSecurityScheme{Type: "openIdConnect", OpenIDConnectURL: "https://provider.test/.well-known/openid-configuration"}
	// OIDC scopes may be externally discovered and do not require an invented local flow map.
	if err := validateTransportContract(metadata, nil, []fusedobject.Webhook{webhook}); err != nil {
		t.Fatalf("documentary OIDC scopes: %v", err)
	}
}

// TestInboundSecurityAlternativesPreserveLocalNames keeps OR alternatives and AND-group uniqueness distinct.
func TestInboundSecurityAlternativesPreserveLocalNames(t *testing.T) {
	webhook := inboundSecurityFixture("signature", fusedobject.InboundSecurityScheme{Type: "apiKey", In: "header", Name: "X-Signature"})
	webhook.Contract.SecurityRequirements = append(webhook.Contract.SecurityRequirements, authrouting.Alternative{Schemes: []authrouting.Requirement{}})
	// A documented anonymous alternative remains valid without weakening the signed alternative's references.
	if err := validateTransportContract(&fusedobject.ServiceMetadata{}, nil, []fusedobject.Webhook{webhook}); err != nil {
		t.Fatalf("signed-or-anonymous alternatives: %v", err)
	}
	webhook.Contract.SecurityRequirements[0].Schemes = append(webhook.Contract.SecurityRequirements[0].Schemes, authrouting.Requirement{Scheme: "signature"})
	// Duplicate names inside one AND group remain malformed even if another alternative is anonymous.
	if err := validateTransportContract(&fusedobject.ServiceMetadata{}, nil, []fusedobject.Webhook{webhook}); err == nil || !strings.Contains(err.Error(), "duplicates a scheme") {
		t.Fatalf("duplicate inbound requirement error = %v", err)
	}
}

// TestInboundSecurityDocumentaryOAuthParity avoids imposing outbound execution limits on retained source declarations.
func TestInboundSecurityDocumentaryOAuthParity(t *testing.T) {
	scopes := make(map[string]string, 300)
	for index := 0; index < 300; index++ {
		scopes["scope:"+strconv.Itoa(index)] = "Declared source scope"
	}
	webhook := inboundSecurityFixture("oauth", fusedobject.InboundSecurityScheme{Type: "oauth2",
		OAuth2MetadataURL: " https://provider.test/metadata ",
		Flows: map[string]fusedobject.InboundOAuthFlow{"deviceAuthorization": {
			DeviceAuthorizationURL: " https://provider.test/device ", TokenURL: "https://provider.test/token", Scopes: scopes,
		}},
	})
	webhook.Contract.SecurityRequirements[0].Schemes[0].Scopes = []string{"scope:1"}
	// Only referenced scopes use runtime requirement limits; the passive catalogue is not an executable OAuth flow set.
	if err := validateTransportContract(&fusedobject.ServiceMetadata{}, nil, []fusedobject.Webhook{webhook}); err != nil {
		t.Fatalf("Registry-compatible documentary OAuth: %v", err)
	}
}

// TestInboundSecurityJSONAndDispatchRoundTrip preserves exact wire metadata across both Engine models.
func TestInboundSecurityJSONAndDispatchRoundTrip(t *testing.T) {
	deprecated := true
	scheme := fusedobject.InboundSecurityScheme{Type: "oauth2", Description: "Documentary only", Deprecated: &deprecated,
		OAuth2MetadataURL: "https://provider.test/metadata", Flows: map[string]fusedobject.InboundOAuthFlow{
			"authorizationCode": {AuthorizationURL: "https://provider.test/authorize", TokenURL: "https://provider.test/token", RefreshURL: "https://provider.test/refresh", Scopes: map[string]string{"events:read": "Read events"}},
		}}
	webhook := inboundSecurityFixture("oauth", scheme)
	original, err := json.Marshal(webhook.Contract)
	// Serialization must retain the new documentary namespace in the public contract JSON.
	if err != nil || !strings.Contains(string(original), "security_schemes") {
		t.Fatalf("marshal inbound contract: %s / %v", original, err)
	}
	var stored models.InboundOperationContract
	// Persisted models must recognize every field emitted by the runtime-facing model.
	if err := json.Unmarshal(original, &stored); err != nil {
		t.Fatalf("decode stored contract: %v", err)
	}
	storedJSON, err := json.Marshal(stored)
	// JSON parity guards camelCase OpenAPI metadata inside the snake_case canonical envelope.
	if err != nil || string(storedJSON) != string(original) {
		t.Fatalf("stored JSON drift: %s / %v", storedJSON, err)
	}
	mapped := mapInboundOperationContract(webhook.Contract)
	// Dispatch mapping must not discard fields that survived JSON decoding.
	if !reflect.DeepEqual(mapped.SecuritySchemes, stored.SecuritySchemes) {
		t.Fatalf("dispatch dropped security definitions: %#v", mapped.SecuritySchemes)
	}
	mappedScheme := mapped.SecuritySchemes["oauth"]
	mappedScheme.Flows["authorizationCode"].Scopes["events:read"] = "changed"
	*mappedScheme.Deprecated = false
	// Dispatch metadata is detached so mutations cannot rewrite the fetched immutable snapshot.
	if scheme.Flows["authorizationCode"].Scopes["events:read"] != "Read events" || !*scheme.Deprecated {
		t.Fatal("dispatch security definitions alias the source contract")
	}
	// Legacy contract absence remains absent through dispatch mapping too.
	if mapInboundOperationContract(nil) != nil {
		t.Fatal("legacy nil contract acquired metadata")
	}
}
