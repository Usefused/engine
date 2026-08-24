package api

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/engine/auth"
	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/messaging"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type EngineGRPCServer struct {
	enginev1.UnimplementedEngineServiceServer
	runtime        *sandbox.EngineGRPCServer
	unifiedRuntime unifiedPhysicalRuntime
	restRuntime    restExecutionRuntime
	store          store.Store
	verifier       ServiceVerifier
	masterKey      []byte
	// configStore and natsClient are only needed by SubscribeWebhooks
	// (webhook_grpc_handler.go) -- resolving a connecting SDK/MCP's
	// webhook_attachment label and bridging to the NATS JetStream durable
	// consumer and queue group.
	configStore    store.ConfigRepository
	natsClient     *messaging.NATSClient
	tokenValidator auth.TokenValidator
}

// NewEngineGRPCServer requires the process-shared validator so SDK execution,
// MCP execution, and revocation can never accidentally use separate caches.
func NewEngineGRPCServer(s store.Store, verifier ServiceVerifier, masterKey []byte, configStore store.ConfigRepository, natsClient *messaging.NATSClient, tokenValidator auth.TokenValidator) *EngineGRPCServer {
	runtime := sandbox.NewEngineGRPCServer()
	return &EngineGRPCServer{
		runtime:        runtime,
		unifiedRuntime: runtime,
		restRuntime:    runtime,
		store:          s,
		verifier:       verifier,
		masterKey:      masterKey,
		configStore:    configStore,
		natsClient:     natsClient,
		tokenValidator: tokenValidator,
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
	// Early authentication and validation exits retain a bounded audit outcome;
	// successful persistence replaces it through the shared start projection.
	span.SetAttributes(attribute.String("outcome", "failed"))

	call, appID, err := s.authenticatedConnectCallFromGRPC(ctx, req.GetBucketId(), req.GetServiceId())
	if err != nil {
		return nil, grpcConnectError(err)
	}
	createdByAppID, err := optionalUUIDValue(req.GetCreatedByAppId())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "created_by_app_id must be a valid UUID")
	}
	if createdByAppID != uuid.Nil && createdByAppID != appID {
		return nil, status.Error(codes.PermissionDenied, "created_by_app_id must match the authenticated app")
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
	response, err := createConnectSession(ctx, s.store, call, endUserRef, appID, returnURL, req.GetResourceInput(), req.GetScopes(), resolved, s.masterKey)
	if err != nil {
		return nil, grpcConnectError(err)
	}
	span.SetAttributes(append(connectAdminAttrs("connect.session.start", call), connectSessionStartTelemetry(response)...)...)
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
	scope, err := s.authenticateAppFromGRPC(ctx)
	if err != nil {
		return nil, grpcConnectError(err)
	}
	conn, err := s.store.GetAuthConnectionByIDForBuckets(ctx, connectionID, []uuid.UUID{scope.BucketID})
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to get auth connection")
	}
	if conn == nil || !appRuntimeSelectsService(scope.Selections, conn.ServiceID) {
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
	scope, err := s.authenticateAppFromGRPC(ctx)
	if err != nil {
		return nil, grpcConnectError(err)
	}
	connection, err := s.store.GetAuthConnectionByIDForBuckets(ctx, connectionID, []uuid.UUID{scope.BucketID})
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to resolve auth connection")
	}
	if connection == nil || !appRuntimeSelectsService(scope.Selections, connection.ServiceID) {
		return nil, status.Error(codes.NotFound, "auth connection not found")
	}
	resources, err := s.store.ListConnectionResources(ctx, connectionID)
	if err != nil {
		return nil, status.Error(codes.Internal, "failed to list connection resources")
	}
	span.SetAttributes(attribute.Int("resource_count", len(resources)))
	return &enginev1.ListConnectionResourcesResponse{Resources: projectProtoConnectionResources(resources)}, nil
}

// authenticatedConnectCallFromGRPC binds a runtime call to the immutable
// app version authenticated by x-app-id plus its family token.
func (s *EngineGRPCServer) authenticatedConnectCallFromGRPC(ctx context.Context, bucketIDRaw, serviceIDRaw string) (connectAdminCall, uuid.UUID, error) {
	scope, err := s.authenticateAppFromGRPC(ctx)
	if err != nil {
		return connectAdminCall{}, uuid.Nil, err
	}
	bucketID, err := uuid.Parse(strings.TrimSpace(bucketIDRaw))
	if err != nil {
		return connectAdminCall{}, uuid.Nil, status.Error(codes.InvalidArgument, "bucket_id must be a valid UUID")
	}
	serviceID, err := uuid.Parse(strings.TrimSpace(serviceIDRaw))
	if err != nil {
		return connectAdminCall{}, uuid.Nil, status.Error(codes.InvalidArgument, "service_id must be a valid UUID")
	}
	if scope.BucketID != bucketID || !appRuntimeSelectsService(scope.Selections, serviceID) {
		return connectAdminCall{}, uuid.Nil, status.Error(codes.PermissionDenied, "app scope does not allow this bucket and service")
	}
	return connectAdminCall{bucketID: bucketID, serviceID: serviceID}, scope.AppID, nil
}

// authenticateAppFromGRPC deliberately avoids control credentials: SDK
// runtime identity is the exact app ID plus a token issued for its family.
func (s *EngineGRPCServer) authenticateAppFromGRPC(ctx context.Context) (*store.AppRuntime, error) {
	scope, _, err := s.authenticatedAppRuntimeFromGRPC(ctx)
	return scope, err
}

// authenticatedAppRuntimeFromGRPC returns the immutable app scope and validated token identity from one authentication pass.
func (s *EngineGRPCServer) authenticatedAppRuntimeFromGRPC(ctx context.Context) (*store.AppRuntime, auth.RuntimeIdentity, error) {
	appID, err := uuid.Parse(strings.TrimSpace(grpcAppID(ctx)))
	if err != nil {
		return nil, auth.RuntimeIdentity{}, status.Error(codes.Unauthenticated, "app authentication is required")
	}
	identity, err := s.tokenValidator.Validate(ctx, appID, grpcAPIKey(ctx))
	if err != nil {
		return nil, auth.RuntimeIdentity{}, status.Error(codes.Unauthenticated, "app authentication failed")
	}
	scope, err := s.store.GetAppRuntime(ctx, appID)
	if err != nil || scope == nil || scope.AccountID != identity.AccountID || scope.AppID != identity.AppID || scope.AppID != appID || scope.BucketID == uuid.Nil {
		return nil, auth.RuntimeIdentity{}, status.Error(codes.PermissionDenied, "app scope is unavailable")
	}
	if _, err := models.DecodeAppSelections(scope.ScopeSchemaVersion, scope.Selections); err != nil {
		return nil, auth.RuntimeIdentity{}, status.Error(codes.PermissionDenied, "app scope is unavailable")
	}
	return scope, identity, nil
}

func appRuntimeSelectsService(raw []byte, serviceID uuid.UUID) bool {
	selections, err := models.DecodeAppSelections(models.AppScopeSchemaVersion, raw)
	if err != nil {
		return false
	}
	for _, selection := range selections {
		if selection.ServiceID == serviceID {
			return true
		}
	}
	return false
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

// grpcAppID reads the exact app version from call metadata.
func grpcAppID(ctx context.Context) string {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return ""
	}
	if vals := md.Get("x-app-id"); len(vals) > 0 {
		return vals[0]
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
		CreatedByAppId:        resp.CreatedByAppID.String(),
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
		ServiceVersionId:      formatOptionalProtoUUID(resp.ServiceVersionID),
		AuthName:              resp.AuthName,
		LastRefreshAttemptAt:  formatOptionalProtoTime(resp.LastRefreshAttemptAt),
		LastRefreshedAt:       formatOptionalProtoTime(resp.LastRefreshedAt),
		RefreshRetryNotBefore: formatOptionalProtoTime(resp.RefreshRetryNotBefore),
		LastFailureCode:       resp.LastFailureCode,
		LastFailureAt:         formatOptionalProtoTime(resp.LastFailureAt),
		LastFailureTraceId:    resp.LastFailureTraceID,
	}
}

// formatOptionalProtoUUID renders absent legacy identities as the protobuf
// string zero value without inventing a service-version association.
func formatOptionalProtoUUID(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
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
