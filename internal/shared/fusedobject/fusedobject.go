package fusedobject

import (
	"database/sql/driver"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
)

type Server struct {
	URL         string `json:"url"`
	Description string `json:"description,omitempty"`
	Environment string `json:"environment,omitempty"`
	IsDefault   bool   `json:"is_default,omitempty"`
}

type Servers []Server

type Resource struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
}

type Webhook struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Method      string    `json:"method"`
	Description string    `json:"description"`
	RequestBody *Schema   `json:"request_body,omitempty"`
}

type Endpoint struct {
	ID             uuid.UUID `json:"id"`
	Name           string    `json:"name"`
	Description    string    `json:"description"`
	ResourceID     uuid.UUID `json:"resource_id,omitempty"`
	ResourceName   string    `json:"resource_name,omitempty"`
	Version        string    `json:"version"`
	Method         string    `json:"method"`
	NormalizedPath string    `json:"normalized_path"`
	Path           string    `json:"path"`
	Deprecated     bool      `json:"deprecated"`
	// IsSSE indicates the vendor endpoint streams Server-Sent Events.
	// When true the Engine sets Accept: text/event-stream and parses
	// the response line-by-line, forwarding each parsed event as a chunk.
	IsSSE        bool       `json:"is_sse,omitempty"`
	Parameters   Parameters `json:"parameters"`
	RequestBody  *Schema    `json:"request_body,omitempty"`
	Responses    Responses  `json:"responses"`
	GraphQLQuery *string    `json:"graphql_query,omitempty"`
	// ProviderProtocol is kept distinct from the SDK/MCP/webhook execution
	// transport used by audit events; this value describes the provider wire.
	ProviderProtocol string `json:"provider_protocol,omitempty"`
	OperationKind    string `json:"operation_kind,omitempty"`
	// Pagination, when set, tells the Engine how to auto-paginate this endpoint:
	// which request param carries the next token and where to read the next token
	// from in the response. A nil value means the endpoint is not paginated.
	Pagination *PaginationConfig `json:"pagination,omitempty"`
}

// PaginationConfig mirrors models.PaginationConfig on the wire so the Engine's
// dispatcher can auto-paginate without understanding the source spec format.
type PaginationConfig struct {
	Type         string `json:"type"`          // "cursor", "offset", "page_number", "next_url"
	RequestParam string `json:"request_param"` // e.g. "cursor", "offset"
	ResponsePath string `json:"response_path"` // JSON path to the next token, e.g. "metadata.next_cursor"
}

type Parameter struct {
	Name        string `json:"name"`
	In          string `json:"in"`
	Required    bool   `json:"required"`
	Type        string `json:"type"`
	Description string `json:"description"`
}

type Parameters []Parameter

type Schema struct {
	Ref        string            `json:"$ref,omitempty"`
	Type       string            `json:"type,omitempty"`
	Format     string            `json:"format,omitempty"`
	Properties map[string]Schema `json:"properties,omitempty"`
	Items      *Schema           `json:"items,omitempty"`
	Required   []string          `json:"required,omitempty"`
	Example    any               `json:"example,omitempty"`
}

type Responses map[string]Schema

type AuthConfig struct {
	Name                string            `json:"name,omitempty"`
	Type                string            `json:"type"`
	Flow                string            `json:"flow,omitempty"`
	Scheme              string            `json:"scheme,omitempty"`
	Location            string            `json:"location,omitempty"`
	KeyName             string            `json:"key_name,omitempty"`
	TokenURL            string            `json:"token_url,omitempty"`
	AuthorizationURL    string            `json:"authorization_url,omitempty"`
	OpenIdConnectUrl    string            `json:"open_id_connect_url,omitempty"`
	Scopes              []string          `json:"scopes,omitempty"`
	PKCERequired        bool              `json:"pkce_required,omitempty"`
	ScopesDelimiter     string            `json:"scopes_delimiter,omitempty"`
	TokenEndpointAuth   string            `json:"token_endpoint_auth,omitempty"`
	ExtraAuthParams     map[string]string `json:"extra_auth_params,omitempty"`
	ExtraTokenParams    map[string]string `json:"extra_token_params,omitempty"`
	RefreshTokenRotates bool              `json:"refresh_token_rotates,omitempty"`
}

type AuthConfigs []AuthConfig

type IncomingWebhookConfig struct {
	AuthType            string   `json:"auth_type"`
	AuthLocation        string   `json:"auth_location,omitempty"`
	AuthKeyName         string   `json:"auth_key_name,omitempty"`
	SignatureHeader     string   `json:"signature_header,omitempty"`
	SigningSecret       string   `json:"signing_secret,omitempty"`
	VerificationHeaders []string `json:"verification_headers,omitempty"`
}

type RateLimitConfig struct {
	Strategy          string `json:"strategy"`
	RequestsPerSecond int    `json:"requests_per_second"`
	RequestsPerMinute int    `json:"requests_per_minute"`
}

type RetryConfig struct {
	Strategy   string `json:"strategy"`
	MaxRetries int    `json:"max_retries"`
	BackoffMs  int    `json:"backoff_ms"`
}

type DefaultHeaders map[string]string

// --- Helpers for JSON ---

func scanJSONB(value interface{}, target interface{}) error {
	if value == nil {
		return nil
	}
	b, ok := value.([]byte)
	if !ok {
		s, ok := value.(string)
		if !ok {
			return errors.New("type assertion to []byte or string failed")
		}
		b = []byte(s)
	}
	return json.Unmarshal(b, target)
}

func (r Responses) Value() (driver.Value, error)  { return json.Marshal(r) }
func (r *Responses) Scan(value interface{}) error { return scanJSONB(value, r) }

func (d DefaultHeaders) Value() (driver.Value, error)  { return json.Marshal(d) }
func (d *DefaultHeaders) Scan(value interface{}) error { return scanJSONB(value, d) }

func (s Schema) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *Schema) Scan(value interface{}) error { return scanJSONB(value, s) }

func (s Servers) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *Servers) Scan(value interface{}) error { return scanJSONB(value, s) }

func (s AuthConfig) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *AuthConfig) Scan(value interface{}) error { return scanJSONB(value, s) }

func (s AuthConfigs) Value() (driver.Value, error) { return json.Marshal(s) }
func (s *AuthConfigs) Scan(value interface{}) error {
	if value == nil {
		return nil
	}
	b, ok := value.([]byte)
	if !ok {
		str, ok := value.(string)
		if !ok {
			return errors.New("type assertion to []byte or string failed")
		}
		b = []byte(str)
	}
	if err := json.Unmarshal(b, s); err == nil {
		return nil
	}
	var single AuthConfig
	if err := json.Unmarshal(b, &single); err != nil {
		return err
	}
	*s = AuthConfigs{single}
	return nil
}

func (s RateLimitConfig) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *RateLimitConfig) Scan(value interface{}) error { return scanJSONB(value, s) }

func (s RetryConfig) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *RetryConfig) Scan(value interface{}) error { return scanJSONB(value, s) }

func (i IncomingWebhookConfig) Value() (driver.Value, error)  { return json.Marshal(i) }
func (i *IncomingWebhookConfig) Scan(value interface{}) error { return scanJSONB(value, i) }

func (p Parameter) Value() (driver.Value, error)  { return json.Marshal(p) }
func (p *Parameter) Scan(value interface{}) error { return scanJSONB(value, p) }

func (p Parameters) Value() (driver.Value, error)  { return json.Marshal(p) }
func (p *Parameters) Scan(value interface{}) error { return scanJSONB(value, p) }
