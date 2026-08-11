package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/Usefused/engine/internal/shared/observability"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
)

const appTokensRoute = "/app-tokens"

// ServiceVerifier is the Registry-lookup capability workspace membership
// writes need: confirm a service ID is real and resolve version tags to exact
// API-version IDs before Engine persists a workspace service version.
// Defined here as a small interface (not sandbox.RegistryClient's full one) so
// this package only depends on the methods it actually uses --
// *sandbox.HTTPRegistryClient satisfies this without sandbox.RegistryClient
// needing to declare it, so none of that interface's existing mocks (used
// throughout the sandbox package's own tests) need updating for this.
// FetchServiceMetadata is already part of sandbox.RegistryClient (used at SDK
// connect), so pulling it in here doesn't grow the concrete client's surface
// -- it lets webhook registration apply (upsertWorkspaceServiceWebhooks,
// workspace_config_handlers.go) fetch a service's IncomingWebhookConfig once
// per apply, instead of the ingress path fetching it once per request.
type ServiceVerifier interface {
	VerifyServiceExists(ctx context.Context, serviceID uuid.UUID, apiKey string) (string, string, string, uuid.UUID, error)
	FetchServiceVersionRevisions(ctx context.Context, refs []sandbox.ServiceVersionRef, apiKey string) ([]sandbox.ServiceVersionRevision, error)
	FetchServiceVersionAuthConfigs(ctx context.Context, refs []sandbox.ServiceVersionRef, apiKey string) ([]sandbox.ServiceVersionAuthConfigs, error)
	FetchLatestServiceVersions(ctx context.Context, serviceIDs []uuid.UUID, apiKey string) ([]sandbox.ServiceVersionResolvedRef, error)
	FetchServiceMetadata(ctx context.Context, serviceID uuid.UUID, version string) (*fusedobject.ServiceMetadata, error)
}

// WorkspaceHandler returns an http.Handler for the /workspace subtree.
// It is mounted at /workspace in main.go and handles Engine-local workspace
// membership — it is NOT a proxy to the Registry.
//
// Why separate from registerProxyRoutes: workspace membership lives in the
// Engine's own DB. Proxying it to the Registry would
// serve stale or wrong data; the Engine is the authoritative source here.
func WorkspaceHandler(s store.Store, verifier ServiceVerifier, masterKey []byte, tokenRevoker AppTokenRevoker) http.Handler {
	r := chi.NewRouter()
	r.Post("/services", addServiceHandler(s, verifier))
	r.Post("/services/{id}/versions/{version_id}/refresh", RefreshServiceContractHandler(s, runtimeContractFetcher(verifier)))
	r.Delete("/services/{id}", removeServiceHandler(s))

	r.Post("/buckets", CreateBucketHandler(s))
	r.Delete("/buckets/{name}", DeleteBucketHandler(s))

	r.Put("/buckets/{id}/values", UpsertBucketValueHandler(s))
	r.Delete("/buckets/{id}/values", DeleteBucketValueHandler(s))

	r.Put("/buckets/{bucket_id}/services/{service_id}/connect-config", UpsertConnectConfigHandler(s, masterKey))
	r.Get("/buckets/{bucket_id}/services/{service_id}/connect-config", GetConnectConfigHandler(s))
	r.Post("/buckets/{bucket_id}/services/{service_id}/connect/sessions", StartConnectSessionHandler(s, verifier, masterKey))
	r.Delete("/buckets/{bucket_id}/auth/connections/{connection_id}", DeleteAuthConnectionHandler(s))
	r.Get("/connect/callback", ConnectCallbackHandler(s, verifier, masterKey))

	r.Put("/secrets", UpsertSecretHandler(s, masterKey))
	r.Put("/secrets/bulk", UpsertSecretsHandler(s, masterKey))
	r.Delete("/secrets", DeleteSecretHandler(s))

	r.Post(appTokensRoute, GenerateAppTokenHandler(s))
	r.Delete(appTokensRoute, RevokeAppTokenHandler(tokenRevoker))

	return r
}

func runtimeContractFetcher(verifier ServiceVerifier) RuntimeContractFetcher {
	fetcher, _ := verifier.(RuntimeContractFetcher)
	return fetcher
}

// addServiceRequest is the JSON body for POST /workspace/services.
type addServiceRequest struct {
	ServiceID        string `json:"service_id"`
	ServiceName      string `json:"service_name"`
	VersionTag       string `json:"version_tag"`
	ServiceVersionID string `json:"service_version_id"`
}

// parseAddServiceRequest decodes and validates the request body, returning
// the parsed service ID alongside the raw request. Split out from
// addServiceHandler so that function's branching stays focused on the
// verify-then-write flow, not body parsing.
func parseAddServiceRequest(r *http.Request) (addServiceRequest, uuid.UUID, error) {
	var req addServiceRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		return req, uuid.Nil, errors.New("invalid request body")
	}
	if req.ServiceID == "" {
		return req, uuid.Nil, errors.New("service_id is required")
	}
	svcID, err := uuid.Parse(req.ServiceID)
	if err != nil {
		return req, uuid.Nil, errors.New("service_id must be a valid UUID")
	}
	if strings.TrimSpace(req.ServiceVersionID) != "" {
		if strings.TrimSpace(req.VersionTag) == "" {
			return req, uuid.Nil, errors.New("service_version_id requires version_tag")
		}
		if _, err := uuid.Parse(strings.TrimSpace(req.ServiceVersionID)); err != nil {
			return req, uuid.Nil, errors.New("service_version_id must be a valid UUID")
		}
	}
	return req, svcID, nil
}

// addServiceHandler handles POST /workspace/services.
// It validates the caller's API key, resolves their workspace, verifies the
// service against the Registry, then writes a workspace service version. An OTEL span
// is emitted because this is a workspace-wide admin action that requires an
// audit trail (compliance requirement) -- covering the verification step too,
// not just the write, so a blocked "add nonexistent/unauthorized service"
// attempt shows up in the audit trail rather than only successful writes.
func addServiceHandler(s store.Store, verifier ServiceVerifier) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("X-API-Key")
		accountID, err := controlActorAccount(r.Context())
		if err != nil {
			http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
			return
		}

		req, svcID, err := parseAddServiceRequest(r)
		if err != nil {
			http.Error(w, fmt.Sprintf(`{"error":%q}`, err.Error()), http.StatusBadRequest)
			return
		}

		if err := verifyWorkspaceActor(r.Context(), accountID); err != nil {
			slog.ErrorContext(r.Context(), "addServiceHandler: workspace not found for account", slog.Any("error", err))
			http.Error(w, `{"error":"workspace not found"}`, http.StatusInternalServerError)
			return
		}

		if err := verifyAndActivateService(r.Context(), s, verifier, addServiceCall{
			accountID: accountID,
			serviceID: svcID,
			apiKey:    apiKey,
			version:   strings.TrimSpace(req.VersionTag),
			versionID: parseOptionalUUID(req.ServiceVersionID),
		}); err != nil {
			writeAddServiceError(w, err)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}
}

// addServiceCall bundles verifyAndActivateService's inputs -- a plain struct
// instead of a long parameter list, since accountID/workspaceID/serviceID/
// apiKey/version all need to travel together through the verify-then-write
// flow and its span attributes.
type addServiceCall struct {
	accountID uuid.UUID
	serviceID uuid.UUID
	apiKey    string
	version   string
	versionID uuid.UUID
}

// verifyAndActivateService confirms the service is real via the Registry.
// The Registry client replaces local caller credentials with FUSED_LICENSE_KEY,
// then records the workspace service using the Registry's own name for the service
// rather than trusting whatever the client sent in the request body.
//
// Why verify at all: the client supplies service_id/service_name directly in
// the POST body, and nothing upstream of this handler confirms either is
// real -- without this, an authenticated user could add an arbitrary UUID
// with a made-up name to their own workspace. VerifyServiceExists is a slim
// GraphQL query (id + name only), not the full FetchServiceMetadata catalogue
// payload, since existence + name is all this flow needs.
func verifyAndActivateService(ctx context.Context, s store.Store, verifier ServiceVerifier, call addServiceCall) error {
	thread := observability.ThreadFromContext(ctx)
	step := thread.Step("engine.workspace.add_service")
	step.AddContext(map[string]any{
		"user_action": "workspace.add_service",
		"account_id":  call.accountID.String(),
		"service_id":  call.serviceID.String(),
	})

	verifiedName, verifiedSlug, currentVersionTag, _, err := verifier.VerifyServiceExists(ctx, call.serviceID, call.apiKey)
	if err != nil {
		slog.WarnContext(ctx, "verifyAndActivateService: registry verification failed", slog.Any("error", err), slog.String("service_id", call.serviceID.String()))
		if errors.Is(err, sandbox.ErrServiceNotFound) {
			step.AddContext(map[string]any{"outcome": "not_found"}).Error(ctx, err)
			return err
		}
		// Any other failure (Registry unreachable, malformed response, etc.) is
		// distinct from "service doesn't exist" -- wrap it so writeAddServiceError
		// can tell the two apart and return 502 instead of 404 for this case.
		step.AddContext(map[string]any{"outcome": "error"}).Error(ctx, err)
		return fmt.Errorf("%w: %w", errRegistryVerificationFailed, err)
	}

	version, serviceVersionID, err := resolveWorkspaceServiceVersionID(ctx, verifier, call.serviceID, call.apiKey, call.version, currentVersionTag, call.versionID)
	if err != nil {
		step.AddContext(map[string]any{"outcome": "version_unavailable"}).Error(ctx, err)
		return err
	}
	step.AddContext(map[string]any{"version_tag": version, "service_version_id": serviceVersionID.String()})

	fetcher, _ := verifier.(RuntimeContractFetcher)
	if err := materializeRuntimeContractSnapshot(ctx, s, fetcher, call.accountID, call.serviceID, serviceVersionID, version, call.apiKey); err != nil {
		step.AddContext(map[string]any{"outcome": "contract_snapshot_failed"}).Error(ctx, err)
		return err
	}

	if err := s.AddWorkspaceServiceVersion(ctx, call.serviceID, verifiedSlug, version, serviceVersionID, verifiedName, call.accountID); err != nil {
		step.AddContext(map[string]any{"outcome": "error"}).Error(ctx, err)
		slog.ErrorContext(ctx, "verifyAndActivateService: AddWorkspaceServiceVersion failed", slog.Any("error", err))
		return fmt.Errorf("failed to add service to workspace: %w", err)
	}

	step.AddContext(map[string]any{"outcome": "success"}).Success(ctx)
	return nil
}

func resolveWorkspaceServiceVersionID(
	ctx context.Context,
	verifier ServiceVerifier,
	serviceID uuid.UUID,
	apiKey, requestedVersion, currentVersionTag string,
	requestedVersionID uuid.UUID,
) (string, uuid.UUID, error) {
	version, err := resolveWorkspaceServiceVersion(requestedVersion, currentVersionTag)
	if err != nil {
		return "", uuid.Nil, err
	}
	if requestedVersionID != uuid.Nil {
		return version, requestedVersionID, nil
	}
	revision, err := fetchServiceVersionRevision(ctx, verifier, serviceID, version, apiKey)
	if err != nil {
		return "", uuid.Nil, err
	}
	return version, revision.ServiceVersionID, nil
}

func parseOptionalUUID(raw string) uuid.UUID {
	if strings.TrimSpace(raw) == "" {
		return uuid.Nil
	}
	id, _ := uuid.Parse(strings.TrimSpace(raw))
	return id
}

func resolveWorkspaceServiceVersion(requestedVersion, currentVersionTag string) (string, error) {
	if requested := strings.TrimSpace(requestedVersion); requested != "" {
		return requested, nil
	}
	if current := strings.TrimSpace(currentVersionTag); current != "" {
		return current, nil
	}
	// A workspace service with no version would make every later SDK/runtime
	// lookup float to whichever Registry version is current at execution time.
	return "", errVersionPinUnavailable
}

func fetchServiceVersionRevision(
	ctx context.Context,
	verifier ServiceVerifier,
	serviceID uuid.UUID,
	version, apiKey string,
) (sandbox.ServiceVersionRevision, error) {
	revisions, err := verifier.FetchServiceVersionRevisions(ctx, []sandbox.ServiceVersionRef{{ServiceID: serviceID, Version: version}}, apiKey)
	if err != nil {
		return sandbox.ServiceVersionRevision{}, fmt.Errorf("resolve service version %s: %w", version, err)
	}
	for _, revision := range revisions {
		if revision.ServiceID == serviceID && revision.Version == version && revision.ServiceVersionID != uuid.Nil {
			return revision, nil
		}
	}
	return sandbox.ServiceVersionRevision{}, errVersionPinUnavailable
}

// writeAddServiceError maps verifyAndActivateService's error back to an HTTP
// response. Split out from addServiceHandler to keep that function's
// branching to the request lifecycle, not error-to-status translation.
func writeAddServiceError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, sandbox.ErrServiceNotFound):
		http.Error(w, `{"error":"service not found in registry"}`, http.StatusNotFound)
	case errors.Is(err, errVersionPinUnavailable):
		http.Error(w, `{"error":"service has no publishable version to pin"}`, http.StatusConflict)
	case errors.Is(err, errRegistryVerificationFailed):
		http.Error(w, `{"error":"unable to verify service with registry"}`, http.StatusBadGateway)
	default:
		http.Error(w, `{"error":"failed to add service to workspace"}`, http.StatusInternalServerError)
	}
}

// errRegistryVerificationFailed is never returned directly -- it exists so
// writeAddServiceError's switch has a named case to fall through to for
// "verification failed but not because the service doesn't exist" (network
// errors, malformed Registry responses, etc.), matched below.
var errRegistryVerificationFailed = errors.New("registry verification failed")

var errVersionPinUnavailable = errors.New("version pin unavailable")

// fetchServiceSlugsForListing batch-resolves every listed service's Registry
// slug (ID -> slug; the reverse direction of resolveWorkspaceServiceSlugs
// above, which resolves config-as-code slug -> ID) in one round trip,
// mirroring the visibility lookups workspace_config_handlers.go already does
// for plan/apply, rather than caching it locally: ServiceName is a one-time
// snapshot taken at add-time (upsertWorkspaceService) and can't answer "what's
// this service's slug today" for rows added before this field existed, so
// it's resolved fresh here instead of adding a column that would need its own
// backfill. A Registry failure degrades to empty slugs rather than failing
// the whole list -- this endpoint has always been a purely local read
// otherwise, and a missing slug in the printout is far better than workspace
// services list becoming unavailable whenever the Registry is unreachable.
func fetchServiceSlugsForListing(ctx context.Context, verifier ServiceVerifier, apiKey string, serviceIDs []uuid.UUID) map[uuid.UUID]string {
	slugs := make(map[uuid.UUID]string, len(serviceIDs))
	if len(serviceIDs) == 0 {
		return slugs
	}
	visResolver, ok := verifier.(ServiceVisibilityResolver)
	if !ok {
		return slugs
	}
	visibility, err := visResolver.FetchServiceVisibility(ctx, serviceIDs, apiKey)
	if err != nil {
		slog.WarnContext(ctx, "fetchServiceSlugsForListing: FetchServiceVisibility failed", slog.Any("error", err))
		return slugs
	}
	for id, vis := range visibility {
		slugs[id] = displaySlug(vis)
	}
	return slugs
}

// displaySlug qualifies vis.Slug with its owning provider when the caller
// doesn't own the service. Registry slugs are only unique per-account, so a
// bare slug for a service someone else owns either 404s against
// `service <slug> show` or -- worse -- silently resolves to a different,
// same-named service the caller happens to own themselves. The Registry's own slug field is always bare regardless of
// ownership; provider qualification is something every caller (CLI included,
// see splitProviderQualifiedServiceRef) has to add on its own using the
// separate `provider`/`is_owner` fields, exactly as done here.
//
// A non-owned service only gets a slug at all when it's public: Registry's
// own slug lookup (resolveAccountScopedService) resolves a provider-qualified
// reference "only if it's public or the caller owns it" -- printing
// "@provider/slug" for a private service someone else owns would produce a
// reference that looks usable but 404s the moment it's actually run through
// `service <slug> show`. Returning "" here degrades that row to the CLI's
// existing UUID fallback instead of handing back a dead-end slug.
func displaySlug(vis sandbox.ServiceVisibility) string {
	if vis.IsOwner {
		return vis.Slug
	}
	if vis.IsPublic && vis.Provider.Handle != "" {
		return "@" + vis.Provider.Handle + "/" + vis.Slug
	}
	return ""
}

func listedServiceIDs(services []store.WorkspaceService) []uuid.UUID {
	ids := make([]uuid.UUID, len(services))
	for i := range services {
		ids[i] = services[i].ServiceID
	}
	return ids
}

// removeServiceHandler handles DELETE /workspace/services/{id}.
// Like addServiceHandler, it emits an OTEL span because removing a service is
// a workspace-wide admin action with compliance-level audit requirements.
func removeServiceHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, err := controlActorAccount(r.Context())
		if err != nil {
			http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
			return
		}

		rawID := chi.URLParam(r, "id")
		svcID, err := uuid.Parse(rawID)
		if err != nil {
			http.Error(w, `{"error":"id must be a valid UUID"}`, http.StatusBadRequest)
			return
		}

		if err := verifyWorkspaceActor(r.Context(), accountID); err != nil {
			http.Error(w, `{"error":"workspace not found"}`, http.StatusInternalServerError)
			return
		}

		ctx := r.Context()
		thread := observability.ThreadFromContext(ctx)
		step := thread.Step("engine.workspace.remove_service")
		step.AddContext(map[string]any{
			"user_action": "workspace.remove_service",
			"account_id":  accountID.String(),
			"service_id":  svcID.String(),
		})

		if err := s.RemoveWorkspaceService(ctx, svcID); err != nil {
			// Surface a 404 when the service was never in the workspace so the
			// client can distinguish "not found" from a real DB error.
			if errors.Is(err, store.ErrWorkspaceServiceNotFound) {
				step.AddContext(map[string]any{"outcome": "not_found"}).Error(ctx, err)
				http.Error(w, `{"error":"service not found in workspace"}`, http.StatusNotFound)
				return
			}
			step.AddContext(map[string]any{"outcome": "error"}).Error(ctx, err)
			http.Error(w, `{"error":"failed to remove service"}`, http.StatusInternalServerError)
			return
		}

		step.AddContext(map[string]any{"outcome": "success"}).Success(ctx)
		w.WriteHeader(http.StatusNoContent)
	}
}

// resolveWorkspaceID validates the caller's X-API-Key and resolves it to
// their workspace, exactly like addServiceHandler/removeServiceHandler do
// inline.
