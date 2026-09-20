package managedauthbroker

import (
	"errors"
	"net/url"

	"github.com/Usefused/engine/internal/shared/fusedobject"
)

var ErrPublishedContractUnavailable = errors.New("published OAuth contract unavailable or invalid")

// publishedAuthContract selects one exact authorization-code contract without allowing consumer-selected flows.
func publishedAuthContract(auths fusedobject.AuthConfigs, name, flowName string) (fusedobject.AuthConfig, fusedobject.OAuth2FlowContract, error) {
	// Other grant families need explicit lifecycle support before they can be published.
	if flowName != "authorizationCode" {
		return fusedobject.AuthConfig{}, fusedobject.OAuth2FlowContract{}, ErrPublishedContractUnavailable
	}
	var selected *fusedobject.AuthConfig
	for i := range auths {
		// Names identify one exact scheme; duplicates cannot be resolved by iteration order.
		if auths[i].Name == name {
			// Duplicate scheme names are ambiguous even if one would otherwise validate.
			if selected != nil {
				return fusedobject.AuthConfig{}, fusedobject.OAuth2FlowContract{}, ErrPublishedContractUnavailable
			}
			selected = &auths[i]
		}
	}
	// Static API keys and OIDC schemes cannot be reinterpreted as published OAuth apps.
	if selected == nil || selected.Type != "oauth2" {
		return fusedobject.AuthConfig{}, fusedobject.OAuth2FlowContract{}, ErrPublishedContractUnavailable
	}
	flow, ok := selected.OAuth2Flows[flowName]
	// An absent selected flow is unavailable rather than a request to use another grant family.
	if !ok {
		return fusedobject.AuthConfig{}, fusedobject.OAuth2FlowContract{}, ErrPublishedContractUnavailable
	}
	return *selected, flow, validatePublishedContract(*selected, flow)
}

// validatePublishedContract confines credential transmission while using the canonical Engine contract types.
func validatePublishedContract(auth fusedobject.AuthConfig, flow fusedobject.OAuth2FlowContract) error {
	// The endpoint must be explicit and encrypted; the HTTP client separately forbids redirects.
	if !validTokenURL(flow.TokenURL) {
		return ErrPublishedContractUnavailable
	}
	// Only supported provider-secret authentication methods may cross this remote trust boundary.
	if auth.TokenEndpointAuthMethod != fusedobject.TokenEndpointAuthMethodClientSecretBasic && auth.TokenEndpointAuthMethod != fusedobject.TokenEndpointAuthMethodClientSecretPost {
		return ErrPublishedContractUnavailable
	}
	// Preserve the core form default and explicit JSON encoding without accepting arbitrary encodings.
	if auth.TokenRequestMediaType != "" && auth.TokenRequestMediaType != fusedobject.TokenRequestMediaTypeForm && auth.TokenRequestMediaType != fusedobject.TokenRequestMediaTypeJSON {
		return ErrPublishedContractUnavailable
	}
	return nil
}

// validTokenURL confines credential transport to one unambiguous encrypted destination.
func validTokenURL(raw string) bool {
	u, err := url.Parse(raw)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.Fragment == ""
}
