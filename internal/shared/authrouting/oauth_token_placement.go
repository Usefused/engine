package authrouting

import (
	"errors"
	"net/http"
	"regexp"
	"strings"
)

// OAuthTokenPlacement is credential-free provider metadata; token values are resolved only at execution.
type OAuthTokenPlacement struct {
	Location string `json:"location"`
	Name     string `json:"name"`
	Format   string `json:"format"`
}

var oauthHeaderName = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")

// Validate admits only explicit OAuth2 header delivery and excludes routing, framing, and browser-owned headers.
func (p *OAuthTokenPlacement) Validate(authType string) error {
	// Omission preserves historical Bearer behavior for existing contracts.
	if p == nil {
		return nil
	}
	// Header delivery is an OAuth2 feature; other schemes must keep their own credential strategy.
	if authType != "oauth2" {
		return errors.New("oauth_token_placement is only valid for OAuth2")
	}
	// Requiring all three fields prevents an incomplete extension from choosing a credential destination.
	if p.Location != "header" || len(p.Name) > 128 || !oauthHeaderName.MatchString(p.Name) || (p.Format != "raw" && p.Format != "bearer") {
		return errors.New("oauth_token_placement requires location header, a valid header name, and format raw or bearer")
	}
	name := strings.ToLower(p.Name)
	// Credentials cannot modify proxy routing or impersonate Engine connection selectors.
	if strings.HasPrefix(name, "proxy-") || strings.HasPrefix(name, "sec-") || strings.HasPrefix(name, "x-fused-") || strings.HasPrefix(name, "x-forwarded-") {
		return errors.New("oauth_token_placement header is reserved")
	}
	// Protocol framing and browser session fields cannot serve as token destinations.
	switch name {
	case "host", "connection", "content-length", "transfer-encoding", "trailer", "te", "upgrade", "keep-alive", "cookie", "set-cookie", "content-type", "content-encoding", "accept", "origin", "referer", "forwarded":
		return errors.New("oauth_token_placement header is reserved")
	}
	return nil
}

// Apply injects a live token without exposing it through errors; callers validate scheme ownership first.
func (p *OAuthTokenPlacement) Apply(req *http.Request, token, defaultScheme string) error {
	// Invalid metadata must never silently downgrade to ordinary Bearer auth.
	if err := p.Validate("oauth2"); err != nil {
		return err
	}
	// Empty credentials are handled by the upstream resolver and cannot manufacture auth headers.
	if token == "" {
		return nil
	}
	name, value := "Authorization", defaultScheme+" "+token
	// An explicit contract replaces the default destination; it never copies a token to both headers.
	if p != nil {
		name, value = p.Name, token
		// Only the declared bounded format can prefix a credential.
		if p.Format == "bearer" {
			value = "Bearer " + token
		}
	}
	for index := range len(value) {
		// Reject all forbidden HTTP control bytes before transport errors can quote a credential.
		if value[index] == 127 || (value[index] < 32 && value[index] != '\t') {
			return errors.New("OAuth token contains invalid header characters")
		}
	}
	req.Header.Set(name, value)
	return nil
}
