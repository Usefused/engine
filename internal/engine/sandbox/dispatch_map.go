package sandbox

import (
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
)

// fusedToService projects the connection-cached Fused object onto the lean
// models.Service the dispatcher needs to build and authenticate a vendor call.
// Only execution-relevant fields are carried; authoring/registry fields are
// intentionally dropped.
func fusedToService(o *fusedobject.ServiceMetadata) *models.Service {
	return &models.Service{
		ID:             o.ID,
		Name:           o.Name,
		BaseURL:        o.BaseURL,
		AuthConfigs:    mapAuthConfigs(o.AuthConfigs),
		DefaultHeaders: models.DefaultHeaders(o.DefaultHeaders),
		RawWSDL:        o.RawWSDL,
		RateLimit:      mapRateLimit(o.RateLimit),
		RetryConfig:    mapRetryConfig(o.RetryConfig),
	}
}

// mapRateLimit converts the wire rate-limit config to the models type
// dispatcher.go's CheckRateLimit reads. Returns nil when unset.
func mapRateLimit(r *fusedobject.RateLimitConfig) *models.RateLimitConfig {
	if r == nil {
		return nil
	}
	return &models.RateLimitConfig{
		Strategy:          r.Strategy,
		RequestsPerSecond: r.RequestsPerSecond,
		RequestsPerMinute: r.RequestsPerMinute,
	}
}

// mapRetryConfig converts the wire retry config to the models type
// dispatcher.go's resolveRetryPolicy reads. Returns nil when unset.
func mapRetryConfig(r *fusedobject.RetryConfig) *models.RetryConfig {
	if r == nil {
		return nil
	}
	return &models.RetryConfig{
		Strategy:   r.Strategy,
		MaxRetries: r.MaxRetries,
		BackoffMs:  r.BackoffMs,
	}
}

// fusedToIntegrationObject projects a single Fused endpoint onto the
// models.IntegrationObject the dispatcher executes against, including pagination
// config so the dispatcher's executePaginated path triggers for paginated
// endpoints.
//
// Pagination falls back to the service-level execution_policy value
// (o.Pagination) when the endpoint has none of its own -- most providers
// paginate identically across every endpoint, so per-endpoint spec-derived
// pagination (when present) always wins, but a service/version owner can
// declare one default instead of annotating every operation individually
// (see plans/plan-service-config-restructure.md item 1).
func fusedToIntegrationObject(o *fusedobject.ServiceMetadata, ep fusedobject.Endpoint) *models.IntegrationObject {
	return &models.IntegrationObject{
		ID:             ep.ID,
		ServiceID:      o.ID,
		Name:           ep.Name,
		Method:         ep.Method,
		Path:           ep.Path,
		NormalizedPath: ep.NormalizedPath,
		IsSSE:          ep.IsSSE,
		Pagination:     resolvePagination(ep.Pagination, o.Pagination),
	}
}

// resolvePagination applies the endpoint-wins-over-service fallback rule
// described on fusedToIntegrationObject, then maps whichever value won to
// the models type the dispatcher reads.
func resolvePagination(endpoint, service *fusedobject.PaginationConfig) *models.PaginationConfig {
	if endpoint != nil {
		return mapPagination(endpoint)
	}
	return mapPagination(service)
}

// mapPagination converts the wire pagination config back to the models type the
// dispatcher reads. Returns nil for non-paginated endpoints.
func mapPagination(p *fusedobject.PaginationConfig) *models.PaginationConfig {
	if p == nil {
		return nil
	}
	return &models.PaginationConfig{
		Type:         p.Type,
		RequestParam: p.RequestParam,
		ResponsePath: p.ResponsePath,
	}
}

// mapAuthConfigs converts the Fused-object auth configs to the models auth
// configs the dispatcher's applyAuth understands. Field names are 1:1; only the
// fields applyAuth reads (Type, Scheme, Location, KeyName) plus common OAuth
// metadata are carried.
func mapAuthConfigs(in fusedobject.AuthConfigs) models.AuthConfigs {
	if len(in) == 0 {
		return nil
	}
	out := make(models.AuthConfigs, 0, len(in))
	for _, a := range in {
		out = append(out, models.AuthConfig{
			Name:     authCredentialName(a),
			Type:     a.Type,
			Flow:     a.Flow,
			Scheme:   a.Scheme,
			Location: a.Location,
			KeyName:  a.KeyName,
			TokenURL: a.TokenURL,
		})
	}
	return out
}
