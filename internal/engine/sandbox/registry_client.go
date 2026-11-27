package sandbox

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Usefused/engine/internal/shared/authrouting"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type RegistryClient interface {
	Handshake(ctx context.Context) (accountID string, workspaceName string, err error)
	FetchServiceMetadata(ctx context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, error)
	FetchEndpointByName(ctx context.Context, serviceID uuid.UUID, version string, endpointName string) (*fusedobject.Endpoint, error)
	// FetchEndpointsByNames is the batched contract reader underlying live
	// connection-discovery lookups. SDK/MCP selection validation instead
	// uses admitted Engine snapshots and never calls this Registry path.
	FetchEndpointsByNames(ctx context.Context, serviceID uuid.UUID, serviceVersionID uuid.UUID, endpointNames []string) ([]fusedobject.Endpoint, error)
	// FetchServiceOperations returns the full operation contract used by
	// connection-profile validation; MCP discovery reads local snapshots.
	FetchServiceOperations(ctx context.Context, serviceID uuid.UUID, serviceVersionID uuid.UUID) ([]fusedobject.Endpoint, error)
	FetchServiceVersionRevisions(ctx context.Context, refs []ServiceVersionRef, apiKey string) ([]ServiceVersionRevision, error)
	SearchCatalogue(ctx context.Context, query string, page int, limit int) ([]CatalogueService, error)
	FetchDriftSnapshots(ctx context.Context, serviceID uuid.UUID, apiKey string) ([]models.DriftSnapshot, error)
	// FetchDriftSnapshotsForServices is FetchDriftSnapshots's batched
	// sibling: the workspace/SDK notification inbox needs pending drift for
	// every activated (or SDK-referenced) service in one Registry round
	// trip, instead of one FetchDriftSnapshots call per service in a loop.
	FetchDriftSnapshotsForServices(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) ([]models.DriftSnapshot, error)
	// FetchServiceChangelogSince calls the serviceChangelogSince GraphQL
	// field (see plans/plan-service-changelog.md's "## Phase 2"), returning
	// every service_changelog row for serviceID created after since, oldest
	// first. Deliberately single-service, not batched like
	// FetchDriftSnapshotsForServices -- the poller's cursor is already
	// tracked one row per service, so there's no shared "since" value that
	// would make a batched call shape simpler.
	FetchServiceChangelogSince(ctx context.Context, serviceID uuid.UUID, since time.Time, apiKey string) ([]models.ServiceChangelogEntry, error)
}

// SDKGenerationClient replays one immutable Engine-owned generation request
// through the licensed Registry boundary and returns its current job state.
type SDKGenerationClient interface {
	GenerateSDK(context.Context, models.SDKGenerationRequest) (models.SDKGenerationResult, error)
}

type CatalogueService struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description"`
	IconURL       string `json:"icon_url"`
	BaseURL       string `json:"base_url"`
	EndpointCount int    `json:"endpoint_count"`
	WebhookCount  int    `json:"webhook_count"`
}

type ServiceVersionRevision struct {
	ServiceID              uuid.UUID `json:"service_id"`
	Version                string    `json:"version"`
	ServiceVersionID       uuid.UUID `json:"service_version_id"`
	Revision               int       `json:"revision"`
	SourceHash             string    `json:"source_hash"`
	GenerationContractHash string    `json:"generation_contract_hash,omitempty"`
	RuntimeContractHash    string    `json:"runtime_contract_hash,omitempty"`
	IsPublic               bool      `json:"is_public"`
}

type ServiceVersionAuthConfigs struct {
	ServiceID        uuid.UUID               `json:"service_id"`
	Version          string                  `json:"version"`
	ServiceVersionID uuid.UUID               `json:"service_version_id"`
	AuthConfigs      fusedobject.AuthConfigs `json:"auth_configs"`
}

// registryAuthConfigGraphQLFields is the complete credential-free provider
// contract needed by connect and snapshot flows. Operation-aware auth keeps a
// separate minimal projection because token exchange metadata is not needed
// to select credentials for an individual provider request.
const registryAuthConfigGraphQLFields = `
	name
	type
	scheme
	basic_password_mode
	location
	key_name
	oauth2_metadata_url
	deprecated
	token_endpoint_auth_method
	oauth_token_placement { location name format }
	token_request_media_type
	scope_parameter
	authorization_token_response_path
	open_id_connect_url
	pkce_required
	scopes_delimiter
	extra_auth_params
	extra_token_params
	refresh_token_rotates
	refresh_token_required
	oauth2_flows
	strategy
	policy_provenance
`

type ServiceVersionExecutionAuthSelection struct {
	ServiceID      uuid.UUID `json:"service_id"`
	Version        string    `json:"version"`
	OperationNames []string  `json:"operation_names"`
	SelectAll      bool      `json:"select_all"`
}

type OperationSecuritySummary struct {
	Name                 string                   `json:"name"`
	SecurityRequirements authrouting.Requirements `json:"security_requirements"`
}

type ServiceVersionExecutionAuthContract struct {
	ServiceID        uuid.UUID                  `json:"service_id"`
	Version          string                     `json:"version"`
	ServiceVersionID uuid.UUID                  `json:"service_version_id"`
	OperationNames   []string                   `json:"operation_names"`
	SelectAll        bool                       `json:"select_all"`
	AuthConfigs      fusedobject.AuthConfigs    `json:"auth_configs"`
	Operations       []OperationSecuritySummary `json:"operations"`
}

type ServiceVersionRef = models.ServiceVersionRef

type EngineHeartbeatRequest struct {
	EngineVersion              string    `json:"engine_version"`
	EngineBuildHash            string    `json:"engine_build_hash"`
	AppliedPlan                string    `json:"applied_plan,omitempty"`
	AppliedEntitlementRevision string    `json:"applied_entitlement_revision,omitempty"`
	ReportedAt                 time.Time `json:"reported_at"`
}

// EngineHeartbeatResponse mirrors the Registry heartbeat response contract.
// Defined locally because backend/ is a separate Go module.
type EngineHeartbeatResponse struct {
	Status        string                 `json:"status"`
	AccountID     string                 `json:"account_id"`
	WorkspaceName string                 `json:"workspace_name"`
	IsSuspended   bool                   `json:"is_suspended"`
	PlanChanged   bool                   `json:"plan_changed"`
	Entitlements  *rawRuntimeEntitlement `json:"entitlements"`
}

type EngineUsageReportRequest struct {
	EngineVersion   string                     `json:"engine_version"`
	EngineBuildHash string                     `json:"engine_build_hash"`
	ReportedAt      time.Time                  `json:"reported_at"`
	Reports         []models.EngineUsageReport `json:"reports"`
}

type sdkPackageLeaseRequest struct {
	Apps []models.SDKPackageLeaseRenewal `json:"apps"`
}

type sdkPackageLeaseResponse struct {
	Renewed int64 `json:"renewed"`
}

type sdkPackageDownloadCountsRequest struct {
	AppIDs []uuid.UUID `json:"app_ids"`
}

type sdkPackageDownloadCount struct {
	AppID     uuid.UUID `json:"app_id"`
	Downloads int64     `json:"downloads"`
}

type sdkPackageDownloadCountsResponse struct {
	Counts []sdkPackageDownloadCount `json:"counts"`
}

// SDKPackageDownloadCountClient is the narrow analytics projection consumed
// by Engine GraphQL; package bytes remain on the separate download interface.
type SDKPackageDownloadCountClient interface {
	FetchSDKPackageDownloadCounts(context.Context, []uuid.UUID) (map[uuid.UUID]int64, error)
}

type publicInsightEligibilityRequest struct {
	ServiceIDs []uuid.UUID `json:"service_ids"`
}

type publicInsightEligibilityResponse struct {
	Services []models.PublicServiceInsightEligibility `json:"services"`
}

type publicInsightReportRequest struct {
	SchemaVersion   int                                 `json:"schema_version"`
	EngineVersion   string                              `json:"engine_version"`
	EngineBuildHash string                              `json:"engine_build_hash"`
	ReportedAt      time.Time                           `json:"reported_at"`
	Reports         []models.PublicServiceInsightReport `json:"reports"`
}

type publicInsightReportResponse struct {
	Results []models.PublicServiceInsightReportResult `json:"results"`
}

type EngineHandshakeResult struct {
	AccountID            string
	EngineID             string
	WorkspaceName        string
	OwnerEmail           string
	Entitlements         models.RuntimeEntitlement
	Identity             ManagedIdentityCapability
	ManagedAuthBrokerURL string
}

type ManagedIdentityCapability struct {
	ProtocolVersion    int    `json:"protocol_version"`
	Available          bool   `json:"available"`
	OrganizationStatus string `json:"organization_status"`
	InstallationID     string `json:"installation_id"`
}

type ManagedLoginTransaction struct {
	TransactionID   uuid.UUID `json:"transaction_id"`
	VerificationURL string    `json:"verification_url"`
	ExpiresAt       time.Time `json:"expires_at"`
}

type ManagedIdentityAssertion struct {
	SchemaVersion   int       `json:"schema_version"`
	TransactionID   uuid.UUID `json:"transaction_id"`
	AccountID       uuid.UUID `json:"account_id"`
	InstallationID  uuid.UUID `json:"installation_id"`
	Purpose         string    `json:"purpose"`
	Provider        string    `json:"provider"`
	Issuer          string    `json:"issuer"`
	ExternalSubject string    `json:"external_subject"`
	VerifiedEmail   string    `json:"verified_email"`
	DisplayName     string    `json:"display_name"`
	AuthMethod      string    `json:"auth_method"`
	EnrollmentRef   string    `json:"enrollment_ref"`
	AuthenticatedAt time.Time `json:"authenticated_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	LogoutToken     string    `json:"logout_token,omitempty"`
	LogoutExpiresAt time.Time `json:"logout_expires_at,omitempty"`
}

type ManagedLogoutResult struct {
	LogoutURL string `json:"logout_url"`
}

type ManagedIdentityRegistryError struct {
	Status    int
	Code      string
	RequestID string
	TraceID   string
}

func (e ManagedIdentityRegistryError) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("managed identity Registry request failed with status %d", e.Status)
	}
	return fmt.Sprintf("managed identity Registry request failed with status %d (%s)", e.Status, e.Code)
}

type rawRuntimeEntitlement struct {
	EntitlementRevision          string `json:"entitlement_revision"`
	Plan                         string `json:"plan"`
	HeartbeatRequired            *bool  `json:"heartbeat_required"`
	UsageReporting               string `json:"usage_reporting"`
	PublicServiceInsightsEnabled bool   `json:"public_service_insights_enabled"`
	HeartbeatIntervalSeconds     int    `json:"heartbeat_interval_seconds"`
	HeartbeatStaleAfterSeconds   int    `json:"heartbeat_stale_after_seconds"`
	MaxBuckets                   *int   `json:"max_buckets,omitempty"`
	MaxAPIFamilies               *int   `json:"max_api_families,omitempty"`
	MaxSDKFamilies               *int   `json:"max_sdk_families,omitempty"`
	MaxMCPFamilies               *int   `json:"max_mcp_families,omitempty"`
	MaxUnifiedAppFamilies        *int   `json:"max_unified_app_families,omitempty"`
	MaxUnifiedAppConcurrency     *int   `json:"max_unified_app_concurrency,omitempty"`
	MaxServices                  *int   `json:"max_services,omitempty"`
	MaxSandboxConcurrency        *int   `json:"max_sandbox_concurrency,omitempty"`
	DriftMonitoringEnabled       bool   `json:"drift_monitoring_enabled"`
	UnifiedAppAlwaysOnEnabled    bool   `json:"unified_app_always_on_enabled"`
	WebhookIngestionEnabled      bool   `json:"webhook_ingestion_enabled"`
	SSOEnabled                   bool   `json:"sso_enabled"`
	LocalRegistryEnabled         bool   `json:"local_registry_enabled"`
	ExecutionRetentionDays       *int   `json:"execution_retention_days,omitempty"`
}

type ConnectionProfileRef struct {
	ServiceVersionID uuid.UUID `json:"service_version_id"`
	AuthType         string    `json:"auth_type"`
	AuthName         string    `json:"auth_name"`
}

type ConnectionProfileRevision struct {
	ProfileID        uuid.UUID                 `json:"profile_id"`
	ServiceID        uuid.UUID                 `json:"service_id"`
	ServiceVersionID uuid.UUID                 `json:"service_version_id"`
	Name             string                    `json:"name"`
	AuthType         string                    `json:"auth_type"`
	AuthName         string                    `json:"auth_name"`
	Revision         int                       `json:"revision"`
	ProfileHash      string                    `json:"profile_hash"`
	Config           connectionprofile.Profile `json:"config"`
	Provenance       string                    `json:"provenance"`
}

type ConnectionProfileContract = models.ServiceVersionConnectionContract

// doBearerGraphQL executes a Registry GraphQL query authenticated with an
// optional bearer/API-key token and decodes its typed data payload.
func doBearerGraphQL[T any](ctx context.Context, c *HTTPRegistryClient, opErrPrefix string, query graphqlQuery, apiKey string) (T, error) {
	var zero T
	req, err := c.newGraphQLRequest(ctx, query)
	// Invalid request construction must fail before issuing any Registry call.
	if err != nil {
		return zero, err
	}
	// Forward caller credentials in both headers expected by Registry auth middleware.
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := c.do(req)
	// Surface transport failures with the caller's operation context.
	if err != nil {
		return zero, fmt.Errorf("%s: %w", opErrPrefix, err)
	}
	defer resp.Body.Close()
	// Reject HTTP error responses before attempting to decode a GraphQL envelope.
	if resp.StatusCode >= 400 {
		return zero, fmt.Errorf("registry returned status %d", resp.StatusCode)
	}
	var result struct {
		Data   T `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	// Malformed JSON payloads cannot produce trustworthy contract or profile data.
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return zero, err
	}
	// Treat any GraphQL error entry as a failed lookup rather than returning partial data.
	if len(result.Errors) > 0 {
		return zero, errors.New(result.Errors[0].Message)
	}
	return result.Data, nil
}

// FetchConnectionProfileContracts loads validation facts for all requested
// pinned versions in one GraphQL round trip.
func (c *HTTPRegistryClient) FetchConnectionProfileContracts(ctx context.Context, versionIDs []uuid.UUID, apiKey string) ([]ConnectionProfileContract, error) {
	// Skip the Registry round trip when no service versions require validation facts.
	if len(versionIDs) == 0 {
		return nil, nil
	}
	// Route Hybrid profile contract validation Local-first with Cloud fallback on misses.
	if c.HasLocalRegistry() {
		return c.fetchHybridConnectionProfileContracts(ctx, versionIDs, apiKey)
	}
	data, err := doBearerGraphQL[struct {
		Contracts []ConnectionProfileContract `json:"connectionProfileContracts"`
	}](ctx, c, "fetch connection profile contracts", graphqlQuery{
		Query: `query ConnectionProfileContracts($ids: [String!]!) {
			connectionProfileContracts(service_version_ids: $ids)
		}`,
		Variables: map[string]interface{}{"ids": uuidStrings(versionIDs)},
	}, apiKey)
	// Propagate request, status, or GraphQL errors without returning partial contracts.
	if err != nil {
		return nil, err
	}
	return data.Contracts, nil
}

// FetchEligibleConnectionProfiles resolves exact Registry publication streams in one bounded request.
func (c *HTTPRegistryClient) FetchEligibleConnectionProfiles(ctx context.Context, refs []ConnectionProfileRef, apiKey string) ([]ConnectionProfileRevision, error) {
	// Skip the Registry round trip when no connection profile references were requested.
	if len(refs) == 0 {
		return nil, nil
	}
	// Route Hybrid profile stream lookups Local-first with Cloud fallback on misses.
	if c.HasLocalRegistry() {
		return c.fetchHybridEligibleConnectionProfiles(ctx, refs, apiKey)
	}
	variables := make([]map[string]interface{}, 0, len(refs))
	for _, ref := range refs {
		// Sending the empty name explicitly preserves Registry's legacy unnamed stream instead of broadening the lookup to every same-family scheme.
		variables = append(variables, map[string]interface{}{"service_version_id": ref.ServiceVersionID.String(), "auth_type": ref.AuthType, "auth_name": ref.AuthName})
	}
	data, err := doBearerGraphQL[struct {
		Profiles []ConnectionProfileRevision `json:"eligibleConnectionProfiles"`
	}](ctx, c, "fetch eligible connection profiles", graphqlQuery{
		Query: `query EligibleConnectionProfiles($refs: [ConnectionProfileRefInput!]!) {
			eligibleConnectionProfiles(refs: $refs) {
				profile_id service_id service_version_id name auth_type auth_name revision profile_hash config provenance
			}
		}`,
		Variables: map[string]interface{}{"refs": variables},
	}, apiKey)
	// Propagate request, status, or GraphQL errors without returning partial profiles.
	if err != nil {
		return nil, err
	}
	return data.Profiles, nil
}

type serviceVersionRevisionBatchResponse struct {
	Versions []ServiceVersionRevision `json:"versions"`
}

type ServiceProviderIdentity struct {
	Name   string `json:"name"`
	Handle string `json:"handle"`
}

type ServiceVisibility struct {
	Name          string                  `json:"name"`
	ServiceID     uuid.UUID               `json:"id"`
	Description   string                  `json:"description"`
	IconURL       string                  `json:"icon_url"`
	BaseURL       string                  `json:"base_url"`
	EndpointCount int                     `json:"endpoint_count"`
	WebhookCount  int                     `json:"webhook_count"`
	IsOwner       bool                    `json:"is_owner"`
	IsPublic      bool                    `json:"is_public"`
	Slug          string                  `json:"slug"`
	Provider      ServiceProviderIdentity `json:"provider"`
	CanonicalRef  string                  `json:"canonical_ref"`
}

// ErrServiceNotFound is returned by VerifyServiceExists when the Registry has
// no service for the given ID, or the caller isn't authorized to see it --
// the Registry's own service resolver (registry/graph/schema.go) returns a
// null service for both cases rather than distinguishing them, so this
// method surfaces both identically. Either way, the caller shouldn't be able
// to add the service to a workspace.
var ErrServiceNotFound = errors.New("service not found in registry")

// VerifyServiceExists confirms a service ID and returns its Registry-authoritative
// name and current version identity without pulling the full catalogue payload
// FetchServiceMetadata does (auth configs, rate limits, every resource's
// endpoints). The Engine reuses that version identity during workspace activation.
//
// The API-key argument is retained at this service boundary because callers
// also use it for local authorization. HTTPRegistryClient.do always replaces
// it with FUSED_LICENSE_KEY before the request crosses into Registry.
// The returned service reference is canonical (@provider/slug), suitable for workspace persistence.
// VerifyServiceExists checks Local Registry first in Hybrid mode and falls back to Cloud only with permission.
func (c *HTTPRegistryClient) VerifyServiceExists(ctx context.Context, serviceID uuid.UUID, apiKey string) (string, string, string, uuid.UUID, error) {
	if c.HasLocalRegistry() {
		return c.verifyHybridServiceExists(ctx, serviceID, apiKey)
	}
	req, err := c.buildVerifyServiceRequest(ctx, serviceID, apiKey)
	if err != nil {
		return "", "", "", uuid.Nil, err
	}

	resp, err := c.do(req)
	if err != nil {
		return "", "", "", uuid.Nil, fmt.Errorf("VerifyServiceExists: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return "", "", "", uuid.Nil, fmt.Errorf("VerifyServiceExists: registry returned status %d: %s", resp.StatusCode, string(body))
	}

	return decodeVerifyServiceResponse(resp)
}

// buildVerifyServiceRequest retrieves the provider-qualified identity needed for collision-free workspace membership.
func (c *HTTPRegistryClient) buildVerifyServiceRequest(ctx context.Context, serviceID uuid.UUID, apiKey string) (*http.Request, error) {
	query := `
		query VerifyService($id: String!) {
			service(id: $id) {
				id
				name
				slug
				canonical_ref
				current_service_version
				service_versions {
					id
					name
				}
			}
		}
	`
	reqBody := graphqlQuery{
		Query:     query,
		Variables: map[string]interface{}{"id": serviceID.String()},
	}
	b, err := json.Marshal(reqBody)
	// Encoding failure must not send a partial verification request.
	if err != nil {
		return nil, fmt.Errorf("VerifyServiceExists: failed to marshal query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(b))
	// Invalid Registry endpoints cannot provide authoritative service metadata.
	if err != nil {
		return nil, fmt.Errorf("VerifyServiceExists: failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)

	return req, nil
}

// decodeVerifyServiceResponse preserves the verified provider namespace alongside the exact current version.
func decodeVerifyServiceResponse(resp *http.Response) (string, string, string, uuid.UUID, error) {
	var gr struct {
		Data struct {
			Service *struct {
				ID                    string `json:"id"`
				Name                  string `json:"name"`
				Slug                  string `json:"slug"`
				CanonicalRef          string `json:"canonical_ref"`
				CurrentServiceVersion string `json:"current_service_version"`
				ServiceVersions       []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"service_versions"`
			} `json:"service"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	// Malformed responses cannot establish workspace membership.
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return "", "", "", uuid.Nil, fmt.Errorf("VerifyServiceExists: failed to decode response: %w", err)
	}
	// A GraphQL failure is not proof that the requested service exists.
	if len(gr.Errors) > 0 {
		return "", "", "", uuid.Nil, fmt.Errorf("VerifyServiceExists: graphql error: %s", gr.Errors[0].Message)
	}
	// Invisible and absent services share the same fail-closed result.
	if gr.Data.Service == nil {
		return "", "", "", uuid.Nil, ErrServiceNotFound
	}
	reference, err := registryWorkspaceServiceReference(gr.Data.Service.Slug, gr.Data.Service.CanonicalRef)
	// Never collapse a verified service back into the account-ambiguous bare slug.
	if err != nil {
		return "", "", "", uuid.Nil, err
	}
	for _, v := range gr.Data.Service.ServiceVersions {
		// Only the explicitly declared current version can supply the activation pin.
		if v.Name == gr.Data.Service.CurrentServiceVersion {
			vid, err := uuid.Parse(v.ID)
			// Invalid immutable identity cannot become an unpinned activation.
			if err != nil {
				return "", "", "", uuid.Nil, fmt.Errorf("VerifyServiceExists: invalid service_version_id: %w", err)
			}
			return gr.Data.Service.Name, reference, gr.Data.Service.CurrentServiceVersion, vid, nil
		}
	}
	return gr.Data.Service.Name, reference, gr.Data.Service.CurrentServiceVersion, uuid.Nil, nil
}

type registryServiceVersionEnvelope struct {
	ID                   string   `json:"id"`
	Name                 string   `json:"name"`
	ContractVersion      int      `json:"contract_version"`
	RequiredCapabilities []string `json:"required_capabilities"`
}

func currentServiceVersionID(current string, versions []struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}) uuid.UUID {
	for _, version := range versions {
		if version.Name == current {
			id, _ := uuid.Parse(version.ID)
			return id
		}
	}
	return uuid.Nil
}

func currentServiceVersionContract(current string, versions []registryServiceVersionEnvelope) (uuid.UUID, fusedobject.ExecutionContractEnvelope) {
	for _, version := range versions {
		if version.Name != current {
			continue
		}
		id, err := uuid.Parse(version.ID)
		if err == nil {
			return id, fusedobject.ExecutionContractEnvelope{
				ContractVersion: version.ContractVersion, RequiredCapabilities: version.RequiredCapabilities,
			}
		}
	}
	return uuid.Nil, fusedobject.ExecutionContractEnvelope{}
}

// FetchServiceVisibility resolves workspace-visible service presentation and publication state.
func (c *HTTPRegistryClient) FetchServiceVisibility(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) (map[uuid.UUID]ServiceVisibility, error) {
	out := map[uuid.UUID]ServiceVisibility{}
	if len(serviceIDs) == 0 {
		return out, nil
	}
	// Partition visibility lookups across Local and Cloud Registries in Hybrid mode.
	if c.HasLocalRegistry() {
		return c.fetchHybridServiceVisibility(ctx, serviceIDs, apiKey)
	}
	reqBody := graphqlQuery{
		Query: `
			query ServiceVisibility($serviceIds: [String!]!) {
				servicesByIds(serviceIds: $serviceIds) {
					id
					name
					slug
					description
					icon_url
					base_url
					endpoint_count
					webhook_count
					provider { name handle }
					canonical_ref
					is_owner
					is_public
				}
			}
		`,
		Variables: map[string]interface{}{"serviceIds": uuidStrings(serviceIDs)},
	}
	data, err := doAPIKeyGraphQL[struct {
		ServicesByIDs []ServiceVisibility `json:"servicesByIds"`
	}](ctx, c, "FetchServiceVisibility", reqBody, apiKey, true)
	// Surface Registry transport or GraphQL failures without populating partial visibility rows.
	if err != nil {
		return nil, err
	}
	for _, vis := range data.ServicesByIDs {
		out[vis.ServiceID] = vis
	}
	return out, nil
}

// doAPIKeyGraphQL executes a Registry GraphQL operation authenticated via
// X-API-Key and decodes its typed data payload with consistent error prefixes.
func doAPIKeyGraphQL[T any](ctx context.Context, c *HTTPRegistryClient, opName string, reqBody graphqlQuery, apiKey string, requireStatusOK bool) (T, error) {
	var zero T
	body, err := json.Marshal(reqBody)
	// Query serialization must succeed before sending any request to Registry.
	if err != nil {
		return zero, fmt.Errorf("%s: marshal query: %w", opName, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	// Reject invalid endpoint or context errors before invoking the HTTP client.
	if err != nil {
		return zero, fmt.Errorf("%s: create request: %w", opName, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := c.do(request)
	// Propagate transport errors tagged with the Registry operation name.
	if err != nil {
		return zero, fmt.Errorf("%s: request failed: %w", opName, err)
	}
	defer response.Body.Close()
	return decodeAPIKeyGraphQLResponse[T](response, opName, requireStatusOK)
}

// decodeAPIKeyGraphQLResponse validates the HTTP status and GraphQL envelope
// for an X-API-Key Registry call and returns its typed data payload.
func decodeAPIKeyGraphQLResponse[T any](response *http.Response, opName string, requireStatusOK bool) (T, error) {
	var zero T
	// Reads require 200 OK while mutations accept any 2xx/3xx non-error status.
	statusFailed := response.StatusCode >= http.StatusBadRequest
	if requireStatusOK {
		statusFailed = response.StatusCode != http.StatusOK
	}
	// Preserve Registry's HTTP error body for caller diagnostics before JSON decoding.
	if statusFailed {
		body, _ := io.ReadAll(response.Body)
		return zero, fmt.Errorf("%s: registry returned %d: %s", opName, response.StatusCode, string(body))
	}
	var decoded struct {
		Data   T `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	// Fail fast on malformed or truncated Registry JSON responses.
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return zero, fmt.Errorf("%s: decode response: %w", opName, err)
	}
	// GraphQL errors indicate the operation did not cleanly succeed even on HTTP 200.
	if len(decoded.Errors) > 0 {
		return zero, fmt.Errorf("%s: graphql error: %s", opName, decoded.Errors[0].Message)
	}
	return decoded.Data, nil
}

// updateVisibilityGraphQL executes a boolean Registry visibility mutation and
// fails if Registry reports that the target was not updated.
func (c *HTTPRegistryClient) updateVisibilityGraphQL(ctx context.Context, opName, query string, variables map[string]interface{}, fieldName, notUpdatedMsg, apiKey string) error {
	data, err := doAPIKeyGraphQL[map[string]bool](ctx, c, opName, graphqlQuery{
		Query:     query,
		Variables: variables,
	}, apiKey, false)
	// Propagate HTTP or GraphQL errors before inspecting the boolean mutation flag.
	if err != nil {
		return err
	}
	// Registry returns false when the visibility change could not be applied.
	if !data[fieldName] {
		return fmt.Errorf("%s: %s", opName, notUpdatedMsg)
	}
	return nil
}

// putExecutionPolicy marshals and PUTs a projected execution policy document to
// the specified Registry API path.
func (c *HTTPRegistryClient) putExecutionPolicy(ctx context.Context, opName, path string, policy any, apiKey string) error {
	body, err := json.Marshal(policy)
	// Reject unserializable policy projections before creating an HTTP request.
	if err != nil {
		return fmt.Errorf("%s: marshal body: %w", opName, err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, c.registryBaseURL()+path, bytes.NewReader(body))
	// Reject invalid URLs or cancelled contexts before issuing the PUT.
	if err != nil {
		return fmt.Errorf("%s: create request: %w", opName, err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := c.do(request)
	// Surface transport failures with the operation name for callers.
	if err != nil {
		return fmt.Errorf("%s: request failed: %w", opName, err)
	}
	defer response.Body.Close()
	// Propagate Registry rejection responses with status and body details.
	if response.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		return fmt.Errorf("%s: registry returned %d: %s", opName, response.StatusCode, string(body))
	}
	return nil
}

// UpdateServicePublic toggles service-level public visibility in Registry.
func (c *HTTPRegistryClient) UpdateServicePublic(ctx context.Context, serviceID uuid.UUID, isPublic bool, apiKey string) error {
	// Dispatch visibility updates to the service's authoritative Registry in Hybrid mode.
	if c.HasLocalRegistry() {
		target, err := c.clientForServiceMutation(ctx, serviceID)
		if err != nil {
			return err
		}
		return target.UpdateServicePublic(ctx, serviceID, isPublic, apiKey)
	}
	return c.updateVisibilityGraphQL(ctx, "UpdateServicePublic", `
			mutation UpdateServicePublic($serviceId: String!, $isPublic: Boolean!) {
				updateServicePublic(serviceId: $serviceId, isPublic: $isPublic)
			}
		`, map[string]interface{}{"serviceId": serviceID.String(), "isPublic": isPublic}, "updateServicePublic", "registry did not update service", apiKey)
}

// PublishServiceExecutionPolicy publishes service-level execution policy
// through the Registry API. Its caller owns the strict Registry projection so
// this transport remains unaware of workspace-only policy controls.
func (c *HTTPRegistryClient) PublishServiceExecutionPolicy(ctx context.Context, serviceID uuid.UUID, policy any, apiKey string) error {
	// Dispatch policy publication to the service's authoritative Registry in Hybrid mode.
	if c.HasLocalRegistry() {
		target, err := c.clientForServiceMutation(ctx, serviceID)
		if err != nil {
			return err
		}
		return target.PublishServiceExecutionPolicy(ctx, serviceID, policy, apiKey)
	}
	return c.putExecutionPolicy(ctx, "PublishServiceExecutionPolicy", "/integrations/"+serviceID.String()+"/execution-policy", policy, apiKey)
}

// UpdateServiceVersionPublic calls the Registry's updateServiceVersionPublic
// mutation to set is_public on one specific version, independent of the
// service-level visibility set by UpdateServicePublic.
func (c *HTTPRegistryClient) UpdateServiceVersionPublic(ctx context.Context, serviceID uuid.UUID, version string, isPublic bool, apiKey string) error {
	// Dispatch version visibility updates to the service's authoritative Registry in Hybrid mode.
	if c.HasLocalRegistry() {
		target, err := c.clientForServiceMutation(ctx, serviceID)
		if err != nil {
			return err
		}
		return target.UpdateServiceVersionPublic(ctx, serviceID, version, isPublic, apiKey)
	}
	return c.updateVisibilityGraphQL(ctx, "UpdateServiceVersionPublic", `
			mutation UpdateServiceVersionPublic($serviceId: String!, $version: String!, $isPublic: Boolean!) {
				updateServiceVersionPublic(serviceId: $serviceId, version: $version, isPublic: $isPublic)
			}
		`, map[string]interface{}{"serviceId": serviceID.String(), "version": version, "isPublic": isPublic}, "updateServiceVersionPublic", "registry did not update version", apiKey)
}

// PublishServiceVersionExecutionPolicy publishes execution policy scoped to
// just one provider version through the Registry API.
func (c *HTTPRegistryClient) PublishServiceVersionExecutionPolicy(ctx context.Context, serviceID uuid.UUID, version string, policy any, apiKey string) error {
	// Dispatch version policy publication to the service's authoritative Registry in Hybrid mode.
	if c.HasLocalRegistry() {
		target, err := c.clientForServiceMutation(ctx, serviceID)
		if err != nil {
			return err
		}
		return target.PublishServiceVersionExecutionPolicy(ctx, serviceID, version, policy, apiKey)
	}
	return c.putExecutionPolicy(ctx, "PublishServiceVersionExecutionPolicy", "/integrations/"+serviceID.String()+"/versions/"+version+"/execution-policy", policy, apiKey)
}

// PublishConnectionProfile calls the Registry's setConnectionProfile mutation
// to publish an immutable, owner-authorized connection-profile revision that
// becomes visible to every consumer of the service. This mirrors the CLI's
// direct `fused service connection-profile set` client call
// (cli/internal/api/api.go SetConnectionProfile) so the declarative
// workspace.yaml apply path (connection_profiles[*].public: true) reaches the
// same Registry mutation instead of a separate one.
func (c *HTTPRegistryClient) PublishConnectionProfile(ctx context.Context, serviceID, serviceVersionID uuid.UUID, name string, profile connectionprofile.Profile, apiKey string) (*ConnectionProfileRevision, error) {
	// Dispatch public connection-profile mutations to the service's owning Registry in Hybrid mode.
	if c.HasLocalRegistry() {
		return c.publishHybridConnectionProfile(ctx, serviceID, serviceVersionID, name, profile, apiKey)
	}
	data, err := doAPIKeyGraphQL[struct {
		Profile *ConnectionProfileRevision `json:"setConnectionProfile"`
	}](ctx, c, "PublishConnectionProfile", graphqlQuery{
		Query: `
			mutation SetConnectionProfile($serviceId: String!, $serviceVersionId: String!, $name: String!, $config: JSON!) {
				setConnectionProfile(service_id: $serviceId, service_version_id: $serviceVersionId, name: $name, config: $config) {
					profile_id service_id service_version_id name auth_type auth_name revision profile_hash config provenance
				}
			}
		`,
		Variables: map[string]interface{}{
			"serviceId": serviceID.String(), "serviceVersionId": serviceVersionID.String(),
			"name": name, "config": profile,
		},
	}, apiKey, false)
	// Propagate Registry HTTP or GraphQL errors before checking the returned profile.
	if err != nil {
		return nil, err
	}
	// A nil profile indicates Registry accepted the call without publishing a revision.
	if data.Profile == nil {
		return nil, fmt.Errorf("PublishConnectionProfile: registry did not return a profile revision")
	}
	return data.Profile, nil
}

// ResolveServiceIDsBySlugs resolves a workspace/SDK config's service map
// keys to service IDs in one Registry round trip, via the batched
// serviceIdsBySlugs GraphQL query (engine_workspace_registration_plan.md,
// Task 5). Each key may be a bare slug ("stripe", resolved within the
// caller's own account) or a "@provider/slug" composite ("@acme-inc/
// custom-crm", resolved within that provider's account) -- see
// parseSlugProviderKey. The returned map is keyed by the exact original
// input string, so callers (unresolvedWorkspaceServiceSlugs and its SDK
// config sibling) can look results up by the same key without needing to
// know about provider-parsing at all.
//
// Replaces the old per-slug GraphQL-alias trick: that was already one HTTP
// request for N slugs, but the Registry still ran N independent pairs of
// repository queries server-side to answer it, and had no way to carry a
// provider per slug at all. serviceIdsBySlugs answers the whole batch in at
// most two repository queries total (see resolveAccountScopedServices).
// ResolveServiceIDsBySlugs resolves slugs against Local Registry first and queries Cloud only for misses when permitted.
func (c *HTTPRegistryClient) ResolveServiceIDsBySlugs(ctx context.Context, slugs []string, apiKey string) (map[string]uuid.UUID, error) {
	if c.HasLocalRegistry() {
		return c.resolveHybridServiceIDsBySlugs(ctx, slugs, apiKey)
	}
	slugs = uniqueNonEmptyStrings(slugs)
	if len(slugs) == 0 {
		return map[string]uuid.UUID{}, nil
	}
	query, variables := serviceSlugResolutionQuery(slugs)
	reqBody := graphqlQuery{Query: query, Variables: variables}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("ResolveServiceIDsBySlugs: marshal query: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("ResolveServiceIDsBySlugs: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)

	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("ResolveServiceIDsBySlugs: request failed: %w", err)
	}
	defer resp.Body.Close()
	return decodeServiceSlugResolution(resp, slugs)
}

// parseSlugProviderKey splits a workspace/SDK config's service map key into
// its slug and (optional) provider: "@acme-inc/custom-crm" -> ("custom-crm",
// "acme-inc"); a bare "stripe" (no "@" prefix) -> ("stripe", "") -- an empty
// provider means "the caller's own account", exactly matching
// resolveAccountScopedService's own convention on the Registry side. A
// malformed "@..." key with no "/" is treated as a literal slug with no
// provider rather than erroring here -- resolution will simply fail to find
// a matching service, the same outcome as any other unresolvable slug.
func parseSlugProviderKey(key string) (slug, provider string) {
	if !strings.HasPrefix(key, "@") {
		return key, ""
	}
	rest := key[1:]
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		return rest[i+1:], rest[:i]
	}
	return key, ""
}

// serviceSlugResolutionQuery builds the batched serviceIdsBySlugs query: one
// "inputs" entry per slug, in the same order as slugs, so the response list
// (which the Registry resolver returns positionally, not by key) can be
// zipped back with slugs directly in decodeServiceSlugResolution.
func serviceSlugResolutionQuery(slugs []string) (string, map[string]interface{}) {
	inputs := make([]map[string]interface{}, len(slugs))
	for i, key := range slugs {
		slug, provider := parseSlugProviderKey(key)
		inputs[i] = map[string]interface{}{"slug": slug, "provider": provider}
	}
	query := `
		query ResolveServiceSlugs($inputs: [ServiceSlugInput!]!) {
			serviceIdsBySlugs(inputs: $inputs) {
				slug
				provider
				serviceId
			}
		}
	`
	return query, map[string]interface{}{"inputs": inputs}
}

func decodeServiceSlugResolution(resp *http.Response, slugs []string) (map[string]uuid.UUID, error) {
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("ResolveServiceIDsBySlugs: registry returned status %d: %s", resp.StatusCode, string(body))
	}
	var gr struct {
		Data struct {
			ServiceIdsBySlugs []struct {
				Slug      string  `json:"slug"`
				Provider  string  `json:"provider"`
				ServiceID *string `json:"serviceId"`
			} `json:"serviceIdsBySlugs"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &gr); err != nil {
		return nil, fmt.Errorf("ResolveServiceIDsBySlugs: decode response: %w", err)
	}
	if len(gr.Errors) > 0 {
		return nil, fmt.Errorf("ResolveServiceIDsBySlugs: graphql error: %s", gr.Errors[0].Message)
	}
	results := gr.Data.ServiceIdsBySlugs
	if len(results) != len(slugs) {
		return nil, fmt.Errorf("ResolveServiceIDsBySlugs: expected %d results, got %d", len(slugs), len(results))
	}
	out := map[string]uuid.UUID{}
	for i, key := range slugs {
		r := results[i]
		if r.ServiceID == nil {
			continue
		}
		id, err := uuid.Parse(*r.ServiceID)
		if err != nil {
			return nil, fmt.Errorf("ResolveServiceIDsBySlugs: invalid id for %s: %w", key, err)
		}
		out[key] = id
	}
	return out, nil
}

func uniqueNonEmptyStrings(items []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item != "" && !seen[item] {
			out = append(out, item)
			seen[item] = true
		}
	}
	return out
}

type HTTPRegistryClient struct {
	endpoint          string
	licenseKey        string
	installationID    uuid.UUID
	runtimeInstanceID uuid.UUID
	httpClient        *http.Client

	local          *HTTPRegistryClient
	sourceResolver ServiceRegistrySourceResolver
	cloudReadAuth  CloudFallbackAuthorizer
	inlineBuilder  InlineSDKContractBuilder

	// sfGroup collapses concurrent identical in-flight Registry calls into one.
	// FetchServiceMetadata and FetchEndpointsByNames are the hot paths:
	// multiple SDK connections arriving simultaneously for the same service +
	// version all share a single outbound request and its result, eliminating
	// the thundering-herd of duplicate round trips to the Registry at connect.
	sfGroup singleflight.Group
}

var _ SDKGenerationClient = (*HTTPRegistryClient)(nil)

// do sends an ordinary bounded Registry request through the shared licensed transport.
func (c *HTTPRegistryClient) do(request *http.Request) (*http.Response, error) {
	return c.doWithClient(request, c.httpClient)
}

// doWithCallerDeadline lets a finite workflow budget replace the generic
// client timeout while retaining that safety net for unbounded callers.
func (c *HTTPRegistryClient) doWithCallerDeadline(request *http.Request) (*http.Response, error) {
	// A missing caller deadline must keep the shared client's ordinary timeout.
	if _, bounded := request.Context().Deadline(); !bounded {
		return c.do(request)
	}
	client := *c.httpClient
	// Import replay already has an earlier finite context deadline; removing the
	// generic ten-second client cap prevents it from winning unexpectedly.
	client.Timeout = 0
	return c.doWithClient(request, &client)
}

// doWithClient applies the one Engine-to-Registry credential boundary before dispatch.
func (c *HTTPRegistryClient) doWithClient(request *http.Request, client *http.Client) (*http.Response, error) {
	// A missing licensed identity must fail locally before any Registry traffic.
	if c.licenseKey == "" {
		return nil, errors.New("Registry licence identity is unavailable")
	}
	outbound := request.Clone(request.Context())
	outbound.Header = request.Header.Clone()
	// Every Registry call uses the licensed workspace identity. Caller-owned
	// control credentials are local to Engine and must never cross this boundary.
	outbound.Header.Set("Authorization", "Bearer "+c.licenseKey)
	outbound.Header.Set("X-API-Key", c.licenseKey)
	// Installation identity is optional only before the Engine handshake completes.
	if c.installationID != uuid.Nil {
		outbound.Header.Set("X-Fused-Installation-ID", c.installationID.String())
	}
	// Runtime identity follows the same handshake boundary as installation identity.
	if c.runtimeInstanceID != uuid.Nil {
		outbound.Header.Set("X-Fused-Runtime-Instance-ID", c.runtimeInstanceID.String())
	}
	return client.Do(outbound)
}

// ConfigureEngineIdentity binds installation and runtime UUIDs on both Cloud and any configured Local Registry client.
func (c *HTTPRegistryClient) ConfigureEngineIdentity(installationID, runtimeInstanceID uuid.UUID) error {
	if installationID == uuid.Nil || runtimeInstanceID == uuid.Nil {
		return errors.New("Engine installation and runtime identities are required")
	}
	c.installationID = installationID
	c.runtimeInstanceID = runtimeInstanceID
	// Keep the Local Registry client's request identity synchronized with Cloud Registry.
	if c.local != nil {
		_ = c.local.ConfigureEngineIdentity(installationID, runtimeInstanceID)
	}
	return nil
}

func NewHTTPRegistryClient(endpoint, licenseKey string) *HTTPRegistryClient {
	if os.Getenv("FUSED_ENV") != "development" && strings.HasPrefix(strings.ToLower(endpoint), "http://") {
		slog.Error("FATAL: Engine is configured with an insecure http:// Registry URL in production. LicenseKey would be transmitted in plaintext. Set FUSED_ENV=development to override.")
		os.Exit(1)
	}

	// Keep connections alive across Registry calls. Go's DefaultTransport caps
	// MaxIdleConnsPerHost at 2, forcing a new TCP handshake under any real
	// concurrency. 32 slots is enough for a busy Engine without over-allocating
	// file descriptors. ForceAttemptHTTP2 enables HTTP/2 ALPN negotiation over
	// https:// (production) even when constructing a custom Transport.
	transport := &http.Transport{
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}

	return &HTTPRegistryClient{
		endpoint:   endpoint,
		licenseKey: licenseKey,
		httpClient: &http.Client{Timeout: 10 * time.Second, Transport: transport},
	}
}

// Registry's revision endpoint admits at most 50 service/version references per request.
const serviceVersionRevisionBatchSize = 50

// fetchRegistryBatches executes bounded Registry reads in source order,
// stopping immediately on cancellation or error so callers never observe partial snapshots.
func fetchRegistryBatches[In, Out any](ctx context.Context, items []In, batchSize int, fetchBatch func([]In) ([]Out, error)) ([]Out, error) {
	// Skip all Registry requests when the caller passes an empty selection.
	if len(items) == 0 {
		return nil, nil
	}
	out := make([]Out, 0, len(items))
	for start := 0; start < len(items); start += batchSize {
		// Stop before issuing another dependency call if the parent context expired.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+batchSize, len(items))
		batch, err := fetchBatch(items[start:end])
		// Abort all-or-nothing when any batch fails so callers never use incomplete data.
		if err != nil {
			return nil, err
		}
		out = append(out, batch...)
	}
	return out, nil
}

// postRegistryJSON sends a JSON POST request authenticated via X-API-Key and
// decodes an HTTP 200 JSON payload.
func postRegistryJSON[T any](ctx context.Context, c *HTTPRegistryClient, path string, payload any, apiKey string) (T, error) {
	var zero T
	body, err := json.Marshal(payload)
	// Serialization must succeed before any Registry request can be sent.
	if err != nil {
		return zero, err
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.registryBaseURL()+path, bytes.NewReader(body))
	// Invalid endpoint configuration cannot be treated as an empty batch.
	if err != nil {
		return zero, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := c.do(request)
	// Transport failures invalidate the entire resolution, including earlier batches.
	if err != nil {
		return zero, err
	}
	defer response.Body.Close()
	// Keep Registry validation and visibility errors intact for the caller.
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return zero, fmt.Errorf("Registry returned %d: %s", response.StatusCode, string(body))
	}
	var decoded T
	// A truncated response must never become a usable partial snapshot.
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return zero, err
	}
	return decoded, nil
}

// FetchServiceVersionRevisions resolves arbitrarily sized selections through bounded
// requests, preserving Registry rows and returning no partial result on failure.
func (c *HTTPRegistryClient) FetchServiceVersionRevisions(ctx context.Context, refs []ServiceVersionRef, apiKey string) ([]ServiceVersionRevision, error) {
	if c.HasLocalRegistry() {
		return c.fetchHybridServiceVersionRevisions(ctx, refs, apiKey)
	}
	return fetchRegistryBatches(ctx, refs, serviceVersionRevisionBatchSize, func(chunk []ServiceVersionRef) ([]ServiceVersionRevision, error) {
		batch, err := c.fetchServiceVersionRevisionBatch(ctx, chunk, apiKey)
		// Earlier successful reads are unusable when any later dependency fails.
		if err != nil {
			return nil, err
		}
		// Validate returned identities before combining rows; absence remains visible
		// to callers, which already reject unresolved versions with domain-specific errors.
		if err := validateServiceVersionRevisionBatch(chunk, batch); err != nil {
			return nil, err
		}
		return batch, nil
	})
}

// validateServiceVersionRevisionBatch permits Registry visibility omissions but rejects
// foreign, excess, or unpinned rows that could otherwise corrupt a planning lookup map.
func validateServiceVersionRevisionBatch(refs []ServiceVersionRef, revisions []ServiceVersionRevision) error {
	remaining := make(map[ServiceVersionRef]int, len(refs))
	for _, ref := range refs {
		remaining[ref]++
	}
	for _, revision := range revisions {
		ref := ServiceVersionRef{ServiceID: revision.ServiceID, Version: revision.Version}
		// Repeated inputs are legitimate; extra response rows are not.
		if remaining[ref] == 0 || revision.ServiceVersionID == uuid.Nil {
			return errors.New("Registry returned an unexpected service version revision identity")
		}
		remaining[ref]--
	}
	return nil
}

// fetchServiceVersionRevisionBatch closes each bounded response before the next request begins.
func (c *HTTPRegistryClient) fetchServiceVersionRevisionBatch(ctx context.Context, refs []ServiceVersionRef, apiKey string) ([]ServiceVersionRevision, error) {
	decoded, err := postRegistryJSON[serviceVersionRevisionBatchResponse](ctx, c, "/integrations/versions/revisions", map[string][]ServiceVersionRef{"versions": refs}, apiKey)
	// Propagate request or decoding failures without returning partial versions.
	if err != nil {
		return nil, err
	}
	return decoded.Versions, nil
}

// Registry bounds service-version GraphQL reads so one large workspace cannot monopolize a resolver.
const registryServiceVersionBatchSize = 100

// FetchServiceVersionAuthConfigs resolves arbitrarily large workspaces through bounded Registry requests.
func (c *HTTPRegistryClient) FetchServiceVersionAuthConfigs(ctx context.Context, refs []ServiceVersionRef, apiKey string) ([]ServiceVersionAuthConfigs, error) {
	// Partition auth config queries across Local and Cloud Registries in Hybrid mode.
	if c.HasLocalRegistry() {
		return c.fetchHybridServiceVersionAuthConfigs(ctx, refs, apiKey)
	}
	return fetchRegistryBatches(ctx, refs, registryServiceVersionBatchSize, func(chunk []ServiceVersionRef) ([]ServiceVersionAuthConfigs, error) {
		return c.fetchServiceVersionAuthConfigBatch(ctx, chunk, apiKey)
	})
}

// fetchServiceVersionAuthConfigBatch performs one request within Registry's documented resolver bound.
func (c *HTTPRegistryClient) fetchServiceVersionAuthConfigBatch(ctx context.Context, refs []ServiceVersionRef, apiKey string) ([]ServiceVersionAuthConfigs, error) {
	data, err := doAPIKeyGraphQL[struct {
		Versions []ServiceVersionAuthConfigs `json:"serviceVersionAuthConfigs"`
	}](ctx, c, "FetchServiceVersionAuthConfigs", graphqlQuery{
		Query: `query ServiceVersionAuthConfigs($refs: [ServiceVersionRefInput!]!) {
			serviceVersionAuthConfigs(refs: $refs) {
				service_id version service_version_id
				auth_configs {` + registryAuthConfigGraphQLFields + `}
			}
		}`,
		Variables: map[string]interface{}{"refs": refs},
	}, apiKey, true)
	// Auth options must be all-or-nothing so the frontend never shows an incomplete credential contract.
	if err != nil {
		return nil, err
	}
	return data.Versions, nil
}

type ServiceVersionResolvedRef struct {
	ServiceID        uuid.UUID `json:"service_id"`
	Version          string    `json:"version"`
	ServiceVersionID uuid.UUID `json:"service_version_id"`
}

type serviceVersionResolveBatchResponse struct {
	Resolved []ServiceVersionResolvedRef `json:"resolved"`
}

// FetchServiceVersionIDs resolves pinned service/version pairs to Registry service_version_ids.
func (c *HTTPRegistryClient) FetchServiceVersionIDs(ctx context.Context, refs []ServiceVersionRef, apiKey string) ([]ServiceVersionResolvedRef, error) {
	// Skip the Registry round trip when no references were supplied.
	if len(refs) == 0 {
		return nil, nil
	}
	// Partition version ID resolution across Local and Cloud Registries in Hybrid mode.
	if c.HasLocalRegistry() {
		return c.fetchHybridServiceVersionIDs(ctx, refs, apiKey)
	}
	decoded, err := postRegistryJSON[serviceVersionResolveBatchResponse](ctx, c, "/integrations/versions/resolve", map[string][]ServiceVersionRef{"versions": refs}, apiKey)
	// Propagate request or decoding errors directly to callers.
	if err != nil {
		return nil, err
	}
	return decoded.Resolved, nil
}

// FetchLatestServiceVersions resolves "version omitted" workspace/SDK config
// entries in one Registry GraphQL request. The Registry picks the latest
// visible public provider version; Engine only attaches the exact version
// identity it receives, avoiding one service(id:) call per service.
func (c *HTTPRegistryClient) FetchLatestServiceVersions(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) ([]ServiceVersionResolvedRef, error) {
	// Skip the Registry round trip when no service IDs need latest-version resolution.
	if len(serviceIDs) == 0 {
		return nil, nil
	}
	// Resolve latest versions Local-first with Cloud fallback in Hybrid mode.
	if c.HasLocalRegistry() {
		return c.fetchHybridLatestServiceVersions(ctx, serviceIDs, apiKey)
	}
	data, err := doAPIKeyGraphQL[struct {
		LatestServiceVersions []ServiceVersionResolvedRef `json:"latestServiceVersions"`
	}](ctx, c, "FetchLatestServiceVersions", graphqlQuery{
		Query: `
			query LatestServiceVersions($serviceIds: [String!]!) {
				latestServiceVersions(serviceIds: $serviceIds) {
					service_id
					version
					service_version_id
				}
			}
		`,
		Variables: map[string]interface{}{"serviceIds": uuidStrings(serviceIDs)},
	}, apiKey, true)
	// Propagate transport or GraphQL errors directly to planning callers.
	if err != nil {
		return nil, err
	}
	return data.LatestServiceVersions, nil
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

// DeprecateServiceVersion marks a single named version of a service as
// deprecated in the Registry. The Registry endpoint is idempotent — calling it
// on an already-deprecated version returns 204 with no mutation.
func (c *HTTPRegistryClient) DeprecateServiceVersion(ctx context.Context, serviceID uuid.UUID, version, apiKey string) error {
	// Route version deprecation to the service's authoritative Registry in Hybrid mode.
	if c.HasLocalRegistry() {
		target, err := c.clientForServiceMutation(ctx, serviceID)
		if err != nil {
			return err
		}
		return target.DeprecateServiceVersion(ctx, serviceID, version, apiKey)
	}
	url := c.registryBaseURL() + "/integrations/" + serviceID.String() + "/versions/" + version + "/deprecate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, nil)
	if err != nil {
		return fmt.Errorf("DeprecateServiceVersion: create request: %w", err)
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("DeprecateServiceVersion: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("DeprecateServiceVersion: registry returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// ArchiveService soft-deletes the service in the Registry. This is only
// meaningful when the calling workspace owns the service; the Registry enforces
// ownership on the DELETE endpoint and returns 403 for non-owners.
func (c *HTTPRegistryClient) ArchiveService(ctx context.Context, serviceID uuid.UUID, apiKey string) error {
	// Route service archiving to the service's authoritative Registry in Hybrid mode.
	if c.HasLocalRegistry() {
		target, err := c.clientForServiceMutation(ctx, serviceID)
		if err != nil {
			return err
		}
		return target.ArchiveService(ctx, serviceID, apiKey)
	}
	url := c.registryBaseURL() + "/integrations/" + serviceID.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("ArchiveService: create request: %w", err)
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("ArchiveService: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("ArchiveService: registry returned %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *HTTPRegistryClient) registryBaseURL() string {
	// Registry clients share one configured GraphQL URL, while lifecycle and
	// policy publishers call REST routes on the same origin.
	return strings.TrimSuffix(strings.TrimRight(c.endpoint, "/"), "/graphql")
}

type graphqlQuery struct {
	Query     string                 `json:"query"`
	Variables map[string]interface{} `json:"variables"`
}

type graphqlResponse struct {
	Data   map[string]json.RawMessage `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type ServiceMetadataRef struct {
	ServiceID uuid.UUID
	Version   string
}

func ServiceMetadataRefKey(ref ServiceMetadataRef) string {
	return ref.ServiceID.String() + ":" + ref.Version
}

// FetchServiceMetadataBatch resolves arbitrarily large webhook selections through bounded Registry requests.
func (c *HTTPRegistryClient) FetchServiceMetadataBatch(ctx context.Context, refs []ServiceMetadataRef) (map[string]*fusedobject.ServiceMetadata, error) {
	// Empty selections retain the historical non-nil map contract without a dependency call.
	if len(refs) == 0 {
		return map[string]*fusedobject.ServiceMetadata{}, nil
	}
	// Partition webhook metadata batches across Local and Cloud Registries in Hybrid mode.
	if c.HasLocalRegistry() {
		return c.fetchHybridServiceMetadataBatch(ctx, refs)
	}
	metadata := make(map[string]*fusedobject.ServiceMetadata, len(refs))
	// Preserve source partitions while keeping each set-based query within Registry's shared bound.
	for start := 0; start < len(refs); start += registryServiceVersionBatchSize {
		// Cancellation stops the next request and prevents an incomplete metadata map from escaping.
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		end := min(start+registryServiceVersionBatchSize, len(refs))
		batch, err := c.fetchServiceMetadataBatch(ctx, refs[start:end])
		// Webhook planning must never proceed with only the earlier successful partitions.
		if err != nil {
			return nil, err
		}
		// Exact identity keys make partition merging deterministic and preserve duplicate-input behavior.
		for key, item := range batch {
			metadata[key] = item
		}
	}
	return metadata, nil
}

// fetchServiceMetadataBatch performs one set-based request within Registry's service-version bound.
func (c *HTTPRegistryClient) fetchServiceMetadataBatch(ctx context.Context, refs []ServiceMetadataRef) (map[string]*fusedobject.ServiceMetadata, error) {
	req, err := c.buildServiceMetadataBatchRequest(ctx, refs)
	// Request construction failure invalidates this partition before transport begins.
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	// Transport failure invalidates the complete caller-owned aggregate.
	if err != nil {
		return nil, fmt.Errorf("FetchServiceMetadataBatch: execute: %w", err)
	}
	defer resp.Body.Close()
	// Registry validation and authorization details remain available to the planning caller.
	if resp.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("FetchServiceMetadataBatch: registry returned %d: %s", resp.StatusCode, body)
	}
	return decodeServiceMetadataBatch(resp.Body, refs)
}

// buildServiceMetadataBatchRequest retains signed-challenge and freshness semantics when loading ingress metadata.
func (c *HTTPRegistryClient) buildServiceMetadataBatchRequest(ctx context.Context, refs []ServiceMetadataRef) (*http.Request, error) {
	versionRefs := make([]ServiceVersionRef, 0, len(refs))
	for _, ref := range refs {
		versionRefs = append(versionRefs, ServiceVersionRef{ServiceID: ref.ServiceID, Version: ref.Version})
	}
	payload, err := json.Marshal(graphqlQuery{Query: `query WebhookMetadata($refs: [ServiceVersionRefInput!]!) {
		serviceWebhookMetadata(refs: $refs) {
			service_id version service_version_id name event_extraction_path
			incoming_webhook_config { auth_type auth_location auth_key_name signature_header verification_headers signature_policy {` + registrySignaturePolicyGraphQLFields + `} }
		}
	}`, Variables: map[string]interface{}{"refs": versionRefs}})
	if err != nil {
		return nil, fmt.Errorf("FetchServiceMetadataBatch: marshal: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("FetchServiceMetadataBatch: request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.licenseKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.licenseKey)
		req.Header.Set("X-API-Key", c.licenseKey)
	}
	return req, nil
}

func decodeServiceMetadataBatch(body io.Reader, refs []ServiceMetadataRef) (map[string]*fusedobject.ServiceMetadata, error) {
	var response struct {
		Data struct {
			Metadata []struct {
				ServiceID             uuid.UUID                          `json:"service_id"`
				Version               string                             `json:"version"`
				ServiceVersionID      uuid.UUID                          `json:"service_version_id"`
				Name                  string                             `json:"name"`
				EventExtractionPath   string                             `json:"event_extraction_path"`
				IncomingWebhookConfig *fusedobject.IncomingWebhookConfig `json:"incoming_webhook_config"`
				Documentation         *fusedobject.ServiceDocumentation  `json:"documentation"`
			} `json:"serviceWebhookMetadata"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(body).Decode(&response); err != nil {
		return nil, fmt.Errorf("FetchServiceMetadataBatch: decode: %w", err)
	}
	if len(response.Errors) > 0 {
		return nil, fmt.Errorf("FetchServiceMetadataBatch: graphql: %s", response.Errors[0].Message)
	}
	result := make(map[string]*fusedobject.ServiceMetadata, len(refs))
	for _, item := range response.Data.Metadata {
		result[ServiceMetadataRefKey(ServiceMetadataRef{ServiceID: item.ServiceID, Version: item.Version})] = &fusedobject.ServiceMetadata{
			ID: item.ServiceID, ServiceVersionID: item.ServiceVersionID, Name: item.Name,
			EventExtractionPath: item.EventExtractionPath, IncomingWebhookConfig: item.IncomingWebhookConfig,
		}
	}
	for _, ref := range refs {
		if result[ServiceMetadataRefKey(ref)] == nil {
			return nil, fmt.Errorf("FetchServiceMetadataBatch: service %s not found", ref.ServiceID)
		}
	}
	return result, nil
}

// FetchServiceMetadata loads runtime service metadata from the authoritative Registry.
func (c *HTTPRegistryClient) FetchServiceMetadata(ctx context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, error) {
	// Route Hybrid runtime metadata reads to the pinned Registry or Local-first fallback.
	if c.HasLocalRegistry() {
		return c.fetchHybridServiceMetadata(ctx, serviceID, version)
	}
	// Collapse concurrent identical fetches: multiple SDK connections arriving
	// at the same time for the same (serviceID, version) share one outbound
	// request rather than each firing their own. The singleflight result is a
	// pointer; callers must treat it as read-only (not mutate the returned object).
	sfKey := serviceID.String() + ":" + version
	v, err, _ := c.sfGroup.Do(sfKey, func() (interface{}, error) {
		return c.fetchServiceMetadata(ctx, serviceID, version)
	})
	if err != nil {
		return nil, err
	}
	return v.(*fusedobject.ServiceMetadata), nil
}

func (c *HTTPRegistryClient) fetchServiceMetadata(ctx context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, error) {
	req, err := c.buildServiceMetadataRequest(ctx, serviceID.String(), version)
	if err != nil {
		return nil, err
	}

	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry returned status %d: %s", resp.StatusCode, string(body))
	}

	var gr struct {
		Data struct {
			Service *struct {
				ID                    uuid.UUID                          `json:"id"`
				CurrentServiceVersion string                             `json:"current_service_version"`
				ServiceVersions       []registryServiceVersionEnvelope   `json:"service_versions"`
				Name                  string                             `json:"name"`
				Description           string                             `json:"description"`
				BaseURL               string                             `json:"base_url"`
				Servers               fusedobject.Servers                `json:"servers"`
				AuthConfigs           fusedobject.AuthConfigs            `json:"auth_configs"`
				RateLimit             *fusedobject.RateLimitConfig       `json:"rate_limit"`
				RetryConfig           *fusedobject.RetryConfig           `json:"retry_config"`
				TimeoutMs             *int                               `json:"timeout_ms"`
				Pagination            *fusedobject.PaginationConfig      `json:"pagination"`
				DefaultHeaders        fusedobject.DefaultHeaders         `json:"default_headers"`
				ConnectConfig         *fusedobject.ServiceConnectConfig  `json:"connect_config"`
				EventExtractionPath   string                             `json:"event_extraction_path"`
				IncomingWebhookConfig *fusedobject.IncomingWebhookConfig `json:"incoming_webhook_config"`
				Documentation         *fusedobject.ServiceDocumentation  `json:"documentation"`
			} `json:"service"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if len(gr.Errors) > 0 {
		return nil, fmt.Errorf("graphql error: %s", gr.Errors[0].Message)
	}

	if gr.Data.Service == nil {
		return nil, fmt.Errorf("service not found for id %s", serviceID.String())
	}

	srv := gr.Data.Service
	serviceVersionID, envelope := currentServiceVersionContract(srv.CurrentServiceVersion, srv.ServiceVersions)
	if err := fusedobject.ValidateExecutionContractEnvelope(envelope); err != nil {
		return nil, fmt.Errorf("FetchServiceMetadata: incompatible contract: %w", err)
	}

	fo := &fusedobject.ServiceMetadata{
		ExecutionContractEnvelope: envelope,
		ID:                        srv.ID,
		ServiceVersionID:          serviceVersionID,
		Name:                      srv.Name,
		Description:               srv.Description,
		BaseURL:                   srv.BaseURL,
		Servers:                   srv.Servers,
		AuthConfigs:               srv.AuthConfigs,
		RateLimit:                 srv.RateLimit,
		RetryConfig:               srv.RetryConfig,
		TimeoutMs:                 srv.TimeoutMs,
		Pagination:                srv.Pagination,
		DefaultHeaders:            srv.DefaultHeaders,
		ConnectConfig:             srv.ConnectConfig,
		EventExtractionPath:       srv.EventExtractionPath,
		IncomingWebhookConfig:     srv.IncomingWebhookConfig,
		Documentation:             srv.Documentation,
	}

	slog.InfoContext(ctx, "Successfully fetched ServiceMetadata from Registry", slog.String("serviceID", serviceID.String()))
	return fo, nil
}

func (c *HTTPRegistryClient) FetchEndpointByName(ctx context.Context, serviceID uuid.UUID, version string, endpointName string) (*fusedobject.Endpoint, error) {
	serviceVersionID, err := uuid.Parse(version)
	if err != nil {
		return nil, fmt.Errorf("invalid service version id %q: %w", version, err)
	}

	endpoints, err := c.FetchEndpointsByNames(ctx, serviceID, serviceVersionID, []string{endpointName})
	if err != nil {
		return nil, err
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("endpoint %s not found for service %s", endpointName, serviceID.String())
	}
	return &endpoints[0], nil
}

// FetchEndpointsByNames resolves named operations from the authoritative Registry.
func (c *HTTPRegistryClient) FetchEndpointsByNames(ctx context.Context, serviceID uuid.UUID, serviceVersionID uuid.UUID, endpointNames []string) ([]fusedobject.Endpoint, error) {
	if len(endpointNames) == 0 {
		return nil, nil
	}
	// Route Hybrid endpoint lookups to the pinned Registry or Local-first fallback.
	if c.HasLocalRegistry() {
		return c.fetchHybridEndpointsByNames(ctx, serviceID, serviceVersionID, endpointNames)
	}
	// Collapse concurrent batch-fetches for the same (service, version, names)
	// set. Sort names so that two callers requesting {"getUser","listUsers"} and
	// {"listUsers","getUser"} share one outbound request.
	sorted := make([]string, len(endpointNames))
	copy(sorted, endpointNames)
	sort.Strings(sorted)
	sfKey := serviceID.String() + ":" + serviceVersionID.String() + ":" + strings.Join(sorted, ",")
	v, err, _ := c.sfGroup.Do(sfKey, func() (interface{}, error) {
		return c.fetchEndpointsByNames(ctx, serviceID, serviceVersionID, endpointNames)
	})
	if err != nil {
		return nil, err
	}
	return v.([]fusedobject.Endpoint), nil
}

// fetchEndpointsByNames loads a subset of service endpoints by name from Registry.
func (c *HTTPRegistryClient) fetchEndpointsByNames(ctx context.Context, serviceID uuid.UUID, serviceVersionID uuid.UUID, endpointNames []string) ([]fusedobject.Endpoint, error) {
	req, err := c.buildEndpointsByNamesRequest(ctx, serviceID.String(), serviceVersionID, endpointNames)
	// Abort if request construction fails before issuing a Registry call.
	if err != nil {
		return nil, err
	}

	resp, err := c.do(req)
	// Preserve transport error wrapping expected by Registry client callers.
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	data, err := decodeLicensedGraphQLResponse[struct {
		EndpointsByNames []fusedobject.Endpoint `json:"endpointsByNames"`
	}](resp)
	// Propagate HTTP, decode, or GraphQL errors without returning partial endpoint sets.
	if err != nil {
		return nil, err
	}
	return data.EndpointsByNames, nil
}

// FetchServiceOperations returns every endpoint on serviceID/serviceVersionID,
// including the schema fields used by connection-profile validation. MCP
// discovery reads the same fields from Engine snapshots instead.
func (c *HTTPRegistryClient) FetchServiceOperations(ctx context.Context, serviceID uuid.UUID, serviceVersionID uuid.UUID) ([]fusedobject.Endpoint, error) {
	// Route Hybrid operation lookups to the pinned Registry or Local-first fallback.
	if c.HasLocalRegistry() {
		return c.fetchHybridServiceOperations(ctx, serviceID, serviceVersionID)
	}
	sfKey := "serviceOperations:" + serviceID.String() + ":" + serviceVersionID.String()
	v, err, _ := c.sfGroup.Do(sfKey, func() (interface{}, error) {
		return c.fetchServiceOperations(ctx, serviceID, serviceVersionID)
	})
	if err != nil {
		return nil, err
	}
	return v.([]fusedobject.Endpoint), nil
}

// fetchServiceOperations fetches all operations for one service version from Registry.
func (c *HTTPRegistryClient) fetchServiceOperations(ctx context.Context, serviceID uuid.UUID, serviceVersionID uuid.UUID) ([]fusedobject.Endpoint, error) {
	query := `
		query GetServiceOperations($serviceId: String!, $version: String!) {
			serviceOperations(serviceId: $serviceId, version: $version) {
				id
				stable_key
				name
				description
				method
				path
				normalized_path
				deprecated
				parameters {
					name
					in
					required
					type
					description
					path_encoding
				}
				request_content
				responses
				graphql_query
				provider_protocol
				operation_kind
				pagination {` + runtimePaginationFields + `}
				security_requirements {` + runtimeSecurityRequirementFields + `}
				documentation
			}
		}
	`
	reqBody := graphqlQuery{
		Query: query,
		Variables: map[string]interface{}{
			"serviceId": serviceID.String(),
			// serviceOperations' "version" accepts the UUID, preserving the exact
			// version required by connection-profile validation.
			"version": serviceVersionID.String(),
		},
	}
	req, err := c.newGraphQLRequest(ctx, reqBody)
	// Abort if GraphQL request construction fails.
	if err != nil {
		return nil, err
	}

	resp, err := c.do(req)
	// Preserve transport error wrapping for singleflight callers.
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	data, err := decodeLicensedGraphQLResponse[struct {
		ServiceOperations []fusedobject.Endpoint `json:"serviceOperations"`
	}](resp)
	// Propagate HTTP, decode, or GraphQL errors without caching partial operations.
	if err != nil {
		return nil, err
	}
	return data.ServiceOperations, nil
}

func (c *HTTPRegistryClient) Handshake(ctx context.Context) (string, string, error) {
	result, err := c.HandshakeWithEntitlements(ctx)
	if err != nil {
		return "", "", err
	}
	return result.AccountID, result.WorkspaceName, nil
}

func (c *HTTPRegistryClient) HandshakeWithEntitlements(ctx context.Context) (EngineHandshakeResult, error) {
	if c.licenseKey == "" {
		return EngineHandshakeResult{}, fmt.Errorf("FUSED_LICENSE_KEY is required but was not provided")
	}

	handshakeURL := c.registryBaseURL() + "/api/engine/handshake"

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, handshakeURL, nil)
	if err != nil {
		return EngineHandshakeResult{}, fmt.Errorf("failed to create handshake request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.licenseKey)

	resp, err := c.do(req)
	if err != nil {
		return EngineHandshakeResult{}, fmt.Errorf("handshake request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return EngineHandshakeResult{}, fmt.Errorf("handshake failed with status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		AccountID            string                    `json:"account_id"`
		EngineID             string                    `json:"engine_id"`
		WorkspaceName        string                    `json:"workspace_name"`
		OwnerEmail           string                    `json:"owner_email"`
		Entitlements         *rawRuntimeEntitlement    `json:"entitlements"`
		Identity             ManagedIdentityCapability `json:"identity"`
		ManagedAuthBrokerURL string                    `json:"managed_auth_broker_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return EngineHandshakeResult{}, fmt.Errorf("failed to decode handshake response: %w", err)
	}

	return EngineHandshakeResult{
		AccountID:            result.AccountID,
		EngineID:             result.EngineID,
		WorkspaceName:        result.WorkspaceName,
		OwnerEmail:           result.OwnerEmail,
		Entitlements:         RuntimeEntitlementFromHandshake(result.Entitlements),
		Identity:             result.Identity,
		ManagedAuthBrokerURL: result.ManagedAuthBrokerURL,
	}, nil
}

func (c *HTTPRegistryClient) CreateManagedLoginTransaction(ctx context.Context, verifier, enrollmentRef string) (ManagedLoginTransaction, error) {
	request := struct {
		Purpose        string `json:"purpose"`
		EngineVerifier string `json:"engine_verifier"`
		EnrollmentRef  string `json:"enrollment_ref"`
	}{Purpose: "browser_login", EngineVerifier: verifier, EnrollmentRef: enrollmentRef}
	var transaction ManagedLoginTransaction
	err := c.postManagedIdentityJSON(ctx, "/api/engine/identity/transactions", request, &transaction)
	return transaction, err
}

func (c *HTTPRegistryClient) ExchangeManagedLoginTransaction(ctx context.Context, id uuid.UUID, verifier string) (ManagedIdentityAssertion, error) {
	request := struct {
		EngineVerifier string `json:"engine_verifier"`
	}{EngineVerifier: verifier}
	var assertion ManagedIdentityAssertion
	err := c.postManagedIdentityJSON(ctx, "/api/engine/identity/transactions/"+id.String()+"/exchange", request, &assertion)
	return assertion, err
}

// MintManagedAuthEnrollmentTicket proves this Engine's own license identity
// to Registry (via the standard c.do license attachment) and asks for one
// single-use ticket to redeem at the managed-auth broker. The ticket is
// consumed on its very next hop, so only the raw value is meaningful here.
func (c *HTTPRegistryClient) MintManagedAuthEnrollmentTicket(ctx context.Context) (string, error) {
	var result struct {
		Ticket string `json:"ticket"`
	}
	err := c.postManagedIdentityJSON(ctx, "/api/engine/managed-auth/enrollment-ticket", struct{}{}, &result)
	return result.Ticket, err
}

func (c *HTTPRegistryClient) StartManagedLogout(ctx context.Context, logoutToken, returnURL string) (string, error) {
	request := struct {
		LogoutToken string `json:"logout_token"`
		ReturnURL   string `json:"return_url"`
	}{LogoutToken: logoutToken, ReturnURL: returnURL}
	var result ManagedLogoutResult
	if err := c.postManagedIdentityJSON(ctx, "/api/engine/identity/logout", request, &result); err != nil {
		return "", err
	}
	if !validManagedLogoutURL(result.LogoutURL) {
		return "", errors.New("managed identity Registry logout response was invalid")
	}
	return result.LogoutURL, nil
}

func validManagedLogoutURL(raw string) bool {
	parsed, err := url.Parse(raw)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil && parsed.Fragment == ""
}

func IsManagedLoginPending(err error) bool {
	var registryError ManagedIdentityRegistryError
	return errors.As(err, &registryError) && registryError.Status == http.StatusNotFound && registryError.Code == "transaction_unavailable"
}

func (c *HTTPRegistryClient) postManagedIdentityJSON(ctx context.Context, path string, requestBody, responseBody any) error {
	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("managed identity Registry request: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.registryBaseURL()+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("managed identity Registry request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.do(request)
	if err != nil {
		return fmt.Errorf("managed identity Registry request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return decodeManagedIdentityRegistryError(response)
	}
	if responseBody == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 16<<10))
		return nil
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 16<<10)).Decode(responseBody); err != nil {
		return errors.New("managed identity Registry response was invalid")
	}
	return nil
}

// decodeManagedIdentityRegistryError retains only bounded automation and
// correlation fields from the shared Registry error envelope.
func decodeManagedIdentityRegistryError(response *http.Response) error {
	var payload struct {
		Error struct {
			Code      string `json:"code"`
			RequestID string `json:"request_id"`
			TraceID   string `json:"trace_id"`
		} `json:"error"`
	}
	_ = json.NewDecoder(io.LimitReader(response.Body, 4<<10)).Decode(&payload)
	return ManagedIdentityRegistryError{
		Status: response.StatusCode, Code: safeManagedIdentityRegistryCode(payload.Error.Code),
		RequestID: safeManagedIdentityRegistryCorrelationID(payload.Error.RequestID),
		TraceID:   safeManagedIdentityRegistryTraceID(payload.Error.TraceID),
	}
}

// safeManagedIdentityRegistryCode admits the Registry's stable lower-case
// classifier grammar while rejecting arbitrary error prose.
func safeManagedIdentityRegistryCode(value string) string {
	value = strings.TrimSpace(value)
	// Registry codes are deliberately short enough for logs and control decisions.
	if value == "" || len(value) > 96 {
		return ""
	}
	for _, char := range value {
		// Prose punctuation or upper-case text is not a trusted automation code.
		if (char < 'a' || char > 'z') && (char < '0' || char > '9') && char != '_' {
			return ""
		}
	}
	return value
}

// safeManagedIdentityRegistryCorrelationID mirrors the Registry's bounded
// request-correlation grammar before retaining a remote identifier.
func safeManagedIdentityRegistryCorrelationID(value string) string {
	value = strings.TrimSpace(value)
	// Empty, oversized, or delimiter-bearing values must not reach Engine logs.
	if value == "" || len(value) > 128 {
		return ""
	}
	for _, char := range value {
		// Registry request IDs permit only letters, digits, and compact path punctuation.
		if !validManagedIdentityRegistryCorrelationRune(char) {
			return ""
		}
	}
	return value
}

// validManagedIdentityRegistryCorrelationRune centralizes the ASCII allowlist
// so correlation values cannot acquire Unicode aliases or prose punctuation.
func validManagedIdentityRegistryCorrelationRune(char rune) bool {
	// ASCII letters and digits are safe across Registry and Engine log transports.
	if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
		return true
	}
	return strings.ContainsRune("._:/-", char)
}

// safeManagedIdentityRegistryTraceID accepts only a canonical lower-case
// OpenTelemetry trace identifier from the Registry envelope.
func safeManagedIdentityRegistryTraceID(value string) string {
	value = strings.TrimSpace(value)
	// A canonical trace identifier is exactly 16 bytes rendered as lower-case hex.
	if len(value) != 32 {
		return ""
	}
	for _, char := range value {
		// Upper-case or non-hex content cannot identify the Registry request span.
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return ""
		}
	}
	return value
}

func (c *HTTPRegistryClient) SendHeartbeat(ctx context.Context, engineVersion, engineBuildHash, appliedPlan, appliedEntitlementRevision string, reportedAt time.Time) (*EngineHeartbeatResponse, error) {
	if c.licenseKey == "" {
		return nil, fmt.Errorf("FUSED_LICENSE_KEY is required but was not provided")
	}

	body, err := json.Marshal(EngineHeartbeatRequest{
		EngineVersion:              engineVersion,
		EngineBuildHash:            engineBuildHash,
		AppliedPlan:                appliedPlan,
		AppliedEntitlementRevision: appliedEntitlementRevision,
		ReportedAt:                 reportedAt,
	})
	if err != nil {
		return nil, fmt.Errorf("heartbeat: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.registryBaseURL()+"/api/engine/heartbeat", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("heartbeat: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.licenseKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Engine-Signature", c.signRegistryPayload(body))

	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("heartbeat: request failed: %w", err)
	}
	defer resp.Body.Close()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("heartbeat: read response body: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("heartbeat failed with status %d: %s", resp.StatusCode, string(respBody))
	}

	var result EngineHeartbeatResponse
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("heartbeat: unmarshal response: %w", err)
	}
	return &result, nil
}

func (c *HTTPRegistryClient) SendUsageReports(ctx context.Context, engineVersion, engineBuildHash string, reports []models.EngineUsageReport, reportedAt time.Time) error {
	if c.licenseKey == "" {
		return fmt.Errorf("FUSED_LICENSE_KEY is required but was not provided")
	}
	if len(reports) == 0 {
		return nil
	}
	return c.postSignedEngineJSON(ctx, "/api/engine/usage-reports", EngineUsageReportRequest{
		EngineVersion:   engineVersion,
		EngineBuildHash: engineBuildHash,
		ReportedAt:      reportedAt,
		Reports:         reports,
	}, nil)
}

// RenewSDKPackageLeases renews SDK package retention on Local Registry when configured and Cloud Registry for legacy packages.
func (c *HTTPRegistryClient) RenewSDKPackageLeases(ctx context.Context, apps []models.SDKPackageLeaseRenewal) (int64, error) {
	if len(apps) == 0 {
		return 0, nil
	}
	if c.HasLocalRegistry() {
		return c.renewHybridSDKPackageLeases(ctx, apps)
	}
	var response sdkPackageLeaseResponse
	err := c.postSignedEngineJSON(ctx, "/sdk-packages/leases/renew", sdkPackageLeaseRequest{Apps: apps}, &response)
	if err != nil {
		return 0, err
	}
	return response.Renewed, nil
}

const maxSDKGenerationResponseBytes = 16 << 20

// GenerateSDK routes SDK compilation to Local Registry in Hybrid mode unless every selected service is Cloud-owned.
func (c *HTTPRegistryClient) GenerateSDK(ctx context.Context, generation models.SDKGenerationRequest) (models.SDKGenerationResult, error) {
	if c.HasLocalRegistry() {
		return c.generateHybridSDK(ctx, generation)
	}
	var result models.SDKGenerationResult
	body, err := json.Marshal(generation)
	// The immutable request must be encoded before any Registry mutation is attempted.
	if err != nil {
		return result, fmt.Errorf("SDK generation request is invalid: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.registryBaseURL()+"/sdks/generate", bytes.NewReader(body))
	// A malformed configured Registry URL must fail locally without changing generation state.
	if err != nil {
		return result, fmt.Errorf("SDK generation request could not be created: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.doWithCallerDeadline(request)
	// Transport ambiguity leaves the Engine's durable job pending for an identical later replay.
	if err != nil {
		return result, fmt.Errorf("SDK generation request failed: %w", err)
	}
	defer response.Body.Close()
	// Registry rejection is reported without reading provider-authored or generated content into the error.
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return result, classifySDKGenerationHTTPError(response.StatusCode, response.Body)
	}
	decoded, err := readBoundedSDKGenerationResponse(response.Body)
	// An oversized or malformed response cannot become Engine runtime authority.
	if err != nil {
		return result, err
	}
	// Decode only after the complete bounded body has been admitted.
	if err := json.Unmarshal(decoded, &result); err != nil {
		return models.SDKGenerationResult{}, errors.New("Registry SDK generation response was invalid")
	}
	return result, nil
}

// classifySDKGenerationHTTPError extracts only the allowlisted generator-unavailable conflict code without exposing raw Registry prose.
func classifySDKGenerationHTTPError(statusCode int, body io.Reader) error {
	payload, _ := io.ReadAll(io.LimitReader(body, 16<<10))
	// Only HTTP 409 carrying the canonical nested Registry error envelope may classify as a retired generator version.
	if statusCode == http.StatusConflict && RegistrySDKGenerationErrorCode(payload) == models.ErrSDKGeneratorVersionUnavailable.Error() {
		return fmt.Errorf("Registry SDK generation returned status %d: %w", statusCode, models.ErrSDKGeneratorVersionUnavailable)
	}
	return fmt.Errorf("Registry SDK generation returned status %d", statusCode)
}

// RegistrySDKGenerationErrorCode decodes only the reviewed nested {"error":{"code":...}} Registry control-plane envelope.
func RegistrySDKGenerationErrorCode(payload []byte) string {
	var envelope struct {
		Error json.RawMessage `json:"error"`
	}
	// Malformed or empty bodies cannot supply an allowlisted Registry error classification.
	if json.Unmarshal(payload, &envelope) != nil || len(envelope.Error) == 0 {
		return ""
	}
	var nested struct {
		Code string `json:"code"`
	}
	// Only the structured nested code field is admitted; raw strings and error prose are ignored.
	if json.Unmarshal(envelope.Error, &nested) == nil && nested.Code != "" {
		return nested.Code
	}
	return ""
}

// readBoundedSDKGenerationResponse admits one complete JSON response while
// distinguishing the exact size ceiling from a truncated transport body.
func readBoundedSDKGenerationResponse(body io.Reader) ([]byte, error) {
	limited, err := io.ReadAll(io.LimitReader(body, maxSDKGenerationResponseBytes+1))
	// Read failures are transport ambiguity and must not be interpreted as a terminal Registry state.
	if err != nil {
		return nil, errors.New("Registry SDK generation response could not be read")
	}
	// One extra byte proves the response exceeded the public Engine admission bound.
	if len(limited) > maxSDKGenerationResponseBytes {
		return nil, errors.New("Registry SDK generation response exceeded the Engine limit")
	}
	return limited, nil
}

// DownloadSDKPackage retrieves SDK archives from Local Registry first in Hybrid mode, falling back to Cloud on 404.
func (c *HTTPRegistryClient) DownloadSDKPackage(ctx context.Context, appID uuid.UUID) (*http.Response, error) {
	if c.HasLocalRegistry() {
		return c.downloadHybridSDKPackage(ctx, appID)
	}
	if appID == uuid.Nil {
		return nil, errors.New("SDK package app ID is required")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, c.registryBaseURL()+"/sdk-packages/"+appID.String()+"/download", nil)
	if err != nil {
		return nil, fmt.Errorf("SDK package download: create request: %w", err)
	}
	response, err := c.do(request)
	if err != nil {
		return nil, fmt.Errorf("SDK package download: request failed: %w", err)
	}
	return response, nil
}

// FetchSDKPackageDownloadCounts returns completed Registry download events for
// an exact, bounded app batch without exposing event rows to the browser.
func (c *HTTPRegistryClient) FetchSDKPackageDownloadCounts(ctx context.Context, appIDs []uuid.UUID) (map[uuid.UUID]int64, error) {
	counts := make(map[uuid.UUID]int64, len(appIDs))
	if len(appIDs) == 0 {
		return counts, nil
	}
	// Merge Local and Cloud package download analytics in Hybrid mode.
	if c.HasLocalRegistry() {
		return c.fetchHybridSDKPackageDownloadCounts(ctx, appIDs)
	}
	if len(appIDs) > 100 {
		return nil, errors.New("SDK package download count batch exceeds 100 apps")
	}
	var response sdkPackageDownloadCountsResponse
	if err := c.postSignedEngineJSON(ctx, "/sdk-packages/download-counts", sdkPackageDownloadCountsRequest{AppIDs: appIDs}, &response); err != nil {
		return nil, fmt.Errorf("fetch SDK package download counts: %w", err)
	}
	for _, item := range response.Counts {
		counts[item.AppID] = item.Downloads
	}
	return counts, nil
}

func (c *HTTPRegistryClient) FetchPublicServiceInsightEligibility(ctx context.Context, serviceIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	if len(serviceIDs) == 0 {
		return map[uuid.UUID]bool{}, nil
	}
	var response publicInsightEligibilityResponse
	if err := c.postSignedEngineJSON(ctx, "/api/engine/public-service-insight-eligibility", publicInsightEligibilityRequest{ServiceIDs: serviceIDs}, &response); err != nil {
		return nil, err
	}
	result := make(map[uuid.UUID]bool, len(response.Services))
	for _, service := range response.Services {
		result[service.ServiceID] = service.Reportable
	}
	return result, nil
}

func (c *HTTPRegistryClient) SendPublicServiceInsightReports(ctx context.Context, engineVersion, engineBuildHash string, reports []models.PublicServiceInsightReport, reportedAt time.Time) ([]models.PublicServiceInsightReportResult, error) {
	if len(reports) == 0 {
		return nil, nil
	}
	var response publicInsightReportResponse
	err := c.postSignedEngineJSON(ctx, "/api/engine/public-service-insight-reports", publicInsightReportRequest{
		SchemaVersion: models.PublicServiceInsightSchemaVersion, EngineVersion: engineVersion,
		EngineBuildHash: engineBuildHash, ReportedAt: reportedAt, Reports: reports,
	}, &response)
	return response.Results, err
}

func (c *HTTPRegistryClient) FetchPublicServiceInsights(ctx context.Context, query models.PublicServiceInsightsQuery) (models.PublicServiceInsights, error) {
	var response models.PublicServiceInsights
	err := c.postSignedEngineJSON(ctx, "/api/engine/public-service-insights/query", query, &response)
	return response, err
}

func (c *HTTPRegistryClient) postSignedEngineJSON(ctx context.Context, path string, requestBody, responseBody any) error {
	if c.licenseKey == "" {
		return fmt.Errorf("FUSED_LICENSE_KEY is required but was not provided")
	}
	body, err := json.Marshal(requestBody)
	if err != nil {
		return fmt.Errorf("Registry request: marshal %s: %w", path, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.registryBaseURL()+path, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("Registry request: create %s: %w", path, err)
	}
	req.Header.Set("Authorization", "Bearer "+c.licenseKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Engine-Signature", c.signRegistryPayload(body))
	resp, err := c.do(req)
	if err != nil {
		return fmt.Errorf("Registry request %s failed: %w", path, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		response, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("Registry request %s returned status %d: %s", path, resp.StatusCode, string(response))
	}
	if responseBody == nil {
		return nil
	}
	if err := json.NewDecoder(resp.Body).Decode(responseBody); err != nil {
		return fmt.Errorf("Registry response %s: %w", path, err)
	}
	return nil
}

func (c *HTTPRegistryClient) signRegistryPayload(body []byte) string {
	mac := hmac.New(sha256.New, []byte(c.licenseKey))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// RuntimeEntitlementFromHandshake maps the Registry wire bundle into the Engine's normalized local authority.
func RuntimeEntitlementFromHandshake(raw *rawRuntimeEntitlement) models.RuntimeEntitlement {
	entitlement := models.DefaultRuntimeEntitlement()
	if raw == nil {
		return entitlement
	}
	entitlement.EntitlementRevision = raw.EntitlementRevision
	if raw.Plan != "" {
		entitlement.Plan = raw.Plan
	}
	if raw.HeartbeatRequired != nil {
		entitlement.HeartbeatRequired = *raw.HeartbeatRequired
	}
	if raw.UsageReporting != "" {
		entitlement.UsageReporting = raw.UsageReporting
	}
	entitlement.PublicServiceInsightsEnabled = raw.PublicServiceInsightsEnabled
	entitlement.HeartbeatIntervalSeconds = raw.HeartbeatIntervalSeconds
	entitlement.HeartbeatStaleAfterSeconds = raw.HeartbeatStaleAfterSeconds
	entitlement.MaxBuckets = raw.MaxBuckets
	entitlement.MaxAPIFamilies = raw.MaxAPIFamilies
	entitlement.MaxSDKFamilies = raw.MaxSDKFamilies
	entitlement.MaxMCPFamilies = raw.MaxMCPFamilies
	entitlement.MaxUnifiedAppFamilies = raw.MaxUnifiedAppFamilies
	entitlement.MaxUnifiedAppConcurrency = raw.MaxUnifiedAppConcurrency
	entitlement.MaxServices = raw.MaxServices
	entitlement.MaxSandboxConcurrency = raw.MaxSandboxConcurrency
	entitlement.DriftMonitoringEnabled = raw.DriftMonitoringEnabled
	entitlement.UnifiedAppAlwaysOnEnabled = raw.UnifiedAppAlwaysOnEnabled
	entitlement.WebhookIngestionEnabled = raw.WebhookIngestionEnabled
	entitlement.SSOEnabled = raw.SSOEnabled
	entitlement.LocalRegistryEnabled = raw.LocalRegistryEnabled
	entitlement.ExecutionRetentionDays = raw.ExecutionRetentionDays
	return entitlement.Normalized()
}

// buildServiceMetadataRequest includes the complete verification policy so exact-version metadata cannot weaken ingress.
func (c *HTTPRegistryClient) buildServiceMetadataRequest(ctx context.Context, serviceID string, version string) (*http.Request, error) {
	query := `
		query GetService($serviceId: String!, $version: String!) {
			service(id: $serviceId, version: $version) {
				id
				current_service_version
				service_versions {
					id
					name
					contract_version
					required_capabilities
				}
				name
				description
				is_public
				base_url
				servers {
					url
					description
					environment
					is_default
					variables { name default enum required }
				}
				default_headers
				connect_config
				auth_configs {` + registryAuthConfigGraphQLFields + `}
				rate_limit {` + runtimeRateLimitFields + `}
				retry_config {` + runtimeRetryFields + `}
				timeout_ms
				pagination {` + runtimePaginationFields + `}
				event_extraction_path
				incoming_webhook_config {
					auth_type
					auth_location
					auth_key_name
					signature_header
					verification_headers
					signature_policy {` + registrySignaturePolicyGraphQLFields + `}
				}
				documentation
			}
		}
	`

	gr := graphqlQuery{
		Query: query,
		Variables: map[string]interface{}{
			"serviceId": serviceID,
			"version":   version,
		},
	}

	body, err := json.Marshal(gr)
	if err != nil {
		return nil, fmt.Errorf("failed to encode graphql query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint, bytes.NewBuffer(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	token := c.licenseKey

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-API-Key", token)
	}
	return req, nil
}

func (c *HTTPRegistryClient) buildEndpointsByNamesRequest(ctx context.Context, serviceID string, serviceVersionID uuid.UUID, names []string) (*http.Request, error) {
	query := `
		query GetEndpoints($serviceId: String!, $serviceVersionId: String!, $names: [String!]!) {
			endpointsByNames(serviceId: $serviceId, serviceVersionId: $serviceVersionId, names: $names) {
				id
				stable_key
				name
				description
				method
				path
				normalized_path
				deprecated
				parameters {
					name
					in
					required
					type
					description
					path_encoding
				}
				request_content
				responses
				graphql_query
				provider_protocol
				operation_kind
				pagination {` + runtimePaginationFields + `}
				security_requirements {` + runtimeSecurityRequirementFields + `}
				documentation
			}
		}
	`
	reqBody := graphqlQuery{
		Query: query,
		Variables: map[string]interface{}{
			"serviceId":        serviceID,
			"serviceVersionId": serviceVersionID.String(),
			"names":            names,
		},
	}
	return c.newGraphQLRequest(ctx, reqBody)
}

func (c *HTTPRegistryClient) newGraphQLRequest(ctx context.Context, reqBody graphqlQuery) (*http.Request, error) {
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal query: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	token := c.licenseKey

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-API-Key", token)
	}
	return req, nil
}

// SearchCatalogue queries Local Registry first in Hybrid mode and falls back to Cloud on zero hits when permitted.
func (c *HTTPRegistryClient) SearchCatalogue(ctx context.Context, searchQuery string, page int, limit int) ([]CatalogueService, error) {
	if c.HasLocalRegistry() {
		return c.searchHybridCatalogue(ctx, searchQuery, page, limit)
	}
	query := `
		query SearchServices($query: String!, $page: Int!, $limit: Int!) {
			services(query: $query, page: $page, limit: $limit) {
				id
				name
				description
				icon_url
				base_url
				endpoint_count
				webhook_count
			}
		}
	`
	reqBody := graphqlQuery{
		Query: query,
		Variables: map[string]interface{}{
			"query": searchQuery,
			"page":  page,
			"limit": limit,
		},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal query: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	token := c.licenseKey

	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("X-API-Key", token)
	}

	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry returned status %d: %s", resp.StatusCode, string(body))
	}

	return parseCatalogueResponse(resp.Body)
}

func parseCatalogueResponse(body io.Reader) ([]CatalogueService, error) {
	var gr graphqlResponse
	if err := json.NewDecoder(body).Decode(&gr); err != nil {
		return nil, fmt.Errorf("failed to decode response: %w", err)
	}

	if len(gr.Errors) > 0 {
		return nil, fmt.Errorf("graphql error: %s", gr.Errors[0].Message)
	}

	rawSvc, ok := gr.Data["services"]
	if !ok || len(rawSvc) == 0 || string(rawSvc) == "null" {
		return []CatalogueService{}, nil
	}

	var services []CatalogueService
	if err := json.Unmarshal(rawSvc, &services); err != nil {
		return nil, fmt.Errorf("failed to unmarshal services: %w", err)
	}

	return services, nil
}

// FetchDriftSnapshots loads pending drift snapshots for a single service from Registry.
func (c *HTTPRegistryClient) FetchDriftSnapshots(ctx context.Context, serviceID uuid.UUID, apiKey string) ([]models.DriftSnapshot, error) {
	// Route drift lookups to the service's authoritative Registry in Hybrid mode.
	if c.HasLocalRegistry() {
		return c.fetchHybridDriftSnapshots(ctx, serviceID, apiKey)
	}
	query := `
		query GetDriftSnapshots($serviceId: String!) {
			driftSnapshots(serviceId: $serviceId) {
				id
				integration_object_id
				webhook_object_id
				status
				diff {
					field
					old_value
					new_value
					severity
					description
				}
			}
		}
	`
	req, err := c.buildLicensedGraphQLRequest(ctx, graphqlQuery{
		Query: query,
		Variables: map[string]interface{}{
			"serviceId": serviceID.String(),
		},
	}, apiKey)
	// Abort if query marshaling or request construction fails.
	if err != nil {
		return nil, err
	}

	resp, err := c.do(req)
	// Surface transport failures with the existing Registry client error prefix.
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()

	data, err := decodeLicensedGraphQLResponse[struct {
		DriftSnapshots []models.DriftSnapshot `json:"driftSnapshots"`
	}](resp)
	// Propagate HTTP, decode, or GraphQL failures directly.
	if err != nil {
		return nil, err
	}
	return data.DriftSnapshots, nil
}

// FetchDriftSnapshotsForServices calls the batched driftSnapshotsForServices
// GraphQL field, resolving pending drift for every given service in one
// round trip instead of one FetchDriftSnapshots call per service. An empty
// serviceIDs slice short-circuits without a request, since a workspace with
// no activated services has nothing to ask Registry about.
func (c *HTTPRegistryClient) FetchDriftSnapshotsForServices(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) ([]models.DriftSnapshot, error) {
	if len(serviceIDs) == 0 {
		return nil, nil
	}
	// Partition batched drift polling across Local and Cloud Registries in Hybrid mode.
	if c.HasLocalRegistry() {
		return c.fetchHybridDriftSnapshotsForServices(ctx, serviceIDs, apiKey)
	}
	req, err := c.buildDriftSnapshotsForServicesRequest(ctx, serviceIDs, apiKey)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()
	return decodeDriftSnapshotsForServices(resp)
}

func (c *HTTPRegistryClient) buildDriftSnapshotsForServicesRequest(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) (*http.Request, error) {
	query := `
		query GetDriftSnapshotsForServices($serviceIds: [String!]!) {
			driftSnapshotsForServices(serviceIds: $serviceIds) {
				id
				integration_object_id
				webhook_object_id
				status
				service_id
				diff {
					field
					old_value
					new_value
					severity
					description
				}
			}
		}
	`
	return c.buildLicensedGraphQLRequest(ctx, graphqlQuery{
		Query: query,
		Variables: map[string]interface{}{
			"serviceIds": uuidStrings(serviceIDs),
		},
	}, apiKey)
}

// buildLicensedGraphQLRequest creates a GraphQL POST request authenticated with
// either the caller's API key or the client's fallback license key.
func (c *HTTPRegistryClient) buildLicensedGraphQLRequest(ctx context.Context, reqBody graphqlQuery, apiKey string) (*http.Request, error) {
	b, err := json.Marshal(reqBody)
	// Query marshaling must succeed before building the HTTP request.
	if err != nil {
		return nil, fmt.Errorf("failed to marshal query: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(b))
	// Reject invalid endpoint or cancelled context errors early.
	if err != nil {
		return nil, fmt.Errorf("failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	// Prefer per-request API key; fall back to the configured license key when anonymous.
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	} else if c.licenseKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.licenseKey)
		req.Header.Set("X-API-Key", c.licenseKey)
	}
	return req, nil
}

// decodeLicensedGraphQLResponse validates an HTTP/GraphQL response for drift
// and changelog queries and returns the typed data block.
func decodeLicensedGraphQLResponse[T any](resp *http.Response) (T, error) {
	var zero T
	// Reject HTTP 4xx/5xx responses with their raw body before JSON decoding.
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return zero, fmt.Errorf("registry returned status %d: %s", resp.StatusCode, string(body))
	}

	var gr struct {
		Data   T `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}

	// Surface invalid JSON payloads instead of returning zero-valued data.
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return zero, fmt.Errorf("failed to decode response: %w", err)
	}

	// Treat any GraphQL error entry as a failed query.
	if len(gr.Errors) > 0 {
		return zero, fmt.Errorf("graphql error: %s", gr.Errors[0].Message)
	}

	return gr.Data, nil
}

// decodeDriftSnapshotsForServices decodes the driftSnapshotsForServices GraphQL response.
func decodeDriftSnapshotsForServices(resp *http.Response) ([]models.DriftSnapshot, error) {
	data, err := decodeLicensedGraphQLResponse[struct {
		DriftSnapshotsForServices []models.DriftSnapshot `json:"driftSnapshotsForServices"`
	}](resp)
	// Propagate HTTP, decode, or GraphQL failures directly.
	if err != nil {
		return nil, err
	}
	return data.DriftSnapshotsForServices, nil
}

// FetchServiceChangelogSince calls serviceChangelogSince, transport-wise an
// exact copy of FetchDriftSnapshotsForServices' shape (single POST, same
// X-API-Key/license-key fallback headers) -- only the query and the fact
// that this is single-service, not batched, differ (see the RegistryClient
// interface doc comment for why).
func (c *HTTPRegistryClient) FetchServiceChangelogSince(ctx context.Context, serviceID uuid.UUID, since time.Time, apiKey string) ([]models.ServiceChangelogEntry, error) {
	// Route changelog polling to the service's owning Registry in Hybrid mode.
	if c.HasLocalRegistry() {
		return c.fetchHybridServiceChangelogSince(ctx, serviceID, since, apiKey)
	}
	req, err := c.buildServiceChangelogSinceRequest(ctx, serviceID, since, apiKey)
	if err != nil {
		return nil, err
	}
	resp, err := c.do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()
	return decodeServiceChangelogSince(resp)
}

// buildServiceChangelogSinceRequest builds the GraphQL request for serviceChangelogSince.
func (c *HTTPRegistryClient) buildServiceChangelogSinceRequest(ctx context.Context, serviceID uuid.UUID, since time.Time, apiKey string) (*http.Request, error) {
	// serviceId is declared String! here, not ID! because the Registry API
	// contract exposes this field argument as String. This schema never
	// registers graphql.ID as an input type anywhere else, so a
	// $serviceId: ID! variable fails query validation with "Variable
	// \"$serviceId\" cannot be non-input type \"ID!\"" before the request
	// ever reaches the resolver.
	query := `
		query ServiceChangelogSince($serviceId: String!, $since: String!) {
			serviceChangelogSince(serviceId: $serviceId, since: $since) {
				id
				service_id
				version
				config_type
				changelog_type
				diff
				plan_id
				config_key
				created_by
				created_at
			}
		}
	`
	return c.buildLicensedGraphQLRequest(ctx, graphqlQuery{
		Query: query,
		Variables: map[string]interface{}{
			"serviceId": serviceID.String(),
			// RFC3339 to match graph/service_changelog.go's
			// time.Parse(time.RFC3339, ...) parsing of this arg.
			"since": since.UTC().Format(time.RFC3339),
		},
	}, apiKey)
}

// serviceChangelogEntryWire is the wire shape serviceChangelogSince returns:
// version/plan_id/config_key/created_by are nullable GraphQL strings and
// created_at is RFC3339 text (see graph/types.go's resolveTime), so this
// decodes into models.ServiceChangelogEntry explicitly rather than trusting
// json.Unmarshal to coerce string IDs into uuid.UUID/*uuid.UUID fields.
type serviceChangelogEntryWire struct {
	ID            string          `json:"id"`
	ServiceID     string          `json:"service_id"`
	Version       *string         `json:"version"`
	ConfigType    string          `json:"config_type"`
	ChangelogType string          `json:"changelog_type"`
	Diff          json.RawMessage `json:"diff"`
	PlanID        *string         `json:"plan_id"`
	ConfigKey     *string         `json:"config_key"`
	CreatedBy     *string         `json:"created_by"`
	CreatedAt     string          `json:"created_at"`
}

// decodeServiceChangelogSince decodes and parses serviceChangelogSince wire entries.
func decodeServiceChangelogSince(resp *http.Response) ([]models.ServiceChangelogEntry, error) {
	data, err := decodeLicensedGraphQLResponse[struct {
		ServiceChangelogSince []serviceChangelogEntryWire `json:"serviceChangelogSince"`
	}](resp)
	// Propagate HTTP, decode, or GraphQL failures before converting wire entries.
	if err != nil {
		return nil, err
	}
	var gr struct {
		Data struct {
			ServiceChangelogSince []serviceChangelogEntryWire
		}
	}
	gr.Data.ServiceChangelogSince = data.ServiceChangelogSince

	entries := make([]models.ServiceChangelogEntry, 0, len(gr.Data.ServiceChangelogSince))
	for _, wire := range gr.Data.ServiceChangelogSince {
		entry, err := wire.toModel()
		if err != nil {
			return nil, err
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

// toModel parses this wire row's string-typed IDs/timestamp into
// models.ServiceChangelogEntry's typed fields, surfacing a clear error
// (rather than a zero-value uuid.Nil) if the Registry ever returns a
// malformed value.
func (w serviceChangelogEntryWire) toModel() (models.ServiceChangelogEntry, error) {
	id, err := uuid.Parse(w.ID)
	if err != nil {
		return models.ServiceChangelogEntry{}, fmt.Errorf("invalid changelog id %q: %w", w.ID, err)
	}
	serviceID, err := uuid.Parse(w.ServiceID)
	if err != nil {
		return models.ServiceChangelogEntry{}, fmt.Errorf("invalid service id %q: %w", w.ServiceID, err)
	}
	createdAt, err := time.Parse(time.RFC3339, w.CreatedAt)
	if err != nil {
		return models.ServiceChangelogEntry{}, fmt.Errorf("invalid created_at %q: %w", w.CreatedAt, err)
	}
	entry := models.ServiceChangelogEntry{
		ID:            id,
		ServiceID:     serviceID,
		Version:       w.Version,
		ConfigType:    models.ServiceChangelogConfigType(w.ConfigType),
		ChangelogType: models.ServiceChangelogType(w.ChangelogType),
		Diff:          w.Diff,
		ConfigKey:     w.ConfigKey,
		CreatedAt:     createdAt,
	}
	if w.PlanID != nil {
		planID, err := uuid.Parse(*w.PlanID)
		if err != nil {
			return models.ServiceChangelogEntry{}, fmt.Errorf("invalid plan_id %q: %w", *w.PlanID, err)
		}
		entry.PlanID = &planID
	}
	if w.CreatedBy != nil {
		createdBy, err := uuid.Parse(*w.CreatedBy)
		if err != nil {
			return models.ServiceChangelogEntry{}, fmt.Errorf("invalid created_by %q: %w", *w.CreatedBy, err)
		}
		entry.CreatedBy = &createdBy
	}
	return entry, nil
}

// registrySignaturePolicyGraphQLFields keeps the complete verifier policy at both metadata boundaries.
const registrySignaturePolicyGraphQLFields = `
	version
	rules {
		name kind
		response { value { location name path field { key separator assignment } } body_field status_code }
		predicates { source { location name path field { key separator assignment } } operator value }
		verification {
			kind
			signature {
				secret_ref
				signature { location name path field { key separator assignment } }
				components { value kind names join algorithm encoding source { location name path field { key separator assignment } } }
				timestamp { header max_age_ms max_future_ms source { location name path field { key separator assignment } } }
				algorithm encoding comparison prefix component_separator
			}
			jwt { secret_ref token { location name path field { key separator assignment } } algorithms issuer audience clock_skew_ms }
			challenge { value { location name path field { key separator assignment } } body_field status_code }
		}
	}
`
