package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/connectresource"
	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/language/ast"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var engineJSONType = graphql.NewScalar(graphql.ScalarConfig{
	Name: "EngineJSON",
	// Variables carry resource input in practice; literals stay disabled so
	// clients cannot smuggle an untyped nested object into the mutation text.
	Serialize:    func(value interface{}) interface{} { return value },
	ParseValue:   func(value interface{}) interface{} { return value },
	ParseLiteral: func(ast.Value) interface{} { return nil },
})

var bucketGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "Bucket",
	Fields: graphql.Fields{
		"id":         &graphql.Field{Type: graphql.String},
		"name":       &graphql.Field{Type: graphql.String},
		"is_default": &graphql.Field{Type: graphql.Boolean},
		"created_at": &graphql.Field{Type: graphql.String},
		"updated_at": &graphql.Field{Type: graphql.String},
	},
})

var bucketValueGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "BucketValue",
	Fields: graphql.Fields{
		"id":         &graphql.Field{Type: graphql.String},
		"bucket_id":  &graphql.Field{Type: graphql.String},
		"service_id": &graphql.Field{Type: graphql.String},
		"key_name":   &graphql.Field{Type: graphql.String},
		"location":   &graphql.Field{Type: graphql.String},
		"value":      &graphql.Field{Type: graphql.String},
		"created_at": &graphql.Field{Type: graphql.String},
		"updated_at": &graphql.Field{Type: graphql.String},
	},
})

var bucketValuePageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "BucketValuePage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewList(bucketValueGraphQLType)},
		"total": &graphql.Field{Type: graphql.Int},
	},
})

var bucketSummaryGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "BucketSummary",
	Fields: graphql.Fields{
		"id":                   &graphql.Field{Type: graphql.String},
		"name":                 &graphql.Field{Type: graphql.String},
		"is_default":           &graphql.Field{Type: graphql.Boolean},
		"secret_count":         &graphql.Field{Type: graphql.Int},
		"value_count":          &graphql.Field{Type: graphql.Int},
		"connected_user_count": &graphql.Field{Type: graphql.Int},
		"created_at":           &graphql.Field{Type: graphql.String},
		"updated_at":           &graphql.Field{Type: graphql.String},
	},
})

var bucketSDKSummaryGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "BucketSDKSummary",
	Fields: graphql.Fields{
		"id":         &graphql.Field{Type: graphql.String},
		"name":       &graphql.Field{Type: graphql.String},
		"kind":       &graphql.Field{Type: graphql.String},
		"active":     &graphql.Field{Type: graphql.Boolean},
		"created_at": &graphql.Field{Type: graphql.String},
	},
})

var bucketSDKSummaryPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "BucketSDKSummaryPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewList(bucketSDKSummaryGraphQLType)},
		"total": &graphql.Field{Type: graphql.Int},
	},
})

var bucketServiceSummaryGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "BucketServiceSummary",
	Fields: graphql.Fields{
		"service_id":           &graphql.Field{Type: graphql.String},
		"service_name":         &graphql.Field{Type: graphql.String},
		"secret_count":         &graphql.Field{Type: graphql.Int},
		"value_count":          &graphql.Field{Type: graphql.Int},
		"connect_config_count": &graphql.Field{Type: graphql.Int},
		"connected_user_count": &graphql.Field{Type: graphql.Int},
	},
})

var bucketServiceSummaryPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "BucketServiceSummaryPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewList(bucketServiceSummaryGraphQLType)},
		"total": &graphql.Field{Type: graphql.Int},
	},
})

var bucketSummaryPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "BucketSummaryPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewList(bucketSummaryGraphQLType)},
		"total": &graphql.Field{Type: graphql.Int},
	},
})

var bucketConnectSummaryGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "BucketConnectSummary",
	Fields: graphql.Fields{
		"bucket_id":            &graphql.Field{Type: graphql.String},
		"connect_config_count": &graphql.Field{Type: graphql.Int},
		"connected_user_count": &graphql.Field{Type: graphql.Int},
	},
})

var workspaceServiceVersionGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceServiceVersion",
	Fields: graphql.Fields{
		"id":                 &graphql.Field{Type: graphql.String},
		"service_version_id": &graphql.Field{Type: graphql.String},
		"version":            &graphql.Field{Type: graphql.String},
		"status":             &graphql.Field{Type: graphql.String},
		"created_at":         &graphql.Field{Type: graphql.String},
		"enabled_at":         &graphql.Field{Type: graphql.String},
	},
})

var workspaceServiceAuthOptionGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceServiceAuthOption",
	Fields: graphql.Fields{
		"id":                       &graphql.Field{Type: graphql.String},
		"label":                    &graphql.Field{Type: graphql.String},
		"auth_type":                &graphql.Field{Type: graphql.String},
		"credential_type":          &graphql.Field{Type: graphql.String},
		"key_name":                 &graphql.Field{Type: graphql.String},
		"key_prefix":               &graphql.Field{Type: graphql.String},
		"required_fields":          &graphql.Field{Type: graphql.NewList(graphql.String)},
		"supports_connected_users": &graphql.Field{Type: graphql.Boolean},
	},
})

var workspaceServiceGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceService",
	Fields: graphql.Fields{
		"id":                 &graphql.Field{Type: graphql.String},
		"workspace_id":       &graphql.Field{Type: graphql.String},
		"service_id":         &graphql.Field{Type: graphql.String},
		"service_name":       &graphql.Field{Type: graphql.String},
		"service_slug":       &graphql.Field{Type: graphql.String},
		"version":            &graphql.Field{Type: graphql.String},
		"service_version_id": &graphql.Field{Type: graphql.String},
		"enabled_versions":   &graphql.Field{Type: graphql.NewList(workspaceServiceVersionGraphQLType)},
		"auth_options":       &graphql.Field{Type: graphql.NewList(workspaceServiceAuthOptionGraphQLType)},
		"added_by":           &graphql.Field{Type: graphql.String},
		"created_at":         &graphql.Field{Type: graphql.String},
	},
})

var workspaceServicePageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceServicePage",
	Fields: graphql.Fields{
		"data":  &graphql.Field{Type: graphql.NewNonNull(graphql.NewList(workspaceServiceGraphQLType))},
		"total": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"page":  &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
		"limit": &graphql.Field{Type: graphql.NewNonNull(graphql.Int)},
	},
})

var workspaceWebhookGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceWebhook",
	Fields: graphql.Fields{
		"label":      &graphql.Field{Type: graphql.String},
		"slug":       &graphql.Field{Type: graphql.String},
		"created_at": &graphql.Field{Type: graphql.String},
		// signature is "set"/"none" only -- never the secret_ref itself. This
		// backs the CLI's `workspace service <slug> webhooks` SIGNATURE
		// column, which exists purely so a user can tell at a glance whether
		// a registration has a signing secret configured without a second
		// lookup, not to expose the reference.
		"signature": &graphql.Field{Type: graphql.String},
	},
})

var appTokenGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppToken",
	Fields: graphql.Fields{
		"id":            &graphql.Field{Type: graphql.String},
		"app_family_id": &graphql.Field{Type: graphql.String},
		"name":          &graphql.Field{Type: graphql.String},
		"allow":         &graphql.Field{Type: graphql.NewList(graphql.String)},
		"expires_at":    &graphql.Field{Type: graphql.String},
		"created_at":    &graphql.Field{Type: graphql.String},
		"last_used_at":  &graphql.Field{Type: graphql.String},
	},
})

var webhookEventGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WebhookEvent",
	Fields: graphql.Fields{
		"id":                  &graphql.Field{Type: graphql.String},
		"account_id":          &graphql.Field{Type: graphql.String},
		"service_id":          &graphql.Field{Type: graphql.String},
		"msg_id":              &graphql.Field{Type: graphql.String},
		"event_name":          &graphql.Field{Type: graphql.String},
		"status":              &graphql.Field{Type: graphql.String},
		"delivery_status":     &graphql.Field{Type: graphql.String},
		"verification_status": &graphql.Field{Type: graphql.String},
		"environment":         &graphql.Field{Type: graphql.String},
		"latency_ms":          &graphql.Field{Type: graphql.Int},
		"retry_count":         &graphql.Field{Type: graphql.Int},
		"credits_consumed":    &graphql.Field{Type: graphql.Float},
		"sdk_record_id":       &graphql.Field{Type: graphql.String},
		"error_reason":        &graphql.Field{Type: graphql.String},
		"payload_size":        &graphql.Field{Type: graphql.Int},
		"created_at":          &graphql.Field{Type: graphql.String},
	},
})

var webhookEventPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WebhookEventPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewList(webhookEventGraphQLType)},
		"total": &graphql.Field{Type: graphql.Int},
	},
})

var webhookAnalyticsGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WebhookAnalytics",
	Fields: graphql.Fields{
		"total_ingested":  &graphql.Field{Type: graphql.Int},
		"total_delivered": &graphql.Field{Type: graphql.Int},
		"total_rejected":  &graphql.Field{Type: graphql.Int},
		"total_failed":    &graphql.Field{Type: graphql.Int},
	},
})

var engineExecutionTimingGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "EngineExecutionTiming",
	Fields: graphql.Fields{
		"name":        &graphql.Field{Type: graphql.String},
		"duration_ms": &graphql.Field{Type: graphql.Float},
	},
})

var engineExecutionEventGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "EngineExecutionEvent",
	Fields: graphql.Fields{
		"id":                        &graphql.Field{Type: graphql.String},
		"trace_id":                  &graphql.Field{Type: graphql.String},
		"span_id":                   &graphql.Field{Type: graphql.String},
		"app_family_id":             &graphql.Field{Type: graphql.String},
		"app_id":                    &graphql.Field{Type: graphql.String},
		"app_version":               &graphql.Field{Type: graphql.String},
		"app_kind":                  &graphql.Field{Type: graphql.String},
		"transport":                 &graphql.Field{Type: graphql.String},
		"provider_protocol":         &graphql.Field{Type: graphql.String},
		"direction":                 &graphql.Field{Type: graphql.String},
		"service_id":                &graphql.Field{Type: graphql.String},
		"service_version_id":        &graphql.Field{Type: graphql.String},
		"service_name":              &graphql.Field{Type: graphql.String},
		"service_slug":              &graphql.Field{Type: graphql.String},
		"service_version":           &graphql.Field{Type: graphql.String},
		"operation_id":              &graphql.Field{Type: graphql.String},
		"webhook_id":                &graphql.Field{Type: graphql.String},
		"operation":                 &graphql.Field{Type: graphql.String},
		"event_name":                &graphql.Field{Type: graphql.String},
		"http_method":               &graphql.Field{Type: graphql.String},
		"request_path":              &graphql.Field{Type: graphql.String},
		"environment":               &graphql.Field{Type: graphql.String},
		"environment_source":        &graphql.Field{Type: graphql.String},
		"provider_host":             &graphql.Field{Type: graphql.String},
		"provider_http_status":      &graphql.Field{Type: graphql.Int},
		"provider_status_class":     &graphql.Field{Type: graphql.String},
		"status":                    &graphql.Field{Type: graphql.String},
		"failure_reason":            &graphql.Field{Type: graphql.String},
		"failure_category":          &graphql.Field{Type: graphql.String},
		"failure_code":              &graphql.Field{Type: graphql.String},
		"latency_ms":                &graphql.Field{Type: graphql.Int},
		"provider_latency_ms":       &graphql.Field{Type: graphql.Int},
		"attempt_count":             &graphql.Field{Type: graphql.Int},
		"auth_scheme_names":         &graphql.Field{Type: graphql.NewList(graphql.String)},
		"auth_scheme_types":         &graphql.Field{Type: graphql.NewList(graphql.String)},
		"auth_scheme_count":         &graphql.Field{Type: graphql.Int},
		"auth_selection_outcome":    &graphql.Field{Type: graphql.String},
		"pagination_type":           &graphql.Field{Type: graphql.String},
		"pagination_page_count":     &graphql.Field{Type: graphql.Int},
		"pagination_item_count":     &graphql.Field{Type: graphql.Int},
		"pagination_byte_count":     &graphql.Field{Type: graphql.Int},
		"pagination_stop_reason":    &graphql.Field{Type: graphql.String},
		"rate_limit_decision":       &graphql.Field{Type: graphql.String},
		"rate_limit_policy_count":   &graphql.Field{Type: graphql.Int},
		"rate_limit_scope_kinds":    &graphql.Field{Type: graphql.NewList(graphql.String)},
		"rate_limit_units":          &graphql.Field{Type: graphql.NewList(graphql.String)},
		"rate_limit_unit_totals":    &graphql.Field{Type: graphql.NewList(graphql.String)},
		"rate_limit_retry_outcome":  &graphql.Field{Type: graphql.String},
		"rate_limit_header_outcome": &graphql.Field{Type: graphql.String},
		"request_bytes":             &graphql.Field{Type: graphql.Int},
		"response_bytes":            &graphql.Field{Type: graphql.Int},
		"verification_status":       &graphql.Field{Type: graphql.String},
		"delivery_status":           &graphql.Field{Type: graphql.String},
		"idempotency_replayed":      &graphql.Field{Type: graphql.Boolean},
		"started_at":                &graphql.Field{Type: graphql.String},
		"ended_at":                  &graphql.Field{Type: graphql.String},
		"timings":                   &graphql.Field{Type: graphql.NewList(engineExecutionTimingGraphQLType)},
	},
})

var serviceConsumerGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "ServiceConsumer",
	Fields: graphql.Fields{
		"id":                 &graphql.Field{Type: graphql.String},
		"name":               &graphql.Field{Type: graphql.String},
		"version":            &graphql.Field{Type: graphql.String},
		"kind":               &graphql.Field{Type: graphql.String},
		"active":             &graphql.Field{Type: graphql.Boolean},
		"service_version_id": &graphql.Field{Type: graphql.String},
		"select_all":         &graphql.Field{Type: graphql.Boolean},
		"operation_count":    &graphql.Field{Type: graphql.Int},
		"webhook_count":      &graphql.Field{Type: graphql.Int},
		"created_at":         &graphql.Field{Type: graphql.String},
	},
})

var engineExecutionEventPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "EngineExecutionEventPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewList(engineExecutionEventGraphQLType)},
		"total": &graphql.Field{Type: graphql.Int},
	},
})

var engineExecutionAnalyticsGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "EngineExecutionAnalytics",
	Fields: graphql.Fields{
		"total_calls":        &graphql.Field{Type: graphql.Int},
		"successful_calls":   &graphql.Field{Type: graphql.Int},
		"failed_calls":       &graphql.Field{Type: graphql.Int},
		"average_latency_ms": &graphql.Field{Type: graphql.Float},
		"median_latency_ms":  &graphql.Field{Type: graphql.Float},
		"p95_latency_ms":     &graphql.Field{Type: graphql.Float},
	},
})

var appExecutionAnalyticsGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AppExecutionAnalytics",
	Fields: graphql.Fields{
		"total_calls":        &graphql.Field{Type: graphql.Int},
		"successful_calls":   &graphql.Field{Type: graphql.Int},
		"failed_calls":       &graphql.Field{Type: graphql.Int},
		"average_latency_ms": &graphql.Field{Type: graphql.Float},
		"median_latency_ms":  &graphql.Field{Type: graphql.Float},
		"p95_latency_ms":     &graphql.Field{Type: graphql.Float},
		"by_service":         &graphql.Field{Type: graphql.NewList(engineExecutionBreakdownGraphQLType)},
	},
})

var engineExecutionBreakdownGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "EngineExecutionBreakdown",
	Fields: graphql.Fields{
		"key":            &graphql.Field{Type: graphql.String},
		"label":          &graphql.Field{Type: graphql.String},
		"total_calls":    &graphql.Field{Type: graphql.Int},
		"inbound_calls":  &graphql.Field{Type: graphql.Int},
		"failed_calls":   &graphql.Field{Type: graphql.Int},
		"p95_latency_ms": &graphql.Field{Type: graphql.Float},
	},
})

var workspaceExecutionAnalyticsGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceExecutionAnalytics",
	Fields: graphql.Fields{
		"total_calls":         &graphql.Field{Type: graphql.Int},
		"inbound_calls":       &graphql.Field{Type: graphql.Int},
		"successful_calls":    &graphql.Field{Type: graphql.Int},
		"failed_calls":        &graphql.Field{Type: graphql.Int},
		"average_latency_ms":  &graphql.Field{Type: graphql.Float},
		"median_latency_ms":   &graphql.Field{Type: graphql.Float},
		"p95_latency_ms":      &graphql.Field{Type: graphql.Float},
		"by_service":          &graphql.Field{Type: graphql.NewList(engineExecutionBreakdownGraphQLType)},
		"most_used_sdk":       &graphql.Field{Type: engineExecutionBreakdownGraphQLType},
		"most_used_service":   &graphql.Field{Type: engineExecutionBreakdownGraphQLType},
		"most_failed_service": &graphql.Field{Type: engineExecutionBreakdownGraphQLType},
		"most_used_bucket":    &graphql.Field{Type: engineExecutionBreakdownGraphQLType},
	},
})

var driftChangeGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "DriftChange",
	Fields: graphql.Fields{
		"field":       &graphql.Field{Type: graphql.String},
		"old_value":   &graphql.Field{Type: graphql.String},
		"new_value":   &graphql.Field{Type: graphql.String},
		"severity":    &graphql.Field{Type: graphql.String},
		"description": &graphql.Field{Type: graphql.String},
	},
})

var workspaceNotificationGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceNotification",
	Fields: graphql.Fields{
		"id":                    &graphql.Field{Type: graphql.String},
		"source":                &graphql.Field{Type: graphql.String},
		"type":                  &graphql.Field{Type: graphql.String},
		"severity":              &graphql.Field{Type: graphql.String},
		"status":                &graphql.Field{Type: graphql.String},
		"service_id":            &graphql.Field{Type: graphql.String},
		"version":               &graphql.Field{Type: graphql.String},
		"config_key":            &graphql.Field{Type: graphql.String},
		"message":               &graphql.Field{Type: graphql.String},
		"integration_object_id": &graphql.Field{Type: graphql.String},
		"webhook_object_id":     &graphql.Field{Type: graphql.String},
		"detected_at":           &graphql.Field{Type: graphql.String},
		"diff":                  &graphql.Field{Type: graphql.NewList(driftChangeGraphQLType)},
	},
})

var workspaceNotificationInboxGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceNotificationInbox",
	Fields: graphql.Fields{
		"items":    &graphql.Field{Type: graphql.NewList(workspaceNotificationGraphQLType)},
		"warnings": &graphql.Field{Type: graphql.NewList(graphql.String)},
		// total_count/pending_count are always populated, paginated or not --
		// see workspaceNotificationInbox's doc comment in
		// workspace_config_handlers.go for why the unbounded (limit=0) path
		// still needs them (numbered-page UI on the full notifications page,
		// per plans/plan-service-changelog.md's Phase 4 pagination follow-up).
		"total_count":   &graphql.Field{Type: graphql.Int},
		"pending_count": &graphql.Field{Type: graphql.Int},
	},
})

var secretMetaGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "SecretMeta",
	Fields: graphql.Fields{
		"id":              &graphql.Field{Type: graphql.String},
		"bucket_id":       &graphql.Field{Type: graphql.String},
		"service_id":      &graphql.Field{Type: graphql.String},
		"key_name":        &graphql.Field{Type: graphql.String},
		"key_names":       &graphql.Field{Type: graphql.NewList(graphql.String)},
		"credential_type": &graphql.Field{Type: graphql.String},
		"last_used_at":    &graphql.Field{Type: graphql.String},
		"expires_at":      &graphql.Field{Type: graphql.String},
		"created_at":      &graphql.Field{Type: graphql.String},
		"updated_at":      &graphql.Field{Type: graphql.String},
	},
})

var secretMetaPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "SecretMetaPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewList(secretMetaGraphQLType)},
		"total": &graphql.Field{Type: graphql.Int},
	},
})

var secretUpsertGraphQLInput = graphql.NewInputObject(graphql.InputObjectConfig{
	Name: "SecretUpsertInput",
	Fields: graphql.InputObjectConfigFieldMap{
		"service_id":      &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		"key_name":        &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		"credential_type": &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		"value":           &graphql.InputObjectFieldConfig{Type: graphql.NewNonNull(graphql.String)},
		"expires_at":      &graphql.InputObjectFieldConfig{Type: graphql.String},
	},
})

// workspaceConnectConfigGraphQLType is the masked, bucket-named projection
// intended for declarative workspace sync rather than runtime secret use.
var workspaceConnectConfigGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceConnectConfig",
	Fields: graphql.Fields{
		"bucket_id":         &graphql.Field{Type: graphql.String},
		"bucket_name":       &graphql.Field{Type: graphql.String},
		"service_id":        &graphql.Field{Type: graphql.String},
		"auth_type":         &graphql.Field{Type: graphql.String},
		"auth_name":         &graphql.Field{Type: graphql.String},
		"enabled":           &graphql.Field{Type: graphql.Boolean},
		"redirect_uri":      &graphql.Field{Type: graphql.String},
		"has_client_id":     &graphql.Field{Type: graphql.Boolean},
		"has_client_secret": &graphql.Field{Type: graphql.Boolean},
		"profiles":          &graphql.Field{Type: graphql.NewList(workspaceConnectionProfileGraphQLType)},
	},
})

var authConnectionGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AuthConnection",
	Fields: graphql.Fields{
		"id":                       &graphql.Field{Type: graphql.String},
		"bucket_id":                &graphql.Field{Type: graphql.String},
		"service_id":               &graphql.Field{Type: graphql.String},
		"service_version_id":       &graphql.Field{Type: graphql.String},
		"end_user_ref":             &graphql.Field{Type: graphql.String},
		"created_by_app_id":        &graphql.Field{Type: graphql.String},
		"auth_type":                &graphql.Field{Type: graphql.String},
		"auth_name":                &graphql.Field{Type: graphql.String},
		"token_type":               &graphql.Field{Type: graphql.String},
		"scopes":                   &graphql.Field{Type: graphql.NewList(graphql.String)},
		"scope_source":             &graphql.Field{Type: graphql.String},
		"issuer":                   &graphql.Field{Type: graphql.String},
		"subject":                  &graphql.Field{Type: graphql.String},
		"expires_at":               &graphql.Field{Type: graphql.String},
		"refresh_token_expires_at": &graphql.Field{Type: graphql.String},
		"last_used_at":             &graphql.Field{Type: graphql.String},
		"last_refresh_attempt_at":  &graphql.Field{Type: graphql.String},
		"last_refreshed_at":        &graphql.Field{Type: graphql.String},
		"refresh_retry_not_before": &graphql.Field{Type: graphql.String},
		"refresh_state":            &graphql.Field{Type: graphql.String},
		"last_failure_code":        &graphql.Field{Type: graphql.String},
		"last_failure_at":          &graphql.Field{Type: graphql.String},
		"last_failure_trace_id":    &graphql.Field{Type: graphql.String},
		"created_at":               &graphql.Field{Type: graphql.String},
		"updated_at":               &graphql.Field{Type: graphql.String},
	},
})

var authConnectionPageGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "AuthConnectionPage",
	Fields: graphql.Fields{
		"items": &graphql.Field{Type: graphql.NewList(authConnectionGraphQLType)},
		"total": &graphql.Field{Type: graphql.Int},
	},
})

var connectionResourceGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "ConnectionResource",
	Fields: graphql.Fields{
		"id":                   &graphql.Field{Type: graphql.String},
		"connection_id":        &graphql.Field{Type: graphql.String},
		"service_id":           &graphql.Field{Type: graphql.String},
		"provider_resource_id": &graphql.Field{Type: graphql.String},
		"resource_type":        &graphql.Field{Type: graphql.String},
		"display_name":         &graphql.Field{Type: graphql.String},
		"base_url":             &graphql.Field{Type: graphql.String},
		"scopes":               &graphql.Field{Type: graphql.NewList(graphql.String)},
		"is_default":           &graphql.Field{Type: graphql.Boolean},
		"created_at":           &graphql.Field{Type: graphql.String},
		"updated_at":           &graphql.Field{Type: graphql.String},
	},
})

var connectSessionGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "ConnectSession",
	Fields: graphql.Fields{
		"authorize_url": &graphql.Field{Type: graphql.String},
		"expires_at":    &graphql.Field{Type: graphql.String},
	},
})

// bucketsGraphQLField exposes the caller's credential buckets through Engine
// GraphQL so UI reads do not bypass the workspace ownership check.
// bucketSummariesGraphQLField uses a Store aggregate query so the UI can show
// bucket contents at a glance without issuing one read per bucket.
func bucketSummariesGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewList(bucketSummaryGraphQLType),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			summaries, _, err := resolveBucketSummaryPage(p, s, 100, 0)
			if err != nil {
				return nil, fmt.Errorf("list bucket summaries: %w", err)
			}
			return projectGraphQLBucketSummaries(summaries), nil
		},
	}
}

func bucketSummaryPageGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: bucketSummaryPageGraphQLType,
		Args: graphql.FieldConfigArgument{
			"limit":  &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 20},
			"offset": &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			limit, offset := bucketPageArgs(p)
			summaries, total, err := resolveBucketSummaryPage(p, s, limit, offset)
			if err != nil {
				return nil, fmt.Errorf("list bucket summary page: %w", err)
			}
			return map[string]interface{}{"items": projectGraphQLBucketSummaries(summaries), "total": total}, nil
		},
	}
}

func bucketSummaryGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: bucketSummaryGraphQLType,
		Args: graphql.FieldConfigArgument{
			"bucket_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.bucket_summary.get")
			defer span.End()
			_, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			bucketID, err := requiredGraphQLUUIDArg(p, "bucket_id")
			if err != nil {
				return nil, err
			}
			span.SetAttributes(attribute.String("bucket_id", bucketID.String()))
			summary, err := s.GetBucketSummary(ctx, bucketID)
			if err != nil {
				return nil, fmt.Errorf("get bucket summary: %w", err)
			}
			return projectGraphQLBucketSummary(summary), nil
		},
	}
}

func resolveBucketSummaryPage(p graphql.ResolveParams, s store.Store, limit, offset int) ([]store.BucketSummary, int, error) {
	ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.bucket_summaries.list")
	defer span.End()
	actor, err := actorFromContext(p.Context)
	if err != nil {
		return nil, 0, err
	}
	span.SetAttributes(
		attribute.String("account_id", actor.accountID.String()),
		attribute.Int("limit", limit),
		attribute.Int("offset", offset),
	)
	authorized, err := graphQLAuthorizedScope(p.Context, accesscontrol.PermissionBucketRead, accesscontrol.ResourceBucket)
	if err != nil {
		return nil, 0, err
	}
	return s.ListAuthorizedBucketSummaries(ctx, authorized, limit, offset)
}

func bucketPageArgs(p graphql.ResolveParams) (int, int) {
	limit, _ := p.Args["limit"].(int)
	offset, _ := p.Args["offset"].(int)
	if limit <= 0 || limit > 100 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

func workspaceServicesGraphQLField(s store.Store, verifier ServiceVerifier) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewList(workspaceServiceGraphQLType),
		Args: graphql.FieldConfigArgument{
			"names": &graphql.ArgumentConfig{Type: graphql.NewList(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.workspace_services.list")
			defer span.End()
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			span.SetAttributes(attribute.String("account_id", actor.accountID.String()))
			authorized, err := graphQLAuthorizedScope(ctx, accesscontrol.PermissionServiceRead, accesscontrol.ResourceService)
			if err != nil {
				return nil, err
			}
			services, err := s.ListAuthorizedWorkspaceServices(ctx, authorized, graphQLStringListArg(p, "names"))
			if err != nil {
				return nil, fmt.Errorf("list workspace services: %w", err)
			}
			serviceIDs := listedServiceIDs(services)
			versions, err := s.ListWorkspaceServiceVersionsForServices(ctx, serviceIDs)
			if err != nil {
				return nil, fmt.Errorf("list workspace service versions: %w", err)
			}
			slugs := fetchServiceSlugsForListing(ctx, verifier, apiKeyFromGraphQLContext(p.Context), serviceIDs)
			authOptions, err := fetchWorkspaceServiceAuthOptions(ctx, verifier, apiKeyFromGraphQLContext(p.Context), services)
			if err != nil {
				return nil, fmt.Errorf("load workspace service auth options: %w", err)
			}
			span.SetAttributes(attribute.Int("service_count", len(services)))
			return projectGraphQLWorkspaceServices(services, versions, slugs, authOptions), nil
		},
	}
}

func workspaceServicePageGraphQLField(s store.Store, verifier ServiceVerifier) *graphql.Field {
	return &graphql.Field{
		Type: workspaceServicePageGraphQLType,
		Args: graphql.FieldConfigArgument{
			"names":  &graphql.ArgumentConfig{Type: graphql.NewList(graphql.String)},
			"limit":  &graphql.ArgumentConfig{Type: graphql.Int},
			"offset": &graphql.ArgumentConfig{Type: graphql.Int},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.workspace_service_page.list")
			defer span.End()
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			span.SetAttributes(attribute.String("account_id", actor.accountID.String()))

			limit, offset := bucketPageArgs(p)

			authorized, err := graphQLAuthorizedScope(ctx, accesscontrol.PermissionServiceRead, accesscontrol.ResourceService)
			if err != nil {
				return nil, err
			}
			services, total, err := s.ListAuthorizedWorkspaceServicesPage(ctx, authorized, graphQLStringListArg(p, "names"), limit, offset)
			if err != nil {
				return nil, fmt.Errorf("list workspace services page: %w", err)
			}
			serviceIDs := listedServiceIDs(services)
			versions, err := s.ListWorkspaceServiceVersionsForServices(ctx, serviceIDs)
			if err != nil {
				return nil, fmt.Errorf("list workspace service versions: %w", err)
			}
			slugs := fetchServiceSlugsForListing(ctx, verifier, apiKeyFromGraphQLContext(p.Context), serviceIDs)
			authOptions, err := fetchWorkspaceServiceAuthOptions(ctx, verifier, apiKeyFromGraphQLContext(p.Context), services)
			if err != nil {
				return nil, fmt.Errorf("load workspace service auth options: %w", err)
			}
			span.SetAttributes(attribute.Int("service_count", len(services)))

			page := 1
			if limit > 0 {
				page = (offset / limit) + 1
			}
			return map[string]interface{}{
				"data":  projectGraphQLWorkspaceServices(services, versions, slugs, authOptions),
				"total": total,
				"page":  page,
				"limit": limit,
			}, nil
		},
	}
}

func workspaceWebhooksGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewList(workspaceWebhookGraphQLType),
		Args: graphql.FieldConfigArgument{
			"service_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.workspace_webhooks.list")
			defer span.End()
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			serviceID, err := requiredGraphQLUUIDArg(p, "service_id")
			if err != nil {
				return nil, err
			}
			span.SetAttributes(attribute.String("account_id", actor.accountID.String()), attribute.String("service_id", serviceID.String()))
			webhooks, err := s.ListWorkspaceWebhooks(ctx, serviceID)
			if err != nil {
				return nil, fmt.Errorf("list workspace webhooks: %w", err)
			}
			return projectGraphQLWorkspaceWebhooks(webhooks), nil
		},
	}
}

func webhookEventsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: webhookEventPageGraphQLType,
		Args: webhookAnalyticsArgs(true),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.webhook_events.list")
			defer span.End()
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			filter, err := webhookAnalyticsFilterFromArgs(p)
			if err != nil {
				return nil, err
			}
			if err := requireActiveWorkspaceService(ctx, s, filter.serviceID); err != nil {
				return nil, err
			}
			span.SetAttributes(
				attribute.String("service_id", filter.serviceID.String()),
				attribute.Int("limit", filter.limit),
				attribute.Int("offset", filter.offset),
			)
			events, total, err := s.ListWebhookEventsByService(ctx, actor.accountID, filter.serviceID, filter.eventName, filter.limit, filter.offset, filter.startDate, filter.endDate)
			if err != nil {
				return nil, fmt.Errorf("list webhook events: %w", err)
			}
			return map[string]interface{}{"items": projectGraphQLWebhookEvents(events), "total": int(total)}, nil
		},
	}
}

func webhookAnalyticsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: webhookAnalyticsGraphQLType,
		Args: webhookAnalyticsArgs(false),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.webhook_analytics.get")
			defer span.End()
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			filter, err := webhookAnalyticsFilterFromArgs(p)
			if err != nil {
				return nil, err
			}
			if err := requireActiveWorkspaceService(ctx, s, filter.serviceID); err != nil {
				return nil, err
			}
			span.SetAttributes(attribute.String("service_id", filter.serviceID.String()))
			analytics, err := s.GetWebhookAnalytics(ctx, actor.accountID, filter.serviceID, filter.eventName, filter.startDate, filter.endDate)
			if err != nil {
				return nil, fmt.Errorf("get webhook analytics: %w", err)
			}
			return projectGraphQLWebhookAnalytics(analytics), nil
		},
	}
}

func engineExecutionEventsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: engineExecutionEventPageGraphQLType,
		Args: engineExecutionActivityArgs(true),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.execution_events.list")
			defer span.End()
			filter, err := engineExecutionActivityFilterFromArgs(p)
			if err != nil {
				return nil, err
			}
			if err := requireActiveWorkspaceService(ctx, s, filter.serviceID); err != nil {
				return nil, err
			}
			span.SetAttributes(
				attribute.String("service_id", filter.serviceID.String()),
				attribute.String("transport", filter.transport),
				attribute.String("status", filter.status),
				attribute.Int("limit", filter.limit),
				attribute.Int("offset", filter.offset),
			)
			actor, err := actorFromContext(ctx)
			if err != nil {
				return nil, err
			}
			events, total, err := s.ListEngineExecutionEventsByService(ctx, filter.storeFilter(actor.accountID))
			if err != nil {
				return nil, fmt.Errorf("list engine execution events: %w", err)
			}
			return map[string]interface{}{"items": projectGraphQLEngineExecutionEvents(events), "total": int(total)}, nil
		},
	}
}

func appExecutionEventsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: engineExecutionEventPageGraphQLType,
		Args: appExecutionActivityArgs(true),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.app_execution_events.list")
			defer span.End()
			actor, err := actorFromContext(ctx)
			if err != nil {
				return nil, err
			}
			filter, err := appExecutionActivityFilterFromArgs(ctx, s, actor.accountID, p)
			if err != nil {
				return nil, err
			}
			reader, ok := s.(store.AppExecutionEventReader)
			if !ok {
				return nil, errors.New("app execution activity is unavailable")
			}
			span.SetAttributes(
				attribute.String("app.family_id", filter.appFamilyID.String()),
				attribute.String("app.id", filter.appID.String()),
				attribute.String("transport", filter.transport),
				attribute.String("status", filter.status),
				attribute.Int("limit", filter.limit),
				attribute.Int("offset", filter.offset),
			)
			events, total, err := reader.ListEngineExecutionEventsByApp(ctx, filter.storeFilter(actor.accountID))
			if err != nil {
				return nil, fmt.Errorf("list app execution events: %w", err)
			}
			return map[string]interface{}{"items": projectGraphQLEngineExecutionEvents(events), "total": int(total)}, nil
		},
	}
}

func appExecutionAnalyticsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: appExecutionAnalyticsGraphQLType,
		Args: appExecutionActivityArgs(false),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.app_execution_analytics.get")
			defer span.End()
			actor, err := actorFromContext(ctx)
			if err != nil {
				return nil, err
			}
			filter, err := appExecutionActivityFilterFromArgs(ctx, s, actor.accountID, p)
			if err != nil {
				return nil, err
			}
			reader, ok := s.(store.AppExecutionAnalyticsReader)
			if !ok {
				return nil, errors.New("app execution analytics is unavailable")
			}
			span.SetAttributes(
				attribute.String("app.family_id", filter.appFamilyID.String()),
				attribute.String("app.id", filter.appID.String()),
				attribute.String("transport", filter.transport),
			)
			analytics, err := reader.GetEngineExecutionAnalyticsByApp(ctx, filter.storeFilter(actor.accountID))
			if err != nil {
				return nil, fmt.Errorf("get app execution analytics: %w", err)
			}
			return projectGraphQLAppExecutionAnalytics(analytics), nil
		},
	}
}

func engineExecutionAnalyticsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: engineExecutionAnalyticsGraphQLType,
		Args: engineExecutionActivityArgs(false),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.execution_analytics.get")
			defer span.End()
			filter, err := engineExecutionActivityFilterFromArgs(p)
			if err != nil {
				return nil, err
			}
			if err := requireActiveWorkspaceService(ctx, s, filter.serviceID); err != nil {
				return nil, err
			}
			span.SetAttributes(attribute.String("service_id", filter.serviceID.String()))
			actor, err := actorFromContext(ctx)
			if err != nil {
				return nil, err
			}
			analytics, err := s.GetEngineExecutionAnalyticsByService(ctx, filter.storeFilter(actor.accountID))
			if err != nil {
				return nil, fmt.Errorf("get engine execution analytics: %w", err)
			}
			return projectGraphQLEngineExecutionAnalytics(analytics), nil
		},
	}
}

func workspaceExecutionAnalyticsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: workspaceExecutionAnalyticsGraphQLType,
		Args: graphql.FieldConfigArgument{
			"start_date": &graphql.ArgumentConfig{Type: graphql.String},
			"end_date":   &graphql.ArgumentConfig{Type: graphql.String},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.workspace_execution_analytics.get")
			defer span.End()
			actor, err := actorFromContext(ctx)
			if err != nil {
				return nil, err
			}
			startDate, endDate, err := workspaceExecutionRange(p)
			if err != nil {
				return nil, err
			}
			// A bounded duration keeps reporting reads diagnosable without turning
			// caller-selected timestamps into high-cardinality telemetry dimensions.
			span.SetAttributes(attribute.Int("range.duration_hours", int(endDate.Sub(startDate).Hours())))
			analytics, err := s.GetWorkspaceExecutionAnalytics(ctx, actor.accountID, startDate, endDate)
			if err != nil {
				return nil, fmt.Errorf("get workspace execution analytics: %w", err)
			}
			return projectGraphQLWorkspaceExecutionAnalytics(analytics), nil
		},
	}
}

// workspaceExecutionRange accepts exact UTC-capable timestamps while bounding
// default and caller-selected scans to the workspace reporting window.
func workspaceExecutionRange(p graphql.ResolveParams) (time.Time, time.Time, error) {
	endDate := time.Now().UTC()
	if value, err := parseOptionalRFC3339(graphQLStringArg(p, "end_date")); err != nil {
		return time.Time{}, time.Time{}, errors.New("invalid end_date")
	} else if value != nil {
		endDate = *value
	}
	startDate := endDate.Add(-7 * 24 * time.Hour)
	if value, err := parseOptionalRFC3339(graphQLStringArg(p, "start_date")); err != nil {
		return time.Time{}, time.Time{}, errors.New("invalid start_date")
	} else if value != nil {
		startDate = *value
	}
	if !startDate.Before(endDate) {
		return time.Time{}, time.Time{}, errors.New("start_date must be before end_date")
	}
	if endDate.Sub(startDate) > 90*24*time.Hour {
		return time.Time{}, time.Time{}, errors.New("activity range cannot exceed 90 days")
	}
	return startDate, endDate, nil
}

func serviceConsumersGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewList(serviceConsumerGraphQLType),
		Args: graphql.FieldConfigArgument{
			"service_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			serviceID, err := requiredGraphQLUUIDArg(p, "service_id")
			if err != nil {
				return nil, err
			}
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.service_consumers.list")
			defer span.End()
			if err := requireActiveWorkspaceService(ctx, s, serviceID); err != nil {
				return nil, err
			}
			repository, ok := s.(store.ServiceConsumerRepository)
			if !ok {
				return nil, errors.New("service consumers are unavailable")
			}
			actor, err := actorFromContext(ctx)
			if err != nil {
				return nil, err
			}
			authorized, err := graphQLAuthorizedScope(ctx, accesscontrol.PermissionAppRead, accesscontrol.ResourceApp)
			if err != nil {
				return nil, err
			}
			consumers, err := repository.ListServiceConsumers(ctx, actor.accountID, authorized, serviceID)
			if err != nil {
				return nil, err
			}
			return projectServiceConsumers(consumers), nil
		},
	}
}

func projectServiceConsumers(consumers []store.ServiceConsumer) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(consumers))
	for _, consumer := range consumers {
		items = append(items, map[string]interface{}{
			"id": consumer.AppID.String(), "name": consumer.Name, "version": consumer.Version,
			"kind": consumer.Kind, "active": consumer.Status.Runnable(),
			"service_version_id": consumer.ServiceVersionID.String(), "select_all": consumer.SelectAll,
			"operation_count": consumer.OperationCount, "webhook_count": consumer.WebhookCount,
			"created_at": formatGraphQLTime(consumer.CreatedAt),
		})
	}
	return items
}

func requireActiveWorkspaceService(ctx context.Context, s store.Store, serviceID uuid.UUID) error {
	enabled, err := s.IsWorkspaceServiceEnabled(ctx, serviceID)
	if err != nil {
		return fmt.Errorf("check workspace service activation: %w", err)
	}
	if !enabled {
		return errors.New("service is not active in this workspace")
	}
	return nil
}

func workspaceNotificationsGraphQLField(configStore store.ConfigRepository, s store.Store, registryClient sandbox.RegistryClient) *graphql.Field {
	return &graphql.Field{
		Type: workspaceNotificationInboxGraphQLType,
		// page/limit are both optional and default to the pre-pagination
		// behavior (limit 0 = unbounded, every item) so the bell panel and
		// contextual banners -- which don't pass either arg -- keep working
		// exactly as before. Only the full notifications page passes limit>0
		// to opt into windowed, numbered-page results. See
		// plans/plan-service-changelog.md's Phase 4 pagination follow-up.
		Args: graphql.FieldConfigArgument{
			"page":        &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 1},
			"limit":       &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
			"unread_only": &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
			"read_only":   &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.workspace_notifications.list")
			defer span.End()
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			span.SetAttributes(attribute.String("account_id", actor.accountID.String()))
			page, _ := p.Args["page"].(int)
			limit, _ := p.Args["limit"].(int)
			unreadOnly, _ := p.Args["unread_only"].(bool)
			readOnly, _ := p.Args["read_only"].(bool)
			inbox, err := workspaceNotificationInbox(ctx, configStore, s, registryClient, apiKeyFromGraphQLContext(p.Context), page, limit, unreadOnly, readOnly)
			if err != nil {
				return nil, fmt.Errorf("load workspace notifications: %w", err)
			}
			return projectGraphQLWorkspaceNotificationInbox(inbox), nil
		},
	}
}

// updateWorkspaceNotificationStatusGraphQLField is Phase 4's one write path
// on top of changelog-derived read-only notifications
// (plans/plan-service-changelog.md's "## Phase 4"): mark read
// ('acknowledged') or dismiss ('dismissed'). "id" is the notification's own
// row id -- not the "engine:"/"registry:" prefixed composite id
// workspaceNotificationInboxItem exposes for the merged inbox (a live drift
// snapshot from the "registry:" side isn't a store.WorkspaceNotification at
// all and was never in scope for this mutation; only Engine-local
// workspace_*/registry_* rows are).
func updateWorkspaceNotificationStatusGraphQLField(configStore store.ConfigRepository) *graphql.Field {
	return &graphql.Field{
		Type: workspaceNotificationGraphQLType,
		Args: graphql.FieldConfigArgument{
			"id":     &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"status": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.workspace_notification.update_status")
			defer span.End()
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			id, err := uuid.Parse(p.Args["id"].(string))
			if err != nil {
				return nil, fmt.Errorf("invalid id")
			}
			status := store.WorkspaceNotificationStatus(p.Args["status"].(string))
			span.SetAttributes(
				attribute.String("account_id", actor.accountID.String()),
				attribute.String("notification_id", id.String()),
				attribute.String("status", string(status)),
			)
			note, err := configStore.UpdateWorkspaceNotificationStatus(ctx, id, status, actor.accountID)
			if err != nil {
				return nil, err
			}
			items := workspaceNotificationInboxItems([]store.WorkspaceNotification{*note})
			return projectGraphQLWorkspaceNotification(items[0]), nil
		},
	}
}

func appTokensGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewList(appTokenGraphQLType),
		Args: graphql.FieldConfigArgument{
			"app_family_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.app_tokens.list")
			defer span.End()
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			appFamilyID, err := requiredGraphQLUUIDArg(p, "app_family_id")
			if err != nil {
				return nil, err
			}
			if _, err := appFamilyOwnedBy(ctx, s, actor.accountID, appFamilyID); err != nil {
				return nil, err
			}
			span.SetAttributes(attribute.String("app.family_id", appFamilyID.String()))
			tokens, err := s.ListAppTokens(ctx, appFamilyID)
			if err != nil {
				return nil, fmt.Errorf("list app tokens: %w", err)
			}
			return projectGraphQLAppTokens(tokens), nil
		},
	}
}

func appFamilyOwnedBy(ctx context.Context, s store.Store, accountID, appFamilyID uuid.UUID) (*store.AppFamily, error) {
	family, err := s.GetAppFamily(ctx, appFamilyID)
	if errors.Is(err, store.ErrAppFamilyNotFound) {
		return nil, workspaceConfigHTTPError{status: http.StatusNotFound, message: "app family not found"}
	}
	if err != nil {
		return nil, workspaceConfigHTTPError{status: http.StatusInternalServerError, message: "failed to load app family"}
	}
	if family.AccountID != accountID {
		return nil, workspaceConfigHTTPError{status: http.StatusForbidden, message: "app family is not available in this workspace"}
	}
	return family, nil
}

// sdkBucketsGraphQLField exposes the credential bucket(s) linked to one SDK
// scope. Runtime credential resolution uses the first linked bucket today, so
// the SDK details UI can present that exact boundary instead of guessing from
// the workspace default.
func sdkBucketsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewList(bucketGraphQLType),
		Args: graphql.FieldConfigArgument{
			"app_family_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.sdk_buckets.list")
			defer span.End()
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			appFamilyID, err := requiredGraphQLUUIDArg(p, "app_family_id")
			if err != nil {
				return nil, err
			}
			if _, err := appFamilyOwnedBy(p.Context, s, actor.accountID, appFamilyID); err != nil {
				return nil, err
			}
			span.SetAttributes(
				attribute.String("account_id", actor.accountID.String()),
				attribute.String("app.family_id", appFamilyID.String()),
			)
			authorized, err := graphQLAuthorizedScope(ctx, accesscontrol.PermissionBucketRead, accesscontrol.ResourceBucket)
			if err != nil {
				return nil, err
			}
			buckets, err := s.ListAuthorizedBucketsForAppFamily(ctx, appFamilyID, authorized)
			if err != nil {
				return nil, fmt.Errorf("list sdk buckets: %w", err)
			}
			return projectGraphQLBuckets(buckets), nil
		},
	}
}

func bucketSDKPageGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: bucketSDKSummaryPageGraphQLType,
		Args: bucketIDPageArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.bucket_sdks.list")
			defer span.End()
			bucketID, err := bucketIDFromArgs(p, s)
			if err != nil {
				return nil, err
			}
			limit, offset := bucketPageArgs(p)
			span.SetAttributes(
				attribute.String("bucket_id", bucketID.String()),
				attribute.Int("limit", limit),
				attribute.Int("offset", offset),
			)
			authorized, err := graphQLAuthorizedScope(ctx, accesscontrol.PermissionAppRead, accesscontrol.ResourceApp)
			if err != nil {
				return nil, err
			}
			scopes, total, err := s.ListAuthorizedAppRuntimesForBucket(ctx, bucketID, authorized, limit, offset)
			if err != nil {
				return nil, fmt.Errorf("list bucket sdks: %w", err)
			}
			return map[string]interface{}{"items": projectGraphQLBucketSDKSummaries(scopes), "total": total}, nil
		},
	}
}

func bucketServicePageGraphQLField(s store.Store) *graphql.Field {
	args := bucketIDPageArgs()
	args["search"] = &graphql.ArgumentConfig{Type: graphql.String}
	return &graphql.Field{
		Type: bucketServiceSummaryPageGraphQLType,
		Args: args,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.bucket_services.list")
			defer span.End()
			bucketID, err := bucketIDFromArgs(p, s)
			if err != nil {
				return nil, err
			}
			limit, offset := bucketPageArgs(p)
			search, _ := p.Args["search"].(string)
			span.SetAttributes(
				attribute.String("bucket_id", bucketID.String()),
				attribute.Int("limit", limit),
				attribute.Int("offset", offset),
			)
			if search != "" {
				span.SetAttributes(attribute.String("search", search))
			}
			authorized, err := graphQLAuthorizedScope(ctx, accesscontrol.PermissionServiceRead, accesscontrol.ResourceService)
			if err != nil {
				return nil, err
			}
			services, total, err := s.ListAuthorizedBucketServiceSummaries(ctx, bucketID, authorized, search, limit, offset)
			if err != nil {
				return nil, fmt.Errorf("list bucket services: %w", err)
			}
			return map[string]interface{}{"items": projectGraphQLBucketServiceSummaries(services), "total": total}, nil
		},
	}
}

// bucketValuesGraphQLField exposes non-secret bucket values through the same
// Engine GraphQL ownership boundary as connect auth.
func bucketValuesGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewList(bucketValueGraphQLType),
		Args: graphql.FieldConfigArgument{
			"bucket_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.bucket_values.list")
			defer span.End()
			bucketID, err := bucketIDFromArgs(p, s)
			if err != nil {
				return nil, err
			}
			span.SetAttributes(attribute.String("bucket_id", bucketID.String()))
			values, err := s.ListBucketValues(ctx, bucketID)
			if err != nil {
				return nil, fmt.Errorf("list bucket values: %w", err)
			}
			return projectGraphQLBucketValues(values), nil
		},
	}
}

func bucketValuePageGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: bucketValuePageGraphQLType,
		Args: bucketIDPageArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.bucket_values.page")
			defer span.End()
			bucketID, err := bucketIDFromArgs(p, s)
			if err != nil {
				return nil, err
			}
			limit, offset := bucketPageArgs(p)
			span.SetAttributes(
				attribute.String("bucket_id", bucketID.String()),
				attribute.Int("limit", limit),
				attribute.Int("offset", offset),
			)
			values, total, err := s.ListBucketValuePage(ctx, bucketID, limit, offset)
			if err != nil {
				return nil, fmt.Errorf("list bucket value page: %w", err)
			}
			return map[string]interface{}{"items": projectGraphQLBucketValues(values), "total": total}, nil
		},
	}
}

// secretMetasGraphQLField returns metadata only so the UI can inspect expiry
// and last-use state without ever reading decrypted secret values.
func secretMetasGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewList(secretMetaGraphQLType),
		Args: graphql.FieldConfigArgument{
			"bucket_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.secret_metas.list")
			defer span.End()
			bucketID, err := bucketIDFromArgs(p, s)
			if err != nil {
				return nil, err
			}
			span.SetAttributes(attribute.String("bucket_id", bucketID.String()))
			secrets, err := s.ListSecretMeta(ctx, bucketID)
			if err != nil {
				return nil, fmt.Errorf("list secret metadata: %w", err)
			}
			return projectGraphQLSecretMetas(secrets), nil
		},
	}
}

func secretMetaPageGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: secretMetaPageGraphQLType,
		Args: bucketIDPageArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.secret_metas.page")
			defer span.End()
			bucketID, err := bucketIDFromArgs(p, s)
			if err != nil {
				return nil, err
			}
			limit, offset := bucketPageArgs(p)
			span.SetAttributes(
				attribute.String("bucket_id", bucketID.String()),
				attribute.Int("limit", limit),
				attribute.Int("offset", offset),
			)
			secrets, total, err := s.ListSecretMetaPage(ctx, bucketID, limit, offset)
			if err != nil {
				return nil, fmt.Errorf("list secret metadata page: %w", err)
			}
			return map[string]interface{}{"items": projectGraphQLSecretMetas(secrets), "total": total}, nil
		},
	}
}

func bucketConnectSummaryGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: bucketConnectSummaryGraphQLType,
		Args: graphql.FieldConfigArgument{
			"bucket_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.bucket_connect_summary.get")
			defer span.End()
			bucketID, err := bucketIDFromArgs(p, s)
			if err != nil {
				return nil, err
			}
			span.SetAttributes(attribute.String("bucket_id", bucketID.String()))
			summary, err := s.GetBucketConnectSummary(ctx, bucketID)
			if err != nil {
				return nil, fmt.Errorf("get bucket connect summary: %w", err)
			}
			return projectGraphQLBucketConnectSummary(summary), nil
		},
	}
}

// workspaceConnectConfigsGraphQLField exposes the complete safe read model
// needed by workspace sync. One resolver avoids per-service GraphQL calls,
// while its store contract guarantees a fixed number of SQL reads.
func workspaceConnectConfigsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewList(workspaceConnectConfigGraphQLType),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.workspace_connect_configs.list")
			defer span.End()
			actor, err := actorFromContext(ctx)
			if err != nil {
				return nil, err
			}
			reader, ok := s.(store.WorkspaceConnectSyncStore)
			if !ok {
				return nil, errors.New("workspace connect config sync is unavailable")
			}
			configs, err := reader.ListWorkspaceConnectConfigs(ctx)
			if err != nil {
				return nil, fmt.Errorf("list workspace connect configs: %w", err)
			}
			profiles, err := reader.ListWorkspaceConnectProfiles(ctx)
			if err != nil {
				return nil, fmt.Errorf("list workspace connect profiles: %w", err)
			}
			span.SetAttributes(attribute.String("account_id", actor.accountID.String()), attribute.Int("config_count", len(configs)), attribute.Int("profile_count", len(profiles)))
			return projectGraphQLWorkspaceConnectConfigs(configs, profiles), nil
		},
	}
}

// projectGraphQLWorkspaceConnectConfigs groups already-scoped profile rows by
// service+auth identity so the GraphQL payload remains deterministic. Unlike
// the old bucket-owned model, one workspace profile now serves every bucket's
// Connect config for the same service and auth type -- profiles carry no
// bucket dimension.
func projectGraphQLWorkspaceConnectConfigs(configs []store.WorkspaceConnectConfig, profiles []store.WorkspaceConnectionProfile) []map[string]interface{} {
	profilesByConfig := make(map[string][]store.WorkspaceConnectionProfile)
	for _, profile := range profiles {
		key := workspaceConnectIdentityKey(profile.ServiceID, profile.AuthType)
		profilesByConfig[key] = append(profilesByConfig[key], profile)
	}
	items := make([]map[string]interface{}, 0, len(configs))
	for _, config := range configs {
		key := workspaceConnectIdentityKey(config.ServiceID, config.AuthType)
		items = append(items, projectGraphQLWorkspaceConnectConfig(config, profilesByConfig[key]))
	}
	return items
}

// projectGraphQLWorkspaceConnectConfig deliberately converts encrypted client
// material to presence flags; sync must never receive decryptable secrets.
func projectGraphQLWorkspaceConnectConfig(config store.WorkspaceConnectConfig, profiles []store.WorkspaceConnectionProfile) map[string]interface{} {
	profileItems := make([]map[string]interface{}, 0, len(profiles))
	for index := range profiles {
		profileItems = append(profileItems, workspaceProfileFields(&profiles[index], nil))
	}
	return map[string]interface{}{
		"bucket_id": config.BucketID.String(), "bucket_name": config.BucketName,
		"service_id": config.ServiceID.String(), "auth_type": config.AuthType, "auth_name": config.AuthName,
		"enabled": config.Enabled, "redirect_uri": config.RedirectURI,
		"has_client_id": config.EncryptedClientID != "", "has_client_secret": config.EncryptedClientSecret != "",
		"profiles": profileItems,
	}
}

// workspaceConnectIdentityKey mirrors the profile table's uniqueness
// dimensions (minus bucket, which profiles no longer carry) so in-memory
// grouping cannot collapse another authentication family.
func workspaceConnectIdentityKey(serviceID uuid.UUID, authType string) string {
	return serviceID.String() + "\x00" + authType
}

// authConnectionsGraphQLField passes service/user filters to Store so the
// database, not Go, owns filtering for production requests.
func authConnectionsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewList(authConnectionGraphQLType),
		Args: graphql.FieldConfigArgument{
			"bucket_id":    &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"service_id":   &graphql.ArgumentConfig{Type: graphql.String},
			"end_user_ref": &graphql.ArgumentConfig{Type: graphql.String},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.auth_connections.list")
			defer span.End()
			bucketID, err := bucketIDFromArgs(p, s)
			if err != nil {
				return nil, err
			}
			serviceID, err := optionalGraphQLUUIDArg(p, "service_id")
			if err != nil {
				return nil, err
			}
			endUserRef, _ := p.Args["end_user_ref"].(string)
			span.SetAttributes(attribute.String("bucket_id", bucketID.String()))
			if serviceID != nil {
				span.SetAttributes(attribute.String("service_id", serviceID.String()))
			}
			connections, err := s.ListAuthConnections(ctx, bucketID, serviceID, endUserRef)
			if err != nil {
				return nil, fmt.Errorf("list auth connections: %w", err)
			}
			return projectGraphQLAuthConnections(connections), nil
		},
	}
}

func authConnectionPageGraphQLField(s store.Store) *graphql.Field {
	args := bucketIDPageArgs()
	args["service_id"] = &graphql.ArgumentConfig{Type: graphql.String}
	args["end_user_ref"] = &graphql.ArgumentConfig{Type: graphql.String}
	return &graphql.Field{
		Type: authConnectionPageGraphQLType,
		Args: args,
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.auth_connections.page")
			defer span.End()
			bucketID, err := bucketIDFromArgs(p, s)
			if err != nil {
				return nil, err
			}
			serviceID, err := optionalGraphQLUUIDArg(p, "service_id")
			if err != nil {
				return nil, err
			}
			endUserRef, _ := p.Args["end_user_ref"].(string)
			limit, offset := bucketPageArgs(p)
			span.SetAttributes(
				attribute.String("bucket_id", bucketID.String()),
				attribute.Int("limit", limit),
				attribute.Int("offset", offset),
			)
			if serviceID != nil {
				span.SetAttributes(attribute.String("service_id", serviceID.String()))
			}
			connections, total, err := s.ListAuthConnectionsPage(ctx, bucketID, serviceID, endUserRef, limit, offset)
			if err != nil {
				return nil, fmt.Errorf("list auth connection page: %w", err)
			}
			return map[string]interface{}{"items": projectGraphQLAuthConnections(connections), "total": total}, nil
		},
	}
}

// connectionResourcesGraphQLField verifies opaque connection ownership before
// returning active admin metadata in one store query.
func connectionResourcesGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewList(connectionResourceGraphQLType),
		Args: graphql.FieldConfigArgument{"connection_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)}},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.connection_resources.list")
			defer span.End()
			connection, err := ownedGraphQLConnection(ctx, p)
			if err != nil {
				return nil, err
			}
			resources, err := s.ListConnectionResources(ctx, connection.ID)
			if err != nil {
				return nil, fmt.Errorf("list connection resources: %w", err)
			}
			span.SetAttributes(attribute.Int("resource_count", len(resources)))
			return projectGraphQLConnectionResources(resources), nil
		},
	}
}

// setDefaultConnectionResourceGraphQLField records the user-triggered routing
// change without putting provider IDs, URLs, or metadata into telemetry.
func setDefaultConnectionResourceGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: connectionResourceGraphQLType,
		Args: graphql.FieldConfigArgument{
			"connection_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"resource_id":   &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.connection_resources.set_default")
			defer span.End()
			connection, err := ownedGraphQLConnection(ctx, p)
			if err != nil {
				return nil, err
			}
			resourceID, err := requiredGraphQLUUIDArg(p, "resource_id")
			if err != nil {
				return nil, err
			}
			resource, err := s.SetDefaultConnectionResource(ctx, connection.ID, resourceID)
			if err != nil {
				return nil, fmt.Errorf("set default connection resource: %w", err)
			}
			span.SetAttributes(attribute.String("action", "connection_resource.set_default"), attribute.String("resource_type", resource.ResourceType))
			return projectGraphQLConnectionResource(*resource), nil
		},
	}
}

// rediscoverConnectionResourcesGraphQLField reruns the attached versioned
// discovery profile with the connection's current, proactively refreshed token.
func rediscoverConnectionResourcesGraphQLField(s store.Store, verifier ServiceVerifier, masterKey []byte) *graphql.Field {
	return &graphql.Field{
		Type: graphql.NewList(connectionResourceGraphQLType),
		Args: graphql.FieldConfigArgument{
			"connection_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.connection_resources.rediscover")
			defer span.End()
			connection, err := ownedGraphQLConnection(ctx, p)
			if err != nil {
				return nil, err
			}
			_, err = actorFromContext(ctx)
			if err != nil {
				return nil, err
			}
			resources, err := rediscoverConnectionResources(ctx, s, verifier, masterKey, connection)
			if err != nil {
				span.SetAttributes(attribute.String("outcome", "error"), attribute.String("auth_type", connection.AuthType))
				return nil, err
			}
			span.SetAttributes(
				attribute.String("outcome", "updated"),
				attribute.String("bucket_id", connection.BucketID.String()),
				attribute.String("service_id", connection.ServiceID.String()),
				attribute.String("auth_type", connection.AuthType),
				attribute.Int("resource_count", len(resources)),
			)
			return projectGraphQLConnectionResources(resources), nil
		},
	}
}

// rediscoverConnectionResources resolves all provider metadata from the pinned
// workspace version, preventing the mutation from accepting caller-owned URLs.
func rediscoverConnectionResources(ctx context.Context, s store.Store, verifier ServiceVerifier, masterKey []byte, connection *store.AuthConnection) ([]store.ConnectionResource, error) {
	metadata, endpoint, auth, err := connectionDiscoveryContract(ctx, s, verifier, connection)
	if err != nil {
		return nil, err
	}
	token, err := sandbox.ResolveConnectionAccessToken(ctx, s, masterKey, connection, metadata.ServiceVersionID, sandbox.AuthCredentialName(auth))
	if err != nil {
		return nil, errors.New("connected token is unavailable")
	}
	discovered, err := connectresource.Discover(ctx, metadata, endpoint, token, connection.TokenType)
	if err != nil {
		return nil, err
	}
	if _, err := storeConnectionResources(ctx, s, connection, discovered); err != nil {
		return nil, err
	}
	return s.ListConnectionResources(ctx, connection.ID)
}

// connectionDiscoveryContract reconstructs discovery only from the pinned
// Registry contract and bucket attachment, keeping caller-controlled URLs out
// of manual lifecycle actions.
func connectionDiscoveryContract(ctx context.Context, s store.Store, verifier ServiceVerifier, connection *store.AuthConnection) (*fusedobject.ServiceMetadata, *fusedobject.Endpoint, fusedobject.AuthConfig, error) {
	metadata, err := connectionDiscoveryMetadata(ctx, s, verifier, connection)
	if err != nil {
		return nil, nil, fusedobject.AuthConfig{}, err
	}
	discoveryVerifier, ok := verifier.(connectionDiscoveryVerifier)
	if !ok {
		return nil, nil, fusedobject.AuthConfig{}, errors.New("resource discovery is unavailable")
	}
	config := metadata.ConnectConfig.ResourceDiscovery
	endpoint, err := discoveryVerifier.FetchEndpointByName(ctx, connection.ServiceID, metadata.ServiceVersionID.String(), config.OperationID)
	if err != nil {
		return nil, nil, fusedobject.AuthConfig{}, errors.New("resource discovery operation is unavailable")
	}
	auth, _, err := selectRuntimeOAuthConfig(metadata.AuthConfigs, connection.AuthType, connection.AuthName, "authorizationCode")
	if err != nil {
		return nil, nil, fusedobject.AuthConfig{}, err
	}
	return metadata, endpoint, auth, nil
}

// connectionDiscoveryMetadata loads and attaches the exact provider profile
// before endpoint and auth selection inspect its discovery declaration.
func connectionDiscoveryMetadata(ctx context.Context, s store.Store, verifier ServiceVerifier, connection *store.AuthConnection) (*fusedobject.ServiceMetadata, error) {
	version, err := connectionDiscoveryVersion(ctx, s, connection)
	if err != nil {
		return nil, fmt.Errorf("load workspace service version: %w", err)
	}
	metadata, err := verifier.FetchServiceMetadata(ctx, connection.ServiceID, version)
	if err != nil {
		return nil, fmt.Errorf("load service metadata: %w", err)
	}
	if metadata == nil || metadata.ServiceVersionID == uuid.Nil ||
		(connection.ServiceVersionID != uuid.Nil && metadata.ServiceVersionID != connection.ServiceVersionID) {
		return nil, errors.New("resource discovery service version does not match the connection")
	}
	call := connectAdminCall{bucketID: connection.BucketID, serviceID: connection.ServiceID}
	metadata, err = attachedConnectMetadata(ctx, s, call, connection.AuthType, metadata)
	if err != nil {
		return nil, err
	}
	if metadata.ConnectConfig == nil || metadata.ConnectConfig.ResourceDiscovery == nil {
		return nil, errors.New("connection does not have resource discovery configured")
	}
	return metadata, nil
}

// connectionDiscoveryVersion preserves a connection's consent-time contract;
// only a legacy unpinned row may resolve the current activated version once.
func connectionDiscoveryVersion(ctx context.Context, s store.Store, connection *store.AuthConnection) (string, error) {
	if connection.ServiceVersionID == uuid.Nil {
		return s.GetLatestWorkspaceServiceVersionByWorkspace(ctx, connection.ServiceID)
	}
	lookup, ok := s.(store.WorkspaceServiceVersionLookupStore)
	if !ok {
		return "", errors.New("exact workspace service version lookup is unavailable")
	}
	version, err := lookup.GetWorkspaceServiceVersion(ctx, connection.ServiceID, connection.ServiceVersionID)
	if err != nil || version == nil {
		return "", errors.New("exact workspace service version is unavailable")
	}
	return version.Version, nil
}

type graphQLResolvedConnectionsContextKey struct{}

// ownedGraphQLConnection reuses the authorization preflight's batch lookup.
// Resolvers cannot broaden an opaque connection ID into an unscoped DB read.
func ownedGraphQLConnection(ctx context.Context, p graphql.ResolveParams) (*store.AuthConnection, error) {
	connectionID, err := requiredGraphQLUUIDArg(p, "connection_id")
	if err != nil {
		return nil, err
	}
	connections, _ := ctx.Value(graphQLResolvedConnectionsContextKey{}).(map[uuid.UUID]store.AuthConnection)
	connection, ok := connections[connectionID]
	if !ok {
		return nil, errors.New("auth connection not found")
	}
	return &connection, nil
}

// upsertSecretsGraphQLField gives the UI a GraphQL mutation while reusing the
// same validation/encryption helpers as the CLI-facing HTTP bulk endpoint.
func upsertSecretsGraphQLField(s store.Store, masterKey []byte) *graphql.Field {
	return &graphql.Field{
		Type: graphql.Boolean,
		Args: graphql.FieldConfigArgument{
			"bucket_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"secrets":   &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewList(secretUpsertGraphQLInput))},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			_, bucketID, payloads, err := secretBulkGraphQLArgs(p, s)
			if err != nil {
				return nil, err
			}
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.secrets.bulk_upsert")
			defer span.End()
			if err := validateSecretBulkPayload(payloads, time.Now()); err != nil {
				return nil, err
			}
			secrets, err := buildEncryptedSecrets(bucketID, payloads, masterKey)
			if err != nil {
				return nil, fmt.Errorf("encrypt secrets: %w", err)
			}
			if err := s.UpsertSecrets(ctx, secrets); err != nil {
				return nil, fmt.Errorf("save secrets: %w", err)
			}
			span.SetAttributes(
				attribute.String("bucket_id", bucketID.String()),
				attribute.Int("secret_count", len(secrets)),
				attribute.Int("mtls_pair_count", countMTLSPairs(payloads)),
			)
			return true, nil
		},
	}
}

// startConnectSessionGraphQLField reuses the runtime connect flow so provider
// metadata, PKCE, and encrypted session state stay centralized in Engine.
func startConnectSessionGraphQLField(s store.Store, verifier ServiceVerifier, masterKey []byte) *graphql.Field {
	return &graphql.Field{
		Type: connectSessionGraphQLType,
		Args: connectSessionMutationArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			call, err := connectAdminCallFromArgs(p, s)
			if err != nil {
				return nil, err
			}
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.connect_session.start")
			defer span.End()
			// GraphQL validation can return before session creation, so establish a
			// secret-safe failed outcome and replace it only after persistence.
			span.SetAttributes(attribute.String("outcome", "failed"))
			span.SetAttributes(connectAdminAttrs("connect.session.start", call)...)
			endUserRef := strings.TrimSpace(fmt.Sprint(p.Args["end_user_ref"]))
			if endUserRef == "" {
				return nil, errors.New("end_user_ref is required")
			}
			createdByAppID, err := optionalGraphQLUUIDArg(p, "created_by_app_id")
			if err != nil {
				return nil, err
			}
			resolved, err := resolveConnectRuntimeConfig(ctx, s, verifier, call, masterKey)
			if err != nil {
				return nil, err
			}
			returnURL, _ := p.Args["return_url"].(string)
			returnURL = strings.TrimSpace(returnURL)
			if returnURL != "" && !isHTTPRedirectURI(returnURL) {
				return nil, errors.New("return_url must be an absolute http or https URL")
			}
			response, err := createConnectSession(ctx, s, call, endUserRef, optionalUUIDValueOrNil(createdByAppID), returnURL, graphQLStringMapArg(p, "resource_input"), graphQLStringSliceArg(p, "scopes"), resolved, masterKey)
			if err != nil {
				return nil, fmt.Errorf("create connect session: %w", err)
			}
			span.SetAttributes(connectSessionStartTelemetry(response)...)
			return projectGraphQLConnectSession(response), nil
		},
	}
}

// deleteAuthConnectionGraphQLField keeps deletion bucket-scoped so a user ref
// connected through one bucket cannot delete another bucket's auth material.
func deleteAuthConnectionGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.Boolean,
		Args: graphql.FieldConfigArgument{
			"bucket_id":     &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"connection_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			call, connectionID, err := authConnectionDeleteArgs(p, s)
			if err != nil {
				return nil, err
			}
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.auth_connections.delete")
			defer span.End()
			span.SetAttributes(connectBucketAdminAttrs("auth_connection.delete", call, connectionID)...)
			if err := s.DeleteAuthConnection(ctx, call.bucketID, connectionID); err != nil {
				if errors.Is(err, store.ErrAuthConnectionNotFound) {
					return nil, fmt.Errorf("auth connection not found")
				}
				return nil, fmt.Errorf("delete auth connection: %w", err)
			}
			span.SetAttributes(attribute.String("outcome", "success"))
			return true, nil
		},
	}
}

func deleteSecretsGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: graphql.Boolean,
		Args: graphql.FieldConfigArgument{
			"bucket_id":  &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"service_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"key_names":  &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.NewList(graphql.NewNonNull(graphql.String)))},
		},
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			bucketID, err := bucketIDFromArgs(p, s)
			if err != nil {
				return nil, err
			}
			serviceID, err := requiredGraphQLUUIDArg(p, "service_id")
			if err != nil {
				return nil, err
			}
			keyNames := compactGraphQLStrings(graphQLStringListArg(p, "key_names"))
			if len(keyNames) == 0 {
				return nil, errors.New("key_names is required")
			}
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.graphql.secrets.delete")
			defer span.End()
			span.SetAttributes(attribute.String("bucket_id", bucketID.String()), attribute.String("service_id", serviceID.String()), attribute.Int("secret_count", len(keyNames)))
			if err := s.DeleteSecrets(ctx, bucketID, serviceID, keyNames); err != nil {
				span.RecordError(err)
				span.SetAttributes(attribute.String("outcome", "failure"))
				return nil, fmt.Errorf("delete secrets: %w", err)
			}
			span.SetAttributes(attribute.String("outcome", "success"))
			return true, nil
		},
	}
}

// connectScopedArgs is shared by config/session fields so every connect action
// names both the bucket and service explicitly.
func connectScopedArgs() graphql.FieldConfigArgument {
	return graphql.FieldConfigArgument{
		"bucket_id":  &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		"service_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
	}
}

type webhookAnalyticsFilter struct {
	serviceID uuid.UUID
	eventName string
	limit     int
	offset    int
	startDate *time.Time
	endDate   *time.Time
}

type engineExecutionActivityFilter struct {
	serviceID uuid.UUID
	transport string
	direction string
	status    string
	limit     int
	offset    int
	startDate *time.Time
	endDate   *time.Time
}

type appExecutionActivityFilter struct {
	appFamilyID uuid.UUID
	appID       uuid.UUID
	transport   string
	direction   string
	status      string
	limit       int
	offset      int
	startDate   *time.Time
	endDate     *time.Time
}

func engineExecutionActivityArgs(includePage bool) graphql.FieldConfigArgument {
	return scopedExecutionActivityArgs("service_id", includePage)
}

func appExecutionActivityArgs(includePage bool) graphql.FieldConfigArgument {
	args := scopedExecutionActivityArgs("app_id", includePage)
	args["include_all_versions"] = &graphql.ArgumentConfig{Type: graphql.Boolean, DefaultValue: false}
	return args
}

func scopedExecutionActivityArgs(scopeArgument string, includePage bool) graphql.FieldConfigArgument {
	args := graphql.FieldConfigArgument{
		scopeArgument: &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		"transport":   &graphql.ArgumentConfig{Type: graphql.String},
		"direction":   &graphql.ArgumentConfig{Type: graphql.String},
		"status":      &graphql.ArgumentConfig{Type: graphql.String},
		"start_date":  &graphql.ArgumentConfig{Type: graphql.String},
		"end_date":    &graphql.ArgumentConfig{Type: graphql.String},
	}
	if includePage {
		args["limit"] = &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 10}
		args["offset"] = &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0}
	}
	return args
}

func appExecutionActivityFilterFromArgs(ctx context.Context, s store.Store, accountID uuid.UUID, p graphql.ResolveParams) (appExecutionActivityFilter, error) {
	appID, err := requiredGraphQLUUIDArg(p, "app_id")
	if err != nil {
		return appExecutionActivityFilter{}, err
	}
	appFamilyID, err := appExecutionActivityFamilyID(ctx, s, accountID, appID)
	if err != nil {
		return appExecutionActivityFilter{}, err
	}
	transport, direction, status, err := engineExecutionDimensionsFromArgs(p)
	if err != nil {
		return appExecutionActivityFilter{}, err
	}
	startDate, endDate, err := engineExecutionDatesFromArgs(p)
	if err != nil {
		return appExecutionActivityFilter{}, err
	}
	limit, offset := bucketPageArgs(p)
	if includeAll, _ := p.Args["include_all_versions"].(bool); includeAll {
		appID = uuid.Nil
	}
	return appExecutionActivityFilter{
		appFamilyID: appFamilyID, appID: appID, transport: transport, direction: direction, status: status,
		limit: limit, offset: offset, startDate: startDate, endDate: endDate,
	}, nil
}

func appExecutionActivityFamilyID(ctx context.Context, s store.Store, accountID, appID uuid.UUID) (uuid.UUID, error) {
	resolver, ok := s.(store.AppFamilyAccessResolver)
	if !ok {
		return uuid.Nil, errors.New("app execution activity is unavailable")
	}
	resolved, err := resolver.ResolveAppFamilyAccess(ctx, accountID, []uuid.UUID{appID})
	if err != nil {
		return uuid.Nil, errors.New("failed to resolve app execution activity")
	}
	if familyID := resolved[appID]; familyID != uuid.Nil {
		return familyID, nil
	}
	return uuid.Nil, errors.New("app execution activity was not found")
}

func engineExecutionActivityFilterFromArgs(p graphql.ResolveParams) (engineExecutionActivityFilter, error) {
	serviceID, err := requiredGraphQLUUIDArg(p, "service_id")
	if err != nil {
		return engineExecutionActivityFilter{}, err
	}
	transport, direction, status, err := engineExecutionDimensionsFromArgs(p)
	if err != nil {
		return engineExecutionActivityFilter{}, err
	}
	startDate, endDate, err := engineExecutionDatesFromArgs(p)
	if err != nil {
		return engineExecutionActivityFilter{}, err
	}
	limit, offset := bucketPageArgs(p)
	return engineExecutionActivityFilter{
		serviceID: serviceID, transport: transport, direction: direction, status: status,
		limit: limit, offset: offset, startDate: startDate, endDate: endDate,
	}, nil
}

func engineExecutionDimensionsFromArgs(p graphql.ResolveParams) (string, string, string, error) {
	transport := strings.ToLower(strings.TrimSpace(graphQLStringArg(p, "transport")))
	if err := validateOptionalExecutionDimension(transport, "transport", models.EngineExecutionTransportSDK, models.EngineExecutionTransportMCP, models.EngineExecutionTransportREST, models.EngineExecutionTransportWebhook); err != nil {
		return "", "", "", err
	}
	direction := strings.ToLower(strings.TrimSpace(graphQLStringArg(p, "direction")))
	if err := validateOptionalExecutionDimension(direction, "direction", models.EngineExecutionDirectionInbound, models.EngineExecutionDirectionOutbound); err != nil {
		return "", "", "", err
	}
	status := strings.ToLower(strings.TrimSpace(graphQLStringArg(p, "status")))
	if err := validateOptionalExecutionDimension(status, "status", models.EngineExecutionStatusSuccess, models.EngineExecutionStatusFailed); err != nil {
		return "", "", "", err
	}
	return transport, direction, status, nil
}

func validateOptionalExecutionDimension(value, name string, allowed ...string) error {
	if value == "" {
		return nil
	}
	for _, candidate := range allowed {
		if value == candidate {
			return nil
		}
	}
	return fmt.Errorf("invalid %s", name)
}

func engineExecutionDatesFromArgs(p graphql.ResolveParams) (*time.Time, *time.Time, error) {
	startDate, err := parseOptionalRFC3339(graphQLStringArg(p, "start_date"))
	if err != nil {
		return nil, nil, errors.New("invalid start_date")
	}
	endDate, err := parseOptionalRFC3339(graphQLStringArg(p, "end_date"))
	if err != nil {
		return nil, nil, errors.New("invalid end_date")
	}
	return startDate, endDate, nil
}

func (f engineExecutionActivityFilter) storeFilter(accountID uuid.UUID) store.EngineExecutionFilter {
	return store.EngineExecutionFilter{
		AccountID: accountID, ServiceID: f.serviceID, Transport: f.transport, Direction: f.direction,
		Status: f.status, Limit: f.limit, Offset: f.offset, StartDate: f.startDate, EndDate: f.endDate,
	}
}

func (f appExecutionActivityFilter) storeFilter(accountID uuid.UUID) store.EngineExecutionFilter {
	return store.EngineExecutionFilter{
		AccountID: accountID, AppFamilyID: f.appFamilyID, AppID: f.appID, Transport: f.transport, Direction: f.direction,
		Status: f.status, Limit: f.limit, Offset: f.offset, StartDate: f.startDate, EndDate: f.endDate,
	}
}

func webhookAnalyticsArgs(includePage bool) graphql.FieldConfigArgument {
	args := graphql.FieldConfigArgument{
		"service_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		"event_name": &graphql.ArgumentConfig{Type: graphql.String},
		"start_date": &graphql.ArgumentConfig{Type: graphql.String},
		"end_date":   &graphql.ArgumentConfig{Type: graphql.String},
	}
	if includePage {
		args["limit"] = &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 10}
		args["offset"] = &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0}
	}
	return args
}

func webhookAnalyticsFilterFromArgs(p graphql.ResolveParams) (webhookAnalyticsFilter, error) {
	serviceID, err := requiredGraphQLUUIDArg(p, "service_id")
	if err != nil {
		return webhookAnalyticsFilter{}, err
	}
	limit, offset := bucketPageArgs(p)
	startDate, err := parseOptionalRFC3339(graphQLStringArg(p, "start_date"))
	if err != nil {
		return webhookAnalyticsFilter{}, fmt.Errorf("invalid start_date")
	}
	endDate, err := parseOptionalRFC3339(graphQLStringArg(p, "end_date"))
	if err != nil {
		return webhookAnalyticsFilter{}, fmt.Errorf("invalid end_date")
	}
	return webhookAnalyticsFilter{
		serviceID: serviceID,
		eventName: strings.TrimSpace(graphQLStringArg(p, "event_name")),
		limit:     limit,
		offset:    offset,
		startDate: startDate,
		endDate:   endDate,
	}, nil
}

func bucketIDPageArgs() graphql.FieldConfigArgument {
	return graphql.FieldConfigArgument{
		"bucket_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		"limit":     &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 20},
		"offset":    &graphql.ArgumentConfig{Type: graphql.Int, DefaultValue: 0},
	}
}

// connectSessionMutationArgs requires an end-user ref because connected auth is
// reusable by bucket plus service plus user, not by SDK instance alone.
func connectSessionMutationArgs() graphql.FieldConfigArgument {
	args := connectScopedArgs()
	args["end_user_ref"] = &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)}
	args["created_by_app_id"] = &graphql.ArgumentConfig{Type: graphql.String}
	args["return_url"] = &graphql.ArgumentConfig{Type: graphql.String}
	args["resource_input"] = &graphql.ArgumentConfig{Type: engineJSONType}
	args["scopes"] = &graphql.ArgumentConfig{Type: graphql.NewList(graphql.NewNonNull(graphql.String))}
	return args
}

// graphQLStringSliceArg keeps list coercion at the GraphQL boundary so scope
// policy receives one concrete representation regardless of caller language.
func graphQLStringSliceArg(p graphql.ResolveParams, name string) []string {
	raw, ok := p.Args[name].([]interface{})
	if !ok {
		return nil
	}
	values := make([]string, 0, len(raw))
	for _, value := range raw {
		if text, ok := value.(string); ok {
			values = append(values, text)
		}
	}
	return values
}

// graphQLStringMapArg accepts the JSON scalar only when every value is a
// string, keeping tenant input separate from arbitrary nested metadata.
func graphQLStringMapArg(p graphql.ResolveParams, name string) map[string]string {
	raw, ok := p.Args[name].(map[string]interface{})
	if !ok {
		return nil
	}
	values := make(map[string]string, len(raw))
	for key, value := range raw {
		if text, ok := value.(string); ok {
			values[key] = text
		}
	}
	return values
}

// connectAdminCallFromArgs builds the common ownership tuple once so each
// resolver does not repeat bucket and service parsing logic.
func connectAdminCallFromArgs(p graphql.ResolveParams, s store.Store) (connectAdminCall, error) {
	bucketID, err := bucketIDFromArgs(p, s)
	if err != nil {
		return connectAdminCall{}, err
	}
	serviceID, err := requiredGraphQLUUIDArg(p, "service_id")
	if err != nil {
		return connectAdminCall{}, err
	}
	return connectAdminCall{bucketID: bucketID, serviceID: serviceID}, nil
}

// secretBulkGraphQLArgs verifies bucket ownership before decoding secret rows,
// keeping plaintext handling scoped to a caller-owned bucket.
func secretBulkGraphQLArgs(p graphql.ResolveParams, s store.Store) (mcpGraphQLActor, uuid.UUID, []SecretUpsertPayload, error) {
	actor, err := actorFromContext(p.Context)
	if err != nil {
		return mcpGraphQLActor{}, uuid.Nil, nil, err
	}
	bucketID, err := bucketIDFromArgs(p, s)
	if err != nil {
		return mcpGraphQLActor{}, uuid.Nil, nil, err
	}
	payloads, err := secretPayloadsFromGraphQLArgs(p)
	if err != nil {
		return mcpGraphQLActor{}, uuid.Nil, nil, err
	}
	return actor, bucketID, payloads, nil
}

// bucketIDFromArgs resolves and verifies bucket ownership before any resolver
// reads or mutates bucket-attached credentials.
func bucketIDFromArgs(p graphql.ResolveParams, s store.Store) (uuid.UUID, error) {
	_, err := actorFromContext(p.Context)
	if err != nil {
		return uuid.Nil, err
	}
	bucketID, err := requiredGraphQLUUIDArg(p, "bucket_id")
	if err != nil {
		return uuid.Nil, err
	}
	// Bucket membership is checked before any connect read/write so a caller
	// cannot infer or mutate auth state attached to another workspace's bucket.
	if _, err := s.GetBucket(p.Context, bucketID); err != nil {
		if errors.Is(err, store.ErrBucketNotFound) {
			return uuid.Nil, fmt.Errorf("bucket not found")
		}
		return uuid.Nil, fmt.Errorf("resolve bucket: %w", err)
	}
	return bucketID, nil
}

// secretPayloadsFromGraphQLArgs maps GraphQL inputs into the REST payload type
// so both admin surfaces share validation and encryption behavior.
func secretPayloadsFromGraphQLArgs(p graphql.ResolveParams) ([]SecretUpsertPayload, error) {
	raw, ok := p.Args["secrets"].([]interface{})
	if !ok || len(raw) == 0 {
		return nil, errors.New("secrets is required")
	}
	payloads := make([]SecretUpsertPayload, 0, len(raw))
	for _, item := range raw {
		payload, err := secretPayloadFromGraphQLInput(item)
		if err != nil {
			return nil, err
		}
		payloads = append(payloads, payload)
	}
	return payloads, nil
}

// secretPayloadFromGraphQLInput keeps parser errors generic because the input
// contains plaintext credential material that must not leak into responses.
func secretPayloadFromGraphQLInput(item interface{}) (SecretUpsertPayload, error) {
	raw, ok := item.(map[string]interface{})
	if !ok {
		return SecretUpsertPayload{}, errors.New("invalid secret input")
	}
	serviceID, err := uuid.Parse(strings.TrimSpace(fmt.Sprint(raw["service_id"])))
	if err != nil {
		return SecretUpsertPayload{}, errors.New("invalid service_id")
	}
	expiresAtRaw, _ := raw["expires_at"].(string)
	expiresAt, err := parseOptionalRFC3339(strings.TrimSpace(expiresAtRaw))
	if err != nil {
		return SecretUpsertPayload{}, errors.New("invalid expires_at")
	}
	return SecretUpsertPayload{
		ServiceID:      serviceID,
		KeyName:        strings.TrimSpace(fmt.Sprint(raw["key_name"])),
		CredentialType: strings.TrimSpace(fmt.Sprint(raw["credential_type"])),
		Value:          fmt.Sprint(raw["value"]),
		ExpiresAt:      expiresAt,
	}, nil
}

// authConnectionDeleteArgs returns the bucket ownership context with the target
// connection ID so the delete path can audit both without exposing tokens.
func authConnectionDeleteArgs(p graphql.ResolveParams, s store.Store) (bucketAdminCall, uuid.UUID, error) {
	bucketID, err := bucketIDFromArgs(p, s)
	if err != nil {
		return bucketAdminCall{}, uuid.Nil, err
	}
	connectionID, err := requiredGraphQLUUIDArg(p, "connection_id")
	if err != nil {
		return bucketAdminCall{}, uuid.Nil, err
	}
	return bucketAdminCall{bucketID: bucketID}, connectionID, nil
}

// requiredGraphQLUUIDArg normalizes ID parsing so malformed IDs fail before
// they can reach Store methods as zero UUIDs.
func requiredGraphQLUUIDArg(p graphql.ResolveParams, name string) (uuid.UUID, error) {
	value, _ := p.Args[name].(string)
	id, err := uuid.Parse(value)
	if err != nil {
		return uuid.Nil, fmt.Errorf("invalid %s", name)
	}
	return id, nil
}

// optionalGraphQLUUIDArg lets filters stay absent instead of using uuid.Nil,
// which Store methods treat differently from "no filter".
func optionalGraphQLUUIDArg(p graphql.ResolveParams, name string) (*uuid.UUID, error) {
	value, _ := p.Args[name].(string)
	if value == "" {
		return nil, nil
	}
	id, err := uuid.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s", name)
	}
	return &id, nil
}

func graphQLStringArg(p graphql.ResolveParams, name string) string {
	value, _ := p.Args[name].(string)
	return value
}

func graphQLStringListArg(p graphql.ResolveParams, name string) []string {
	switch raw := p.Args[name].(type) {
	case []string:
		return compactGraphQLStrings(raw)
	case []interface{}:
		values := make([]string, 0, len(raw))
		for _, item := range raw {
			value, _ := item.(string)
			values = append(values, value)
		}
		return compactGraphQLStrings(values)
	default:
		return nil
	}
}

func compactGraphQLStrings(raw []string) []string {
	values := make([]string, 0, len(raw))
	for _, value := range raw {
		value = strings.TrimSpace(value)
		if value != "" {
			values = append(values, value)
		}
	}
	return values
}

func apiKeyFromGraphQLContext(ctx context.Context) string {
	if r := requestFromContext(ctx); r != nil {
		return r.Header.Get("X-API-Key")
	}
	return ""
}

// optionalUUIDValueOrNil adapts optional GraphQL input to the existing session
// model, where uuid.Nil means no SDK created the connect attempt.
func optionalUUIDValueOrNil(id *uuid.UUID) uuid.UUID {
	if id == nil {
		return uuid.Nil
	}
	return *id
}

// projectGraphQLBuckets converts store rows to GraphQL-safe scalar maps and
// avoids leaking fields unrelated to bucket selection.
func projectGraphQLBuckets(buckets []store.Bucket) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(buckets))
	for _, bucket := range buckets {
		items = append(items, map[string]interface{}{
			"id":   bucket.ID.String(),
			"name": bucket.Name, "is_default": bucket.IsDefault,
			"created_at": formatGraphQLTime(bucket.CreatedAt), "updated_at": formatGraphQLTime(bucket.UpdatedAt),
		})
	}
	return items
}

func projectGraphQLBucketSummaries(summaries []store.BucketSummary) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(summaries))
	for _, summary := range summaries {
		items = append(items, map[string]interface{}{
			"id":   summary.ID.String(),
			"name": summary.Name, "is_default": summary.IsDefault,
			"secret_count": summary.SecretCount, "value_count": summary.ValueCount,
			"connected_user_count": summary.ConnectedUserCount,
			"created_at":           formatGraphQLTime(summary.CreatedAt), "updated_at": formatGraphQLTime(summary.UpdatedAt),
		})
	}
	return items
}

func projectGraphQLBucketSummary(summary *store.BucketSummary) map[string]interface{} {
	if summary == nil {
		return nil
	}
	items := projectGraphQLBucketSummaries([]store.BucketSummary{*summary})
	return items[0]
}

func projectGraphQLBucketConnectSummary(summary *store.BucketConnectSummary) map[string]interface{} {
	if summary == nil {
		return nil
	}
	return map[string]interface{}{
		"bucket_id":            summary.BucketID.String(),
		"connect_config_count": summary.ConnectConfigCount,
		"connected_user_count": summary.ConnectedUserCount,
	}
}

func projectGraphQLWorkspaceServices(services []store.WorkspaceService, versions map[uuid.UUID][]store.WorkspaceServiceVersion, slugs map[uuid.UUID]string, authOptions map[uuid.UUID][]map[string]interface{}) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(services))
	for _, service := range services {
		items = append(items, map[string]interface{}{
			"id":         service.ID.String(),
			"service_id": service.ServiceID.String(), "service_name": service.ServiceName,
			"service_slug": slugs[service.ServiceID], "version": service.Version,
			"service_version_id": service.ServiceVersionID.String(),
			"enabled_versions":   projectGraphQLWorkspaceServiceVersions(versions[service.ServiceID]),
			"auth_options":       authOptions[service.ServiceID],
			"added_by":           service.AddedBy.String(), "created_at": formatGraphQLTime(service.CreatedAt),
		})
	}
	return items
}

func projectGraphQLWorkspaceServiceVersions(versions []store.WorkspaceServiceVersion) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(versions))
	for _, version := range versions {
		items = append(items, map[string]interface{}{
			"id": version.ID.String(), "service_version_id": version.ServiceVersionID.String(),
			"version": version.Version, "status": version.Status,
			"created_at": formatGraphQLTime(version.CreatedAt), "enabled_at": formatGraphQLTime(version.EnabledAt),
		})
	}
	return items
}

type serviceVersionAuthConfigFetcher interface {
	FetchServiceVersionAuthConfigs(ctx context.Context, refs []sandbox.ServiceVersionRef, apiKey string) ([]sandbox.ServiceVersionAuthConfigs, error)
}

// mcpGraphQLAuthConfigCacheKey is the context key for a request-scoped cache
// of fetchWorkspaceServiceAuthOptions results. workspaceServices and
// workspaceServicePage both project the same per-service auth options from
// the same Registry call; without this, a single GraphQL query selecting
// both fields would issue two identical batched auth-config requests instead
// of one (code requirement: no N+1 queries). Injected once per HTTP request
// by mcpGraphQLHandler -- a resolver invoked without one (e.g. a narrower
// unit test building the field directly) simply always fetches, never
// panics, since authConfigCacheFromContext degrades to nil.
type mcpGraphQLAuthConfigCacheKey struct{}

type authConfigCacheEntry struct {
	result map[uuid.UUID][]map[string]interface{}
	err    error
}

// authConfigCache is safe for concurrent resolver use -- graphql-go may
// execute sibling top-level fields concurrently.
type authConfigCache struct {
	mu      sync.Mutex
	entries map[string]authConfigCacheEntry
}

func newAuthConfigCache() *authConfigCache {
	return &authConfigCache{entries: map[string]authConfigCacheEntry{}}
}

func authConfigCacheFromContext(ctx context.Context) *authConfigCache {
	cache, _ := ctx.Value(mcpGraphQLAuthConfigCacheKey{}).(*authConfigCache)
	return cache
}

func fetchWorkspaceServiceAuthOptions(ctx context.Context, verifier ServiceVerifier, apiKey string, services []store.WorkspaceService) (map[uuid.UUID][]map[string]interface{}, error) {
	out := make(map[uuid.UUID][]map[string]interface{}, len(services))
	fetcher, ok := verifier.(serviceVersionAuthConfigFetcher)
	if !ok || len(services) == 0 {
		return out, nil
	}
	refs := workspaceServiceVersionRefs(services)
	if len(refs) == 0 {
		return out, nil
	}

	cache := authConfigCacheFromContext(ctx)
	key := authConfigCacheKeyForRefs(refs)
	if cache != nil {
		cache.mu.Lock()
		entry, hit := cache.entries[key]
		cache.mu.Unlock()
		if hit {
			return entry.result, entry.err
		}
	}

	configs, err := fetcher.FetchServiceVersionAuthConfigs(ctx, refs, apiKey)
	if err != nil {
		trace.SpanFromContext(ctx).RecordError(err)
		if cache != nil {
			cache.mu.Lock()
			cache.entries[key] = authConfigCacheEntry{err: err}
			cache.mu.Unlock()
		}
		return nil, err
	}
	for _, config := range configs {
		out[config.ServiceID] = projectGraphQLServiceAuthOptions(config.AuthConfigs)
	}
	if cache != nil {
		cache.mu.Lock()
		cache.entries[key] = authConfigCacheEntry{result: out}
		cache.mu.Unlock()
	}
	return out, nil
}

// authConfigCacheKeyForRefs builds a stable cache key independent of the
// order two different listing queries (e.g. an unpaged list vs. a paged one)
// happened to produce their service/version pairs in.
func authConfigCacheKeyForRefs(refs []sandbox.ServiceVersionRef) string {
	keyed := make([]string, len(refs))
	for i, ref := range refs {
		keyed[i] = ref.ServiceID.String() + "@" + ref.Version
	}
	sort.Strings(keyed)
	return strings.Join(keyed, ",")
}

func workspaceServiceVersionRefs(services []store.WorkspaceService) []sandbox.ServiceVersionRef {
	refs := make([]sandbox.ServiceVersionRef, 0, len(services))
	for _, service := range services {
		version := service.ServiceVersionID.String()
		if service.ServiceVersionID == uuid.Nil {
			version = service.Version
		}
		if strings.TrimSpace(version) == "" {
			continue
		}
		refs = append(refs, sandbox.ServiceVersionRef{ServiceID: service.ServiceID, Version: version})
	}
	return refs
}

func projectGraphQLServiceAuthOptions(auths fusedobject.AuthConfigs) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(auths))
	seen := map[string]int{}
	for _, auth := range auths {
		option, ok := projectGraphQLServiceAuthOption(auth)
		if !ok {
			continue
		}
		id := option["id"].(string)
		seen[id]++
		if seen[id] > 1 {
			option["id"] = fmt.Sprintf("%s-%d", id, seen[id])
		}
		items = append(items, option)
	}
	return items
}

func projectGraphQLServiceAuthOption(auth fusedobject.AuthConfig) (map[string]interface{}, bool) {
	authType := sandbox.CanonicalFusedAuthType(auth)
	keyName := sandbox.AuthCredentialName(auth)
	if !supportedBucketAuthType(authType) || keyName == "" {
		return nil, false
	}
	return map[string]interface{}{
		"id":                       authOptionID(authType, keyName),
		"label":                    authOptionLabel(authType),
		"auth_type":                authType,
		"credential_type":          bucketSecretCredentialType(authType),
		"key_name":                 singleAuthKeyName(authType, keyName),
		"key_prefix":               pairedAuthKeyPrefix(authType, keyName),
		"required_fields":          authRequiredFields(authType),
		"supports_connected_users": authType == "oauth" || authType == "oidc",
	}, true
}

func supportedBucketAuthType(authType string) bool {
	switch authType {
	case "api_key", "bearer", "basic", "oauth", "oidc", "mtls":
		return true
	default:
		return false
	}
}

func authOptionID(authType, keyName string) string {
	return authType + ":" + keyName
}

func authOptionLabel(authType string) string {
	switch authType {
	case "api_key":
		return "API key"
	case "bearer":
		return "Bearer token"
	case "basic":
		return "Basic"
	case "oauth":
		return "OAuth token"
	case "oidc":
		return "OIDC token"
	case "mtls":
		return "mTLS"
	default:
		return authType
	}
}

func bucketSecretCredentialType(authType string) string {
	if authType == "api_key" {
		return "apiKey"
	}
	return authType
}

func singleAuthKeyName(authType, keyName string) string {
	if authType == "basic" || authType == "mtls" {
		return ""
	}
	return keyName
}

func pairedAuthKeyPrefix(authType, keyName string) string {
	if authType == "basic" || authType == "mtls" {
		return keyName
	}
	return ""
}

func authRequiredFields(authType string) []string {
	switch authType {
	case "basic":
		return []string{"username", "password"}
	case "mtls":
		return []string{"certificate", "private_key"}
	default:
		return []string{"value"}
	}
}

func projectGraphQLWorkspaceWebhooks(webhooks []store.WorkspaceWebhook) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(webhooks))
	for _, webhook := range webhooks {
		items = append(items, map[string]interface{}{
			"label": webhook.Label, "slug": webhook.Slug, "created_at": formatGraphQLTime(webhook.CreatedAt),
			"signature": webhookSignatureStatus(webhook.SecretBucketID),
		})
	}
	return items
}

// webhookSignatureStatus never surfaces SecretRef itself -- only whether one
// is configured -- so this GraphQL field can never leak a bucket secret
// reference to a client that only asked "is this signed."
func webhookSignatureStatus(secretBucketID *uuid.UUID) string {
	if secretBucketID == nil || *secretBucketID == uuid.Nil {
		return "none"
	}
	return "set"
}

func projectGraphQLAppTokens(tokens []store.AppTokenMetadata) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(tokens))
	for _, token := range tokens {
		items = append(items, map[string]interface{}{
			"id": token.ID.String(), "app_family_id": token.AppFamilyID.String(), "name": token.Name,
			"allow":      projectTokenAllow(token.AllowAll, token.AllowedOperations),
			"expires_at": formatOptionalGraphQLTime(token.ExpiresAt),
			"created_at": formatGraphQLTime(token.CreatedAt), "last_used_at": formatOptionalGraphQLTime(token.LastUsedAt),
		})
	}
	return items
}

// projectGraphQLWebhookEvents preserves the UI's analytics shape while moving
// the transport to Engine GraphQL; all filtering has already happened in SQL.
func projectGraphQLWebhookEvents(events []models.WebhookEvent) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(events))
	for _, event := range events {
		items = append(items, projectGraphQLWebhookEvent(event))
	}
	return items
}

func projectGraphQLWebhookEvent(event models.WebhookEvent) map[string]interface{} {
	sdkRecordID := ""
	if event.SDKRecordID != nil {
		sdkRecordID = event.SDKRecordID.String()
	}
	return map[string]interface{}{
		"id": event.ID.String(), "account_id": event.AccountID.String(), "service_id": event.ServiceID.String(),
		"msg_id": event.MsgID, "event_name": event.EventType,
		"status": event.DeliveryStatus, "delivery_status": event.DeliveryStatus,
		"verification_status": event.VerificationStatus, "latency_ms": event.LatencyMs,
		"environment": event.Environment,
		"retry_count": event.RetryCount, "credits_consumed": event.CreditsConsumed,
		"sdk_record_id": sdkRecordID, "error_reason": event.ErrorReason,
		"payload_size": event.PayloadSize, "created_at": formatGraphQLTime(event.CreatedAt),
	}
}

func projectGraphQLWebhookAnalytics(analytics models.WebhookAnalytics) map[string]interface{} {
	return map[string]interface{}{
		"total_ingested":  int(analytics.TotalIngested),
		"total_delivered": int(analytics.TotalDelivered),
		"total_rejected":  int(analytics.TotalRejected),
		"total_failed":    int(analytics.TotalFailed),
	}
}

func projectGraphQLEngineExecutionEvents(events []models.EngineExecutionEvent) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(events))
	for _, event := range events {
		providerLatency := interface{}(nil)
		if event.ProviderLatencyMs != nil {
			providerLatency = int(*event.ProviderLatencyMs)
		}
		providerHTTPStatus := interface{}(nil)
		if event.ProviderHTTPStatus != nil {
			providerHTTPStatus = *event.ProviderHTTPStatus
		}
		items = append(items, map[string]interface{}{
			"id": event.ID.String(), "trace_id": event.TraceID, "span_id": event.SpanID,
			"app_family_id": optionalGraphQLUUID(event.AppFamilyID), "app_id": optionalGraphQLUUID(event.AppID),
			"app_version": event.AppVersion, "app_kind": executionAppKind(event.Transport),
			"transport": event.Transport, "provider_protocol": event.ProviderProtocol,
			"direction": event.Direction, "service_id": event.ServiceID.String(),
			"service_version_id": event.ServiceVersionID,
			"service_name":       event.ServiceName, "service_slug": event.ServiceSlug, "service_version": event.ServiceVersion,
			"operation_id": optionalGraphQLUUID(event.OperationID),
			"webhook_id":   optionalGraphQLUUID(event.WebhookID), "operation": event.EndpointName, "event_name": event.EventName,
			"http_method": event.HTTPMethod, "request_path": event.RequestPath,
			"environment": event.Environment, "environment_source": event.EnvironmentSource,
			"provider_host": event.ProviderHost, "provider_http_status": providerHTTPStatus, "provider_status_class": event.ProviderStatusClass,
			"status":         event.Status,
			"failure_reason": event.FailureReason, "failure_category": event.FailureCategory, "failure_code": event.FailureCode,
			"latency_ms":          int(event.LatencyMs),
			"provider_latency_ms": providerLatency, "idempotency_replayed": event.IdempotencyReplayed,
			"attempt_count": event.AttemptCount, "request_bytes": int(event.RequestBytes), "response_bytes": int(event.ResponseBytes),
			"auth_scheme_names": event.AuthSchemeNames, "auth_scheme_types": event.AuthSchemeTypes,
			"auth_scheme_count": int(event.AuthSchemeCount), "auth_selection_outcome": event.AuthSelectionOutcome,
			"pagination_type": event.PaginationType, "pagination_page_count": int(event.PaginationPageCount),
			"pagination_item_count": int(event.PaginationItemCount), "pagination_byte_count": int(event.PaginationByteCount),
			"pagination_stop_reason": event.PaginationStopReason,
			"rate_limit_decision":    event.RateLimitDecision, "rate_limit_policy_count": int(event.RateLimitPolicyCount),
			"rate_limit_scope_kinds": event.RateLimitScopeKinds, "rate_limit_units": event.RateLimitUnits,
			"rate_limit_unit_totals":   graphQLInt64Strings(event.RateLimitUnitTotals),
			"rate_limit_retry_outcome": event.RateLimitRetryOutcome, "rate_limit_header_outcome": event.RateLimitHeaderOutcome,
			"verification_status": event.VerificationStatus, "delivery_status": event.DeliveryStatus,
			"started_at": formatGraphQLTime(event.StartedAt), "ended_at": formatGraphQLTime(event.EndedAt),
			"timings": engineExecutionTimingEntries(event.Timings),
		})
	}
	return items
}

// executionAppKind keeps ingress transport separate from immutable app kind;
// REST receipts belong to SDK apps even though their transport remains rest.
func executionAppKind(transport string) string {
	if transport == models.EngineExecutionTransportREST {
		return string(store.AppKindSDK)
	}
	return transport
}

func graphQLInt64Strings(values []int64) []string {
	encoded := make([]string, len(values))
	for i, value := range values {
		encoded[i] = strconv.FormatInt(value, 10)
	}
	return encoded
}

func optionalGraphQLUUID(id uuid.UUID) interface{} {
	if id == uuid.Nil {
		return nil
	}
	return id.String()
}

func engineExecutionTimingEntries(encoded []byte) []map[string]interface{} {
	var timings map[string]float64
	if len(encoded) == 0 || json.Unmarshal(encoded, &timings) != nil {
		return []map[string]interface{}{}
	}
	keys := make([]string, 0, len(timings))
	for name := range timings {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	items := make([]map[string]interface{}, 0, len(keys))
	for _, name := range keys {
		items = append(items, map[string]interface{}{"name": name, "duration_ms": timings[name]})
	}
	return items
}

func projectGraphQLEngineExecutionAnalytics(analytics models.EngineExecutionAnalytics) map[string]interface{} {
	return map[string]interface{}{
		"total_calls": int(analytics.TotalCalls), "successful_calls": int(analytics.SuccessfulCalls),
		"failed_calls": int(analytics.FailedCalls), "average_latency_ms": analytics.AverageLatencyMs,
		"median_latency_ms": analytics.MedianLatencyMs, "p95_latency_ms": analytics.P95LatencyMs,
	}
}

func projectGraphQLAppExecutionAnalytics(analytics models.AppExecutionAnalytics) map[string]interface{} {
	result := projectGraphQLEngineExecutionAnalytics(analytics.EngineExecutionAnalytics)
	result["by_service"] = projectGraphQLExecutionBreakdowns(analytics.ByService)
	return result
}

func projectGraphQLWorkspaceExecutionAnalytics(analytics models.WorkspaceExecutionAnalytics) map[string]interface{} {
	return map[string]interface{}{
		"total_calls": int(analytics.TotalCalls), "inbound_calls": int(analytics.InboundCalls),
		"successful_calls": int(analytics.SuccessfulCalls),
		"failed_calls":     int(analytics.FailedCalls), "average_latency_ms": analytics.AverageLatencyMs,
		"median_latency_ms": analytics.MedianLatencyMs, "p95_latency_ms": analytics.P95LatencyMs,
		"by_service":          projectGraphQLExecutionBreakdowns(analytics.ByService),
		"most_used_sdk":       projectGraphQLExecutionBreakdown(analytics.MostUsedSDK),
		"most_used_service":   projectGraphQLExecutionBreakdown(analytics.MostUsedService),
		"most_failed_service": projectGraphQLExecutionBreakdown(analytics.MostFailedService),
		"most_used_bucket":    projectGraphQLExecutionBreakdown(analytics.MostUsedBucket),
	}
}

func projectGraphQLExecutionBreakdowns(items []models.EngineExecutionBreakdown) []map[string]interface{} {
	result := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		result = append(result, projectGraphQLExecutionBreakdown(&item))
	}
	return result
}

// projectGraphQLExecutionBreakdown keeps highlight and service-row projections
// identical while preserving GraphQL null for a range with no matching group.
func projectGraphQLExecutionBreakdown(item *models.EngineExecutionBreakdown) map[string]interface{} {
	if item == nil {
		return nil
	}
	return map[string]interface{}{
		"key": item.Key, "label": item.Label, "total_calls": int(item.TotalCalls),
		"inbound_calls": int(item.InboundCalls), "failed_calls": int(item.FailedCalls),
		"p95_latency_ms": item.P95LatencyMs,
	}
}

func projectGraphQLWorkspaceNotificationInbox(inbox workspaceNotificationInboxResponse) map[string]interface{} {
	warnings := inbox.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	return map[string]interface{}{
		"items":         projectGraphQLWorkspaceNotifications(inbox.Items),
		"warnings":      warnings,
		"total_count":   inbox.TotalCount,
		"pending_count": inbox.PendingCount,
	}
}

func projectGraphQLWorkspaceNotifications(items []workspaceNotificationInboxItem) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(items))
	for _, item := range items {
		out = append(out, projectGraphQLWorkspaceNotification(item))
	}
	return out
}

func projectGraphQLWorkspaceNotification(item workspaceNotificationInboxItem) map[string]interface{} {
	return map[string]interface{}{
		"id": item.ID, "source": item.Source, "type": item.Type,
		"severity": item.Severity, "status": item.Status, "service_id": item.ServiceID,
		"version": item.Version, "config_key": item.ConfigKey, "message": item.Message,
		"integration_object_id": item.IntegrationObjectID, "webhook_object_id": item.WebhookObjectID,
		"detected_at": item.DetectedAt, "diff": projectGraphQLDriftChanges(item.Diff),
	}
}

func projectGraphQLDriftChanges(changes []models.DriftChange) []map[string]interface{} {
	out := make([]map[string]interface{}, 0, len(changes))
	for _, change := range changes {
		out = append(out, map[string]interface{}{
			"field": change.Field, "old_value": fmt.Sprint(change.OldValue),
			"new_value": fmt.Sprint(change.NewValue), "severity": change.Severity,
			"description": change.Description,
		})
	}
	return out
}

func projectGraphQLBucketSDKSummaries(scopes []store.AppRuntime) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(scopes))
	for _, scope := range scopes {
		items = append(items, map[string]interface{}{
			"id": scope.AppID.String(), "name": scope.Name,
			"kind": scope.Kind, "active": scope.Status.Runnable(),
			"created_at": formatGraphQLTime(scope.CreatedAt),
		})
	}
	return items
}

func projectGraphQLBucketServiceSummaries(services []store.BucketServiceSummary) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(services))
	for _, service := range services {
		items = append(items, map[string]interface{}{
			"service_id": service.ServiceID.String(), "service_name": service.ServiceName,
			"secret_count": service.SecretCount, "value_count": service.ValueCount,
			"connect_config_count": service.ConnectConfigCount, "connected_user_count": service.ConnectedUserCount,
		})
	}
	return items
}

// projectGraphQLBucketValues maps stored bucket values into UI-safe fields
// without adding any service-side filtering in Go.
func projectGraphQLBucketValues(values []store.BucketValue) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(values))
	for _, value := range values {
		items = append(items, map[string]interface{}{
			"id":        value.ID.String(),
			"bucket_id": value.BucketID.String(), "service_id": value.ServiceID.String(),
			"key_name": value.KeyName, "location": value.Location, "value": value.Value,
			"created_at": formatGraphQLTime(value.CreatedAt), "updated_at": formatGraphQLTime(value.UpdatedAt),
		})
	}
	return items
}

// projectGraphQLSecretMetas keeps secret projections metadata-only by design;
// values remain write-only in Engine storage.
func projectGraphQLSecretMetas(secrets []store.WorkspaceSecretMeta) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(secrets))
	for _, secret := range secrets {
		items = append(items, map[string]interface{}{
			"id":        secret.ID.String(),
			"bucket_id": secret.BucketID.String(), "service_id": secret.ServiceID.String(),
			"key_name": secret.KeyName, "key_names": secret.KeyNames, "credential_type": secret.CredentialType,
			"last_used_at": formatOptionalTime(secret.LastUsedAt), "expires_at": formatOptionalTime(secret.ExpiresAt),
			"created_at": formatGraphQLTime(secret.CreatedAt), "updated_at": formatGraphQLTime(secret.UpdatedAt),
		})
	}
	return items
}

// projectGraphQLAuthConnections exposes connection metadata in one mapping path
// so list and future mutation responses cannot drift.
func projectGraphQLAuthConnections(connections []store.AuthConnection) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(connections))
	for _, conn := range connections {
		items = append(items, projectGraphQLAuthConnection(conn))
	}
	return items
}

// projectGraphQLAuthConnection omits encrypted token fields and returns only
// lifecycle metadata the UI needs for auditing and cleanup.
func projectGraphQLAuthConnection(conn store.AuthConnection) map[string]interface{} {
	resp := projectAuthConnection(conn)
	return map[string]interface{}{
		"id": resp.ID.String(), "bucket_id": resp.BucketID.String(), "service_id": resp.ServiceID.String(),
		"service_version_id": formatOptionalGraphQLUUID(resp.ServiceVersionID),
		"end_user_ref":       resp.EndUserRef, "created_by_app_id": resp.CreatedByAppID.String(),
		"auth_type": resp.AuthType, "auth_name": resp.AuthName, "token_type": resp.TokenType, "scopes": resp.Scopes,
		"scope_source": resp.ScopeSource, "issuer": resp.Issuer, "subject": resp.Subject,
		"expires_at":               formatOptionalGraphQLTime(resp.ExpiresAt),
		"refresh_token_expires_at": formatOptionalGraphQLTime(resp.RefreshTokenExpiresAt),
		"last_used_at":             formatOptionalGraphQLTime(resp.LastUsedAt),
		"last_refresh_attempt_at":  formatOptionalGraphQLTime(resp.LastRefreshAttemptAt),
		"last_refreshed_at":        formatOptionalGraphQLTime(resp.LastRefreshedAt),
		"refresh_retry_not_before": formatOptionalGraphQLTime(resp.RefreshRetryNotBefore),
		"refresh_state":            resp.RefreshState,
		"last_failure_code":        resp.LastFailureCode,
		"last_failure_at":          formatOptionalGraphQLTime(resp.LastFailureAt),
		"last_failure_trace_id":    resp.LastFailureTraceID,
		"created_at":               formatGraphQLTime(resp.CreatedAt), "updated_at": formatGraphQLTime(resp.UpdatedAt),
	}
}

// formatOptionalGraphQLUUID preserves null for unpinned legacy connections
// while returning ordinary version identities as stable strings.
func formatOptionalGraphQLUUID(value *uuid.UUID) interface{} {
	if value == nil {
		return nil
	}
	return value.String()
}

// projectGraphQLConnectionResources shares one safe projection for list and
// mutation responses and deliberately omits raw discovery metadata.
func projectGraphQLConnectionResources(resources []store.ConnectionResource) []map[string]interface{} {
	items := make([]map[string]interface{}, 0, len(resources))
	for _, resource := range resources {
		items = append(items, projectGraphQLConnectionResource(resource))
	}
	return items
}

// projectGraphQLConnectionResource exposes base URLs only on authenticated
// admin GraphQL; generated SDK projections intentionally omit them.
func projectGraphQLConnectionResource(resource store.ConnectionResource) map[string]interface{} {
	return map[string]interface{}{
		"id": resource.ID.String(), "connection_id": resource.ConnectionID.String(), "service_id": resource.ServiceID.String(),
		"provider_resource_id": resource.ProviderResourceID, "resource_type": resource.ResourceType,
		"display_name": resource.DisplayName, "base_url": resource.BaseURL, "scopes": resource.Scopes,
		"is_default": resource.IsDefault, "created_at": formatGraphQLTime(resource.CreatedAt), "updated_at": formatGraphQLTime(resource.UpdatedAt),
	}
}

// projectGraphQLConnectSession returns the browser redirect target after Engine
// has already persisted one-time callback state.
func projectGraphQLConnectSession(resp connectSessionStartResponse) map[string]interface{} {
	return map[string]interface{}{
		"authorize_url": resp.AuthorizeURL,
		"expires_at":    formatGraphQLTime(resp.ExpiresAt),
	}
}

// formatGraphQLTime uses a stable RFC3339-like shape matching the existing MCP
// GraphQL endpoint, so UI date parsing stays consistent.
func formatGraphQLTime(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.Format(mcpGraphQLTimeFormat)
}

// formatOptionalGraphQLTime keeps missing provider dates empty instead of using
// a zero timestamp that could be mistaken for a real expiry.
func formatOptionalGraphQLTime(t *time.Time) string {
	if t == nil {
		return ""
	}
	return formatGraphQLTime(*t)
}

func parseOptionalRFC3339(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
