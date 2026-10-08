package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/mcpcatalog"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type mcpDiscoveryInput struct {
	URL        string `json:"url"`
	BucketName string `json:"bucket_name,omitempty"`
	SecretName string `json:"secret_name,omitempty"`
}
type mcpCatalogRequest struct {
	scope      store.MCPCatalogScope
	repository store.MCPCatalogStore
}

// admitMCPCatalog enforces service permission and exact active-version membership even when mounted without route middleware.
func admitMCPCatalog(w http.ResponseWriter, r *http.Request, s store.Store, permission accesscontrol.Permission) (mcpCatalogRequest, bool) {
	actor, ok := accesscontrol.ActorFromContext(r.Context())
	// App tokens and missing actor identities cannot use control-plane discovery.
	if !ok || actor.SubjectID == uuid.Nil || actor.WorkspaceID == uuid.Nil || actor.Kind == accesscontrol.SubjectApp {
		accesscontrol.WriteAuthorizationError(w, accesscontrol.ErrAuthenticationRequired, r.Context())
		return mcpCatalogRequest{}, false
	}
	path, err := parseRefreshServiceContractPath(r)
	// Malformed service or version IDs cannot select a catalog scope.
	if err != nil {
		writeMCPCatalogError(w, r, err)
		return mcpCatalogRequest{}, false
	}
	err = accesscontrol.AuthorizeAll(r.Context(), accesscontrol.SnapshotAuthorizer{}, accesscontrol.Requirement{Permission: permission, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: path.serviceID}})
	// The handler repeats the route boundary so direct internal mounts cannot bypass it.
	if err != nil {
		accesscontrol.WriteAuthorizationError(w, err, r.Context())
		return mcpCatalogRequest{}, false
	}
	repository, err := activeMCPCatalogRepository(r.Context(), s, path)
	// Local storage admission must succeed before any provider request can begin.
	if err != nil {
		writeMCPCatalogError(w, r, err)
		return mcpCatalogRequest{}, false
	}
	return mcpCatalogRequest{scope: store.MCPCatalogScope{ServiceID: path.serviceID, VersionID: path.serviceVersionID, SubjectID: actor.SubjectID}, repository: repository}, true
}

// admitOwnedMCPCatalog separates workspace management authority from Registry-owned service authorship.
func admitOwnedMCPCatalog(w http.ResponseWriter, r *http.Request, s store.Store, ownership ServiceVisibilityResolver) (mcpCatalogRequest, bool) {
	call, ok := admitMCPCatalog(w, r, s, accesscontrol.PermissionServiceManage)
	// Existing authentication, permission, and version failures already have a safe response.
	if !ok {
		return mcpCatalogRequest{}, false
	}
	actor, _ := accesscontrol.ActorFromContext(r.Context())
	err := accesscontrol.AuthorizeAll(r.Context(), accesscontrol.SnapshotAuthorizer{}, accesscontrol.Requirement{Permission: accesscontrol.PermissionCatalogueImport, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}})
	// Import permission is independent of permission to manage workspace service membership.
	if err != nil {
		accesscontrol.WriteAuthorizationError(w, err, r.Context())
		return mcpCatalogRequest{}, false
	}
	// Discovery and apply both revalidate ownership before touching credentials or catalog drafts.
	if err = requireMCPCatalogOwner(r.Context(), ownership, call.scope.ServiceID); err != nil {
		writeMCPCatalogError(w, r, err)
		return mcpCatalogRequest{}, false
	}
	return call, true
}

// requireMCPCatalogOwner fails closed when the authoritative Registry cannot confirm the publisher.
func requireMCPCatalogOwner(ctx context.Context, ownership ServiceVisibilityResolver, serviceID uuid.UUID) error {
	// A missing verifier is a deployment limitation, never an implicit ownership grant.
	if ownership == nil {
		return refreshHTTPError{status: http.StatusServiceUnavailable, message: "Service ownership could not be verified. Try again later."}
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// The Registry client supplies the Engine licence identity; no browser credential is forwarded.
	services, err := ownership.FetchServiceVisibility(ctx, []uuid.UUID{serviceID}, "")
	// Upstream failures must not grant import rights or leak Registry response bodies.
	if err != nil {
		return refreshHTTPError{status: http.StatusServiceUnavailable, message: "Service ownership could not be verified. Try again later."}
	}
	service, exists := services[serviceID]
	// Neither workspace activation nor a public catalog entry proves service ownership.
	if !exists || !service.IsOwner {
		return refreshHTTPError{status: http.StatusForbidden, message: "Only the service owner can import an MCP server."}
	}
	return nil
}

// activeMCPCatalogRepository admits only a usable Engine-local version with durable catalog storage.
func activeMCPCatalogRepository(ctx context.Context, s store.Store, path refreshServiceContractPath) (store.MCPCatalogStore, error) {
	activeStore, ok := s.(store.WorkspaceServiceVersionStatusStore)
	// Stores without exact-version admission must fail closed.
	if !ok {
		return nil, store.ErrMCPCatalogUnavailable
	}
	active, err := activeStore.IsWorkspaceServiceVersionActive(ctx, path.serviceID, path.serviceVersionID)
	// Membership lookup failures cannot imply that a public Registry version is active locally.
	if err != nil {
		return nil, err
	}
	// A browsable Registry version is not sufficient authority for an Engine-local attachment.
	if !active {
		return nil, refreshHTTPError{status: 404, message: "Enable this service version in the workspace before importing an MCP server."}
	}
	repository, ok := s.(store.MCPCatalogStore)
	// A successful discovery is useless without durable server-held preview storage.
	if !ok {
		return nil, store.ErrMCPCatalogUnavailable
	}
	return repository, nil
}

// readAuthorizedMCPCatalog revokes access to credential-derived metadata as soon as bucket use is revoked.
func readAuthorizedMCPCatalog(ctx context.Context, call mcpCatalogRequest) (*store.MCPCatalogSnapshot, error) {
	snapshot, err := call.repository.GetMCPCatalog(ctx, call.scope)
	// Missing snapshots are a valid first-import state; failures remain distinguishable.
	if err != nil || snapshot == nil {
		return snapshot, err
	}
	// Credential-derived definitions remain visible only while the actor can use their bucket.
	if err = authorizeMCPCatalogBucket(ctx, snapshot.BucketID); err != nil {
		return nil, err
	}
	return snapshot, nil
}

// GetMCPCatalogHandler returns a saved private snapshot without contacting any provider.
func GetMCPCatalogHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		call, ok := admitMCPCatalog(w, r, s, accesscontrol.PermissionServiceRead)
		// Authorization failures have already written the shared safe error envelope.
		if !ok {
			return
		}
		snapshot, err := readAuthorizedMCPCatalog(r.Context(), call)
		// Storage and authorization failures cannot be presented as an empty saved catalog.
		if err != nil {
			writeMCPCatalogError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, map[string]any{"catalog": snapshot})
	}
}

// DiscoverMCPCatalogHandler saves only a complete server-held preview; no provider tool or resource is executed.
func DiscoverMCPCatalogHandler(s store.Store, masterKey []byte, ownership ServiceVisibilityResolver) http.HandlerFunc {
	return discoverMCPCatalogHandler(s, masterKey, mcpcatalog.Discover, ownership)
}

// discoverMCPCatalogHandler injects protocol discovery for deterministic HTTP tests while production uses the restricted client.
func discoverMCPCatalogHandler(s store.Store, masterKey []byte, discover func(context.Context, string, string) (mcpcatalog.Catalog, error), ownership ServiceVisibilityResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
		defer cancel()
		r = r.WithContext(ctx)
		call, ok := admitOwnedMCPCatalog(w, r, s, ownership)
		// Failed admission stops before provider access or persistence can occur.
		if !ok {
			return
		}
		var input mcpDiscoveryInput
		// Strict decoding prevents misspelled credential fields from silently selecting anonymous access.
		if err := decodeMCPCatalogBody(w, r, &input); err != nil {
			writeMCPCatalogError(w, r, err)
			return
		}
		// Reject unsafe or credential-bearing URLs before accessing credentials or sending requests.
		if err := mcpcatalog.ValidateEndpoint(input.URL); err != nil {
			writeMCPCatalogError(w, r, err)
			return
		}
		prior, err := readAuthorizedMCPCatalog(ctx, call)
		// A refresh diff must be based on an accessible, successfully loaded prior revision.
		if err != nil {
			writeMCPCatalogError(w, r, err)
			return
		}
		bucketID, token, err := resolveMCPCatalogToken(ctx, s, masterKey, input)
		// Credential resolution must finish successfully before the provider is contacted.
		if err != nil {
			writeMCPCatalogError(w, r, err)
			return
		}
		catalog, err := discover(ctx, input.URL, token)
		// Credential resolution must finish successfully before the provider is contacted.
		if err != nil {
			writeMCPCatalogError(w, r, err)
			return
		}
		draft := store.MCPCatalogDraft{MCPCatalogSnapshot: store.MCPCatalogSnapshot{ID: uuid.New(), URL: input.URL, BucketName: input.BucketName, SecretName: input.SecretName, BucketID: bucketID, CreatedAt: time.Now().UTC(), Catalog: catalog}, ExpiresAt: time.Now().UTC().Add(15 * time.Minute)}
		before := mcpcatalog.Catalog{}
		// The first import uses an empty base; later previews pin the exact previously approved revision.
		if prior != nil {
			draft.BaseID = &prior.ID
			before = prior.Catalog
		}
		draft.Changes = mcpcatalog.Diff(before, catalog)
		// Only a durably stored preview may be offered for approval.
		if err = call.repository.SaveMCPCatalogDraft(ctx, call.scope, draft); err != nil {
			writeMCPCatalogError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, draft)
	}
}

// ApplyMCPCatalogHandler promotes only a server-held draft under its original service, version, and actor scope.
func ApplyMCPCatalogHandler(s store.Store, ownership ServiceVisibilityResolver) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		call, ok := admitOwnedMCPCatalog(w, r, s, ownership)
		// Failed admission stops before provider access or persistence can occur.
		if !ok {
			return
		}
		var input struct {
			DraftID uuid.UUID `json:"draft_id"`
		}
		// Apply accepts only one exact server-issued draft ID, never browser-authored definitions.
		if err := decodeMCPCatalogBody(w, r, &input); err != nil {
			writeMCPCatalogError(w, r, err)
			return
		}
		draft, err := call.repository.GetMCPCatalogDraft(r.Context(), call.scope, input.DraftID)
		// Only an existing server-held draft can reach the approval boundary.
		if err != nil {
			writeMCPCatalogError(w, r, err)
			return
		}
		// Credential grants are rechecked at apply, not merely when discovery began.
		if err = authorizeMCPCatalogBucket(r.Context(), draft.BucketID); err != nil {
			accesscontrol.WriteAuthorizationError(w, err, r.Context())
			return
		}
		snapshot, err := call.repository.ApplyMCPCatalogDraft(r.Context(), call.scope, input.DraftID)
		// A stale preview or failed transaction must leave the currently approved snapshot selected.
		if err != nil {
			writeMCPCatalogError(w, r, err)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, snapshot)
	}
}

// decodeMCPCatalogBody bounds request size and rejects trailing JSON or unknown fields.
func decodeMCPCatalogBody(w http.ResponseWriter, r *http.Request, target any) error {
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16384))
	decoder.DisallowUnknownFields()
	// Reject incomplete or misspelled requests before selecting credentials or a draft.
	if err := decoder.Decode(target); err != nil {
		return refreshHTTPError{status: 400, message: "invalid MCP catalog request"}
	}
	// A second JSON value must never influence a supposedly reviewed action.
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		return refreshHTTPError{status: 400, message: "invalid MCP catalog request"}
	}
	return nil
}

// authorizeMCPCatalogBucket requires explicit use access for metadata obtained through a bucket's credential.
func authorizeMCPCatalogBucket(ctx context.Context, id uuid.UUID) error {
	// Anonymous discovery does not invent a default bucket or grant.
	if id == uuid.Nil {
		return nil
	}
	return accesscontrol.AuthorizeAll(ctx, accesscontrol.SnapshotAuthorizer{}, accesscontrol.Requirement{Permission: accesscontrol.PermissionBucketUse, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceBucket, ID: id}})
}

// resolveMCPCatalogToken reads an exact generic bucket secret after authorization, without creating buckets or copying credentials into previews.
func resolveMCPCatalogToken(ctx context.Context, s store.Store, masterKey []byte, input mcpDiscoveryInput) (uuid.UUID, string, error) {
	// Both omitted fields select anonymous discovery; partial references are invalid.
	if input.BucketName == "" && input.SecretName == "" {
		return uuid.Nil, "", nil
	}
	// A partial credential reference must never silently fall back to anonymous discovery.
	if input.BucketName == "" || input.SecretName == "" {
		return uuid.Nil, "", refreshHTTPError{status: 400, message: "bucket_name and secret_name are both required"}
	}
	key, err := bucketSecretStorageKey(input.SecretName)
	// Generic secret references must remain within their dedicated namespace.
	if err != nil {
		return uuid.Nil, "", refreshHTTPError{status: 400, message: "invalid bucket secret name"}
	}
	bucket, err := resolveMCPCatalogBucket(ctx, s, input.BucketName)
	// Credential destination and bucket authority are checked before any secret lookup.
	if err != nil {
		return uuid.Nil, "", err
	}
	secret, err := s.GetSecret(ctx, bucket.ID, uuid.Nil, key)
	// Missing credential material cannot be interpreted as an anonymous connection.
	if err != nil || secret == nil {
		return uuid.Nil, "", refreshHTTPError{status: 422, message: "bucket credential is unavailable"}
	}
	token, err := decryptMCPCatalogToken(masterKey, secret)
	return bucket.ID, token, err
}

// resolveMCPCatalogBucket requires credential-management authority because the actor chooses a new destination for the token.
func resolveMCPCatalogBucket(ctx context.Context, s store.Store, name string) (*store.Bucket, error) {
	actor, _ := accesscontrol.ActorFromContext(ctx)
	// Bucket runtime-use authority alone does not permit forwarding an existing secret to arbitrary endpoints.
	if err := accesscontrol.AuthorizeAll(ctx, accesscontrol.SnapshotAuthorizer{}, accesscontrol.Requirement{Permission: accesscontrol.PermissionCredentialsManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: actor.WorkspaceID}}); err != nil {
		return nil, err
	}
	bucket, err := s.GetBucketByName(ctx, name)
	// Missing buckets never trigger creation or a fallback to another bucket.
	if err != nil || bucket == nil {
		return nil, refreshHTTPError{status: 422, message: "bucket credential is unavailable"}
	}
	// Credential-derived definitions remain visible only while the actor can use their bucket.
	if err = authorizeMCPCatalogBucket(ctx, bucket.ID); err != nil {
		return nil, err
	}
	return bucket, nil
}

// decryptMCPCatalogToken admits only unexpired, valid header material and never returns cryptographic error detail.
func decryptMCPCatalogToken(masterKey []byte, secret *store.WorkspaceSecret) (string, error) {
	// Expired secrets may remain as metadata but cannot authorize discovery.
	if secret.ExpiresAt != nil && !secret.ExpiresAt.After(time.Now()) {
		return "", refreshHTTPError{status: 422, message: "bucket credential has expired"}
	}
	dek, err := store.UnwrapDEK(masterKey, secret.EncryptedDEK)
	// Key-management errors must not reveal ciphertext or configuration details.
	if err != nil {
		return "", store.ErrMCPCatalogUnavailable
	}
	token, err := store.DecryptWithDEK(dek, secret.EncryptedValue)
	// Invalid header material is rejected without reflecting secret bytes.
	if err != nil || !mcpcatalog.ValidToken(token) {
		return "", refreshHTTPError{status: 422, message: "bucket credential is invalid"}
	}
	return token, nil
}

// writeMCPCatalogError emits stable user guidance without returning raw provider URLs, bodies, tokens, or database diagnostics.
func writeMCPCatalogError(w http.ResponseWriter, r *http.Request, err error) {
	// Permission failures retain the application's standard missing-grant response.
	if errors.Is(err, accesscontrol.ErrPermissionDenied) || errors.Is(err, accesscontrol.ErrAuthenticationRequired) {
		accesscontrol.WriteAuthorizationError(w, err, r.Context())
		return
	}
	status, code, message := 500, "mcp_catalog_failed", "The MCP catalog could not be saved or loaded. Reload the saved catalog before retrying."
	var input refreshHTTPError
	// Only controlled local validation errors are eligible for direct display.
	switch {
	case errors.As(err, &input):
		status, code, message = input.status, "mcp_catalog_request_invalid", input.message
	case errors.Is(err, store.ErrMCPCatalogConflict):
		status, code, message = 409, "mcp_catalog_preview_stale", store.ErrMCPCatalogConflict.Error()
	case errors.Is(err, mcpcatalog.ErrEndpoint):
		status, code, message = 400, "mcp_catalog_endpoint_invalid", mcpcatalog.ErrEndpoint.Error()
	case errors.Is(err, mcpcatalog.ErrInvalidCatalog):
		status, code, message = 422, "mcp_catalog_invalid", mcpcatalog.ErrInvalidCatalog.Error()
	case errors.Is(err, mcpcatalog.ErrDiscovery):
		status, code, message = 502, "mcp_catalog_discovery_failed", mcpcatalog.ErrDiscovery.Error()
	}
	// No request payload or untrusted provider error text enters this shared envelope.
	writeWorkspaceConfigError(w, workspaceConfigHTTPError{status: status, code: code, message: strings.TrimSpace(message), remediation: "Check the connection and discover again when ready."}, r.Context())
}
