package authrouting

import "strings"

type BasicPasswordMode string

const (
	BasicPasswordRequired BasicPasswordMode = "required"
	BasicPasswordOptional BasicPasswordMode = "optional"
	BasicPasswordEmpty    BasicPasswordMode = "empty"
)

// EffectiveBasicPasswordMode preserves conventional Basic auth when a provider contract omits Fused's password-mode extension.
func EffectiveBasicPasswordMode(mode BasicPasswordMode) (BasicPasswordMode, bool) {
	// OpenAPI Basic auth normally carries username and password, so omission must not silently weaken the credential pair.
	if mode == "" {
		return BasicPasswordRequired, true
	}
	// Explicit modes retain provider-reviewed exceptions while unknown values still fail closed.
	switch mode {
	case BasicPasswordRequired, BasicPasswordOptional, BasicPasswordEmpty:
		return mode, true
	default:
		return "", false
	}
}

// Requirements preserves OpenAPI's ordered OR-of-AND transport contract.
// Outer order is provider preference; every scheme inside one alternative is required.
type Requirements []Alternative

type Alternative struct {
	Schemes         []Requirement    `json:"schemes"`
	ServerSelection *ServerSelection `json:"server_selection,omitempty"`
}

type ServerSelection struct {
	Scheme    string `json:"scheme"`
	ServerURL string `json:"server_url"`
}

type Requirement struct {
	Scheme string   `json:"scheme"`
	Scopes []string `json:"scopes"`
}

func CanonicalType(rawType, scheme string) string {
	authType := strings.ToLower(strings.TrimSpace(rawType))
	authType = strings.ReplaceAll(authType, "-", "_")
	switch authType {
	case "apikey", "api_key":
		return "api_key"
	case "oauth", "oauth2", "oauth2_authorization_code":
		return "oauth"
	case "oauth1", "oauth_1":
		return "oauth1"
	case "openidconnect", "open_id_connect", "oidc":
		return "oidc"
	case "mutualtls", "mutual_tls", "mtls":
		return "mtls"
	case "basic", "bearer":
		return authType
	case "http":
		return strings.ToLower(strings.TrimSpace(scheme))
	default:
		return authType
	}
}
