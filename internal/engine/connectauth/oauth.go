package connectauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

// ClientCredentials is the decrypted OAuth app material Engine may use with a
// provider; keeping it in this package avoids each caller inventing its own
// token endpoint shape.
type ClientCredentials struct {
	ClientID     string
	ClientSecret string
	RedirectURI  string
}

// TokenResponse is the subset of OAuth/OIDC token responses Engine persists or
// dispatches with; unknown provider fields are deliberately ignored.
type TokenResponse struct {
	AccessToken           string `json:"access_token"`
	RefreshToken          string `json:"refresh_token,omitempty"`
	IDToken               string `json:"id_token,omitempty"`
	TokenType             string `json:"token_type,omitempty"`
	Scope                 string `json:"scope,omitempty"`
	ExpiresIn             int64  `json:"expires_in,omitempty"`
	RefreshTokenExpiresIn int64  `json:"refresh_token_expires_in,omitempty"`
}

// TokenEndpointError preserves the provider's OAuth error code without
// exposing token-response bodies through Engine or generated SDK errors.
type TokenEndpointError struct {
	StatusCode  int
	Code        string
	Description string
}

// Error intentionally omits the provider description because providers may
// echo request details that are unsafe to propagate into SDK responses.
func (e *TokenEndpointError) Error() string {
	return fmt.Sprintf("token endpoint returned %d (%s)", e.StatusCode, e.Code)
}

type TokenScopeSet struct {
	Scopes []string
	Source string
}

// IsReconnectRequiredRefreshError distinguishes a permanently rejected user
// grant from transient provider failures that should remain retryable.
func IsReconnectRequiredRefreshError(err error) bool {
	var endpointErr *TokenEndpointError
	// Only OAuth invalid_grant means the stored user grant can no longer be
	// refreshed; invalid_client instead points to workspace app configuration.
	return errors.As(err, &endpointErr) && strings.EqualFold(endpointErr.Code, "invalid_grant")
}

// DecryptClientCredentials centralizes connect-config decryption so callback
// and refresh flows use the same encrypted bucket config contract.
func DecryptClientCredentials(cfg *store.ConnectConfig, masterKey []byte) (ClientCredentials, error) {
	dek, err := store.UnwrapDEK(masterKey, cfg.EncryptedDEK)
	if err != nil {
		return ClientCredentials{}, err
	}
	clientID, err := store.DecryptWithDEK(dek, cfg.EncryptedClientID)
	if err != nil {
		return ClientCredentials{}, err
	}
	clientSecret, err := store.DecryptWithDEK(dek, cfg.EncryptedClientSecret)
	if err != nil {
		return ClientCredentials{}, err
	}
	return ClientCredentials{ClientID: clientID, ClientSecret: clientSecret, RedirectURI: cfg.RedirectURI}, nil
}

// ExchangeAuthorizationCode builds the auth-code grant in one place so browser
// callback and later tests share the same provider-facing behavior.
func ExchangeAuthorizationCode(ctx context.Context, client *http.Client, auth fusedobject.AuthConfig, creds ClientCredentials, code, verifier string) (TokenResponse, error) {
	form := tokenBaseForm(auth, creds)
	form.Set("grant_type", "authorization_code")
	form.Set("code", code)
	form.Set("code_verifier", verifier)
	return doTokenForm(ctx, client, auth, creds, form)
}

// RefreshAccessToken builds the refresh-token grant without touching browser
// state; dispatch-time refresh should depend only on bucket-stored material.
func RefreshAccessToken(ctx context.Context, client *http.Client, auth fusedobject.AuthConfig, creds ClientCredentials, refreshToken string) (TokenResponse, error) {
	form := tokenBaseForm(auth, creds)
	form.Set("grant_type", "refresh_token")
	form.Set("refresh_token", refreshToken)
	return doTokenForm(ctx, client, auth, creds, form)
}

// TokenScopes prefers provider-granted scopes because consent screens may
// narrow what the registry originally requested.
func TokenScopes(token TokenResponse, fallback []string) []string {
	return TokenScopeMetadata(token, fallback, "registry").Scopes
}

// TokenScopeMetadata records the caller's fallback provenance so a selected
// consent subset is never mislabeled as the service's full Registry catalogue.
func TokenScopeMetadata(token TokenResponse, fallback []string, fallbackSource string) TokenScopeSet {
	if strings.TrimSpace(token.Scope) == "" {
		if len(fallback) == 0 {
			return TokenScopeSet{Source: "none"}
		}
		return TokenScopeSet{Scopes: fallback, Source: fallbackSource}
	}
	return TokenScopeSet{Scopes: strings.Fields(token.Scope), Source: "provider"}
}

// TokenExpiresAt treats missing expires_in as unknown rather than expired,
// matching providers that issue opaque policy-managed tokens.
func TokenExpiresAt(expiresIn int64) *time.Time {
	if expiresIn <= 0 {
		return nil
	}
	expiresAt := time.Now().UTC().Add(time.Duration(expiresIn) * time.Second)
	return &expiresAt
}

// RefreshTokenExpiresAt keeps reconnect timing provider-driven without storing
// any refresh token material outside the encrypted token column.
func RefreshTokenExpiresAt(expiresIn int64) *time.Time {
	return TokenExpiresAt(expiresIn)
}

// DefaultTokenType normalizes provider omissions to the dispatcher default.
func DefaultTokenType(value string) string {
	if strings.TrimSpace(value) == "" {
		return "Bearer"
	}
	return value
}

// OIDCClaims decodes unsigned id_token claims only for metadata/audit storage;
// OAuth security decisions stay in the caller's nonce and token-flow checks.
func OIDCClaims(idToken string) map[string]any {
	parts := strings.Split(idToken, ".")
	if len(parts) < 2 {
		return nil
	}
	decoded, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil
	}
	var claims map[string]any
	if err := json.Unmarshal(decoded, &claims); err != nil {
		return nil
	}
	return claims
}

// ClaimString avoids scattering untyped claim assertions through storage code.
func ClaimString(claims map[string]any, key string) string {
	value, _ := claims[key].(string)
	return value
}

// ClaimBytes stores the claim map compactly so admin/debug views can inspect
// provider identity context without decrypting token fields.
func ClaimBytes(claims map[string]any) []byte {
	if len(claims) == 0 {
		return nil
	}
	raw, _ := json.Marshal(claims)
	return raw
}

// tokenBaseForm respects provider auth mode because some providers reject
// client_secret in the body when HTTP Basic token auth is configured.
func tokenBaseForm(auth fusedobject.AuthConfig, creds ClientCredentials) url.Values {
	form := url.Values{}
	form.Set("redirect_uri", creds.RedirectURI)
	if strings.EqualFold(auth.TokenEndpointAuth, "basic") {
		form.Set("client_id", creds.ClientID)
	} else {
		form.Set("client_id", creds.ClientID)
		form.Set("client_secret", creds.ClientSecret)
	}
	for key, value := range auth.ExtraTokenParams {
		form.Set(key, value)
	}
	return form
}

// doTokenForm is the single provider HTTP boundary for token grants, so
// malformed responses fail before any bucket credential row is updated.
func doTokenForm(ctx context.Context, client *http.Client, auth fusedobject.AuthConfig, creds ClientCredentials, form url.Values) (TokenResponse, error) {
	req, err := newTokenRequest(ctx, auth, creds, form)
	if err != nil {
		return TokenResponse{}, err
	}
	return doTokenRequest(client, req)
}

// newTokenRequest keeps Basic auth construction next to request creation so
// secrets are attached in exactly one provider-facing path.
func newTokenRequest(ctx context.Context, auth fusedobject.AuthConfig, creds ClientCredentials, form url.Values) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, auth.TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// GitHub's OAuth token endpoint otherwise defaults to form-encoded output;
	// asking for JSON keeps provider responses aligned with TokenResponse while
	// the parser below still tolerates older OAuth-style form bodies.
	req.Header.Set("Accept", "application/json")
	if strings.EqualFold(auth.TokenEndpointAuth, "basic") {
		req.SetBasicAuth(creds.ClientID, creds.ClientSecret)
	}
	return req, nil
}

// doTokenRequest fails closed on malformed token responses so the bucket never
// stores an incomplete connection that would fail later during execution.
func doTokenRequest(client *http.Client, req *http.Request) (TokenResponse, error) {
	resp, err := client.Do(req)
	if err != nil {
		return TokenResponse{}, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return TokenResponse{}, err
	}
	// Non-success bodies still carry the standards-defined OAuth error code
	// needed to decide whether reconnecting the user can repair the grant.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return TokenResponse{}, decodeTokenEndpointError(resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	token, err := decodeTokenResponse(resp.Header.Get("Content-Type"), body)
	if err != nil {
		return TokenResponse{}, err
	}
	if strings.TrimSpace(token.AccessToken) == "" {
		return TokenResponse{}, errors.New("token endpoint omitted access_token")
	}
	return token, nil
}

// decodeTokenEndpointError accepts both OAuth response encodings so provider
// formatting differences cannot hide a permanent invalid_grant decision.
func decodeTokenEndpointError(statusCode int, contentType string, body []byte) error {
	code, description := decodeJSONTokenEndpointError(body)
	// Form parsing is the compatibility path for providers that use the older
	// application/x-www-form-urlencoded OAuth response convention.
	if code == "" || strings.Contains(strings.ToLower(contentType), "application/x-www-form-urlencoded") {
		formCode, formDescription := decodeFormTokenEndpointError(body)
		// A parsed form code is authoritative only when present; otherwise a
		// valid JSON result must survive a misleading provider content type.
		if formCode != "" {
			code, description = formCode, formDescription
		}
	}
	// A stable fallback keeps ordinary non-OAuth failures typed without ever
	// treating an unrecognised provider response as a reconnect instruction.
	if code == "" {
		code = "token_endpoint_error"
	}
	return &TokenEndpointError{StatusCode: statusCode, Code: code, Description: description}
}

// decodeJSONTokenEndpointError extracts only the standard safe fields rather
// than retaining the provider's complete response body in memory or errors.
func decodeJSONTokenEndpointError(body []byte) (string, string) {
	var payload struct {
		Code        string `json:"error"`
		Description string `json:"error_description"`
	}
	// Malformed JSON is expected for form responses and falls through cleanly.
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", ""
	}
	return strings.TrimSpace(payload.Code), strings.TrimSpace(payload.Description)
}

// decodeFormTokenEndpointError mirrors the token success decoder while
// limiting the retained result to OAuth's error and error_description fields.
func decodeFormTokenEndpointError(body []byte) (string, string) {
	values, err := url.ParseQuery(string(body))
	// Invalid form data must remain an unclassified provider failure because a
	// reconnect decision requires an explicit OAuth error code.
	if err != nil {
		return "", ""
	}
	return strings.TrimSpace(values.Get("error")), strings.TrimSpace(values.Get("error_description"))
}

// decodeTokenResponse prefers JSON but accepts OAuth form bodies because a
// provider's response format should not decide whether valid credentials land.
func decodeTokenResponse(contentType string, body []byte) (TokenResponse, error) {
	if strings.Contains(strings.ToLower(contentType), "application/x-www-form-urlencoded") {
		return decodeFormTokenResponse(body)
	}
	var token TokenResponse
	if err := json.Unmarshal(body, &token); err == nil {
		return token, nil
	}
	// Some OAuth providers omit Content-Type on token responses; retrying as a
	// form body preserves interoperability without accepting malformed tokens.
	return decodeFormTokenResponse(body)
}

// decodeFormTokenResponse maps classic application/x-www-form-urlencoded token
// bodies into the same internal shape as JSON responses.
func decodeFormTokenResponse(body []byte) (TokenResponse, error) {
	values, err := url.ParseQuery(string(body))
	if err != nil {
		return TokenResponse{}, err
	}
	return TokenResponse{
		AccessToken:           values.Get("access_token"),
		RefreshToken:          values.Get("refresh_token"),
		IDToken:               values.Get("id_token"),
		TokenType:             values.Get("token_type"),
		Scope:                 values.Get("scope"),
		ExpiresIn:             parseExpiresIn(values.Get("expires_in")),
		RefreshTokenExpiresIn: parseExpiresIn(values.Get("refresh_token_expires_in")),
	}, nil
}

// parseExpiresIn treats malformed provider TTLs as unknown expiry rather than
// failing an otherwise valid token exchange.
func parseExpiresIn(value string) int64 {
	expiresIn, _ := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	return expiresIn
}
