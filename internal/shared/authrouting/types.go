package authrouting

import "strings"

type BasicPasswordMode string

const (
	BasicPasswordRequired BasicPasswordMode = "required"
	BasicPasswordOptional BasicPasswordMode = "optional"
	BasicPasswordEmpty    BasicPasswordMode = "empty"
)

// Requirements preserves OpenAPI's ordered OR-of-AND transport contract.
// Outer order is provider preference; every scheme inside one alternative is required.
type Requirements []Alternative

type Alternative struct {
	Schemes []Requirement `json:"schemes"`
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
