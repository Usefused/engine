package api

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type EngineGRPCServer struct {
	enginev1.UnimplementedEngineServiceServer
	runtime   *sandbox.EngineGRPCServer
	store     store.Store
	verifier  ServiceVerifier
	masterKey []byte
}

// NewEngineGRPCServer composes the existing sandbox execution server with
// Engine-local auth management dependencies so SDK RPCs and UI GraphQL can
// share the same connect/session helpers.
func NewEngineGRPCServer(s store.Store, verifier ServiceVerifier, masterKey []byte) *EngineGRPCServer {
	return &EngineGRPCServer{
		runtime:   sandbox.NewEngineGRPCServer(),
		store:     s,
		verifier:  verifier,
		masterKey: masterKey,
	}
}

// Connect delegates to sandbox because SDK handshakes are still execution
// cache concerns, not connect-auth concerns.
func (s *EngineGRPCServer) Connect(ctx context.Context, req *enginev1.ConnectRequest) (*enginev1.ConnectResponse, error) {
	// Endpoint execution already lives in sandbox; keep this adapter focused on
	// adding auth-management RPCs instead of duplicating the runtime path.
	return s.runtime.Connect(ctx, req)
}

// Disconnect delegates to sandbox so cache refcount release semantics remain
// identical for old and new SDKs.
func (s *EngineGRPCServer) Disconnect(ctx context.Context, req *enginev1.DisconnectRequest) (*enginev1.DisconnectResponse, error) {
	// Delegation keeps cache refcount semantics exactly aligned with the
	// pre-existing SDK handshake implementation.
	return s.runtime.Disconnect(ctx, req)
}

// Execute delegates provider calls to sandbox, keeping auth-management RPCs
// from forking the runtime dispatch implementation.
func (s *EngineGRPCServer) Execute(req *enginev1.ExecuteRequest, stream enginev1.EngineService_ExecuteServer) error {
	// Provider calls retain the sandbox transport so retry, idempotency, and
	// dispatch observability stay on their single established path.
	return s.runtime.Execute(req, stream)
}

// StartConnectSession creates a browser authorization URL from the same
// Engine-owned session path used by REST/GraphQL, so SDKs never see OAuth app
// secrets or PKCE verifiers.
func (s *EngineGRPCServer) StartConnectSession(ctx context.Context, req *enginev1.StartConnectSessionRequest) (*enginev1.StartConnectSessionResponse, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.grpc.connect_session.start")
	defer span.End()

	call, err := s.connectAdminCallFromGRPC(ctx, req.GetBucketId(), req.GetServiceId())
	if err != nil {
		return nil, grpcConnectError(err)
	}
	createdByArtifactID, err := optionalUUIDValue(req.GetCreatedByArtifactId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "created_by_artifact_id must be a valid UUID")
	}
	endUserRef := strings.TrimSpace(req.GetEndUserRef())
	if endUserRef == "" {
		return nil, status.Error(codes.InvalidArgument, "end_user_ref is required")
	}
	returnURL := strings.TrimSpace(req.GetReturnUrl())
	if returnURL != "" && !isHTTPRedirectURI(returnURL) {
		return nil, status.Error(codes.InvalidArgument, "return_url must be an absolute http or https URL")
	}
	resolved, err := resolveConnectRuntimeConfig(ctx, s.store, s.verifier, call, s.masterKey)
	if err != nil {
		return nil, grpcConnectError(err)
	}
	response, err := createConnectSession(ctx, s.store, call, endUserRef, createdByArtifactID, returnURL, req.GetResourceInput(), req.GetScopes(), resolved, s.masterKey)
	if err != nil {
		return nil, grpcConnectError(err)
	}
	span.SetAttributes(append(connectAdminAttrs("connect.session.start", call), attribute.String("outcome", "success"), attribute.Int("scope_count", len(response.Scopes)))...)
	return &enginev1.StartConnectSessionResponse{
		AuthorizeUrl: response.AuthorizeURL,
		ExpiresAt:    formatProtoTime(response.ExpiresAt),
	}, nil
}

// GetConnection lets app servers read callback metadata by opaque connection
// ID while store-level bucket scoping prevents cross-workspace disclosure.
func (s *EngineGRPCServer) GetConnection(ctx context.Context, req *enginev1.GetConnectionRequest) (*enginev1.GetConnectionResponse, error) {
	connectionID, err := uuid.Parse(strings.TrimSpace(req.GetConnectionId()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "connection_id must be a valid UUID")
	}
	bucketIDs, err := s.actorBucketIDsFromGRPC(ctx)
	if err != nil {
		return nil, grpcConnectError(err)
	}
	conn, err := s.store.GetAuthConnectionByIDForBuckets(ctx, connectionID, bucketIDs)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to get auth connection")
	}
	if conn == nil {
		return &enginev1.GetConnectionResponse{Found: false}, nil
	}
	return &enginev1.GetConnectionResponse{Found: true, Connection: projectProtoAuthConnection(*conn)}, nil
}

// ListConnectionResources exposes only display/routing-selection metadata and
// proves connection ownership before querying its active resource rows.
func (s *EngineGRPCServer) ListConnectionResources(ctx context.Context, req *enginev1.ListConnectionResourcesRequest) (*enginev1.ListConnectionResourcesResponse, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.grpc.connection_resources.list")
	defer span.End()
	connectionID, err := uuid.Parse(strings.TrimSpace(req.GetConnectionId()))
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "connection_id must be a valid UUID")
	}
	bucketIDs, err := s.actorBucketIDsFromGRPC(ctx)
	if err != nil {
		return nil, grpcConnectError(err)
	}
	connection, err := s.store.GetAuthConnectionByIDForBuckets(ctx, connectionID, bucketIDs)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to resolve auth connection")
	}
	if connection == nil {
		return nil, status.Error(codes.NotFound, "auth connection not found")
	}
	resources, err := s.store.ListConnectionResources(ctx, connectionID)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to list connection resources")
	}
	span.SetAttributes(attribute.Int("resource_count", len(resources)))
	return &enginev1.ListConnectionResourcesResponse{Resources: projectProtoConnectionResources(resources)}, nil
}

// connectAdminCallFromGRPC normalizes the two IDs every connect mutation needs
// before entering the shared connect runtime helpers.
func (s *EngineGRPCServer) connectAdminCallFromGRPC(ctx context.Context, bucketIDRaw, serviceIDRaw string) (connectAdminCall, error) {
	bucketCall, err := s.bucketAdminCallFromGRPC(ctx, bucketIDRaw)
	if err != nil {
		return connectAdminCall{}, err
	}
	serviceID, err := uuid.Parse(strings.TrimSpace(serviceIDRaw))
	if err != nil {
		return connectAdminCall{}, status.Error(codes.InvalidArgument, "service_id must be a valid UUID")
	}
	return connectAdminCall{bucketID: bucketCall.bucketID, serviceID: serviceID}, nil
}

// bucketAdminCallFromGRPC authenticates the SDK token and proves bucket
// ownership before a gRPC caller can create or inspect bucket-attached auth.
func (s *EngineGRPCServer) bucketAdminCallFromGRPC(ctx context.Context, bucketIDRaw string) (bucketAdminCall, error) {
	err := s.workspaceFromGRPC(ctx)
	if err != nil {
		return bucketAdminCall{}, err
	}
	bucketID, err := uuid.Parse(strings.TrimSpace(bucketIDRaw))
	if err != nil {
		return bucketAdminCall{}, status.Error(codes.InvalidArgument, "bucket_id must be a valid UUID")
	}
	// Bucket ownership is checked before connect mutations so SDK-created
	// sessions cannot target auth material outside the caller's workspace.
	if _, err := s.store.GetBucket(ctx, bucketID); err != nil {
		if errors.Is(err, store.ErrBucketNotFound) {
			return bucketAdminCall{}, status.Error(codes.NotFound, "bucket not found")
		}
		return bucketAdminCall{}, fmt.Errorf("resolve bucket: %w", err)
	}
	return bucketAdminCall{bucketID: bucketID}, nil
}

// workspaceFromGRPC validates the metadata token once and verifies the
// resolved account owns the Engine's singleton workspace.
func (s *EngineGRPCServer) workspaceFromGRPC(ctx context.Context) error {
	token := grpcAPIKey(ctx)
	accountID, err := validateAPIKey(ctx, s.store, token)
	if err != nil {
		return status.Error(codes.Unauthenticated, "unauthorized")
	}
	if err := s.store.VerifyWorkspaceOwner(ctx, accountID); err != nil {
		return status.Error(codes.Internal, "failed to resolve workspace")
	}
	return nil
}

// actorBucketIDsFromGRPC scopes opaque connection-id lookup to buckets owned
// by the caller instead of trusting the caller to provide a bucket id.
func (s *EngineGRPCServer) actorBucketIDsFromGRPC(ctx context.Context) ([]uuid.UUID, error) {
	err := s.workspaceFromGRPC(ctx)
	if err != nil {
		return nil, err
	}
	buckets, err := s.store.ListBuckets(ctx)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to list buckets")
	}
	bucketIDs := make([]uuid.UUID, 0, len(buckets))
	for _, bucket := range buckets {
		bucketIDs = append(bucketIDs, bucket.ID)
	}
	return bucketIDs, nil
}

// grpcAPIKey accepts both native gRPC metadata and bearer-style edge metadata
// so deployed gateways can forward whichever credential form they already use.
func grpcAPIKey(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if vals := md.Get("x-api-key"); len(vals) > 0 {
		return vals[0]
	}
	if vals := md.Get("authorization"); len(vals) > 0 {
		return strings.TrimPrefix(vals[0], "Bearer ")
	}
	return ""
}

// grpcConnectError converts HTTP-shaped connect errors from shared helpers into
// gRPC status codes without exposing internal error strings to SDK users.
func grpcConnectError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := status.FromError(err); ok {
		return err
	}
	var httpErr connectRuntimeHTTPError
	if errors.As(err, &httpErr) {
		return status.Error(grpcCodeFromHTTPStatus(httpErr.status), httpErr.message)
	}
	return status.Error(codes.Internal, "connect auth failed")
}

// grpcCodeFromHTTPStatus preserves the coarse semantics callers need for
// retries/UX while hiding implementation-specific connect failures.
func grpcCodeFromHTTPStatus(statusCode int) codes.Code {
	switch statusCode {
	case 400:
		return codes.InvalidArgument
	case 401:
		return codes.Unauthenticated
	case 403:
		return codes.PermissionDenied
	case 404:
		return codes.NotFound
	default:
		return codes.Internal
	}
}

// projectProtoAuthConnection maps only non-sensitive connection lifecycle
// fields into the SDK-facing proto response.
func projectProtoAuthConnection(conn store.AuthConnection) *enginev1.AuthConnection {
	resp := projectAuthConnection(conn)
	return &enginev1.AuthConnection{
		Id:                    resp.ID.String(),
		BucketId:              resp.BucketID.String(),
		ServiceId:             resp.ServiceID.String(),
		EndUserRef:            resp.EndUserRef,
		CreatedByArtifactId:   resp.CreatedByArtifactID.String(),
		AuthType:              resp.AuthType,
		TokenType:             resp.TokenType,
		Scopes:                resp.Scopes,
		ScopeSource:           resp.ScopeSource,
		Issuer:                resp.Issuer,
		Subject:               resp.Subject,
		ExpiresAt:             formatOptionalProtoTime(resp.ExpiresAt),
		RefreshTokenExpiresAt: formatOptionalProtoTime(resp.RefreshTokenExpiresAt),
		LastUsedAt:            formatOptionalProtoTime(resp.LastUsedAt),
		RefreshState:          resp.RefreshState,
		CreatedAt:             formatProtoTime(resp.CreatedAt),
		UpdatedAt:             formatProtoTime(resp.UpdatedAt),
	}
}

// projectProtoConnectionResources intentionally omits base URLs and provider
// metadata; SDK callers need only opaque IDs and human-readable choices.
func projectProtoConnectionResources(resources []store.ConnectionResource) []*enginev1.ConnectionResource {
	items := make([]*enginev1.ConnectionResource, 0, len(resources))
	for _, resource := range resources {
		items = append(items, &enginev1.ConnectionResource{
			Id: resource.ID.String(), ConnectionId: resource.ConnectionID.String(), ServiceId: resource.ServiceID.String(),
			ResourceType: resource.ResourceType, DisplayName: resource.DisplayName, Scopes: resource.Scopes,
			IsDefault: resource.IsDefault, CreatedAt: formatProtoTime(resource.CreatedAt), UpdatedAt: formatProtoTime(resource.UpdatedAt),
		})
	}
	return items
}

// formatProtoTime keeps timestamp strings consistent across generated SDKs and
// avoids forcing extra protobuf well-known type handling into dynamic clients.
func formatProtoTime(t time.Time) string {
	return t.UTC().Format(time.RFC3339Nano)
}

// formatOptionalProtoTime preserves absent provider dates as empty strings so
// SDKs can distinguish missing metadata from real zero times.
func formatOptionalProtoTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatProtoTime(*t)
}
