package fusedobject

import (
	"database/sql/driver"
	"encoding/json"
	"errors"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/Usefused/engine/internal/shared/serverrouting"
	"github.com/google/uuid"
)

type Server struct {
	URL         string                   `json:"url"`
	Description string                   `json:"description,omitempty"`
	Environment string                   `json:"environment,omitempty"`
	IsDefault   bool                     `json:"is_default,omitempty"`
	Variables   []serverrouting.Variable `json:"variables,omitempty"`
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
	StableKey      string    `json:"stable_key"`
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
	IsSSE          bool            `json:"is_sse,omitempty"`
	Parameters     Parameters      `json:"parameters"`
	RequestContent *RequestContent `json:"request_content,omitempty"`
	Responses      Responses       `json:"responses"`
	GraphQLQuery   *string         `json:"graphql_query,omitempty"`
	// ProviderProtocol is kept distinct from the SDK/MCP/webhook execution
	// transport used by audit events; this value describes the provider wire.
	ProviderProtocol string `json:"provider_protocol,omitempty"`
	OperationKind    string `json:"operation_kind,omitempty"`
	// Pagination, when set, tells the Engine how to auto-paginate this endpoint:
	// which request param carries the next token and where to read the next token
	// from in the response. A nil value means the endpoint is not paginated.
	Pagination           *PaginationConfig        `json:"pagination,omitempty"`
	SecurityRequirements authrouting.Requirements `json:"security_requirements"`
}

// PaginationConfig is the versioned Registry-to-Engine execution contract.
type PaginationConfig = paginationpolicy.Config

const PathEncodingPreserveSlashes = "preserve_slashes"

type Parameter struct {
	Name         string `json:"name"`
	In           string `json:"in"`
	Required     bool   `json:"required"`
	Type         string `json:"type"`
	Description  string `json:"description"`
	PathEncoding string `json:"path_encoding,omitempty"`
}

type Parameters []Parameter

type Schema struct {
	Ref                  string            `json:"$ref,omitempty"`
	Type                 string            `json:"type,omitempty"`
	Format               string            `json:"format,omitempty"`
	Properties           map[string]Schema `json:"properties,omitempty"`
	Items                *Schema           `json:"items,omitempty"`
	AdditionalProperties *Schema           `json:"additional_properties,omitempty"`
	Required             []string          `json:"required,omitempty"`
	Example              any               `json:"example,omitempty"`
}

const (
	RequestSerializationJSON      = "json"
	RequestSerializationForm      = "form_urlencoded"
	RequestSerializationMultipart = "multipart"
	RequestSerializationRaw       = "raw"
	RequestBinaryEncodingBase64   = "base64"
)

type RequestContent struct {
	MediaType        string                 `json:"media_type"`
	Serialization    string                 `json:"serialization"`
	Required         bool                   `json:"required,omitempty"`
	Schema           *Schema                `json:"schema,omitempty"`
	PayloadParameter string                 `json:"payload_parameter,omitempty"`
	BinaryEncoding   string                 `json:"binary_encoding,omitempty"`
	Parts            map[string]RequestPart `json:"parts,omitempty"`
}

type RequestPart struct {
	ContentType    string `json:"content_type,omitempty"`
	BinaryEncoding string `json:"binary_encoding,omitempty"`
}

type Responses map[string]Schema

type AuthConfig struct {
	Name                    string                        `json:"name,omitempty"`
	Type                    string                        `json:"type"`
	Flow                    string                        `json:"flow,omitempty"`
	Scheme                  string                        `json:"scheme,omitempty"`
	BasicPasswordMode       authrouting.BasicPasswordMode `json:"basic_password_mode,omitempty"`
	Location                string                        `json:"location,omitempty"`
	KeyName                 string                        `json:"key_name,omitempty"`
	TokenURL                string                        `json:"token_url,omitempty"`
	AuthorizationURL        string                        `json:"authorization_url,omitempty"`
	OpenIdConnectUrl        string                        `json:"open_id_connect_url,omitempty"`
	Scopes                  []string                      `json:"scopes,omitempty"`
	PKCERequired            bool                          `json:"pkce_required,omitempty"`
	ScopesDelimiter         string                        `json:"scopes_delimiter,omitempty"`
	TokenEndpointAuthMethod TokenEndpointAuthMethod       `json:"token_endpoint_auth_method,omitempty"`
	ExtraAuthParams         map[string]string             `json:"extra_auth_params,omitempty"`
	ExtraTokenParams        map[string]string             `json:"extra_token_params,omitempty"`
	RefreshTokenRotates     bool                          `json:"refresh_token_rotates,omitempty"`
}

type AuthConfigs []AuthConfig

type TokenEndpointAuthMethod string

const (
	TokenEndpointAuthMethodClientSecretBasic TokenEndpointAuthMethod = "client_secret_basic"
	TokenEndpointAuthMethodClientSecretPost  TokenEndpointAuthMethod = "client_secret_post"
)

type IncomingWebhookConfig struct {
	AuthType            string   `json:"auth_type"`
	AuthLocation        string   `json:"auth_location,omitempty"`
	AuthKeyName         string   `json:"auth_key_name,omitempty"`
	SignatureHeader     string   `json:"signature_header,omitempty"`
	SigningSecret       string   `json:"signing_secret,omitempty"`
	VerificationHeaders []string `json:"verification_headers,omitempty"`
}

type RateLimitConfig = ratelimitpolicy.Config

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

func (r RequestContent) Value() (driver.Value, error)  { return json.Marshal(r) }
func (r *RequestContent) Scan(value interface{}) error { return scanJSONB(value, r) }

func (s Servers) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *Servers) Scan(value interface{}) error { return scanJSONB(value, s) }

func (s AuthConfig) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *AuthConfig) Scan(value interface{}) error { return scanJSONB(value, s) }

func (s AuthConfigs) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *AuthConfigs) Scan(value interface{}) error { return scanJSONB(value, s) }

func (s RetryConfig) Value() (driver.Value, error)  { return json.Marshal(s) }
func (s *RetryConfig) Scan(value interface{}) error { return scanJSONB(value, s) }

func (i IncomingWebhookConfig) Value() (driver.Value, error)  { return json.Marshal(i) }
func (i *IncomingWebhookConfig) Scan(value interface{}) error { return scanJSONB(value, i) }

func (p Parameter) Value() (driver.Value, error)  { return json.Marshal(p) }
func (p *Parameter) Scan(value interface{}) error { return scanJSONB(value, p) }

func (p Parameters) Value() (driver.Value, error)  { return json.Marshal(p) }
func (p *Parameters) Scan(value interface{}) error { return scanJSONB(value, p) }
