package sandbox

import (
	"encoding/json"
	"errors"
	"net/mail"
	"net/url"
	"strings"

	"github.com/Usefused/engine/internal/shared/fusedobject"
)

const maxDocumentationText = 64 << 10

// validatePassiveContracts bounds preserved documentation and inbound metadata
// even though those fields do not grant outbound execution capabilities.
func validatePassiveContracts(metadata *fusedobject.ServiceMetadata, endpoints []fusedobject.Endpoint, webhooks []fusedobject.Webhook) error {
	// Documentation remains bounded even though it cannot authorize execution.
	if err := validateServiceDocumentation(metadata.Documentation); err != nil {
		return err
	}
	for _, endpoint := range endpoints {
		// Endpoint documentation admission is independent of credential routing.
		if err := validateOperationDocumentation(endpoint.Documentation); err != nil {
			return err
		}
	}
	return validateInboundContracts(webhooks)
}

// validateInboundContracts preserves legacy uploads while requiring self-contained standard contracts.
func validateInboundContracts(webhooks []fusedobject.Webhook) error {
	for _, webhook := range webhooks {
		// Independently uploaded legacy webhooks have no standard security namespace to resolve.
		if webhook.Contract == nil {
			continue
		}
		// A present contract cannot borrow missing definitions from outbound credentials.
		if err := validateInboundContract(webhook.Method, *webhook.Contract); err != nil {
			return err
		}
	}
	return nil
}

// validateInboundContract checks documentary security without creating executable verification policy.
func validateInboundContract(method string, contract fusedobject.InboundOperationContract) error {
	// Identity must be valid before its associated transport and security are trusted.
	if err := validateInboundIdentity(contract); err != nil {
		return err
	}
	endpoint := fusedobject.Endpoint{
		Method: method, Path: contract.Path, OperationServers: contract.OperationServers,
		Parameters: contract.Parameters, RequestContent: contract.RequestContent, Responses: contract.Responses,
	}
	// Shared transport checks retain the same inbound shape guarantees as before.
	if err := validateInboundOperationTransport(endpoint); err != nil {
		return err
	}
	// Only contract-local definitions may satisfy inbound security names.
	if err := validateInboundSecurity(contract); err != nil {
		return err
	}
	// Documentary server selections must still reference an effective operation server.
	if err := validateSecurityServerSelections(contract.SecurityRequirements, contract.OperationServers); err != nil {
		return err
	}
	// External documentation never bypasses passive metadata validation.
	if err := validateExternalDocumentation(contract.ExternalDocs); err != nil {
		return err
	}
	return validateNamespacedExtensions(contract.Extensions)
}

func validateInboundIdentity(contract fusedobject.InboundOperationContract) error {
	if contract.Kind != fusedobject.InboundOperationKindWebhook && contract.Kind != fusedobject.InboundOperationKindCallback {
		return errors.New("runtime inbound contract kind is invalid")
	}
	if strings.TrimSpace(contract.Path) == "" || len(contract.Path) > 2048 {
		return errors.New("runtime inbound contract path is invalid")
	}
	if contract.Kind == fusedobject.InboundOperationKindWebhook {
		return validateWebhookIdentity(contract)
	}
	if contract.Parent == nil || strings.TrimSpace(contract.RuntimeExpression) == "" || len(contract.RuntimeExpression) > 2048 {
		return errors.New("runtime callback contract identity is invalid")
	}
	return validateCallbackParent(*contract.Parent)
}

func validateWebhookIdentity(contract fusedobject.InboundOperationContract) error {
	if contract.Parent != nil || contract.RuntimeExpression != "" {
		return errors.New("runtime webhook contract callback identity is invalid")
	}
	return nil
}

func validateCallbackParent(parent fusedobject.CallbackParent) error {
	if strings.TrimSpace(parent.OperationID) == "" || strings.TrimSpace(parent.Method) == "" || strings.TrimSpace(parent.Path) == "" || strings.TrimSpace(parent.CallbackName) == "" {
		return errors.New("runtime callback parent is invalid")
	}
	return nil
}

func validateOperationDocumentation(documentation *fusedobject.OperationDocumentation) error {
	if documentation == nil {
		return nil
	}
	if !validDocumentationText(documentation.Summary) || !validDocumentationText(documentation.Description) || !validDocumentationTags(documentation.Tags) {
		return errors.New("runtime operation documentation is invalid")
	}
	if err := validateExternalDocumentation(documentation.ExternalDocs); err != nil {
		return err
	}
	return validateNamespacedExtensions(documentation.Extensions)
}

func validateServiceDocumentation(documentation *fusedobject.ServiceDocumentation) error {
	if documentation == nil {
		return nil
	}
	if !validDocumentationText(documentation.TermsOfService) || len(documentation.Tags) > 256 {
		return errors.New("runtime service documentation is invalid")
	}
	if err := validateContactDocumentation(documentation.Contact); err != nil {
		return err
	}
	if err := validateLicenseDocumentation(documentation.License); err != nil {
		return err
	}
	if err := validateDocumentationTagSet(documentation.Tags); err != nil {
		return err
	}
	if err := validateExternalDocumentation(documentation.ExternalDocs); err != nil {
		return err
	}
	return validateNamespacedExtensions(documentation.Extensions)
}

func validateDocumentationTagSet(tags []fusedobject.TagDocumentation) error {
	for _, tag := range tags {
		if strings.TrimSpace(tag.Name) == "" || !validDocumentationText(tag.Description) {
			return errors.New("runtime service documentation tag is invalid")
		}
		if err := validateExternalDocumentation(tag.ExternalDocs); err != nil {
			return errors.New("runtime service documentation tag is invalid")
		}
	}
	return nil
}

func validateContactDocumentation(contact *fusedobject.ContactDocumentation) error {
	if contact == nil {
		return nil
	}
	if !validDocumentationText(contact.Name) || !validDocumentationURL(contact.URL, true) {
		return errors.New("runtime service contact documentation is invalid")
	}
	if contact.Email != "" {
		if _, err := mail.ParseAddress(contact.Email); err != nil {
			return errors.New("runtime service contact email is invalid")
		}
	}
	return nil
}

func validateLicenseDocumentation(license *fusedobject.LicenseDocumentation) error {
	if license == nil {
		return nil
	}
	if strings.TrimSpace(license.Name) == "" || !validDocumentationText(license.Name) || !validDocumentationText(license.Identifier) || !validDocumentationURL(license.URL, true) {
		return errors.New("runtime service license documentation is invalid")
	}
	return nil
}

func validateExternalDocumentation(documentation *fusedobject.ExternalDocumentation) error {
	if documentation == nil {
		return nil
	}
	if !validDocumentationText(documentation.Description) || !validDocumentationURL(documentation.URL, false) {
		return errors.New("runtime external documentation is invalid")
	}
	return nil
}

func validDocumentationURL(raw string, optional bool) bool {
	if raw == "" {
		return optional
	}
	parsed, err := url.Parse(raw)
	return err == nil && parsed.IsAbs() && parsed.User == nil && (parsed.Scheme == "https" || parsed.Scheme == "http") && len(raw) <= 2048
}

func validDocumentationText(value string) bool {
	return len(value) <= maxDocumentationText && !strings.ContainsRune(value, '\x00')
}

func validDocumentationTags(tags []string) bool {
	if len(tags) > 256 {
		return false
	}
	for _, tag := range tags {
		if strings.TrimSpace(tag) == "" || len(tag) > 256 {
			return false
		}
	}
	return true
}

func validateNamespacedExtensions(extensions fusedobject.NamespacedExtensions) error {
	if len(extensions) > 256 {
		return errors.New("runtime documentation extensions are too large")
	}
	for name, extension := range extensions {
		canonical := strings.ToLower(strings.TrimSpace(name))
		if !strings.HasPrefix(canonical, "x-") || len(name) > 256 || extension.Provenance != "source_spec" || !json.Valid(extension.Value) || len(extension.Value) > 1<<20 {
			return errors.New("runtime documentation extension is invalid")
		}
		if strings.HasPrefix(canonical, "x-fused-") {
			// Reviewed execution extensions are projected into typed fields; seeing
			// one here means Engine cannot safely determine its authority.
			return unsupportedExecutionCapability()
		}
	}
	return nil
}
