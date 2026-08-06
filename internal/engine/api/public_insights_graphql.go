package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	entitlementpkg "github.com/Usefused/engine/internal/engine/entitlement"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
)

const publicInsightCacheTTL = 5 * time.Minute

type publicInsightClient interface {
	FetchPublicServiceInsights(context.Context, models.PublicServiceInsightsQuery) (models.PublicServiceInsights, error)
}

type publicInsightCacheEntry struct {
	value     models.PublicServiceInsights
	createdAt time.Time
}

type publicInsightReader struct {
	client publicInsightClient
	mu     sync.RWMutex
	cache  map[string]publicInsightCacheEntry
}

func newPublicInsightReader(registryClient sandbox.RegistryClient) *publicInsightReader {
	client, _ := any(registryClient).(publicInsightClient)
	return &publicInsightReader{client: client, cache: make(map[string]publicInsightCacheEntry)}
}

func (r *publicInsightReader) Fetch(ctx context.Context, query models.PublicServiceInsightsQuery) (models.PublicServiceInsights, error) {
	if !entitlementpkg.LiveEntitlement.Load().PublicServiceInsightsEnabled {
		return models.PublicServiceInsights{}, errors.New("cross-engine insights are not included in the current plan")
	}
	if r == nil || r.client == nil {
		return models.PublicServiceInsights{}, errors.New("cross-engine insights are temporarily unavailable")
	}
	keyBytes, _ := json.Marshal(query)
	key := string(keyBytes)
	now := time.Now().UTC()
	if cached, ok := r.load(key); ok && now.Sub(cached.createdAt) < publicInsightCacheTTL {
		return cached.value, nil
	}
	value, err := r.client.FetchPublicServiceInsights(ctx, query)
	if err != nil {
		if cached, ok := r.load(key); ok {
			cached.value.PartialData = true
			return cached.value, nil
		}
		return models.PublicServiceInsights{}, fmt.Errorf("cross-engine insights are temporarily unavailable: %w", err)
	}
	r.store(key, publicInsightCacheEntry{value: value, createdAt: now})
	return value, nil
}

func (r *publicInsightReader) load(key string) (publicInsightCacheEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	entry, ok := r.cache[key]
	return entry, ok
}

func (r *publicInsightReader) store(key string, entry publicInsightCacheEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cache[key] = entry
}

var publicInsightPointGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "PublicServiceInsightPoint",
	Fields: graphql.Fields{
		"key": &graphql.Field{Type: graphql.String}, "label": &graphql.Field{Type: graphql.String},
		"total_calls": &graphql.Field{Type: graphql.Int}, "failed_calls": &graphql.Field{Type: graphql.Int},
		"p50_latency_ms": &graphql.Field{Type: graphql.Float}, "p95_latency_ms": &graphql.Field{Type: graphql.Float},
	},
})

var publicServiceInsightsGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "PublicServiceInsights",
	Fields: graphql.Fields{
		"source": &graphql.Field{Type: graphql.String}, "generated_at": &graphql.Field{Type: graphql.String},
		"data_through": &graphql.Field{Type: graphql.String}, "partial_data": &graphql.Field{Type: graphql.Boolean},
		"total_calls": &graphql.Field{Type: graphql.Int}, "successful_calls": &graphql.Field{Type: graphql.Int},
		"failed_calls": &graphql.Field{Type: graphql.Int}, "p50_latency_ms": &graphql.Field{Type: graphql.Float},
		"p95_latency_ms":      &graphql.Field{Type: graphql.Float},
		"time_series":         &graphql.Field{Type: graphql.NewList(publicInsightPointGraphQLType)},
		"top_operations":      &graphql.Field{Type: graphql.NewList(publicInsightPointGraphQLType)},
		"version_breakdown":   &graphql.Field{Type: graphql.NewList(publicInsightPointGraphQLType)},
		"transport_breakdown": &graphql.Field{Type: graphql.NewList(publicInsightPointGraphQLType)},
	},
})

func publicServiceInsightsGraphQLField(s store.Store, reader *publicInsightReader) *graphql.Field {
	return &graphql.Field{
		Type: publicServiceInsightsGraphQLType,
		Args: graphql.FieldConfigArgument{
			"service_id":           &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"start_date":           &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"end_date":             &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"granularity":          &graphql.ArgumentConfig{Type: graphql.String, DefaultValue: "day"},
			"service_version_id":   &graphql.ArgumentConfig{Type: graphql.String},
			"registry_object_kind": &graphql.ArgumentConfig{Type: graphql.String},
			"registry_object_id":   &graphql.ArgumentConfig{Type: graphql.String},
			"transport":            &graphql.ArgumentConfig{Type: graphql.String},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.public_service_insights.get")
			defer span.End()
			query, err := publicInsightQueryFromArgs(p)
			if err != nil {
				return nil, err
			}
			if err := requireActiveWorkspaceService(ctx, s, query.ServiceID); err != nil {
				return nil, err
			}
			span.SetAttributes(attribute.String("service_id", query.ServiceID.String()))
			insights, err := reader.Fetch(ctx, query)
			if err != nil {
				return nil, err
			}
			return projectGraphQLPublicServiceInsights(insights), nil
		},
	}
}

func publicInsightQueryFromArgs(p graphql.ResolveParams) (models.PublicServiceInsightsQuery, error) {
	serviceID, err := requiredGraphQLUUIDArg(p, "service_id")
	if err != nil {
		return models.PublicServiceInsightsQuery{}, err
	}
	startDate, endDate, err := publicInsightRangeFromArgs(p)
	if err != nil {
		return models.PublicServiceInsightsQuery{}, err
	}
	query := models.PublicServiceInsightsQuery{
		ServiceID: serviceID, StartDate: startDate, EndDate: endDate,
		Granularity: graphQLStringArg(p, "granularity"), RegistryObjectKind: graphQLStringArg(p, "registry_object_kind"),
		Transport: graphQLStringArg(p, "transport"),
	}
	if query.Granularity != "hour" && query.Granularity != "day" {
		return models.PublicServiceInsightsQuery{}, errors.New("granularity must be hour or day")
	}
	if err := setOptionalPublicInsightUUIDs(p, &query); err != nil {
		return models.PublicServiceInsightsQuery{}, err
	}
	return query, nil
}

func publicInsightRangeFromArgs(p graphql.ResolveParams) (time.Time, time.Time, error) {
	startDate, err := parseOptionalRFC3339(graphQLStringArg(p, "start_date"))
	if err != nil || startDate == nil {
		return time.Time{}, time.Time{}, errors.New("invalid start_date")
	}
	endDate, err := parseOptionalRFC3339(graphQLStringArg(p, "end_date"))
	if err != nil || endDate == nil || !startDate.Before(*endDate) || endDate.Sub(*startDate) > 90*24*time.Hour {
		return time.Time{}, time.Time{}, errors.New("invalid end_date")
	}
	return *startDate, *endDate, nil
}

func setOptionalPublicInsightUUIDs(p graphql.ResolveParams, query *models.PublicServiceInsightsQuery) error {
	for argument, target := range map[string]**uuid.UUID{
		"service_version_id": &query.ServiceVersionID, "registry_object_id": &query.RegistryObjectID,
	} {
		raw := graphQLStringArg(p, argument)
		if raw == "" {
			continue
		}
		parsed, err := uuid.Parse(raw)
		if err != nil {
			return fmt.Errorf("invalid %s", argument)
		}
		*target = &parsed
	}
	return nil
}

func projectGraphQLPublicServiceInsights(insights models.PublicServiceInsights) map[string]interface{} {
	return map[string]interface{}{
		"source": insights.Source, "generated_at": formatGraphQLTime(insights.GeneratedAt),
		"data_through": formatOptionalGraphQLTime(insights.DataThrough), "partial_data": insights.PartialData,
		"total_calls": int(insights.TotalCalls), "successful_calls": int(insights.SuccessfulCalls),
		"failed_calls": int(insights.FailedCalls), "p50_latency_ms": insights.P50LatencyMs, "p95_latency_ms": insights.P95LatencyMs,
		"time_series":         projectGraphQLPublicInsightPoints(insights.TimeSeries),
		"top_operations":      projectGraphQLPublicInsightPoints(insights.TopOperations),
		"version_breakdown":   projectGraphQLPublicInsightPoints(insights.VersionBreakdown),
		"transport_breakdown": projectGraphQLPublicInsightPoints(insights.TransportBreakdown),
	}
}

func projectGraphQLPublicInsightPoints(points []models.PublicServiceInsightPoint) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(points))
	for _, point := range points {
		result = append(result, map[string]interface{}{
			"key": point.Key, "label": point.Label, "total_calls": int(point.TotalCalls),
			"failed_calls": int(point.FailedCalls), "p50_latency_ms": point.P50LatencyMs, "p95_latency_ms": point.P95LatencyMs,
		})
	}
	return result
}
