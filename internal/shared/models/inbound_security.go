package models

// InboundSecurityScheme persists documentary OpenAPI definitions without
// granting outbound credentials or executable webhook verification authority.
type InboundSecurityScheme struct {
	Type              string                      `json:"type"`
	Description       string                      `json:"description,omitempty"`
	Name              string                      `json:"name,omitempty"`
	In                string                      `json:"in,omitempty"`
	Scheme            string                      `json:"scheme,omitempty"`
	BearerFormat      string                      `json:"bearerFormat,omitempty"`
	Flows             map[string]InboundOAuthFlow `json:"flows,omitempty"`
	OpenIDConnectURL  string                      `json:"openIdConnectUrl,omitempty"`
	OAuth2MetadataURL string                      `json:"oauth2MetadataUrl,omitempty"`
	Deprecated        *bool                       `json:"deprecated,omitempty"`
}

// InboundOAuthFlow preserves source scope declarations without token behavior.
type InboundOAuthFlow struct {
	AuthorizationURL       string            `json:"authorizationUrl,omitempty"`
	DeviceAuthorizationURL string            `json:"deviceAuthorizationUrl,omitempty"`
	TokenURL               string            `json:"tokenUrl,omitempty"`
	RefreshURL             string            `json:"refreshUrl,omitempty"`
	Scopes                 map[string]string `json:"scopes"`
}
