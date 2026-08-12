package engine

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/models"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var ErrAuthRouting = errors.New("provider authentication routing failed")

type AuthRoutingError struct{ Code string }

func (e *AuthRoutingError) Error() string { return "provider authentication routing failed: " + e.Code }
func (e *AuthRoutingError) Unwrap() error { return ErrAuthRouting }

func authRoutingError(code string) error { return &AuthRoutingError{Code: code} }

func selectRequestAuth(auths models.AuthConfigs, requirements authrouting.Requirements, credentials map[string]any) (models.AuthConfigs, error) {
	if requirements == nil {
		return nil, authRoutingError("invalid_contract")
	}
	if len(requirements) == 0 {
		return nil, authRoutingError("invalid_contract")
	}
	definitions, err := authDefinitions(auths)
	if err != nil {
		return nil, err
	}
	selectorType := selectedCredentialAuthType(credentials)
	selectorName := selectedCredentialAuthName(credentials)
	for _, alternative := range requirements {
		selected, satisfiable, err := satisfiableAlternative(alternative, definitions, credentials, selectorType, selectorName)
		if err != nil {
			return nil, err
		}
		if satisfiable {
			return selected, nil
		}
	}
	return nil, authRoutingError("unsatisfied")
}

func authDefinitions(auths models.AuthConfigs) (map[string]models.AuthConfig, error) {
	definitions := make(map[string]models.AuthConfig, len(auths))
	for _, auth := range auths {
		name := strings.TrimSpace(auth.Name)
		if name == "" {
			return nil, authRoutingError("invalid_contract")
		}
		if _, exists := definitions[name]; exists {
			return nil, authRoutingError("invalid_contract")
		}
		definitions[name] = auth
	}
	return definitions, nil
}

func satisfiableAlternative(alternative authrouting.Alternative, definitions map[string]models.AuthConfig, credentials map[string]any, selectorType, selectorName string) (models.AuthConfigs, bool, error) {
	if len(alternative.Schemes) == 0 {
		return models.AuthConfigs{}, selectorType == "" && selectorName == "", nil
	}
	selected := make(models.AuthConfigs, 0, len(alternative.Schemes))
	typeMatched := selectorType == ""
	nameMatched := selectorName == ""
	seen := make(map[string]struct{}, len(alternative.Schemes))
	for _, requirement := range alternative.Schemes {
		auth, err := requiredAuth(requirement.Scheme, definitions, seen, credentials)
		if err != nil {
			return nil, false, err
		}
		typeMatched = typeMatched || authrouting.CanonicalType(auth.Type, auth.Scheme) == selectorType
		nameMatched = nameMatched || auth.Name == selectorName
		if !authSatisfied(auth, credentials) {
			return nil, false, nil
		}
		selected = append(selected, auth)
	}
	return selected, typeMatched && nameMatched, nil
}

func requiredAuth(name string, definitions map[string]models.AuthConfig, seen map[string]struct{}, credentials map[string]any) (models.AuthConfig, error) {
	auth, ok := definitions[name]
	if !ok {
		return models.AuthConfig{}, authRoutingError("invalid_contract")
	}
	if _, exists := seen[name]; exists {
		return models.AuthConfig{}, authRoutingError("invalid_contract")
	}
	seen[name] = struct{}{}
	return selectOAuth2Flow(auth, credentials)
}

func authSatisfied(auth models.AuthConfig, credentials map[string]any) bool {
	switch authrouting.CanonicalType(auth.Type, auth.Scheme) {
	case "basic":
		return basicAuthSatisfied(auth, credentials)
	case "mtls":
		return credentialPresent(credentials, auth.Name+"_cert") && credentialPresent(credentials, auth.Name+"_key")
	case "oauth1":
		return credentialPresent(credentials, auth.Name+"_consumer_key") && credentialPresent(credentials, auth.Name+"_consumer_secret")
	case "digest":
		return credentialPresent(credentials, auth.Name+"_username") && credentialPresent(credentials, auth.Name+"_password")
	default:
		return credentialPresent(credentials, auth.Name)
	}
}

func selectOAuth2Flow(auth models.AuthConfig, credentials map[string]any) (models.AuthConfig, error) {
	canonical := authrouting.CanonicalType(auth.Type, auth.Scheme)
	if (canonical != "oauth" && canonical != "oidc") || len(auth.OAuth2Flows) == 0 {
		return auth, nil
	}
	selected := strings.TrimSpace(credentialValue(credentials, "fused_oauth2_flow"))
	if selected == "" && len(auth.OAuth2Flows) == 1 {
		for name := range auth.OAuth2Flows {
			selected = name
		}
	}
	flow, ok := auth.OAuth2Flows[selected]
	if selected == "" {
		return auth, authRoutingError("oauth2_flow_required")
	}
	if !ok {
		return auth, authRoutingError("oauth2_flow_invalid")
	}
	auth.SelectedOAuth2Flow = &flow
	return auth, nil
}

func basicAuthSatisfied(auth models.AuthConfig, credentials map[string]any) bool {
	username := credentialValue(credentials, auth.Name+"_username")
	password, passwordSet := credentials[auth.Name+"_password"].(string)
	if username == "" {
		return false
	}
	switch auth.BasicPasswordMode {
	case authrouting.BasicPasswordRequired:
		return passwordSet && password != ""
	case authrouting.BasicPasswordOptional:
		return true
	case authrouting.BasicPasswordEmpty:
		return !passwordSet || password == ""
	default:
		return false
	}
}

func applySelectedAuth(req *http.Request, auths models.AuthConfigs, credentials map[string]any) {
	_ = applySelectedAuthChecked(req, auths, credentials)
}

func applySelectedAuthChecked(req *http.Request, auths models.AuthConfigs, credentials map[string]any) error {
	for _, auth := range auths {
		switch authrouting.CanonicalType(auth.Type, auth.Scheme) {
		case "basic", "bearer":
			applyHTTPAuth(req, auth, credentials)
		case "oauth", "oidc":
			applyOAuth(req, auth, credentials)
		case "api_key":
			applyAPIKey(req, auth, credentials)
		case "oauth1":
			if err := applyOAuth1(req, auth, credentials); err != nil {
				return err
			}
		case "digest", "mtls":
		default:
			return authRoutingError("unsupported_strategy")
		}
	}
	return nil
}

func applyBasicAuth(req *http.Request, auth models.AuthConfig, credentials map[string]any) {
	username := credentialValue(credentials, auth.Name+"_username")
	password := credentialValue(credentials, auth.Name+"_password")
	req.SetBasicAuth(username, password)
}

func credentialPresent(credentials map[string]any, name string) bool {
	return credentialValue(credentials, name) != ""
}

func credentialValue(credentials map[string]any, name string) string {
	value, _ := credentials[name].(string)
	return value
}

func selectedCredentialAuthType(credentials map[string]any) string {
	value := credentialValue(credentials, "fused_auth_type")
	return authrouting.CanonicalType(strings.ToLower(strings.TrimSpace(value)), "")
}

func selectedCredentialAuthName(credentials map[string]any) string {
	return strings.TrimSpace(credentialValue(credentials, "fused_auth_name"))
}

func authSelectionOutcome(selected models.AuthConfigs) string {
	if len(selected) == 0 {
		return "anonymous"
	}
	return "selected"
}

func recordAuthSelection(ctx context.Context, ctxSpan trace.Span, selected models.AuthConfigs, outcome string) {
	names := make([]string, 0, len(selected))
	types := make([]string, 0, len(selected))
	for _, auth := range selected {
		names = append(names, auth.Name)
		types = append(types, authrouting.CanonicalType(auth.Type, auth.Scheme))
	}
	ctxSpan.SetAttributes(
		attribute.Int("auth.scheme_count", len(selected)),
		attribute.String("auth.strategy", authSelectionStrategy(types)),
		attribute.String("auth.selection_outcome", outcome),
	)
	RecordAuthSummary(ctx, AuthExecutionSummary{SchemeNames: names, SchemeTypes: types, SchemeCount: int64(len(selected)), Outcome: outcome})
}

func authSelectionStrategy(types []string) string {
	if len(types) == 0 {
		return "anonymous"
	}
	if len(types) > 1 {
		return "mixed"
	}
	switch types[0] {
	case "oauth1":
		return "oauth1_signature"
	case "digest":
		return "http_challenge"
	default:
		return types[0]
	}
}
