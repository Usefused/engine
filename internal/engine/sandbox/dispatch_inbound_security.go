package sandbox

import (
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

// mapInboundSecuritySchemes detaches documentary definitions from the immutable
// fetched contract before they enter Engine's persisted dispatch model.
func mapInboundSecuritySchemes(source map[string]fusedobject.InboundSecurityScheme) map[string]models.InboundSecurityScheme {
	// Preserve absence for legacy and explicitly anonymous contracts.
	if source == nil {
		return nil
	}
	result := make(map[string]models.InboundSecurityScheme, len(source))
	for name, scheme := range source {
		mapped := models.InboundSecurityScheme{
			Type: scheme.Type, Description: scheme.Description, Name: scheme.Name, In: scheme.In,
			Scheme: scheme.Scheme, BearerFormat: scheme.BearerFormat, OpenIDConnectURL: scheme.OpenIDConnectURL,
			OAuth2MetadataURL: scheme.OAuth2MetadataURL,
		}
		// A mutable boolean pointer must not alias the fetched contract.
		if scheme.Deprecated != nil {
			deprecated := *scheme.Deprecated
			mapped.Deprecated = &deprecated
		}
		// Omitted flow maps remain omitted instead of acquiring invented source metadata.
		if scheme.Flows != nil {
			mapped.Flows = make(map[string]models.InboundOAuthFlow, len(scheme.Flows))
		}
		for flowName, flow := range scheme.Flows {
			mapped.Flows[flowName] = models.InboundOAuthFlow{
				AuthorizationURL: flow.AuthorizationURL, DeviceAuthorizationURL: flow.DeviceAuthorizationURL,
				TokenURL: flow.TokenURL, RefreshURL: flow.RefreshURL, Scopes: cloneInboundScopes(flow.Scopes),
			}
		}
		result[name] = mapped
	}
	return result
}

// cloneInboundScopes keeps dispatch metadata mutations isolated from the source snapshot.
func cloneInboundScopes(source map[string]string) map[string]string {
	// Nil is distinct from an explicitly present empty scope catalogue.
	if source == nil {
		return nil
	}
	result := make(map[string]string, len(source))
	for name, description := range source {
		result[name] = description
	}
	return result
}
