package db

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

func initEngineSchema(ctx context.Context, pool *pgxpool.Pool) error {
	for _, q := range engineSchemaQueries() {
		if _, err := pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("failed to execute query %q: %w", q, err)
		}
	}
	for _, q := range engineMigrationQueries() {
		if _, err := pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("failed to execute query %q: %w", q, err)
		}
	}

	log.Println("Engine database schema initialization complete.")
	return nil
}

func engineSchemaQueries() []string {
	return []string{
		// Workspaces
		`CREATE TABLE IF NOT EXISTS fused_workspaces (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			account_id uuid NOT NULL,
			name text NOT NULL,
			slug text NOT NULL UNIQUE,
			singleton_key smallint NOT NULL DEFAULT 1 UNIQUE,
			created_at timestamptz DEFAULT NOW(),
			updated_at timestamptz DEFAULT NOW(),
			CHECK (singleton_key = 1)
		);`,

		// Buckets
		`CREATE TABLE IF NOT EXISTS fused_buckets (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			name text NOT NULL,
			is_default boolean NOT NULL DEFAULT false,
			created_at timestamptz DEFAULT NOW(),
			updated_at timestamptz DEFAULT NOW(),
			CONSTRAINT uq_workspace_buckets UNIQUE (name)
		);`,

		// Connect auth config is bucket-scoped, not SDK-scoped. SDKs already
		// attach to buckets, so this lets a regenerated or sibling SDK reuse
		// the same OAuth/OIDC app setup without copying secrets.
		`CREATE TABLE IF NOT EXISTS fused_connect_configs (
			id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			bucket_id                uuid NOT NULL REFERENCES fused_buckets(id) ON DELETE CASCADE,
			service_id               uuid NOT NULL,
			auth_type                text NOT NULL,
			enabled                  boolean NOT NULL DEFAULT true,
			encrypted_dek            text NOT NULL,
			encrypted_client_id      text NOT NULL,
			encrypted_client_secret  text NOT NULL,
			redirect_uri             text NOT NULL,
			created_at               timestamptz DEFAULT NOW(),
			updated_at               timestamptz DEFAULT NOW(),
			CONSTRAINT uq_fused_connect_configs UNIQUE (bucket_id, service_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_connect_configs_bucket
		ON fused_connect_configs(bucket_id);`,

		// User connections live in the same bucket credential namespace as
		// workspace secrets. The unique key intentionally excludes artifact_id: SDKs
		// linked to the same bucket should share connected users.
		`CREATE TABLE IF NOT EXISTS fused_auth_connections (
			id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			bucket_id          uuid NOT NULL REFERENCES fused_buckets(id) ON DELETE CASCADE,
			service_id         uuid NOT NULL,
			end_user_ref       text NOT NULL,
			created_by_artifact_id  uuid,
			auth_type          text NOT NULL,
			encrypted_dek      text NOT NULL,
			access_token       text NOT NULL,
			refresh_token      text,
			id_token           text,
			token_type         text NOT NULL DEFAULT 'Bearer',
			scopes             text[] NOT NULL DEFAULT '{}',
			-- GitHub App user tokens deliberately have an empty scope
			-- string; scope_source lets admin/debug views distinguish that
			-- from a parser miss.
			scope_source       text NOT NULL DEFAULT 'none',
			issuer             text NOT NULL DEFAULT '',
			subject            text NOT NULL DEFAULT '',
			identity_claims    jsonb NOT NULL DEFAULT '{}'::jsonb,
			expires_at         timestamptz,
			-- Non-sensitive provider metadata that lets the Engine prompt
			-- reconnect before refresh material becomes unusable.
			refresh_token_expires_at timestamptz,
			last_used_at       timestamptz,
			refresh_state      text NOT NULL DEFAULT 'ok',
			-- Failure diagnostics contain stable codes and trace
			-- correlation only; provider bodies, user references, and
			-- credentials never enter these fields.
			last_failure_code  text NOT NULL DEFAULT '',
			last_failure_at    timestamptz,
			last_failure_trace_id text NOT NULL DEFAULT '',
			created_at         timestamptz DEFAULT NOW(),
			updated_at         timestamptz DEFAULT NOW(),
			CONSTRAINT uq_fused_auth_connections UNIQUE (bucket_id, service_id, end_user_ref),
			CONSTRAINT chk_fused_auth_connections_refresh_state
				CHECK (refresh_state IN ('ok', 'failed', 'expired', 'reconnect_required'))
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_auth_connections_bucket_service
		ON fused_auth_connections(bucket_id, service_id);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_auth_connections_refresh
		ON fused_auth_connections(expires_at)
		WHERE refresh_token IS NOT NULL AND refresh_state = 'ok';`,

		// Provider resources carry routing context only. Keeping them separate
		// from token rows lets one connection own several tenant endpoints without
		// duplicating or exposing credential material.
		`CREATE TABLE IF NOT EXISTS fused_connection_resources (
			id                   uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			connection_id        uuid NOT NULL REFERENCES fused_auth_connections(id) ON DELETE CASCADE,
			bucket_id            uuid NOT NULL REFERENCES fused_buckets(id) ON DELETE CASCADE,
			service_id           uuid NOT NULL,
			provider_resource_id text NOT NULL,
			resource_type         text NOT NULL,
			display_name          text NOT NULL DEFAULT '',
			base_url              text NOT NULL,
			metadata_json         jsonb NOT NULL DEFAULT '{}'::jsonb,
			scopes                text[] NOT NULL DEFAULT '{}',
			is_default            boolean NOT NULL DEFAULT false,
			is_active             boolean NOT NULL DEFAULT true,
			created_at            timestamptz NOT NULL DEFAULT NOW(),
			updated_at            timestamptz NOT NULL DEFAULT NOW(),
			CONSTRAINT uq_fused_connection_resource UNIQUE (connection_id, resource_type, provider_resource_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_connection_resources_connection
		ON fused_connection_resources(connection_id, is_active);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_fused_connection_resources_default
		ON fused_connection_resources(connection_id) WHERE is_default AND is_active;`,

		// Connect sessions are public-browser state handles. State and nonce
		// are hashes; the PKCE verifier is encrypted because the callback
		// needs the original value for the OAuth token exchange. return_url
		// is captured before provider redirect so the callback can safely
		// return the browser after token storage without trusting a
		// callback-time query parameter. resource_input is non-secret
		// tenant context captured before redirect; storing it on the
		// one-time session binds callback routing to that start.
		// requested_scopes is the callback's fallback when a provider omits
		// scope metadata from the token response.
		`CREATE TABLE IF NOT EXISTS fused_connect_sessions (
			id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
				bucket_id          uuid NOT NULL REFERENCES fused_buckets(id) ON DELETE CASCADE,
				service_id         uuid NOT NULL,
				end_user_ref       text NOT NULL,
				state_hash         text NOT NULL UNIQUE,
				nonce_hash         text NOT NULL DEFAULT '',
				encrypted_dek      text NOT NULL DEFAULT '',
				pkce_verifier      text NOT NULL DEFAULT '',
				created_by_artifact_id  uuid,
				return_url         text NOT NULL DEFAULT '',
				resource_input     jsonb NOT NULL DEFAULT '{}'::jsonb,
				requested_scopes   text[] NOT NULL DEFAULT '{}',
				expires_at         timestamptz NOT NULL,
				used_at            timestamptz,
				created_at         timestamptz DEFAULT NOW()
			);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_connect_sessions_state_hash
			ON fused_connect_sessions(state_hash);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_connect_sessions_expires
		ON fused_connect_sessions(expires_at);`,

		// API Keys
		`CREATE TABLE IF NOT EXISTS fused_api_keys (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			account_id uuid NOT NULL,
			key_hash text UNIQUE NOT NULL,
			name text NOT NULL,
			created_at timestamp with time zone DEFAULT NOW(),
			last_used_at timestamp with time zone
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_api_keys_account ON fused_api_keys(account_id);`,

		// SDK Scopes
		`CREATE TABLE IF NOT EXISTS fused_artifact_scopes (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			account_id uuid NOT NULL,
			artifact_id uuid UNIQUE NOT NULL,
			scope_schema_version integer NOT NULL DEFAULT 1,
			selections jsonb NOT NULL,
			deactivated_at timestamptz,
			created_at timestamp with time zone DEFAULT NOW(),
			-- kind labels how this scope is meant to be connected to ("sdk" or
			-- "mcp"); a scope's shape (selections+bucket) is identical either
			-- way, this is purely a listing/UI distinction, not enforcement.
			kind text NOT NULL DEFAULT 'sdk',
			-- name is an optional user-supplied label (CLI --name flag, or a
			-- workspace config's name); reactivate-only activate calls never
			-- set it, so it can be NULL for scopes created before this column
			-- existed or that were only ever reactivated.
			name text,
			version text,
			config_key text
		);`,

		// MCP Sessions
		`CREATE TABLE IF NOT EXISTS fused_mcp_sessions (
			id uuid PRIMARY KEY,
			artifact_id uuid,
			session_id text,
			started_at timestamp with time zone DEFAULT NOW(),
			ended_at timestamp with time zone,
			last_ping_at timestamp with time zone DEFAULT NOW(),
			client_info jsonb
		);`,

		// MCP Analytics
		`CREATE TABLE IF NOT EXISTS fused_mcp_analytics (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			artifact_id uuid,
			session_id text,
			endpoint_name text,
			service_name text,
			latency_ms bigint,
			failed boolean,
			timestamp timestamp with time zone,
			params jsonb,
			result jsonb
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_mcp_analytics_artifact_id ON fused_mcp_analytics(artifact_id);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_mcp_analytics_sdk_endpoint ON fused_mcp_analytics(artifact_id, endpoint_name);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_mcp_analytics_sdk_service ON fused_mcp_analytics(artifact_id, service_name);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_mcp_sessions_sdk_started ON fused_mcp_sessions(artifact_id, started_at DESC);`,

		// Engine execution receipts are compact product/audit records. OTEL owns
		// rich step-level detail; this table keeps user history queryable even
		// when an observability backend is disabled or has shorter retention.
		`CREATE TABLE IF NOT EXISTS fused_engine_execution_events (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			trace_id text,
			span_id text,
			artifact_id uuid NOT NULL,
			transport text NOT NULL,
			service_id uuid,
			service_version_id text,
			endpoint_name text NOT NULL,
			environment text,
			status text NOT NULL,
			failure_reason text,
			latency_ms bigint NOT NULL,
			provider_latency_ms bigint,
			idempotency_key_hash text,
			request_body_hash text,
			timings jsonb,
			started_at timestamp with time zone NOT NULL,
			ended_at timestamp with time zone NOT NULL,
			created_at timestamp with time zone DEFAULT NOW(),
			idempotency_replayed boolean NOT NULL DEFAULT false
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_sdk_started
		ON fused_engine_execution_events(artifact_id, started_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_service_version
		ON fused_engine_execution_events(service_id, service_version_id);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_endpoint
		ON fused_engine_execution_events(artifact_id, endpoint_name, started_at DESC);`,

		// Runtime entitlement is a singleton cache of the Registry handshake
		// contract. Engine remains useful during short Registry outages, but
		// keeping the last contract locally lets support/debug surfaces explain
		// which heartbeat and usage-reporting behavior this process believes it
		// is operating under.
		`CREATE TABLE IF NOT EXISTS fused_runtime_entitlements (
			singleton_key smallint PRIMARY KEY DEFAULT 1,
			plan text NOT NULL,
			heartbeat_required boolean NOT NULL,
			usage_reporting text NOT NULL,
			heartbeat_interval_seconds integer NOT NULL CHECK (heartbeat_interval_seconds > 0),
			heartbeat_stale_after_seconds integer NOT NULL CHECK (heartbeat_stale_after_seconds > 0),
			refreshed_at timestamptz NOT NULL,
			updated_at timestamptz NOT NULL DEFAULT NOW(),
			CHECK (singleton_key = 1)
		);`,

		// Pending usage reports are local aggregate counters, not raw execution
		// logs. A partial unique index lets many executions in the same minute
		// fold into one pending report; once flushed, late arrivals for that
		// bucket create a new report_id so Registry idempotency stays exact.
		`CREATE TABLE IF NOT EXISTS fused_engine_usage_counter_reports (
			report_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			metric text NOT NULL,
			bucket_start timestamptz NOT NULL,
			bucket_seconds integer NOT NULL CHECK (bucket_seconds > 0),
			count bigint NOT NULL CHECK (count >= 0),
			flushed_at timestamptz,
			created_at timestamptz NOT NULL DEFAULT NOW(),
			updated_at timestamptz NOT NULL DEFAULT NOW()
		);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_fused_engine_usage_pending_counter
		ON fused_engine_usage_counter_reports(metric, bucket_start, bucket_seconds)
		WHERE flushed_at IS NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_usage_pending
		ON fused_engine_usage_counter_reports(created_at)
		WHERE flushed_at IS NULL;`,

		// Workspace service membership is intentionally separate from enabled
		// service versions. The parent answers "is this service available here?";
		// the child rows answer "which exact Registry service versions are usable?"
		`CREATE TABLE IF NOT EXISTS fused_workspace_services (
			id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			service_id     uuid NOT NULL,
			service_slug   text,
			service_name   text,
			added_by       uuid,
			created_at     timestamptz DEFAULT clock_timestamp(),
			UNIQUE(service_id)
		);`,

		// clock_timestamp() records enablement order inside a single transaction;
		// NOW() would give every row in one apply the same timestamp.
		`CREATE TABLE IF NOT EXISTS fused_workspace_service_versions (
			id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			service_id         uuid NOT NULL,
			service_version_id uuid NOT NULL,
			version            text NOT NULL,
			status             text NOT NULL DEFAULT 'public',
			enabled_by         uuid,
			created_at         timestamptz DEFAULT clock_timestamp(),
			enabled_at         timestamptz DEFAULT clock_timestamp(),
			FOREIGN KEY (service_id)
				REFERENCES fused_workspace_services(service_id)
				ON DELETE CASCADE,
			UNIQUE(service_id, service_version_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_workspace_service_versions_latest
		ON fused_workspace_service_versions(service_id, enabled_at DESC, id DESC);`,

		// Activated runtime contract snapshots are Engine-local copies of the
		// Registry's GraphQL projection for one exact service version. They are
		// deliberately keyed by service_version_id, not workspace/bucket/SDK, so
		// the singleton Engine workspace stores each activated provider version
		// once and every runtime surface reads the same pinned contract.
		`CREATE TABLE IF NOT EXISTS fused_service_contract_snapshots (
			id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			service_id         uuid NOT NULL,
			service_version_id uuid NOT NULL UNIQUE,
			version            text NOT NULL,
			revision           integer NOT NULL DEFAULT 0,
			source_hash        text NOT NULL DEFAULT '',
			contract_hash      text NOT NULL,
			contract_status    text NOT NULL DEFAULT 'active'
				CHECK (contract_status IN ('active', 'stale', 'refresh_failed')),
			service_metadata   jsonb NOT NULL,
			fetched_at         timestamptz NOT NULL DEFAULT NOW(),
			refreshed_at       timestamptz NOT NULL DEFAULT NOW(),
			last_refresh_error text NOT NULL DEFAULT ''
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_contract_snapshots_service
		ON fused_service_contract_snapshots(service_id);`,

		`CREATE TABLE IF NOT EXISTS fused_service_contract_endpoints (
			snapshot_id     uuid NOT NULL REFERENCES fused_service_contract_snapshots(id) ON DELETE CASCADE,
			endpoint_id     uuid NOT NULL,
			name            text NOT NULL,
			method          text NOT NULL DEFAULT '',
			path            text NOT NULL DEFAULT '',
			normalized_path text NOT NULL DEFAULT '',
			operation_json  jsonb NOT NULL,
			PRIMARY KEY (snapshot_id, endpoint_id),
			UNIQUE (snapshot_id, name)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_contract_endpoints_lookup
		ON fused_service_contract_endpoints(snapshot_id, name);`,

		`CREATE TABLE IF NOT EXISTS fused_service_contract_webhooks (
			snapshot_id  uuid NOT NULL REFERENCES fused_service_contract_snapshots(id) ON DELETE CASCADE,
			webhook_id   uuid NOT NULL,
			name         text NOT NULL,
			method       text NOT NULL DEFAULT '',
			webhook_json jsonb NOT NULL,
			PRIMARY KEY (snapshot_id, webhook_id),
			UNIQUE (snapshot_id, name)
		);`,

		// Phase 2 of plans/plan-service-changelog.md: one cursor row per
		// service, read and written once per service per tick by the
		// changelog poller (internal/engine/sandbox/service_changelog_poller.go)
		// -- a deliberate per-service round trip, not a single batched
		// read/write across every service. last_checked_at defaults to the
		// Unix epoch so a newly-activated service's very first poll fetches
		// its entire history rather than nothing.
		`CREATE TABLE IF NOT EXISTS fused_service_changelog_cursor (
			service_id       uuid PRIMARY KEY,
			last_checked_at  timestamptz NOT NULL DEFAULT 'epoch',
			updated_at       timestamptz NOT NULL DEFAULT NOW()
		);`,

		// Engine's local, queryable copy of provider-contract changelog events.
		// Phase 3 matches against this cache, not a live Registry call, so it
		// needs to exist whether or not a workspace happens to be actively
		// syncing when a change lands. registry_changelog_id is UNIQUE so a
		// re-fetch of the same event (e.g. a crash between insert and cursor
		// advance)
		// is a no-op via ON CONFLICT DO NOTHING, never a duplicate.
		`CREATE TABLE IF NOT EXISTS fused_service_changelog_cache (
			id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			registry_changelog_id uuid NOT NULL UNIQUE,
			service_id            uuid NOT NULL,
			version               text,
			config_type           text NOT NULL,
			changelog_type        text NOT NULL,
			diff                  jsonb,
			registry_created_at   timestamptz NOT NULL,
			fetched_at            timestamptz NOT NULL DEFAULT NOW()
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_service_changelog_cache_service_id
		ON fused_service_changelog_cache(service_id, registry_created_at DESC);`,

		// Webhook registrations are Engine-owned (not Registry-owned) so
		// ingress resolves a request with a single indexed local read instead
		// of a NATS round trip to the Registry. auth_type/auth_location/
		// auth_key_name/signature_header/verification_headers/
		// event_extraction_path are denormalized from the service's
		// IncomingWebhookConfig at apply time rather than joined per request
		// -- this is what keeps ingress to exactly one query. A workspace can
		// register multiple independent, human-labeled webhooks for the same
		// service (e.g. one per repo, one per environment); label is not an
		// event filter, just an identity so re-applying the same workspace
		// YAML updates the existing row instead of minting a new slug.
		`CREATE TABLE IF NOT EXISTS fused_workspace_webhooks (
			id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			service_id            uuid NOT NULL,
			service_version_id    uuid NOT NULL,
			label                 text NOT NULL,
			slug                  text NOT NULL,
			auth_type             text NOT NULL DEFAULT '',
			auth_location         text NOT NULL DEFAULT '',
			auth_key_name         text NOT NULL DEFAULT '',
			signature_header      text NOT NULL DEFAULT '',
			verification_headers  text[] NOT NULL DEFAULT '{}',
			event_extraction_path text NOT NULL DEFAULT '',
			-- secret_ref stores a canonical ${bucket.<name>.secret.<key>}
			-- reference instead of a literal signing secret -- the actual
			-- value lives in fused_workspace_secrets under the referenced
			-- bucket's generic named-secret namespace. Empty means "no
			-- signing secret configured".
			secret_ref            text NOT NULL DEFAULT '',
			-- owning_config_key is NULL for a registration created the legacy
			-- way (workspace apply's runtime_config.webhooks). A kind: webhook
			-- artifact apply sets this to its own config_key (see
			-- plans/plan-webhook-kind.md) so (a) workspace apply's prune never
			-- deletes or fights over a row it doesn't own, and (b)
			-- (service_id, label) uniqueness for kind: webhook artifacts can be
			-- enforced as "this pair belongs to config_key X" rather than a
			-- silent overwrite when two different artifacts both target the
			-- same service+name.
			owning_config_key     text,
			created_at            timestamptz DEFAULT NOW(),
			updated_at            timestamptz DEFAULT NOW(),
			UNIQUE(service_id, label),
			UNIQUE(slug)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_workspace_webhooks_slug
		ON fused_workspace_webhooks(slug);`,

		// Config-as-code state is Engine-owned because workspace policy and SDK
		// generation plans must be checked against the workspace allowlist before
		// any Registry-side generation is allowed.
		`CREATE TABLE IF NOT EXISTS fused_config_states (
			id                uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			config_key        text NOT NULL,
			config_type       text NOT NULL CHECK (config_type IN ('workspace', 'sdk', 'mcp', 'webhook')),
			source_hash       text NOT NULL,
			generation        integer NOT NULL DEFAULT 1 CHECK (generation >= 1),
			desired_state     jsonb NOT NULL DEFAULT '{}'::jsonb,
			managed_resources jsonb NOT NULL DEFAULT '{}'::jsonb,
			latest_resource_id uuid,
			updated_by        uuid,
			created_at        timestamptz DEFAULT NOW(),
			updated_at        timestamptz DEFAULT NOW(),
			UNIQUE(config_key)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_config_states_workspace_type
		ON fused_config_states(config_type);`,

		// Plans are remote, immutable execution receipts except for their actions
		// JSON. The revision lets the CLI/UI refetch when a user switches from a
		// recommended deprecate action to force removal before applying.
		`CREATE TABLE IF NOT EXISTS fused_config_plans (
			id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			config_key       text NOT NULL,
			config_type      text NOT NULL CHECK (config_type IN ('workspace', 'sdk', 'mcp', 'webhook')),
			source_hash      text NOT NULL,
			base_generation  integer NOT NULL DEFAULT 0 CHECK (base_generation >= 0),
			status           text NOT NULL DEFAULT 'pending'
				CHECK (status IN ('pending', 'applied', 'superseded', 'stale', 'failed')),
			actions          jsonb NOT NULL DEFAULT '[]'::jsonb,
			desired_state    jsonb NOT NULL DEFAULT '{}'::jsonb,
			resolved_payload jsonb NOT NULL DEFAULT '{}'::jsonb,
			blockers         jsonb NOT NULL DEFAULT '[]'::jsonb,
			warnings         jsonb NOT NULL DEFAULT '[]'::jsonb,
			revision         integer NOT NULL DEFAULT 1 CHECK (revision >= 1),
			created_by       uuid,
			created_at       timestamptz DEFAULT NOW(),
			applied_at       timestamptz,
			superseded_at    timestamptz
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_config_plans_workspace_key_created
		ON fused_config_plans(config_key, created_at DESC);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_fused_config_plans_one_pending
		ON fused_config_plans(config_key)
		WHERE status = 'pending';`,

		// Engine-local notifications are separate from Registry drift snapshots:
		// self-hosted Engines need a local inbox for workspace activation changes,
		// while Registry remains the owner of catalog endpoint/webhook drift.
		`CREATE TABLE IF NOT EXISTS fused_workspace_notifications (
			id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			type         text NOT NULL,
			severity     text NOT NULL CHECK (severity IN ('breaking', 'non-breaking')),
			status       text NOT NULL DEFAULT 'pending'
				CHECK (status IN ('pending', 'acknowledged', 'dismissed')),
			service_id   uuid,
			version      text,
			config_key   text,
			message      text NOT NULL,
			metadata     jsonb NOT NULL DEFAULT '{}'::jsonb,
			created_by   uuid,
			resolved_by  uuid,
			created_at   timestamptz DEFAULT NOW(),
			updated_at   timestamptz DEFAULT NOW()
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_workspace_notifications_workspace_status
		ON fused_workspace_notifications(status, created_at DESC);`,

		// Idempotency cache: stores the final response of an Execute call keyed
		// by (artifact_id, idempotency_key_hash) so a retried/duplicate request with
		// the same key replays the original result instead of re-hitting the
		// vendor. 24h TTL mirrors Stripe's idempotency key retention. The key
		// itself is hashed before storage, consistent with how execution audit
		// events already only ever store idempotency_key_hash, never the raw key.
		`CREATE TABLE IF NOT EXISTS fused_engine_idempotency_keys (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			artifact_id uuid NOT NULL,
			idempotency_key_hash text NOT NULL,
			request_body_hash text,
			environment text,
			response_body bytea,
			created_at timestamptz NOT NULL DEFAULT NOW(),
			expires_at timestamptz NOT NULL,
			UNIQUE(artifact_id, idempotency_key_hash)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_idempotency_keys_expires
		ON fused_engine_idempotency_keys(expires_at);`,

		// Webhook Events
		`CREATE TABLE IF NOT EXISTS fused_webhook_events (
			id uuid PRIMARY KEY,
			account_id uuid,
			service_id uuid,
			msg_id text,
			event_type text,
			error_reason text,
			sdk_record_id uuid,
			verification_status text,
			delivery_status text,
			environment text,
			latency_ms integer,
			retry_count integer,
			credits_consumed integer,
			payload_size integer,
			created_at timestamp with time zone DEFAULT NOW(),
			updated_at timestamp with time zone DEFAULT NOW()
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_webhook_events_account_id ON fused_webhook_events(account_id);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_webhook_events_service_id ON fused_webhook_events(service_id);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_fused_webhook_events_msg_id ON fused_webhook_events(msg_id);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_webhook_events_created_at ON fused_webhook_events(created_at);`,

		// SDK Tokens
		`CREATE TABLE IF NOT EXISTS fused_artifact_tokens (
			id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			artifact_id      uuid NOT NULL REFERENCES fused_artifact_scopes(artifact_id) ON DELETE CASCADE,
			token_hash  text NOT NULL UNIQUE,
			name        text NOT NULL,
			last_used_at timestamptz,
			created_at  timestamptz DEFAULT NOW()
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_artifact_tokens_artifact_id ON fused_artifact_tokens(artifact_id);`,

		// Workspace Secrets
		`CREATE TABLE IF NOT EXISTS fused_workspace_secrets (
			id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			bucket_id        uuid REFERENCES fused_buckets(id) ON DELETE CASCADE,
			service_id       uuid NOT NULL,
			key_name         text NOT NULL,
			credential_type  text NOT NULL,
			encrypted_dek    text NOT NULL,
			encrypted_value  text NOT NULL,
			last_used_at     timestamptz,
			expires_at       timestamptz,
			created_at       timestamptz DEFAULT NOW(),
			updated_at       timestamptz DEFAULT NOW()
		);`,

		// Bucket Values
		`CREATE TABLE IF NOT EXISTS fused_bucket_values (
			id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			bucket_id        uuid NOT NULL REFERENCES fused_buckets(id) ON DELETE CASCADE,
			service_id       uuid NOT NULL,
			key_name         text NOT NULL,
			location         text NOT NULL,
			value            text NOT NULL,
			created_at       timestamptz DEFAULT NOW(),
			updated_at       timestamptz DEFAULT NOW(),
			CONSTRAINT uq_bucket_values UNIQUE (bucket_id, service_id, key_name)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_bucket_values_lookup ON fused_bucket_values(bucket_id, service_id);`,

		// Connection profiles are workspace-scoped, not bucket-scoped: the
		// effective profile identity is workspace + service + service_version +
		// auth_type, and every artifact execution in the workspace shares it
		// regardless of which bucket the artifact selected. Baseline and
		// override are kept as separate rows (the "layer" column) rather than
		// one row with an override flag, so a reset is a targeted delete of the
		// override row and the pinned provider baseline survives untouched --
		// no Registry call is needed to restore it. See
		// plans/workspace_connection_profile_scope_plan.md ("Target Engine
		// Schema").
		`CREATE TABLE IF NOT EXISTS fused_workspace_connection_profiles (
			id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			service_id              uuid NOT NULL,
			service_version_id      uuid NOT NULL,
			auth_type               text NOT NULL,
			layer                   text NOT NULL,
			registry_profile_id     uuid,
			profile_revision        integer NOT NULL,
			profile_hash            text NOT NULL,
			provenance              text NOT NULL,
			profile_snapshot        jsonb NOT NULL,
			-- is_public tracks whether this profile was published to the Registry
			-- via setConnectionProfile (workspace.yaml connection_profiles[*].public:
			-- true), so fused sync can round-trip that intent back into YAML
			-- without re-deriving it from Registry state. Purely local bookkeeping;
			-- does not affect resolution/reconciliation precedence.
			is_public               boolean NOT NULL DEFAULT false,
			created_at              timestamptz NOT NULL DEFAULT NOW(),
			updated_at              timestamptz NOT NULL DEFAULT NOW(),
			CONSTRAINT uq_fused_workspace_connection_profile
				UNIQUE (service_id, service_version_id, auth_type, layer),
			CONSTRAINT chk_fused_workspace_connection_profile_layer
				CHECK (layer IN ('baseline', 'override')),
			CONSTRAINT chk_fused_workspace_connection_profile_provenance
				CHECK (provenance IN ('workspace', 'provider', 'fused')),
			-- A baseline is always a pinned Registry/Fused publication; only an
			-- override is workspace-authored. Keeping this closed prevents a
			-- baseline row from silently becoming untraceable local content.
			CONSTRAINT chk_fused_workspace_connection_profile_baseline_provenance
				CHECK (layer <> 'baseline' OR provenance IN ('provider', 'fused')),
			-- A baseline is a pinned Registry publication, so allowing it without
			-- the publication ID would make provenance and reset auditing unverifiable.
			CONSTRAINT chk_fused_workspace_connection_profile_baseline_registry_id
				CHECK (layer <> 'baseline' OR registry_profile_id IS NOT NULL),
			-- An override always has workspace provenance and, since it is not a
			-- Registry publication, never carries a registry_profile_id.
			CONSTRAINT chk_fused_workspace_connection_profile_override_provenance
				CHECK (layer <> 'override' OR (provenance = 'workspace' AND registry_profile_id IS NULL))
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_workspace_connection_profiles_lookup
		ON fused_workspace_connection_profiles(service_id, service_version_id, auth_type);`,

		// Compiled bindings are stored against the owning profile row (baseline
		// or override); the parent's layer -- not a per-binding flag -- decides
		// precedence, and dropping the profile drops its bindings via the FK.
		// There is no bucket_id here: routing bindings follow the same
		// workspace-scoped identity as their profile, not a bucket.
		`CREATE TABLE IF NOT EXISTS fused_workspace_connection_bindings (
			id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			service_id              uuid NOT NULL,
			service_version_id      uuid NOT NULL,
			profile_id              uuid NOT NULL REFERENCES fused_workspace_connection_profiles(id) ON DELETE CASCADE,
			source_kind             text NOT NULL,
			literal_value           text,
			source_path             text,
			target_location         text NOT NULL,
			target_name             text,
			operation_ids           text[] NOT NULL DEFAULT '{}',
			mode                    text NOT NULL,
			provenance              text NOT NULL,
			source_profile_revision integer,
			created_at              timestamptz NOT NULL DEFAULT NOW(),
			updated_at              timestamptz NOT NULL DEFAULT NOW(),
			CONSTRAINT chk_fused_workspace_connection_binding_source CHECK (
				(literal_value IS NOT NULL AND source_kind = 'literal' AND source_path IS NULL)
				OR
				(literal_value IS NULL AND source_kind = 'connection_resource' AND source_path IS NOT NULL)
			),
			CONSTRAINT chk_fused_workspace_connection_binding_location
				CHECK (target_location IN ('base_url', 'header', 'query', 'path', 'body')),
			CONSTRAINT chk_fused_workspace_connection_binding_mode CHECK (mode IN ('default', 'force')),
			CONSTRAINT chk_fused_workspace_connection_binding_provenance
				CHECK (provenance IN ('workspace', 'provider', 'fused'))
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_workspace_connection_bindings_execution
		ON fused_workspace_connection_bindings(service_id, service_version_id, target_location);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_workspace_connection_bindings_operations
		ON fused_workspace_connection_bindings USING GIN(operation_ids);`,

		// fused_workspace_execution_policies stores workspace-local execution
		// policy overrides. Registry-published policy still arrives through the
		// runtime contract snapshot; this table only records values a workspace
		// wants to enforce locally, so a row existing here always means
		// "workspace override", full stop. We intentionally support service and
		// version scope only: endpoint-level local policy would create a second
		// provider contract language inside Engine, while provider-owned endpoint
		// policy already reaches every workspace through the snapshot. Resolution
		// precedence -- local override wins over snapshot value when present --
		// belongs at runtime read time, beside the code that consumes the
		// contract fields.
		`CREATE TABLE IF NOT EXISTS fused_workspace_execution_policies (
			id                      uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			service_id              uuid NOT NULL,
			service_version_id      uuid,
			rate_limit              jsonb,
			retry_config            jsonb,
			pagination              jsonb,
			event_extraction_path   text,
			incoming_webhook_config jsonb,
			-- base_url is this workspace's own local override/workaround for a
			-- wrong or missing spec-derived base_url -- takes effect here
			-- immediately on apply regardless of publish, same as every other
			-- column on this table (see LocalObjectCache.applyExecutionPolicyOverride).
			base_url                text,
			created_at timestamptz NOT NULL DEFAULT NOW(),
			updated_at timestamptz NOT NULL DEFAULT NOW()
		);`,
		// Exactly one service-default override row per service.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_fused_workspace_execution_policies_service_default
		ON fused_workspace_execution_policies(service_id) WHERE service_version_id IS NULL;`,
		// Exactly one version-override row per version.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_fused_workspace_execution_policies_version_override
		ON fused_workspace_execution_policies(service_version_id) WHERE service_version_id IS NOT NULL;`,

		// An artifact resolves credentials from exactly one bucket. Keeping the
		// bucket in a separate table preserves lifecycle joins without implying
		// that one SDK can choose among several buckets at runtime.
		`CREATE TABLE IF NOT EXISTS fused_artifact_buckets (
			artifact_id      uuid NOT NULL REFERENCES fused_artifact_scopes(artifact_id) ON DELETE CASCADE,
			bucket_id   uuid NOT NULL REFERENCES fused_buckets(id) ON DELETE CASCADE,
			created_at  timestamptz DEFAULT NOW(),
			PRIMARY KEY (artifact_id)
		);`,
	}
}

func engineMigrationQueries() []string {
	return []string{
		// Mono-workspace: fused_buckets/fused_connect_configs/fused_auth_connections/
		// fused_connect_sessions dropped their per-row workspace_id (the CREATE
		// TABLE statements above no longer declare it -- there is only ever one
		// workspace, so bucket_id/etc. already uniquely scope these rows). But
		// CREATE TABLE IF NOT EXISTS is a no-op on a database that already ran
		// the old schema, so any such database still carries the NOT NULL
		// workspace_id column and, on fused_buckets, the old composite unique
		// constraint -- causing inserts to fail (missing workspace_id) or
		// ON CONFLICT (name) to fail (no matching constraint) without this.
		`ALTER TABLE fused_buckets DROP CONSTRAINT IF EXISTS uq_workspace_buckets;`,
		`ALTER TABLE fused_buckets DROP COLUMN IF EXISTS workspace_id;`,
		`ALTER TABLE fused_buckets ADD CONSTRAINT uq_workspace_buckets UNIQUE (name);`,
		`ALTER TABLE fused_connect_configs DROP COLUMN IF EXISTS workspace_id;`,
		`ALTER TABLE fused_auth_connections DROP COLUMN IF EXISTS workspace_id;`,
		`ALTER TABLE fused_connect_sessions DROP COLUMN IF EXISTS workspace_id;`,

		// bucket_id moved off fused_artifact_scopes onto fused_artifact_buckets (a
		// bucket is now resolved via LEFT JOIN, see
		// GetArtifactScope/ListArtifactScopes). The CREATE TABLE statement above no
		// longer declares this column, but any database where
		// fused_artifact_scopes was already created still has it as NOT NULL
		// with no default -- SaveArtifactScope's INSERT (which correctly no
		// longer supplies bucket_id) would fail that constraint on any such
		// database without this drop.
		`ALTER TABLE fused_artifact_scopes DROP COLUMN IF EXISTS bucket_id;`,
		// Existing development databases used a composite SDK/bucket key. The
		// unique index closes that shape without adding runtime compatibility:
		// every artifact has one immutable bucket assignment.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_fused_artifact_buckets_artifact_id ON fused_artifact_buckets(artifact_id);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_fused_artifact_scopes_artifact_identity
		ON fused_artifact_scopes(account_id, kind, name, version)
		WHERE name IS NOT NULL AND version IS NOT NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_fused_artifact_scopes_account_kind ON fused_artifact_scopes(account_id, kind, created_at DESC);`,

		`ALTER TABLE fused_workspace_services ADD COLUMN IF NOT EXISTS service_slug text;`,
		// Existing installations may still carry the two-kind checks from the
		// original SDK-only config implementation. Recreate them explicitly so
		// kind=mcp can be planned and applied without a manual schema edit.

		// These historical entries used to backfill fused_bucket_bindings
		// (materializing fused_bucket_values rows and stripping untrusted
		// literal base_url overrides). fused_bucket_bindings is dropped
		// outright later in this list as part of the workspace-scoped
		// connection-profile migration -- see
		// plans/workspace_connection_profile_scope_plan.md -- so backfilling it
		// here would either target a table that no longer exists on a fresh
		// install (schema no longer creates it) or populate rows that the drop
		// below immediately discards on an existing install. Removed rather
		// than left as dead/broken statements.
		`ALTER TABLE fused_workspace_connection_profiles
			DROP CONSTRAINT IF EXISTS chk_fused_workspace_connection_profile_baseline_registry_id;`,
		`ALTER TABLE fused_workspace_connection_profiles
			ADD CONSTRAINT chk_fused_workspace_connection_profile_baseline_registry_id
			CHECK (layer <> 'baseline' OR registry_profile_id IS NOT NULL);`,
		// Same drop-then-recreate idiom as
		// chk_fused_workspace_connection_profile_baseline_registry_id just
		// above: unconditionally dropping (if present) and re-adding is
		// idempotent on its own, so no DO block / pg_constraint inspection
		// is needed to distinguish "missing", "stale definition", and
		// "already correct" -- all three converge to the same end state.
		`ALTER TABLE fused_auth_connections
			DROP CONSTRAINT IF EXISTS chk_fused_auth_connections_refresh_state;`,
		`ALTER TABLE fused_auth_connections
			ADD CONSTRAINT chk_fused_auth_connections_refresh_state
			CHECK (refresh_state IN ('ok', 'failed', 'expired', 'reconnect_required'));`,

		// Migrate fused_workspace_secrets
		`ALTER TABLE fused_workspace_secrets DROP CONSTRAINT IF EXISTS uq_workspace_secrets;`,
		`DROP INDEX IF EXISTS idx_fused_workspace_secrets_lookup;`,
		`ALTER TABLE fused_workspace_secrets DROP COLUMN IF EXISTS artifact_id CASCADE;`,

		`ALTER TABLE fused_workspace_secrets ALTER COLUMN bucket_id SET NOT NULL;`,
		`ALTER TABLE fused_workspace_secrets ADD CONSTRAINT uq_workspace_secrets UNIQUE (bucket_id, service_id, key_name);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_workspace_secrets_lookup ON fused_workspace_secrets(bucket_id, service_id);`,

		// The product is not live, so connection-profile ownership moves from
		// bucket-scoped to workspace-scoped by dropping the old tables outright
		// rather than migrating rows -- see
		// plans/workspace_connection_profile_scope_plan.md. Bindings are
		// dropped first because they FK to attachments.
		`DROP TABLE IF EXISTS fused_bucket_bindings;`,
		`DROP TABLE IF EXISTS fused_bucket_profile_attachments;`,

		// kind: webhook (plans/plan-webhook-kind.md) is a fourth config_type,
		// and its registrations need to be distinguishable from the legacy
		// runtime_config.webhooks path on the same fused_workspace_webhooks
		// table -- see this table's owning_config_key column comment above.
		`ALTER TABLE fused_config_states DROP CONSTRAINT IF EXISTS fused_config_states_config_type_check;`,
		`ALTER TABLE fused_config_states ADD CONSTRAINT fused_config_states_config_type_check CHECK (config_type IN ('workspace', 'sdk', 'mcp', 'webhook'));`,
		`ALTER TABLE fused_config_plans DROP CONSTRAINT IF EXISTS fused_config_plans_config_type_check;`,
		`ALTER TABLE fused_config_plans ADD CONSTRAINT fused_config_plans_config_type_check CHECK (config_type IN ('workspace', 'sdk', 'mcp', 'webhook'));`,
		`ALTER TABLE fused_workspace_webhooks ADD COLUMN IF NOT EXISTS owning_config_key text;`,

		// fused_workspace_notifications' severity column was originally
		// created CHECK (severity IN ('breaking')) -- Phase 3 of the service
		// changelog system (plans/plan-service-changelog.md) adds
		// WorkspaceNotificationSeverityNonBreaking for changelog-derived
		// notifications that are genuinely not breaking (a new version, a
		// deprecation warning), so any pre-existing database still carrying
		// the old constraint would reject those inserts without this widen.
		// Same drop-then-recreate idiom as the config_type_check pairs above.
		`ALTER TABLE fused_workspace_notifications DROP CONSTRAINT IF EXISTS fused_workspace_notifications_severity_check;`,
		`ALTER TABLE fused_workspace_notifications ADD CONSTRAINT fused_workspace_notifications_severity_check CHECK (severity IN ('breaking', 'non-breaking'));`,

		// resolved_by records which actor (account) transitioned a notification
		// out of 'pending' (see UpdateWorkspaceNotificationStatus, Phase 4 of
		// plans/plan-service-changelog.md) -- NULL for every row until that first
		// transition happens, same optional/audit-only shape as created_by.
		`ALTER TABLE fused_workspace_notifications ADD COLUMN IF NOT EXISTS resolved_by uuid;`,

		// base_url was added to fused_workspace_execution_policies' CREATE
		// TABLE statement above, but CREATE TABLE IF NOT EXISTS is a no-op on
		// any database that already ran the old schema -- same failure mode
		// documented at the top of this function. Those databases never got
		// the column, so UpsertWorkspaceExecutionPolicyOverride's INSERT
		// (which always lists base_url) fails with "column base_url does not
		// exist", surfaced to the CLI as a generic 500 on workspace apply.
		`ALTER TABLE fused_workspace_execution_policies ADD COLUMN IF NOT EXISTS base_url text;`,
	}
}
