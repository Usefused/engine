package sandbox

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

const (
	// RegistrySourceLocal identifies services and specs stored in the customer's Enterprise Local Registry.
	RegistrySourceLocal = "local"
	// RegistrySourceCloud identifies services resolved from Fused Cloud Registry.
	RegistrySourceCloud = "cloud"
)

// ErrLocalRegistryUnauthorized indicates Local Registry was invoked without an active Enterprise entitlement.
var ErrLocalRegistryUnauthorized = errors.New("Local Registry requires an active Enterprise license")

type registryTargetContextKey struct{}
type registrySourceOutContextKey struct{}

// RegistrySourceCell captures which Registry ('local' or 'cloud') resolved an unpinned operation.
type RegistrySourceCell struct {
	mu     sync.Mutex
	source string
}

// Record stores the winning Registry source ('local' or 'cloud') in a thread-safe cell.
func (c *RegistrySourceCell) Record(source string) {
	// Nil cells allow callers that do not persist provenance to omit context decoration.
	if c == nil || (source != RegistrySourceLocal && source != RegistrySourceCloud) {
		return
	}
	c.mu.Lock()
	c.source = source
	c.mu.Unlock()
}

// Value returns the recorded Registry source or empty string when unset.
func (c *RegistrySourceCell) Value() string {
	// Nil cells safely report an unset provenance value.
	if c == nil {
		return ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.source
}

// WithRegistryTarget pins all Registry client calls on ctx to an explicit 'local' or 'cloud' target.
func WithRegistryTarget(ctx context.Context, target string) context.Context {
	normalized := strings.ToLower(strings.TrimSpace(target))
	// Ignore invalid targets rather than poisoning downstream context state.
	if normalized != RegistrySourceLocal && normalized != RegistrySourceCloud {
		return ctx
	}
	return context.WithValue(ctx, registryTargetContextKey{}, normalized)
}

// RegistryTargetFromContext returns any explicit 'local' or 'cloud' Registry target attached to ctx.
func RegistryTargetFromContext(ctx context.Context) string {
	// Guard nil contexts in unit tests and background helpers.
	if ctx == nil {
		return ""
	}
	target, _ := ctx.Value(registryTargetContextKey{}).(string)
	return target
}

// WithRegistrySourceOut attaches a mutable cell to ctx so callers learn whether Local or Cloud Registry answered.
func WithRegistrySourceOut(ctx context.Context) (context.Context, *RegistrySourceCell) {
	cell := &RegistrySourceCell{}
	return context.WithValue(ctx, registrySourceOutContextKey{}, cell), cell
}

// recordContextRegistrySource records source into any RegistrySourceCell carried on ctx.
func recordContextRegistrySource(ctx context.Context, source string) {
	// Skip recording when the caller did not request provenance capture.
	if ctx == nil {
		return
	}
	if cell, ok := ctx.Value(registrySourceOutContextKey{}).(*RegistrySourceCell); ok {
		cell.Record(source)
	}
}

// ServiceRegistrySourceResolver looks up pinned 'local' or 'cloud' provenance for activated workspace services.
type ServiceRegistrySourceResolver func(ctx context.Context, serviceIDs []uuid.UUID) (map[uuid.UUID]string, error)

// CloudFallbackAuthorizer reports whether ctx is authorized to fall back to Fused Cloud Registry on a Local miss.
type CloudFallbackAuthorizer func(ctx context.Context) bool

// InlineSDKContractBuilder exports local snapshots for Cloud-pinned services in mixed SDK builds.
type InlineSDKContractBuilder func(ctx context.Context, refs []store.ServiceContractMetadataRef) ([]models.InlineSDKServiceContract, error)

// SetInlineSDKContractBuilder wires snapshot export so mixed Local+Cloud SDKs compile offline on Local Registry.
func (c *HTTPRegistryClient) SetInlineSDKContractBuilder(builder InlineSDKContractBuilder) {
	c.inlineBuilder = builder
}

// NewLocalHTTPRegistryClient constructs an HTTPRegistryClient for Enterprise Local Registry, allowing loopback HTTP.
func NewLocalHTTPRegistryClient(endpoint, licenseKey string) *HTTPRegistryClient {
	normalized := normalizeLocalRegistryGraphQLEndpoint(endpoint)
	if !allowedLocalRegistryTransport(normalized) {
		slog.Error("FATAL: Engine is configured with an insecure non-loopback http:// Local Registry URL in production. Use https:// or loopback.")
		os.Exit(1)
	}
	transport := &http.Transport{
		MaxIdleConnsPerHost: 32,
		IdleConnTimeout:     90 * time.Second,
		ForceAttemptHTTP2:   true,
	}
	return &HTTPRegistryClient{
		endpoint:   normalized,
		licenseKey: licenseKey,
		httpClient: &http.Client{
			Timeout:   10 * time.Second,
			Transport: transport,
		},
	}
}

// normalizeLocalRegistryGraphQLEndpoint appends /graphql when operators configure a bare Local Registry base URL.
func normalizeLocalRegistryGraphQLEndpoint(endpoint string) string {
	trimmed := strings.TrimRight(strings.TrimSpace(endpoint), "/")
	// Leave empty or already-qualified /graphql endpoints unchanged.
	if trimmed == "" || strings.HasSuffix(trimmed, "/graphql") {
		return trimmed
	}
	return trimmed + "/graphql"
}

// allowedLocalRegistryTransport permits HTTPS everywhere, development HTTP, and loopback HTTP for co-located sidecars.
func allowedLocalRegistryTransport(endpoint string) bool {
	// Explicit development mode permits any local test transport.
	if os.Getenv("FUSED_ENV") == "development" {
		return true
	}
	parsed, err := url.Parse(strings.TrimSpace(endpoint))
	// Malformed URLs fail closed before sending license headers.
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return false
	}
	if strings.EqualFold(parsed.Scheme, "https") {
		return true
	}
	if !strings.EqualFold(parsed.Scheme, "http") {
		return false
	}
	host := parsed.Hostname()
	// Loopback sidecar deployments never traverse untrusted networks.
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// ConfigureHybridLocalRegistry attaches a default Local Registry client and permission-gated Cloud fallback policy.
func (c *HTTPRegistryClient) ConfigureHybridLocalRegistry(local *HTTPRegistryClient, resolver ServiceRegistrySourceResolver, cloudAuth CloudFallbackAuthorizer) {
	c.local = local
	c.sourceResolver = resolver
	c.cloudReadAuth = cloudAuth
	// Propagate already-configured Engine identities so Local Registry requests carry matching headers.
	if local != nil && c.installationID != uuid.Nil && c.runtimeInstanceID != uuid.Nil {
		_ = local.ConfigureEngineIdentity(c.installationID, c.runtimeInstanceID)
	}
}

// HasLocalRegistry reports whether this client is configured for Hybrid Local-first operation.
func (c *HTTPRegistryClient) HasLocalRegistry() bool {
	return c != nil && c.local != nil
}

// cloudClone returns a shallow copy targeting only Fused Cloud Registry so delegated calls do not recurse into Hybrid hooks.
func (c *HTTPRegistryClient) cloudClone() *HTTPRegistryClient {
	return &HTTPRegistryClient{
		endpoint:          c.endpoint,
		licenseKey:        c.licenseKey,
		installationID:    c.installationID,
		runtimeInstanceID: c.runtimeInstanceID,
		httpClient:        c.httpClient,
	}
}

// checkLocalRegistryEntitled enforces live Enterprise license entitlement before using Local Registry.
func (c *HTTPRegistryClient) checkLocalRegistryEntitled() error {
	// Enforce the live heartbeat-updated entitlement so downgrades take effect immediately.
	if !entitlement.LiveEntitlement.Load().LocalRegistryEnabled {
		return ErrLocalRegistryUnauthorized
	}
	return nil
}

// allowCloudFallback evaluates whether ctx holds permission to query Fused Cloud Registry after a Local miss.
func (c *HTTPRegistryClient) allowCloudFallback(ctx context.Context) bool {
	// Default fail-closed when Hybrid mode has no authorizer wired.
	if c.cloudReadAuth == nil {
		return false
	}
	return c.cloudReadAuth(ctx)
}

// pinnedServiceSource looks up whether serviceID is already bound to 'local' or 'cloud' in the workspace store.
func (c *HTTPRegistryClient) pinnedServiceSource(ctx context.Context, serviceID uuid.UUID) string {
	// Explicit context targets take precedence during import preflight and apply.
	if target := RegistryTargetFromContext(ctx); target != "" {
		return target
	}
	if c.sourceResolver == nil || serviceID == uuid.Nil {
		return ""
	}
	sources, err := c.sourceResolver(ctx, []uuid.UUID{serviceID})
	// Treat lookup failure as unpinned rather than misrouting to Cloud without authorization.
	if err != nil {
		return ""
	}
	return sources[serviceID]
}

// clientForPinnedService returns the exact client when serviceID is already pinned, or nil when unpinned.
func (c *HTTPRegistryClient) clientForPinnedService(ctx context.Context, serviceID uuid.UUID) (*HTTPRegistryClient, string, error) {
	if err := c.checkLocalRegistryEntitled(); err != nil {
		return nil, "", err
	}
	switch c.pinnedServiceSource(ctx, serviceID) {
	case RegistrySourceLocal:
		return c.local, RegistrySourceLocal, nil
	case RegistrySourceCloud:
		// Even pinned Cloud services require Cloud read authority for actor-driven control requests.
		if !c.allowCloudFallback(ctx) {
			return nil, "", ErrServiceNotFound
		}
		return c.cloudClone(), RegistrySourceCloud, nil
	default:
		return nil, "", nil
	}
}
