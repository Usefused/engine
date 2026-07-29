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
	"os"
	"sort"
	"strings"
	"time"

	"golang.org/x/sync/singleflight"

	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

type RegistryClient interface {
	Handshake(ctx context.Context) (accountID string, workspaceName string, err error)
	FetchServiceMetadata(ctx context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, error)
	FetchEndpointByName(ctx context.Context, serviceID uuid.UUID, version string, endpointName string) (*fusedobject.Endpoint, error)
	// FetchEndpointsByNames is FetchEndpointByName's batched sibling:
	// validateSDKOperations (sdk_config_handlers.go) used to call
	// FetchEndpointByName once per operation name when validating an SDK
	// config's selected operations -- this resolves the whole operation
	// list for a service in one Registry round trip instead.
	FetchEndpointsByNames(ctx context.Context, serviceID uuid.UUID, serviceVersionID uuid.UUID, endpointNames []string) ([]fusedobject.Endpoint, error)
	// FetchServiceOperations returns every endpoint on one service+version,
	// unfiltered by name -- unlike FetchEndpointsByNames, which requires
	// already knowing which names to ask for. Used to build a per-session MCP
	// fixture (mcp_session_fixture.go) from an SDKSelection's EndpointIDs or
	// SelectAll flag, neither of which carries endpoint names to fetch by.
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
	ValidateSDKSelections(ctx context.Context, selections []models.SDKSelection) error
}

type CatalogueService struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
}

type ServiceVersionRevision struct {
	ServiceID        uuid.UUID `json:"service_id"`
	Version          string    `json:"version"`
	ServiceVersionID uuid.UUID `json:"service_version_id"`
	Revision         int       `json:"revision"`
	SourceHash       string    `json:"source_hash"`
}

type ServiceVersionAuthConfigs struct {
	ServiceID        uuid.UUID               `json:"service_id"`
	Version          string                  `json:"version"`
	ServiceVersionID uuid.UUID               `json:"service_version_id"`
	AuthConfigs      fusedobject.AuthConfigs `json:"auth_configs"`
}

type ServiceVersionRef = models.ServiceVersionRef

type EngineHeartbeatRequest struct {
	EngineVersion   string    `json:"engine_version"`
	EngineBuildHash string    `json:"engine_build_hash"`
	ReportedAt      time.Time `json:"reported_at"`
}

type EngineUsageReportRequest struct {
	EngineVersion   string                     `json:"engine_version"`
	EngineBuildHash string                     `json:"engine_build_hash"`
	ReportedAt      time.Time                  `json:"reported_at"`
	Reports         []models.EngineUsageReport `json:"reports"`
}

type EngineHandshakeResult struct {
	AccountID     string
	WorkspaceName string
	Entitlements  models.RuntimeEntitlement
}

type rawRuntimeEntitlement struct {
	Plan                       string `json:"plan"`
	HeartbeatRequired          *bool  `json:"heartbeat_required"`
	UsageReporting             string `json:"usage_reporting"`
	HeartbeatIntervalSeconds   int    `json:"heartbeat_interval_seconds"`
	HeartbeatStaleAfterSeconds int    `json:"heartbeat_stale_after_seconds"`
}

type ConnectionProfileRef struct {
	ServiceVersionID uuid.UUID `json:"service_version_id"`
	AuthType         string    `json:"auth_type"`
}

type ConnectionProfileRevision struct {
	ProfileID        uuid.UUID                 `json:"profile_id"`
	ServiceID        uuid.UUID                 `json:"service_id"`
	ServiceVersionID uuid.UUID                 `json:"service_version_id"`
	Name             string                    `json:"name"`
	AuthType         string                    `json:"auth_type"`
	Revision         int                       `json:"revision"`
	ProfileHash      string                    `json:"profile_hash"`
	Config           connectionprofile.Profile `json:"config"`
	Provenance       string                    `json:"provenance"`
}

type ConnectionProfileContract = models.ServiceVersionConnectionContract

// FetchConnectionProfileContracts loads validation facts for all requested
// pinned versions in one GraphQL round trip.
func (c *HTTPRegistryClient) FetchConnectionProfileContracts(ctx context.Context, versionIDs []uuid.UUID, apiKey string) ([]ConnectionProfileContract, error) {
	if len(versionIDs) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(versionIDs))
	for _, id := range versionIDs {
		ids = append(ids, id.String())
	}
	req, err := c.newGraphQLRequest(ctx, graphqlQuery{
		Query: `query ConnectionProfileContracts($ids: [String!]!) {
			connectionProfileContracts(service_version_ids: $ids)
		}`,
		Variables: map[string]interface{}{"ids": ids},
	})
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch connection profile contracts: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("registry returned status %d", resp.StatusCode)
	}
	var result struct {
		Data struct {
			Contracts []ConnectionProfileContract `json:"connectionProfileContracts"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Errors) > 0 {
		return nil, errors.New(result.Errors[0].Message)
	}
	return result.Data.Contracts, nil
}

func (c *HTTPRegistryClient) FetchEligibleConnectionProfiles(ctx context.Context, refs []ConnectionProfileRef, apiKey string) ([]ConnectionProfileRevision, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	variables := make([]map[string]interface{}, 0, len(refs))
	for _, ref := range refs {
		variables = append(variables, map[string]interface{}{"service_version_id": ref.ServiceVersionID.String(), "auth_type": ref.AuthType})
	}
	req, err := c.newGraphQLRequest(ctx, graphqlQuery{
		Query: `query EligibleConnectionProfiles($refs: [ConnectionProfileRefInput!]!) {
			eligibleConnectionProfiles(refs: $refs) {
				profile_id service_id service_version_id name auth_type revision profile_hash config provenance
			}
		}`,
		Variables: map[string]interface{}{"refs": variables},
	})
	if err != nil {
		return nil, err
	}
	if apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
		req.Header.Set("X-API-Key", apiKey)
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch eligible connection profiles: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("registry returned status %d", resp.StatusCode)
	}
	var result struct {
		Data struct {
			Profiles []ConnectionProfileRevision `json:"eligibleConnectionProfiles"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Errors) > 0 {
		return nil, errors.New(result.Errors[0].Message)
	}
	return result.Data.Profiles, nil
}

type serviceVersionRevisionBatchResponse struct {
	Versions []ServiceVersionRevision `json:"versions"`
}

type ServiceProviderIdentity struct {
	Name   string `json:"name"`
	Handle string `json:"handle"`
}

type ServiceVisibility struct {
	ServiceID    uuid.UUID               `json:"id"`
	IsOwner      bool                    `json:"is_owner"`
	IsPublic     bool                    `json:"is_public"`
	Slug         string                  `json:"slug"`
	Provider     ServiceProviderIdentity `json:"provider"`
	CanonicalRef string                  `json:"canonical_ref"`
}

// ErrServiceNotFound is returned by VerifyServiceExists when the Registry has
// no service for the given ID, or the caller isn't authorized to see it --
// the Registry's own service resolver (registry/graph/schema.go) returns a
// null service for both cases rather than distinguishing them, so this
// method surfaces both identically. Either way, the caller shouldn't be able
// to add the service to a workspace.
var ErrServiceNotFound = errors.New("service not found in registry")

// VerifyServiceExists confirms a service ID is real and returns its
// Registry-authoritative name, without pulling the full catalogue payload
// FetchServiceMetadata does (auth configs, rate limits, every resource's
// endpoints). Used by the Engine's "Add to Workspace" flow, which only needs
// to know the service exists and what it's called -- not its whole definition.
//
// apiKey is the requesting user's own API key, not the Engine's shared
// license key: forwarding it means the Registry's own visibility rules
// (private/public, ownership) apply for this specific account, exactly as
// they would for any other request that account makes -- same principle as
// the GraphQL proxy in internal/engine/api forwarding the original key
// unchanged (Registry owns identity/authorization resolution).
func (c *HTTPRegistryClient) VerifyServiceExists(ctx context.Context, serviceID uuid.UUID, apiKey string) (string, string, string, uuid.UUID, error) {
	req, err := c.buildVerifyServiceRequest(ctx, serviceID, apiKey)
	if err != nil {
		return "", "", "", uuid.Nil, err
	}

	resp, err := c.httpClient.Do(req)
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

func (c *HTTPRegistryClient) buildVerifyServiceRequest(ctx context.Context, serviceID uuid.UUID, apiKey string) (*http.Request, error) {
	query := `
		query VerifyService($id: String!) {
			service(id: $id) {
				id
				name
				slug
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
	if err != nil {
		return nil, fmt.Errorf("VerifyServiceExists: failed to marshal query: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("VerifyServiceExists: failed to create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-API-Key", apiKey)

	return req, nil
}

func decodeVerifyServiceResponse(resp *http.Response) (string, string, string, uuid.UUID, error) {
	var gr struct {
		Data struct {
			Service *struct {
				ID                    string `json:"id"`
				Name                  string `json:"name"`
				Slug                  string `json:"slug"`
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
	if err := json.NewDecoder(resp.Body).Decode(&gr); err != nil {
		return "", "", "", uuid.Nil, fmt.Errorf("VerifyServiceExists: failed to decode response: %w", err)
	}
	if len(gr.Errors) > 0 {
		return "", "", "", uuid.Nil, fmt.Errorf("VerifyServiceExists: graphql error: %s", gr.Errors[0].Message)
	}
	if gr.Data.Service == nil {
		return "", "", "", uuid.Nil, ErrServiceNotFound
	}
	for _, v := range gr.Data.Service.ServiceVersions {
		if v.Name == gr.Data.Service.CurrentServiceVersion {
			vid, err := uuid.Parse(v.ID)
			if err != nil {
				return "", "", "", uuid.Nil, fmt.Errorf("VerifyServiceExists: invalid service_version_id: %w", err)
			}
			return gr.Data.Service.Name, gr.Data.Service.Slug, gr.Data.Service.CurrentServiceVersion, vid, nil
		}
	}
	return gr.Data.Service.Name, gr.Data.Service.Slug, gr.Data.Service.CurrentServiceVersion, uuid.Nil, nil
}

func currentServiceVersionID(current string, versions []struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}) uuid.UUID {
	for _, version := range versions {
		if version.Name != current {
			continue
		}
		id, err := uuid.Parse(version.ID)
		if err == nil {
			return id
		}
	}
	return uuid.Nil
}

func (c *HTTPRegistryClient) FetchServiceVisibility(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) (map[uuid.UUID]ServiceVisibility, error) {
	out := map[uuid.UUID]ServiceVisibility{}
	if len(serviceIDs) == 0 {
		return out, nil
	}
	reqBody := graphqlQuery{
		Query: `
			query ServiceVisibility($serviceIds: [String!]!) {
				servicesByIds(serviceIds: $serviceIds) {
					id
					slug
					provider { name handle }
					canonical_ref
					is_owner
					is_public
				}
			}
		`,
		Variables: map[string]interface{}{"serviceIds": uuidStrings(serviceIDs)},
	}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("FetchServiceVisibility: marshal query: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("FetchServiceVisibility: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("FetchServiceVisibility: request failed: %w", err)
	}
	defer response.Body.Close()
	visibilities, err := decodeServiceVisibility(response)
	if err != nil {
		return nil, err
	}
	for _, vis := range visibilities {
		out[vis.ServiceID] = vis
	}
	return out, nil
}

func decodeServiceVisibility(response *http.Response) ([]ServiceVisibility, error) {
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("FetchServiceVisibility: registry returned %d: %s", response.StatusCode, string(body))
	}
	var decoded struct {
		Data struct {
			ServicesByIDs []ServiceVisibility `json:"servicesByIds"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("FetchServiceVisibility: decode response: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return nil, fmt.Errorf("FetchServiceVisibility: graphql error: %s", decoded.Errors[0].Message)
	}
	return decoded.Data.ServicesByIDs, nil
}

func (c *HTTPRegistryClient) UpdateServicePublic(ctx context.Context, serviceID uuid.UUID, isPublic bool, apiKey string) error {
	reqBody := graphqlQuery{
		Query: `
			mutation UpdateServicePublic($serviceId: String!, $isPublic: Boolean!) {
				updateServicePublic(serviceId: $serviceId, isPublic: $isPublic)
			}
		`,
		Variables: map[string]interface{}{"serviceId": serviceID.String(), "isPublic": isPublic},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("UpdateServicePublic: marshal query: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("UpdateServicePublic: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("UpdateServicePublic: request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		return fmt.Errorf("UpdateServicePublic: registry returned %d: %s", response.StatusCode, string(body))
	}
	var decoded struct {
		Data struct {
			UpdateServicePublic bool `json:"updateServicePublic"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return fmt.Errorf("UpdateServicePublic: decode response: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return fmt.Errorf("UpdateServicePublic: graphql error: %s", decoded.Errors[0].Message)
	}
	if !decoded.Data.UpdateServicePublic {
		return fmt.Errorf("UpdateServicePublic: registry did not update service")
	}
	return nil
}

// PublishServiceExecutionPolicy publishes service-level execution policy
// through the Registry API. The request body mirrors the Engine's
// workspaceExecutionPolicy shape so no field mapping is required.
func (c *HTTPRegistryClient) PublishServiceExecutionPolicy(ctx context.Context, serviceID uuid.UUID, policy any, apiKey string) error {
	body, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("PublishServiceExecutionPolicy: marshal body: %w", err)
	}
	url := c.endpoint + "/integrations/" + serviceID.String() + "/execution-policy"
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("PublishServiceExecutionPolicy: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("PublishServiceExecutionPolicy: request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		return fmt.Errorf("PublishServiceExecutionPolicy: registry returned %d: %s", response.StatusCode, string(body))
	}
	return nil
}

// UpdateServiceVersionPublic calls the Registry's updateServiceVersionPublic
// mutation to set is_public on one specific version, independent of the
// service-level visibility set by UpdateServicePublic.
func (c *HTTPRegistryClient) UpdateServiceVersionPublic(ctx context.Context, serviceID uuid.UUID, version string, isPublic bool, apiKey string) error {
	reqBody := graphqlQuery{
		Query: `
			mutation UpdateServiceVersionPublic($serviceId: String!, $version: String!, $isPublic: Boolean!) {
				updateServiceVersionPublic(serviceId: $serviceId, version: $version, isPublic: $isPublic)
			}
		`,
		Variables: map[string]interface{}{"serviceId": serviceID.String(), "version": version, "isPublic": isPublic},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("UpdateServiceVersionPublic: marshal query: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("UpdateServiceVersionPublic: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("UpdateServiceVersionPublic: request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		return fmt.Errorf("UpdateServiceVersionPublic: registry returned %d: %s", response.StatusCode, string(body))
	}
	var decoded struct {
		Data struct {
			UpdateServiceVersionPublic bool `json:"updateServiceVersionPublic"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return fmt.Errorf("UpdateServiceVersionPublic: decode response: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return fmt.Errorf("UpdateServiceVersionPublic: graphql error: %s", decoded.Errors[0].Message)
	}
	if !decoded.Data.UpdateServiceVersionPublic {
		return fmt.Errorf("UpdateServiceVersionPublic: registry did not update version")
	}
	return nil
}

// PublishServiceVersionExecutionPolicy publishes execution policy scoped to
// just one provider version through the Registry API.
func (c *HTTPRegistryClient) PublishServiceVersionExecutionPolicy(ctx context.Context, serviceID uuid.UUID, version string, policy any, apiKey string) error {
	body, err := json.Marshal(policy)
	if err != nil {
		return fmt.Errorf("PublishServiceVersionExecutionPolicy: marshal body: %w", err)
	}
	url := c.endpoint + "/integrations/" + serviceID.String() + "/versions/" + version + "/execution-policy"
	request, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("PublishServiceVersionExecutionPolicy: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("PublishServiceVersionExecutionPolicy: request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		return fmt.Errorf("PublishServiceVersionExecutionPolicy: registry returned %d: %s", response.StatusCode, string(body))
	}
	return nil
}

// PublishConnectionProfile calls the Registry's setConnectionProfile mutation
// to publish an immutable, owner-authorized connection-profile revision that
// becomes visible to every consumer of the service. This mirrors the CLI's
// direct `fused service connection-profile set` client call
// (cli/internal/api/api.go SetConnectionProfile) so the declarative
// workspace.yaml apply path (connection_profiles[*].public: true) reaches the
// same Registry mutation instead of a separate one.
func (c *HTTPRegistryClient) PublishConnectionProfile(ctx context.Context, serviceID, serviceVersionID uuid.UUID, name string, profile connectionprofile.Profile, apiKey string) (*ConnectionProfileRevision, error) {
	reqBody := graphqlQuery{
		Query: `
			mutation SetConnectionProfile($serviceId: String!, $serviceVersionId: String!, $name: String!, $config: JSON!) {
				setConnectionProfile(service_id: $serviceId, service_version_id: $serviceVersionId, name: $name, config: $config) {
					profile_id service_id service_version_id name auth_type revision profile_hash config provenance
				}
			}
		`,
		Variables: map[string]interface{}{
			"serviceId": serviceID.String(), "serviceVersionId": serviceVersionID.String(),
			"name": name, "config": profile,
		},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("PublishConnectionProfile: marshal query: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("PublishConnectionProfile: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("PublishConnectionProfile: request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode >= http.StatusBadRequest {
		body, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("PublishConnectionProfile: registry returned %d: %s", response.StatusCode, string(body))
	}
	var decoded struct {
		Data struct {
			Profile *ConnectionProfileRevision `json:"setConnectionProfile"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("PublishConnectionProfile: decode response: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return nil, fmt.Errorf("PublishConnectionProfile: graphql error: %s", decoded.Errors[0].Message)
	}
	if decoded.Data.Profile == nil {
		return nil, fmt.Errorf("PublishConnectionProfile: registry did not return a profile revision")
	}
	return decoded.Data.Profile, nil
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
func (c *HTTPRegistryClient) ResolveServiceIDsBySlugs(ctx context.Context, slugs []string, apiKey string) (map[string]uuid.UUID, error) {
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

	resp, err := c.httpClient.Do(req)
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

func (c *HTTPRegistryClient) ValidateSDKSelections(ctx context.Context, selections []models.SDKSelection) error {
	if len(selections) == 0 {
		return nil
	}
	inputs := make([]map[string]interface{}, len(selections))
	for i, sel := range selections {
		inputs[i] = map[string]interface{}{
			"serviceId":        sel.ServiceID.String(),
			"serviceVersionId": sel.ServiceVersionID.String(),
			"operationNames":   sel.OperationNames,
			"webhookNames":     sel.WebhookNames,
		}
	}
	query := `
		query ValidateSDKSelections($inputs: [SDKSelectionInput!]!) {
			validateSDKSelections(inputs: $inputs)
		}
	`
	reqBody := graphqlQuery{Query: query, Variables: map[string]interface{}{"inputs": inputs}}
	b, err := json.Marshal(reqBody)
	if err != nil {
		return fmt.Errorf("ValidateSDKSelections: marshal query: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(b))
	if err != nil {
		return fmt.Errorf("ValidateSDKSelections: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.licenseKey != "" {
		req.Header.Set("X-API-Key", c.licenseKey)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("ValidateSDKSelections: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("ValidateSDKSelections: registry responded with status %d", resp.StatusCode)
	}

	var res struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Data struct {
			ValidateSDKSelections bool `json:"validateSDKSelections"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&res); err != nil {
		return fmt.Errorf("ValidateSDKSelections: decode response: %w", err)
	}
	if len(res.Errors) > 0 {
		return errors.New(res.Errors[0].Message)
	}
	return nil
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
	endpoint   string
	licenseKey string
	httpClient *http.Client

	// sfGroup collapses concurrent identical in-flight Registry calls into one.
	// FetchServiceMetadata and FetchEndpointsByNames are the hot paths:
	// multiple SDK connections arriving simultaneously for the same service +
	// version all share a single outbound request and its result, eliminating
	// the thundering-herd of duplicate round trips to the Registry at connect.
	sfGroup singleflight.Group
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

func (c *HTTPRegistryClient) FetchServiceVersionRevision(ctx context.Context, serviceID uuid.UUID, version, apiKey string) (ServiceVersionRevision, error) {
	revisions, err := c.FetchServiceVersionRevisions(ctx, []ServiceVersionRef{{ServiceID: serviceID, Version: version}}, apiKey)
	if err != nil {
		return ServiceVersionRevision{}, err
	}
	if len(revisions) == 0 {
		return ServiceVersionRevision{}, fmt.Errorf("service version revision not found")
	}
	return revisions[0], nil
}

func (c *HTTPRegistryClient) FetchServiceVersionRevisions(ctx context.Context, refs []ServiceVersionRef, apiKey string) ([]ServiceVersionRevision, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string][]ServiceVersionRef{"versions": refs})
	if err != nil {
		return nil, err
	}
	requestURL := c.registryBaseURL() + "/integrations/versions/revisions"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("Registry returned %d: %s", response.StatusCode, string(body))
	}
	var decoded serviceVersionRevisionBatchResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded.Versions, nil
}

func (c *HTTPRegistryClient) FetchServiceVersionAuthConfigs(ctx context.Context, refs []ServiceVersionRef, apiKey string) ([]ServiceVersionAuthConfigs, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	reqBody := graphqlQuery{
		Query: `query ServiceVersionAuthConfigs($refs: [ServiceVersionRefInput!]!) {
			serviceVersionAuthConfigs(refs: $refs) {
				service_id version service_version_id
				auth_configs { name type flow scheme location key_name token_url authorization_url open_id_connect_url scopes }
			}
		}`,
		Variables: map[string]interface{}{"refs": refs},
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("FetchServiceVersionAuthConfigs: marshal query: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("FetchServiceVersionAuthConfigs: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("FetchServiceVersionAuthConfigs: request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("FetchServiceVersionAuthConfigs: registry returned %d: %s", response.StatusCode, string(body))
	}
	var decoded struct {
		Data struct {
			Versions []ServiceVersionAuthConfigs `json:"serviceVersionAuthConfigs"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("FetchServiceVersionAuthConfigs: decode response: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return nil, fmt.Errorf("FetchServiceVersionAuthConfigs: graphql error: %s", decoded.Errors[0].Message)
	}
	return decoded.Data.Versions, nil
}

type ServiceVersionResolvedRef struct {
	ServiceID        uuid.UUID `json:"service_id"`
	Version          string    `json:"version"`
	ServiceVersionID uuid.UUID `json:"service_version_id"`
}

type serviceVersionResolveBatchResponse struct {
	Resolved []ServiceVersionResolvedRef `json:"resolved"`
}

func (c *HTTPRegistryClient) FetchServiceVersionIDs(ctx context.Context, refs []ServiceVersionRef, apiKey string) ([]ServiceVersionResolvedRef, error) {
	if len(refs) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(map[string][]ServiceVersionRef{"versions": refs})
	if err != nil {
		return nil, err
	}
	requestURL := c.registryBaseURL() + "/integrations/versions/resolve"
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("Registry returned %d: %s", response.StatusCode, string(body))
	}
	var decoded serviceVersionResolveBatchResponse
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, err
	}
	return decoded.Resolved, nil
}

// FetchLatestServiceVersions resolves "version omitted" workspace/SDK config
// entries in one Registry GraphQL request. The Registry picks the latest
// visible public provider version; Engine only attaches the exact version
// identity it receives, avoiding one service(id:) call per service.
func (c *HTTPRegistryClient) FetchLatestServiceVersions(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) ([]ServiceVersionResolvedRef, error) {
	if len(serviceIDs) == 0 {
		return nil, nil
	}
	reqBody := graphqlQuery{
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
	}
	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("FetchLatestServiceVersions: marshal query: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("FetchLatestServiceVersions: create request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-API-Key", apiKey)
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, fmt.Errorf("FetchLatestServiceVersions: request failed: %w", err)
	}
	defer response.Body.Close()
	return decodeLatestServiceVersions(response)
}

func uuidStrings(ids []uuid.UUID) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		out = append(out, id.String())
	}
	return out
}

func decodeLatestServiceVersions(response *http.Response) ([]ServiceVersionResolvedRef, error) {
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return nil, fmt.Errorf("FetchLatestServiceVersions: registry returned %d: %s", response.StatusCode, string(body))
	}
	var decoded struct {
		Data struct {
			LatestServiceVersions []ServiceVersionResolvedRef `json:"latestServiceVersions"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return nil, fmt.Errorf("FetchLatestServiceVersions: decode response: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return nil, fmt.Errorf("FetchLatestServiceVersions: graphql error: %s", decoded.Errors[0].Message)
	}
	return decoded.Data.LatestServiceVersions, nil
}

// DeprecateServiceVersion marks a single named version of a service as
// deprecated in the Registry. The Registry endpoint is idempotent — calling it
// on an already-deprecated version returns 204 with no mutation.
func (c *HTTPRegistryClient) DeprecateServiceVersion(ctx context.Context, serviceID uuid.UUID, version, apiKey string) error {
	url := c.registryBaseURL() + "/integrations/" + serviceID.String() + "/versions/" + version + "/deprecate"
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, nil)
	if err != nil {
		return fmt.Errorf("DeprecateServiceVersion: create request: %w", err)
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := c.httpClient.Do(req)
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
	url := c.registryBaseURL() + "/integrations/" + serviceID.String()
	req, err := http.NewRequestWithContext(ctx, http.MethodDelete, url, nil)
	if err != nil {
		return fmt.Errorf("ArchiveService: create request: %w", err)
	}
	req.Header.Set("X-API-Key", apiKey)
	resp, err := c.httpClient.Do(req)
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
	return strings.TrimSuffix(c.endpoint, "/graphql")
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

func (c *HTTPRegistryClient) FetchServiceMetadata(ctx context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, error) {
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

	resp, err := c.httpClient.Do(req)
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
				ID                    uuid.UUID `json:"id"`
				CurrentServiceVersion string    `json:"current_service_version"`
				ServiceVersions       []struct {
					ID   string `json:"id"`
					Name string `json:"name"`
				} `json:"service_versions"`
				Name                  string                             `json:"name"`
				Description           string                             `json:"description"`
				BaseURL               string                             `json:"base_url"`
				Servers               fusedobject.Servers                `json:"servers"`
				AuthConfigs           fusedobject.AuthConfigs            `json:"auth_configs"`
				RateLimit             *fusedobject.RateLimitConfig       `json:"rate_limit"`
				RetryConfig           *fusedobject.RetryConfig           `json:"retry_config"`
				Pagination            *fusedobject.PaginationConfig      `json:"pagination"`
				DefaultHeaders        fusedobject.DefaultHeaders         `json:"default_headers"`
				ConnectConfig         *fusedobject.ServiceConnectConfig  `json:"connect_config"`
				EventExtractionPath   string                             `json:"event_extraction_path"`
				IncomingWebhookConfig *fusedobject.IncomingWebhookConfig `json:"incoming_webhook_config"`
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

	fo := &fusedobject.ServiceMetadata{
		ID:                    srv.ID,
		ServiceVersionID:      currentServiceVersionID(srv.CurrentServiceVersion, srv.ServiceVersions),
		Name:                  srv.Name,
		Description:           srv.Description,
		BaseURL:               srv.BaseURL,
		Servers:               srv.Servers,
		AuthConfigs:           srv.AuthConfigs,
		RateLimit:             srv.RateLimit,
		RetryConfig:           srv.RetryConfig,
		Pagination:            srv.Pagination,
		DefaultHeaders:        srv.DefaultHeaders,
		ConnectConfig:         srv.ConnectConfig,
		EventExtractionPath:   srv.EventExtractionPath,
		IncomingWebhookConfig: srv.IncomingWebhookConfig,
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

func (c *HTTPRegistryClient) FetchEndpointsByNames(ctx context.Context, serviceID uuid.UUID, serviceVersionID uuid.UUID, endpointNames []string) ([]fusedobject.Endpoint, error) {
	if len(endpointNames) == 0 {
		return nil, nil
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

func (c *HTTPRegistryClient) fetchEndpointsByNames(ctx context.Context, serviceID uuid.UUID, serviceVersionID uuid.UUID, endpointNames []string) ([]fusedobject.Endpoint, error) {
	req, err := c.buildEndpointsByNamesRequest(ctx, serviceID.String(), serviceVersionID, endpointNames)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
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
			EndpointsByNames []fusedobject.Endpoint `json:"endpointsByNames"`
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

	return gr.Data.EndpointsByNames, nil
}

// FetchServiceOperations returns every endpoint on serviceID/serviceVersionID,
// including the schema fields (parameters, request_body, responses)
// FetchEndpointsByNames' query intentionally omits -- that query backs the
// dispatch hot path's by-name lookups (cache.go's prefetchEndpoints/
// GetEndpoint), which don't need full schema; this one backs building a
// session's MCP fixture (mcp_session_fixture.go), which does.
func (c *HTTPRegistryClient) FetchServiceOperations(ctx context.Context, serviceID uuid.UUID, serviceVersionID uuid.UUID) ([]fusedobject.Endpoint, error) {
	sfKey := "serviceOperations:" + serviceID.String() + ":" + serviceVersionID.String()
	v, err, _ := c.sfGroup.Do(sfKey, func() (interface{}, error) {
		return c.fetchServiceOperations(ctx, serviceID, serviceVersionID)
	})
	if err != nil {
		return nil, err
	}
	return v.([]fusedobject.Endpoint), nil
}

func (c *HTTPRegistryClient) fetchServiceOperations(ctx context.Context, serviceID uuid.UUID, serviceVersionID uuid.UUID) ([]fusedobject.Endpoint, error) {
	query := `
		query GetServiceOperations($serviceId: String!, $version: String!) {
			serviceOperations(serviceId: $serviceId, version: $version) {
				id
				name
				description
				method
				path
				normalized_path
				is_sse
				deprecated
				parameters {
					name
					in
					required
					type
					description
				}
				request_body
				responses
				pagination
			}
		}
	`
	reqBody := graphqlQuery{
		Query: query,
		Variables: map[string]interface{}{
			"serviceId": serviceID.String(),
			// serviceOperations' "version" arg accepts either a version name or
			// a service_version_id UUID. Passing the UUID directly here resolves
			// the exact version an SDKSelection pinned, the same way
			// FetchEndpointsByNames' serviceVersionId arg does.
			"version": serviceVersionID.String(),
		},
	}
	req, err := c.newGraphQLRequest(ctx, reqBody)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
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
			ServiceOperations []fusedobject.Endpoint `json:"serviceOperations"`
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

	return gr.Data.ServiceOperations, nil
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

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return EngineHandshakeResult{}, fmt.Errorf("handshake request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return EngineHandshakeResult{}, fmt.Errorf("handshake failed with status %d: %s", resp.StatusCode, string(body))
	}

	var result struct {
		AccountID     string                 `json:"account_id"`
		WorkspaceName string                 `json:"workspace_name"`
		Entitlements  *rawRuntimeEntitlement `json:"entitlements"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return EngineHandshakeResult{}, fmt.Errorf("failed to decode handshake response: %w", err)
	}

	return EngineHandshakeResult{
		AccountID:     result.AccountID,
		WorkspaceName: result.WorkspaceName,
		Entitlements:  runtimeEntitlementFromHandshake(result.Entitlements),
	}, nil
}

func (c *HTTPRegistryClient) SendHeartbeat(ctx context.Context, engineVersion, engineBuildHash string, reportedAt time.Time) error {
	if c.licenseKey == "" {
		return fmt.Errorf("FUSED_LICENSE_KEY is required but was not provided")
	}

	body, err := json.Marshal(EngineHeartbeatRequest{
		EngineVersion:   engineVersion,
		EngineBuildHash: engineBuildHash,
		ReportedAt:      reportedAt,
	})
	if err != nil {
		return fmt.Errorf("heartbeat: marshal request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.registryBaseURL()+"/api/engine/heartbeat", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("heartbeat: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.licenseKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Engine-Signature", c.signRegistryPayload(body))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("heartbeat: request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("heartbeat failed with status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *HTTPRegistryClient) SendUsageReports(ctx context.Context, engineVersion, engineBuildHash string, reports []models.EngineUsageReport, reportedAt time.Time) error {
	if c.licenseKey == "" {
		return fmt.Errorf("FUSED_LICENSE_KEY is required but was not provided")
	}
	if len(reports) == 0 {
		return nil
	}
	body, err := json.Marshal(EngineUsageReportRequest{
		EngineVersion:   engineVersion,
		EngineBuildHash: engineBuildHash,
		ReportedAt:      reportedAt,
		Reports:         reports,
	})
	if err != nil {
		return fmt.Errorf("usage reports: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.registryBaseURL()+"/api/engine/usage-reports", bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("usage reports: create request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+c.licenseKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Engine-Signature", c.signRegistryPayload(body))

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("usage reports: request failed: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("usage reports failed with status %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

func (c *HTTPRegistryClient) signRegistryPayload(body []byte) string {
	mac := hmac.New(sha256.New, []byte(c.licenseKey))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func runtimeEntitlementFromHandshake(raw *rawRuntimeEntitlement) models.RuntimeEntitlement {
	entitlement := models.DefaultRuntimeEntitlement()
	if raw == nil {
		return entitlement
	}
	if raw.Plan != "" {
		entitlement.Plan = raw.Plan
	}
	if raw.HeartbeatRequired != nil {
		entitlement.HeartbeatRequired = *raw.HeartbeatRequired
	}
	if raw.UsageReporting != "" {
		entitlement.UsageReporting = raw.UsageReporting
	}
	entitlement.HeartbeatIntervalSeconds = raw.HeartbeatIntervalSeconds
	entitlement.HeartbeatStaleAfterSeconds = raw.HeartbeatStaleAfterSeconds
	return entitlement.Normalized()
}

func (c *HTTPRegistryClient) buildServiceMetadataRequest(ctx context.Context, serviceID string, version string) (*http.Request, error) {
	query := `
		query GetService($serviceId: String!, $version: String!) {
			service(id: $serviceId, version: $version) {
				id
				current_service_version
				service_versions {
					id
					name
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
				}
				default_headers
				connect_config
				auth_configs {
					name
					type
					flow
					scheme
					location
					key_name
					token_url
					authorization_url
					open_id_connect_url
					scopes
				}
				rate_limit {
					strategy
					requests_per_second
					requests_per_minute
				}
				retry_config {
					strategy
					max_retries
					backoff_ms
				}
				pagination {
					type
					request_param
					response_path
				}
				event_extraction_path
				incoming_webhook_config {
					auth_type
					auth_location
					auth_key_name
					signature_header
					verification_headers
				}
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
				name
				description
				method
				path
				normalized_path
				is_sse
				deprecated
				pagination
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

func (c *HTTPRegistryClient) SearchCatalogue(ctx context.Context, searchQuery string, page int, limit int) ([]CatalogueService, error) {
	query := `
		query SearchServices($query: String!, $page: Int!, $limit: Int!) {
			services(query: $query, page: $page, limit: $limit) {
				id
				name
				description
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

	resp, err := c.httpClient.Do(req)
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

func (c *HTTPRegistryClient) FetchDriftSnapshots(ctx context.Context, serviceID uuid.UUID, apiKey string) ([]models.DriftSnapshot, error) {
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
	reqBody := graphqlQuery{
		Query: query,
		Variables: map[string]interface{}{
			"serviceId": serviceID.String(),
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
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	} else if c.licenseKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.licenseKey)
		req.Header.Set("X-API-Key", c.licenseKey)
	}

	resp, err := c.httpClient.Do(req)
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
			DriftSnapshots []models.DriftSnapshot `json:"driftSnapshots"`
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

	return gr.Data.DriftSnapshots, nil
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
	req, err := c.buildDriftSnapshotsForServicesRequest(ctx, serviceIDs, apiKey)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
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
	ids := make([]string, len(serviceIDs))
	for i, id := range serviceIDs {
		ids[i] = id.String()
	}
	reqBody := graphqlQuery{
		Query: query,
		Variables: map[string]interface{}{
			"serviceIds": ids,
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
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	} else if c.licenseKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.licenseKey)
		req.Header.Set("X-API-Key", c.licenseKey)
	}
	return req, nil
}

func decodeDriftSnapshotsForServices(resp *http.Response) ([]models.DriftSnapshot, error) {
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry returned status %d: %s", resp.StatusCode, string(body))
	}

	var gr struct {
		Data struct {
			DriftSnapshotsForServices []models.DriftSnapshot `json:"driftSnapshotsForServices"`
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

	return gr.Data.DriftSnapshotsForServices, nil
}

// FetchServiceChangelogSince calls serviceChangelogSince, transport-wise an
// exact copy of FetchDriftSnapshotsForServices' shape (single POST, same
// X-API-Key/license-key fallback headers) -- only the query and the fact
// that this is single-service, not batched, differ (see the RegistryClient
// interface doc comment for why).
func (c *HTTPRegistryClient) FetchServiceChangelogSince(ctx context.Context, serviceID uuid.UUID, since time.Time, apiKey string) ([]models.ServiceChangelogEntry, error) {
	req, err := c.buildServiceChangelogSinceRequest(ctx, serviceID, since, apiKey)
	if err != nil {
		return nil, err
	}
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to execute request: %w", err)
	}
	defer resp.Body.Close()
	return decodeServiceChangelogSince(resp)
}

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
	reqBody := graphqlQuery{
		Query: query,
		Variables: map[string]interface{}{
			"serviceId": serviceID.String(),
			// RFC3339 to match graph/service_changelog.go's
			// time.Parse(time.RFC3339, ...) parsing of this arg.
			"since": since.UTC().Format(time.RFC3339),
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
	if apiKey != "" {
		req.Header.Set("X-API-Key", apiKey)
	} else if c.licenseKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.licenseKey)
		req.Header.Set("X-API-Key", c.licenseKey)
	}
	return req, nil
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

func decodeServiceChangelogSince(resp *http.Response) ([]models.ServiceChangelogEntry, error) {
	if resp.StatusCode >= 400 {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("registry returned status %d: %s", resp.StatusCode, string(body))
	}

	var gr struct {
		Data struct {
			ServiceChangelogSince []serviceChangelogEntryWire `json:"serviceChangelogSince"`
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
