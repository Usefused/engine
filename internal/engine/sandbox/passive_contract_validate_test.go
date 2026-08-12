package sandbox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

func TestValidatePassiveContractsAcceptsFullCallbackAndLegacyWebhook(t *testing.T) {
	callback := fusedobject.Webhook{Name: "status", Method: "POST", Contract: &fusedobject.InboundOperationContract{
		Kind: "callback", RuntimeExpression: "{$request.body#/callbackUrl}", Path: "{$request.body#/callbackUrl}",
		Parent: &fusedobject.CallbackParent{OperationID: "createJob", Method: "POST", Path: "/jobs", CallbackName: "status"},
		Tags:   []string{}, Parameters: fusedobject.Parameters{}, Responses: fusedobject.Responses{},
		SecurityRequirements: authrouting.Requirements{{Schemes: []authrouting.Requirement{}}},
		Extensions:           fusedobject.NamespacedExtensions{"x-provider-doc": {Value: json.RawMessage(`{"note":"inert"}`), Provenance: "source_spec"}},
	}}
	metadata := &fusedobject.ServiceMetadata{Documentation: &fusedobject.ServiceDocumentation{Tags: []fusedobject.TagDocumentation{}}}
	if err := validateTransportContract(metadata, nil, []fusedobject.Webhook{callback, {Name: "uploaded", Method: "POST"}}); err != nil {
		t.Fatalf("validate callback and independent upload: %v", err)
	}
}

func TestValidatePassiveContractsRejectsExecutionExtensionAuthority(t *testing.T) {
	metadata := &fusedobject.ServiceMetadata{Documentation: &fusedobject.ServiceDocumentation{
		Tags:       []fusedobject.TagDocumentation{},
		Extensions: fusedobject.NamespacedExtensions{"x-fused-future-execution": {Value: json.RawMessage(`true`), Provenance: "source_spec"}},
	}}
	err := validateTransportContract(metadata, nil, nil)
	if err == nil || !strings.Contains(err.Error(), fusedobject.ExecutionCapabilityRequiredCode) {
		t.Fatalf("execution extension error = %v", err)
	}
}

func TestValidateLinkExtensionsRemainDocumentationOnly(t *testing.T) {
	links := map[string]fusedobject.LinkContract{"next": {
		OperationID: "getJob", Parameters: map[string]any{"id": "$response.body#/id"},
		Extensions: fusedobject.NamespacedExtensions{"x-provider-note": {Value: json.RawMessage(`"read only"`), Provenance: "source_spec"}},
	}}
	if err := validateLinkContracts(links); err != nil {
		t.Fatalf("documentation link rejected: %v", err)
	}
	links["next"] = fusedobject.LinkContract{OperationID: "getJob", Extensions: fusedobject.NamespacedExtensions{
		"x-fused-follow-link": {Value: json.RawMessage(`true`), Provenance: "source_spec"},
	}}
	if err := validateLinkContracts(links); err == nil {
		t.Fatal("execution-like link extension must fail closed")
	}
}

// TestRuntimeContractQueryRequestsInboundDocumentationFields keeps the passive validator aligned with the canonical GraphQL projection.
func TestRuntimeContractQueryRequestsInboundDocumentationFields(t *testing.T) {
	for _, field := range []string{"contract", "documentation", "request_content", "responses"} {
		if !strings.Contains(runtimeContractsQuery, field) {
			t.Fatalf("runtime contract query missing %q", field)
		}
	}
}

func TestPassiveContractCountsSeparateCallbacksAndWebhooks(t *testing.T) {
	snapshots := []store.ServiceContractSnapshot{{
		Endpoints: []fusedobject.Endpoint{{Responses: fusedobject.Responses{"200": {Links: map[string]fusedobject.LinkContract{"next": {OperationID: "next"}}}}}},
		Webhooks:  []fusedobject.Webhook{{Contract: &fusedobject.InboundOperationContract{Kind: "callback"}}, {Contract: &fusedobject.InboundOperationContract{Kind: "webhook"}}, {}},
	}}
	callbacks, webhooks, links := passiveContractCounts(snapshots)
	if callbacks != 1 || webhooks != 2 || links != 1 {
		t.Fatalf("counts callbacks=%d webhooks=%d links=%d", callbacks, webhooks, links)
	}
}
