package fusedobject

import (
	"database/sql/driver"
	"encoding/json"
	"errors"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/paginationpolicy"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/Usefused/engine/internal/shared/retrypolicy"
	"github.com/Usefused/engine/internal/shared/serverrouting"
	"github.com/Usefused/engine/internal/shared/signaturepolicy"
	"github.com/Usefused/engine/internal/shared/workflowcontract"
	"github.com/google/uuid"
)

type Server struct {
	URL         string                   `json:"url"`
	Name        string                   `json:"name,omitempty"`
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
	ID          uuid.UUID                 `json:"id"`
	Name        string                    `json:"name"`
	Method      string                    `json:"method"`
	Description string                    `json:"description"`
	RequestBody *Schema                   `json:"request_body,omitempty"`
	Contract    *InboundOperationContract `json:"contract,omitempty"`
}

type Endpoint struct {
	ID               uuid.UUID       `json:"id"`
	StableKey        string          `json:"stable_key"`
	Name             string          `json:"name"`
	Description      string          `json:"description"`
	ResourceID       uuid.UUID       `json:"resource_id,omitempty"`
	ResourceName     string          `json:"resource_name,omitempty"`
	Version          string          `json:"version"`
	Method           string          `json:"method"`
	NormalizedPath   string          `json:"normalized_path"`
	Path             string          `json:"path"`
	OperationServers Servers         `json:"operation_servers,omitempty"`
	Deprecated       bool            `json:"deprecated"`
	Parameters       Parameters      `json:"parameters"`
	RequestContent   *RequestContent `json:"request_content,omitempty"`
	Responses        Responses       `json:"responses"`
	GraphQLQuery     *string         `json:"graphql_query,omitempty"`
	// ProviderProtocol is kept distinct from the SDK/MCP/webhook execution
	// transport used by audit events; this value describes the provider wire.
	ProviderProtocol string `json:"provider_protocol,omitempty"`
	OperationKind    string `json:"operation_kind,omitempty"`
	// Pagination, when set, tells the Engine how to auto-paginate this endpoint:
	// which request param carries the next token and where to read the next token
	// from in the response. A nil value means the endpoint is not paginated.
	Pagination           *PaginationConfig        `json:"pagination,omitempty"`
	SecurityRequirements authrouting.Requirements `json:"security_requirements"`
	Documentation        *OperationDocumentation  `json:"documentation,omitempty"`
}

// PaginationConfig is the versioned Registry-to-Engine execution contract.
type PaginationConfig = paginationpolicy.Config

const PathEncodingPreserveSlashes = "preserve_slashes"

type Parameter struct {
	Name          string                      `json:"name"`
	In            string                      `json:"in"`
	Required      bool                        `json:"required"`
	Type          string                      `json:"type"`
	Description   string                      `json:"description"`
	PathEncoding  string                      `json:"path_encoding,omitempty"`
	Serialization ParameterSerialization      `json:"serialization"`
	Schema        *SchemaContract             `json:"schema,omitempty"`
	Content       map[string]ParameterContent `json:"content,omitempty"`
	Deprecated    *bool                       `json:"deprecated,omitempty"`
	Example       any                         `json:"example,omitempty"`
	Examples      map[string]any              `json:"examples,omitempty"`
}

type ParameterSerialization struct {
	Style           string `json:"style"`
	Explode         *bool  `json:"explode"`
	AllowReserved   *bool  `json:"allow_reserved"`
	AllowEmptyValue *bool  `json:"allow_empty_value"`
}

type ParameterContent struct {
	Schema         *SchemaContract            `json:"schema,omitempty"`
	ItemSchema     *SchemaContract            `json:"item_schema,omitempty"`
	Encoding       map[string]RequestEncoding `json:"encoding,omitempty"`
	PrefixEncoding []RequestEncoding          `json:"prefix_encoding,omitempty"`
	ItemEncoding   *RequestEncoding           `json:"item_encoding,omitempty"`
	Example        any                        `json:"example,omitempty"`
	Examples       map[string]any             `json:"examples,omitempty"`
}

type SchemaContract struct {
	Dialect               string                       `json:"dialect"`
	Raw                   json.RawMessage              `json:"raw"`
	ContentHash           string                       `json:"content_hash"`
	Projection            Schema                       `json:"projection"`
	ProjectionDiagnostics []SchemaProjectionDiagnostic `json:"projection_diagnostics,omitempty"`
}

type SchemaProjectionDiagnostic struct {
	Code    string `json:"code"`
	Keyword string `json:"keyword"`
	Pointer string `json:"pointer"`
	Message string `json:"message"`
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
	Required         bool                             `json:"required,omitempty"`
	PayloadParameter string                           `json:"payload_parameter,omitempty"`
	Representations  []RequestRepresentation          `json:"representations"`
	DefaultMediaType string                           `json:"default_media_type,omitempty"`
	UploadWorkflow   *workflowcontract.UploadWorkflow `json:"upload_workflow,omitempty"`
}

type RequestRepresentation struct {
	MediaType      string                     `json:"media_type"`
	Serialization  string                     `json:"serialization"`
	Schema         *SchemaContract            `json:"schema,omitempty"`
	ItemSchema     *SchemaContract            `json:"item_schema,omitempty"`
	Encoding       map[string]RequestEncoding `json:"encoding,omitempty"`
	PrefixEncoding []RequestEncoding          `json:"prefix_encoding,omitempty"`
	ItemEncoding   *RequestEncoding           `json:"item_encoding,omitempty"`
	Example        any                        `json:"example,omitempty"`
	Examples       map[string]any             `json:"examples,omitempty"`
}

type RequestEncoding struct {
	ContentType    string                     `json:"content_type,omitempty"`
	Headers        map[string]HeaderContract  `json:"headers,omitempty"`
	Style          string                     `json:"style,omitempty"`
	Explode        *bool                      `json:"explode,omitempty"`
	AllowReserved  *bool                      `json:"allow_reserved,omitempty"`
	Encoding       map[string]RequestEncoding `json:"encoding,omitempty"`
	PrefixEncoding []RequestEncoding          `json:"prefix_encoding,omitempty"`
	ItemEncoding   *RequestEncoding           `json:"item_encoding,omitempty"`
	BinaryEncoding string                     `json:"binary_encoding,omitempty"`
}

type HeaderContract struct {
	Description   string                      `json:"description,omitempty"`
	Required      *bool                       `json:"required,omitempty"`
	Deprecated    *bool                       `json:"deprecated,omitempty"`
	Serialization ParameterSerialization      `json:"serialization"`
	Schema        *SchemaContract             `json:"schema,omitempty"`
	Content       map[string]ParameterContent `json:"content,omitempty"`
	Example       any                         `json:"example,omitempty"`
	Examples      map[string]any              `json:"examples,omitempty"`
}

type ResponseRepresentation struct {
	MediaType      string               `json:"media_type"`
	Schema         *SchemaContract      `json:"schema,omitempty"`
	ItemSchema     *SchemaContract      `json:"item_schema,omitempty"`
	SSE            *SSEResponseContract `json:"sse,omitempty"`
	PrefixEncoding []RequestEncoding    `json:"prefix_encoding,omitempty"`
	ItemEncoding   *RequestEncoding     `json:"item_encoding,omitempty"`
	Example        any                  `json:"example,omitempty"`
	Examples       map[string]any       `json:"examples,omitempty"`
}

type SSEResponseContract struct {
	ItemMode     string  `json:"item_mode"`
	DoneSentinel *string `json:"done_sentinel,omitempty"`
}

type LinkContract struct {
	OperationRef string               `json:"operation_ref,omitempty"`
	OperationID  string               `json:"operation_id,omitempty"`
	Description  string               `json:"description,omitempty"`
	Parameters   map[string]any       `json:"parameters,omitempty"`
	RequestBody  any                  `json:"request_body,omitempty"`
	Server       *Server              `json:"server,omitempty"`
	Extensions   NamespacedExtensions `json:"extensions,omitempty"`
}

type InboundOperationContract struct {
	Kind                 string                   `json:"kind"`
	RuntimeExpression    string                   `json:"runtime_expression,omitempty"`
	Parent               *CallbackParent          `json:"parent,omitempty"`
	Path                 string                   `json:"path"`
	Summary              string                   `json:"summary,omitempty"`
	Description          string                   `json:"description,omitempty"`
	Tags                 []string                 `json:"tags"`
	ExternalDocs         *ExternalDocumentation   `json:"external_docs,omitempty"`
	Deprecated           bool                     `json:"deprecated"`
	OperationServers     Servers                  `json:"operation_servers,omitempty"`
	Parameters           Parameters               `json:"parameters"`
	RequestContent       *RequestContent          `json:"request_content,omitempty"`
	Responses            Responses                `json:"responses"`
	SecurityRequirements authrouting.Requirements `json:"security_requirements"`
	Extensions           NamespacedExtensions     `json:"extensions,omitempty"`
}

const (
	InboundOperationKindWebhook  = "webhook"
	InboundOperationKindCallback = "callback"
)

type CallbackParent struct {
	OperationID  string `json:"operation_id"`
	Method       string `json:"method"`
	Path         string `json:"path"`
	CallbackName string `json:"callback_name"`
}

type OperationDocumentation struct {
	Summary      string                 `json:"summary,omitempty"`
	Description  string                 `json:"description,omitempty"`
	Tags         []string               `json:"tags"`
	ExternalDocs *ExternalDocumentation `json:"external_docs,omitempty"`
	Extensions   NamespacedExtensions   `json:"extensions,omitempty"`
}

type ServiceDocumentation struct {
	TermsOfService string                 `json:"terms_of_service,omitempty"`
	Contact        *ContactDocumentation  `json:"contact,omitempty"`
	License        *LicenseDocumentation  `json:"license,omitempty"`
	Tags           []TagDocumentation     `json:"tags"`
	ExternalDocs   *ExternalDocumentation `json:"external_docs,omitempty"`
	Extensions     NamespacedExtensions   `json:"extensions,omitempty"`
}

type ContactDocumentation struct {
	Name  string `json:"name,omitempty"`
	URL   string `json:"url,omitempty"`
	Email string `json:"email,omitempty"`
}

type LicenseDocumentation struct {
	Name       string `json:"name,omitempty"`
	Identifier string `json:"identifier,omitempty"`
	URL        string `json:"url,omitempty"`
}

type TagDocumentation struct {
	Name         string                 `json:"name"`
	Summary      string                 `json:"summary,omitempty"`
	Description  string                 `json:"description,omitempty"`
	Parent       string                 `json:"parent,omitempty"`
	Kind         string                 `json:"kind,omitempty"`
	ExternalDocs *ExternalDocumentation `json:"external_docs,omitempty"`
}

type ExternalDocumentation struct {
	Description string `json:"description,omitempty"`
	URL         string `json:"url"`
}

type NamespacedExtensions map[string]NamespacedExtension

type NamespacedExtension struct {
	Value      json.RawMessage `json:"value"`
	Provenance string          `json:"provenance"`
}

type ResponseContract struct {
	Summary         string                    `json:"summary,omitempty"`
	Description     string                    `json:"description"`
	Headers         map[string]HeaderContract `json:"headers,omitempty"`
	Representations []ResponseRepresentation  `json:"representations"`
	Links           map[string]LinkContract   `json:"links,omitempty"`
}

type Responses map[string]ResponseContract

type AuthConfig struct {
	Name                    string                        `json:"name,omitempty"`
	Type                    string                        `json:"type"`
	Scheme                  string                        `json:"scheme,omitempty"`
	BasicPasswordMode       authrouting.BasicPasswordMode `json:"basic_password_mode,omitempty"`
	Location                string                        `json:"location,omitempty"`
	KeyName                 string                        `json:"key_name,omitempty"`
	OpenIdConnectUrl        string                        `json:"open_id_connect_url,omitempty"`
	OAuth2MetadataURL       string                        `json:"oauth2_metadata_url,omitempty"`
	Deprecated              *bool                         `json:"deprecated,omitempty"`
	PKCERequired            bool                          `json:"pkce_required,omitempty"`
	ScopesDelimiter         string                        `json:"scopes_delimiter,omitempty"`
	TokenEndpointAuthMethod TokenEndpointAuthMethod       `json:"token_endpoint_auth_method,omitempty"`
	TokenRequestMediaType   TokenRequestMediaType         `json:"token_request_media_type,omitempty"`
	ExtraAuthParams         map[string]string             `json:"extra_auth_params,omitempty"`
	ExtraTokenParams        map[string]string             `json:"extra_token_params,omitempty"`
	RefreshTokenRotates     bool                          `json:"refresh_token_rotates,omitempty"`
	RefreshTokenRequired    bool                          `json:"refresh_token_required,omitempty"`
	OAuth2Flows             OAuth2Flows                   `json:"oauth2_flows,omitempty"`
	Strategy                *AuthRuntimeStrategy          `json:"strategy,omitempty"`
	PolicyProvenance        map[string]string             `json:"policy_provenance,omitempty"`
}

type OAuth2Flows map[string]OAuth2FlowContract

type OAuth2FlowContract struct {
	AuthorizationURL       string            `json:"authorization_url,omitempty"`
	DeviceAuthorizationURL string            `json:"device_authorization_url,omitempty"`
	TokenURL               string            `json:"token_url,omitempty"`
	RefreshURL             string            `json:"refresh_url,omitempty"`
	Scopes                 map[string]string `json:"scopes"`
}

type AuthRuntimeStrategy struct {
	Kind      string                 `json:"kind"`
	OAuth1    *OAuth1Strategy        `json:"oauth1,omitempty"`
	Challenge *HTTPChallengeStrategy `json:"challenge,omitempty"`
}

type OAuth1Strategy struct {
	SignatureMethod   string `json:"signature_method"`
	ParameterLocation string `json:"parameter_location"`
}

type HTTPChallengeStrategy struct {
	Scheme string `json:"scheme"`
}

type AuthConfigs []AuthConfig

type TokenEndpointAuthMethod string

// TokenRequestMediaType names the exact provider-facing token body format;
// the empty value intentionally retains OAuth's form-encoded default.
type TokenRequestMediaType string

const (
	TokenEndpointAuthMethodClientSecretBasic TokenEndpointAuthMethod = "client_secret_basic"
	TokenEndpointAuthMethodClientSecretPost  TokenEndpointAuthMethod = "client_secret_post"
	TokenRequestMediaTypeForm                TokenRequestMediaType   = "application/x-www-form-urlencoded"
	TokenRequestMediaTypeJSON                TokenRequestMediaType   = "application/json"
)

type IncomingWebhookConfig struct {
	AuthType            string                  `json:"auth_type"`
	AuthLocation        string                  `json:"auth_location,omitempty"`
	AuthKeyName         string                  `json:"auth_key_name,omitempty"`
	SignatureHeader     string                  `json:"signature_header,omitempty"`
	VerificationHeaders []string                `json:"verification_headers,omitempty"`
	SignaturePolicy     *signaturepolicy.Config `json:"signature_policy,omitempty"`
}

type RateLimitConfig = ratelimitpolicy.Config

type RetryConfig = retrypolicy.Config

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

func (i IncomingWebhookConfig) Value() (driver.Value, error)  { return json.Marshal(i) }
func (i *IncomingWebhookConfig) Scan(value interface{}) error { return scanJSONB(value, i) }

func (p Parameter) Value() (driver.Value, error)  { return json.Marshal(p) }
func (p *Parameter) Scan(value interface{}) error { return scanJSONB(value, p) }

func (p Parameters) Value() (driver.Value, error)  { return json.Marshal(p) }
func (p *Parameters) Scan(value interface{}) error { return scanJSONB(value, p) }
