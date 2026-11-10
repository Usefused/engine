package api

import (
	"context"
	"fmt"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"go.opentelemetry.io/otel"
)

// workspaceWebhookGraphQLField resolves an exact receiving route within the actor's authorized service scope.
func workspaceWebhookGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: workspaceWebhookGraphQLType,
		Args: graphql.FieldConfigArgument{
			"service_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"slug":       &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		// Detail reads share the existing control trace and bounded database budget.
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.webhook.get")
			defer span.End()
			ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
			defer cancel()
			serviceID, err := requiredGraphQLUUIDArg(p, "service_id")
			// Invalid route identities cannot become broader registration discovery.
			if err != nil {
				return nil, err
			}
			slug := graphQLStringArg(p, "slug")
			// The exact filter's empty value means unrestricted listing, so never admit it here.
			if slug == "" {
				return nil, fmt.Errorf("webhook slug is required")
			}
			scope, err := graphQLAuthorizedScope(ctx, accesscontrol.PermissionServiceRead, accesscontrol.ResourceService)
			// The SQL boundary must retain the caller's full resource restrictions.
			if err != nil {
				return nil, err
			}
			rows, _, err := s.ListAuthorizedWorkspaceWebhookPage(ctx, scope, store.WorkspaceWebhookFilter{ServiceID: &serviceID, Slug: slug}, 1, 0)
			// Storage errors and genuine absence remain distinct UI states.
			if err != nil {
				return nil, err
			}
			// Unknown, removed, and mismatched service/registration routes disclose no metadata.
			if len(rows) == 0 {
				return nil, nil
			}
			row := rows[0]
			item := projectGraphQLWorkspaceWebhooks([]store.WorkspaceWebhook{row.WorkspaceWebhook}, ctx)[0]
			item["service_id"], item["service_name"], item["service_ref"] = row.ServiceID.String(), row.ServiceName, row.ServiceRef
			return item, nil
		},
	}
}

// storeFilter carries only validated scope and filters into canonical SQL reporting.
func (f webhookAnalyticsFilter) storeFilter(accountID uuid.UUID) store.WebhookActivityFilter {
	return store.WebhookActivityFilter{AccountID: accountID, ServiceID: f.serviceID, RegistrationID: f.registrationID,
		EventName: f.eventName, Status: f.status, Limit: f.limit, Offset: f.offset, StartDate: f.startDate, EndDate: f.endDate}
}

// readWebhookEvents never broadens an exact-registration query when an alternate store lacks support.
func readWebhookEvents(ctx context.Context, s store.Store, filter store.WebhookActivityFilter) ([]models.WebhookEvent, int64, error) {
	// Current PostgreSQL stores use one predicate builder for scoped and service-wide reads.
	if reader, ok := s.(store.WebhookActivityReader); ok {
		return reader.ListWebhookActivity(ctx, filter)
	}
	// Older adapters may serve service Activity, but cannot silently answer a narrower request with all events.
	if filter.RegistrationID != nil || filter.Status != "" {
		return nil, 0, fmt.Errorf("registration activity is unavailable")
	}
	return s.ListWebhookEventsByService(ctx, filter.AccountID, filter.ServiceID, filter.EventName, filter.Limit, filter.Offset, filter.StartDate, filter.EndDate)
}

// readWebhookAnalytics applies the same fail-closed adapter contract as the receipt list.
func readWebhookAnalytics(ctx context.Context, s store.Store, filter store.WebhookActivityFilter) (models.WebhookAnalytics, error) {
	// A supported reader aggregates precisely the receipts admitted by the shared predicate builder.
	if reader, ok := s.(store.WebhookActivityReader); ok {
		return reader.GetWebhookActivityAnalytics(ctx, filter)
	}
	// Unsupported scope is an explicit error, never a service-wide success-shaped aggregate.
	if filter.RegistrationID != nil || filter.Status != "" {
		return models.WebhookAnalytics{}, fmt.Errorf("registration analytics are unavailable")
	}
	return s.GetWebhookAnalytics(ctx, filter.AccountID, filter.ServiceID, filter.EventName, filter.StartDate, filter.EndDate)
}
