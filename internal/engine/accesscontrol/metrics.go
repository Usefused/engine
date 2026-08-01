package accesscontrol

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

var (
	accessMeter                   = otel.Meter("engine.accesscontrol")
	authenticationDuration, _     = accessMeter.Float64Histogram("engine.authn.duration", metric.WithUnit("ms"))
	authorizationDuration, _      = accessMeter.Float64Histogram("engine.authz.duration", metric.WithUnit("ms"))
	accessCacheRequests, _        = accessMeter.Int64Counter("engine.authorization.cache.requests")
	negativeCacheRequests, _      = accessMeter.Int64Counter("engine.authentication.negative_cache.requests")
	authorizationDecisions, _     = accessMeter.Int64Counter("engine.authorization.decisions")
	authorizationQueryDuration, _ = accessMeter.Float64Histogram("engine.authorization.query.duration", metric.WithUnit("ms"))
	authorizationRevisionLag, _   = accessMeter.Int64Histogram("engine.authorization.revision.lag", metric.WithUnit("{revision}"))
)

func recordAuthentication(ctx context.Context, started time.Time, outcome string, cacheHit bool) {
	attributes := metric.WithAttributes(
		attribute.String("outcome", outcome),
		attribute.Bool("cache_hit", cacheHit),
	)
	authenticationDuration.Record(ctx, elapsedMilliseconds(started), attributes)
	accessCacheRequests.Add(ctx, 1, metric.WithAttributes(attribute.String("cache", "principal"), attribute.Bool("hit", cacheHit)))
}

func recordNegativeCache(ctx context.Context, result string) {
	negativeCacheRequests.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
}

func recordAuthorizationDuration(ctx context.Context, started time.Time, outcome string) {
	authorizationDuration.Record(ctx, elapsedMilliseconds(started), metric.WithAttributes(attribute.String("outcome", outcome)))
	authorizationDecisions.Add(ctx, 1, metric.WithAttributes(attribute.String("outcome", outcome)))
}

func recordAuthorizationQuery(ctx context.Context, query string, started time.Time) {
	authorizationQueryDuration.Record(ctx, elapsedMilliseconds(started), metric.WithAttributes(attribute.String("query", query)))
}

func recordRevisionLag(ctx context.Context, loaded, current int64) {
	lag := loaded - current
	if lag < 0 {
		lag = 0
	}
	authorizationRevisionLag.Record(ctx, lag)
}

func elapsedMilliseconds(started time.Time) float64 {
	return float64(time.Since(started).Microseconds()) / 1000
}
