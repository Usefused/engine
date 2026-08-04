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

		// Control-plane subjects are deliberately independent of Registry
		// accounts. The Engine authenticates these local principals before it
		// uses its own licence identity for outbound Registry requests.
		`CREATE TABLE IF NOT EXISTS fused_subjects (
			id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			kind         text NOT NULL,
			display_name text NOT NULL,
			status       text NOT NULL DEFAULT 'active',
			created_at   timestamptz NOT NULL DEFAULT NOW(),
			updated_at   timestamptz NOT NULL DEFAULT NOW(),
			CONSTRAINT chk_fused_subjects_kind
				CHECK (kind IN ('bootstrap', 'user', 'service_account', 'artifact')),
			CONSTRAINT chk_fused_subjects_status
				CHECK (status IN ('invited', 'active', 'suspended', 'archived'))
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_subjects_status
		ON fused_subjects(status, id);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_fused_subjects_bootstrap
		ON fused_subjects(kind) WHERE kind = 'bootstrap';`,

		// User identity is an Engine-local projection over the canonical subject.
		// Display casing is preserved separately from the normalized unique key.
		`CREATE TABLE IF NOT EXISTS fused_users (
			subject_id       uuid PRIMARY KEY REFERENCES fused_subjects(id) ON DELETE CASCADE,
			email_normalized text NOT NULL UNIQUE,
			email_display    text NOT NULL,
			created_at       timestamptz NOT NULL DEFAULT NOW(),
			updated_at       timestamptz NOT NULL DEFAULT NOW(),
			CONSTRAINT chk_fused_users_email_normalized CHECK (email_normalized <> ''),
			CONSTRAINT chk_fused_users_email_display CHECK (email_display <> '')
		);`,

		// Team rows and memberships exist before their management API so the
		// effective-grants query has one stable direct-plus-team shape from its
		// first release. CRUD is layered on without replacing authentication.
		`CREATE TABLE IF NOT EXISTS fused_teams (
			id          uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			name        text NOT NULL,
			slug        text NOT NULL UNIQUE,
			description text NOT NULL DEFAULT '',
			status      text NOT NULL DEFAULT 'active',
			created_at  timestamptz NOT NULL DEFAULT NOW(),
			updated_at  timestamptz NOT NULL DEFAULT NOW(),
			CONSTRAINT chk_fused_teams_status
				CHECK (status IN ('active', 'archived'))
		);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_fused_teams_slug_ci ON fused_teams(lower(slug));`,
		`CREATE TABLE IF NOT EXISTS fused_team_memberships (
			team_id               uuid NOT NULL REFERENCES fused_teams(id) ON DELETE CASCADE,
			member_subject_id     uuid NOT NULL REFERENCES fused_subjects(id) ON DELETE CASCADE,
			membership_role       text NOT NULL DEFAULT 'member',
			created_by_subject_id uuid REFERENCES fused_subjects(id) ON DELETE SET NULL,
			created_at            timestamptz NOT NULL DEFAULT NOW(),
			PRIMARY KEY (team_id, member_subject_id),
			CONSTRAINT chk_fused_team_memberships_role
				CHECK (membership_role IN ('member', 'manager'))
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_team_memberships_member
		ON fused_team_memberships(member_subject_id, team_id);`,

		// Raw control credentials are never stored. Authentication hashes an
		// inbound key and performs a point lookup against this table.
		`CREATE TABLE IF NOT EXISTS fused_control_credentials (
			id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			subject_id   uuid NOT NULL REFERENCES fused_subjects(id) ON DELETE RESTRICT,
			key_hash     text NOT NULL UNIQUE,
			key_prefix   text NOT NULL,
			name         text NOT NULL,
			expires_at   timestamptz,
			last_used_at timestamptz,
			revoked_at   timestamptz,
			created_at   timestamptz NOT NULL DEFAULT NOW()
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_control_credentials_active_hash
		ON fused_control_credentials(key_hash) WHERE revoked_at IS NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_fused_control_credentials_subject_recent
		ON fused_control_credentials(subject_id, created_at DESC, id);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_fused_control_credentials_active_name_ci
		ON fused_control_credentials(subject_id, lower(name)) WHERE revoked_at IS NULL;`,

		// Roles are stable permission bundles. A binding determines both the
		// principal (or team) receiving the role and its resource boundary.
		`CREATE TABLE IF NOT EXISTS fused_roles (
			id           uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			slug         text NOT NULL UNIQUE,
			display_name text NOT NULL,
			scope_type   text NOT NULL,
			system_role  boolean NOT NULL DEFAULT false,
			created_at   timestamptz NOT NULL DEFAULT NOW(),
			CONSTRAINT chk_fused_roles_scope_type
				CHECK (scope_type IN ('workspace', 'service', 'bucket', 'artifact'))
		);`,
		`CREATE TABLE IF NOT EXISTS fused_role_permissions (
			role_id    uuid NOT NULL REFERENCES fused_roles(id) ON DELETE CASCADE,
			permission text NOT NULL,
			PRIMARY KEY (role_id, permission),
			CONSTRAINT chk_fused_role_permissions_name CHECK (permission <> '')
		);`,
		`CREATE TABLE IF NOT EXISTS fused_role_bindings (
			id                    uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			subject_type          text NOT NULL,
			subject_id            uuid NOT NULL,
			role_id               uuid NOT NULL REFERENCES fused_roles(id) ON DELETE CASCADE,
			resource_type         text NOT NULL,
			resource_id           uuid NOT NULL,
			created_by_subject_id uuid REFERENCES fused_subjects(id) ON DELETE SET NULL,
			created_at            timestamptz NOT NULL DEFAULT NOW(),
			CONSTRAINT chk_fused_role_bindings_subject_type
				CHECK (subject_type IN ('subject', 'team', 'workspace')),
			CONSTRAINT chk_fused_role_bindings_resource_type
				CHECK (resource_type IN ('workspace', 'service', 'bucket', 'artifact')),
			CONSTRAINT uq_fused_role_binding
				UNIQUE (subject_type, subject_id, role_id, resource_type, resource_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_role_bindings_subject_scope
		ON fused_role_bindings(subject_type, subject_id, resource_type, resource_id);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_role_bindings_resource
		ON fused_role_bindings(resource_type, resource_id);`,

		// A singleton revision lets every process invalidate complete effective
		// authorization snapshots without querying Postgres per permission.
		`CREATE TABLE IF NOT EXISTS fused_authorization_state (
			singleton_key smallint PRIMARY KEY DEFAULT 1,
			revision      bigint NOT NULL DEFAULT 1,
			updated_at    timestamptz NOT NULL DEFAULT NOW(),
			CHECK (singleton_key = 1),
			CHECK (revision > 0)
		);`,
		`INSERT INTO fused_authorization_state (singleton_key, revision)
		VALUES (1, 1)
		ON CONFLICT (singleton_key) DO NOTHING;`,

		// Authorization audit events contain identifiers and sanitized metadata,
		// never request bodies, credentials, tokens, or provider secret values.
		`CREATE TABLE IF NOT EXISTS fused_audit_events (
			id                  uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			occurred_at         timestamptz NOT NULL DEFAULT NOW(),
			actor_subject_id    uuid REFERENCES fused_subjects(id) ON DELETE SET NULL,
			actor_credential_id uuid REFERENCES fused_control_credentials(id) ON DELETE SET NULL,
			action              text NOT NULL,
			permission          text,
			resource_type       text,
			resource_id         uuid,
			request_id          text NOT NULL DEFAULT '',
			trace_id            text NOT NULL DEFAULT '',
			method              text NOT NULL DEFAULT '',
			path                text NOT NULL DEFAULT '',
			outcome             text NOT NULL,
			status_code         integer NOT NULL DEFAULT 0,
			reason_code         text NOT NULL DEFAULT '',
			source_ip           text NOT NULL DEFAULT '',
			user_agent          text NOT NULL DEFAULT '',
			missing_requirements jsonb NOT NULL DEFAULT '[]'::jsonb,
			metadata            jsonb NOT NULL DEFAULT '{}'::jsonb,
			CONSTRAINT chk_fused_audit_events_outcome
				CHECK (outcome IN ('attempted', 'allowed', 'denied', 'succeeded', 'failed')),
			CONSTRAINT chk_fused_audit_events_resource_type
				CHECK (resource_type IS NULL OR resource_type IN ('workspace', 'service', 'bucket', 'artifact')),
			CONSTRAINT chk_fused_audit_events_status_code
				CHECK (status_code BETWEEN 0 AND 599)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_audit_events_occurred_at
		ON fused_audit_events(occurred_at DESC, id);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_audit_events_actor
		ON fused_audit_events(actor_subject_id, occurred_at DESC);`,

		// Buckets
		`CREATE TABLE IF NOT EXISTS fused_buckets (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			name text NOT NULL,
			is_default boolean NOT NULL DEFAULT false,
			created_at timestamptz DEFAULT NOW(),
			updated_at timestamptz DEFAULT NOW(),
			CONSTRAINT uq_workspace_buckets UNIQUE (name)
		);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_fused_buckets_name_ci ON fused_buckets(lower(name));`,

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

		// SDK Scopes
		`CREATE TABLE IF NOT EXISTS fused_artifact_scopes (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			account_id uuid NOT NULL,
			artifact_id uuid UNIQUE NOT NULL,
			owner_subject_id uuid REFERENCES fused_subjects(id) ON DELETE RESTRICT,
			owner_team_id uuid REFERENCES fused_teams(id) ON DELETE RESTRICT,
			CONSTRAINT chk_fused_artifact_scopes_owner CHECK (
				(owner_subject_id IS NOT NULL)::int + (owner_team_id IS NOT NULL)::int = 1
			),
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
			-- set it, so it can be NULL for scopes that were only reactivated.
			name text,
			version text,
			config_key text
		);`,
		// SDK and MCP names are separate user-facing namespaces. Version remains
		// part of the identity so a generated SDK can publish multiple releases.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_fused_artifact_identity_ci
		ON fused_artifact_scopes(kind, lower(name), COALESCE(version, '')) WHERE name IS NOT NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_fused_artifact_reference_latest
		ON fused_artifact_scopes(kind, lower(name), created_at DESC, artifact_id DESC)
		WHERE name IS NOT NULL AND deactivated_at IS NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_fused_artifact_scopes_subject_owner_kind
		ON fused_artifact_scopes(owner_subject_id, kind, created_at DESC, artifact_id);`,

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

		`CREATE INDEX IF NOT EXISTS idx_fused_mcp_sessions_sdk_started ON fused_mcp_sessions(artifact_id, started_at DESC);`,

		// Engine execution receipts are compact product/audit records. OTEL owns
		// rich step-level detail; this table keeps user history queryable even
		// when an observability backend is disabled or has shorter retention.
		`CREATE TABLE IF NOT EXISTS fused_engine_execution_events (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			trace_id text,
			span_id text,
			account_id uuid,
			artifact_id uuid,
			transport text NOT NULL,
			direction text NOT NULL DEFAULT 'outbound',
			service_id uuid,
			service_version_id text,
			operation_id uuid,
			webhook_id uuid,
			endpoint_name text NOT NULL,
			external_id text,
			event_name text,
			http_method text,
			request_path text,
			environment text,
			environment_source text,
			provider_host text,
			provider_http_status integer,
			provider_status_class text,
			status text NOT NULL,
			failure_reason text,
			failure_category text,
			failure_code text,
			latency_ms bigint NOT NULL,
			provider_latency_ms bigint,
			attempt_count integer NOT NULL DEFAULT 1,
			request_bytes bigint NOT NULL DEFAULT 0,
			response_bytes bigint NOT NULL DEFAULT 0,
			verification_status text,
			delivery_status text,
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
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_service_started
		ON fused_engine_execution_events(service_id, started_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_started
		ON fused_engine_execution_events(started_at);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_endpoint
		ON fused_engine_execution_events(artifact_id, endpoint_name, started_at DESC);`,

		// Public-service insight reporting starts from committed canonical events.
		// The projection marker and outbox are updated in one transaction so a
		// Registry outage never blocks execution or causes double counting.
		`CREATE TABLE IF NOT EXISTS fused_public_service_insight_projected_events (
			event_id uuid PRIMARY KEY REFERENCES fused_engine_execution_events(id) ON DELETE CASCADE,
			report_id uuid NOT NULL,
			projected_at timestamptz NOT NULL DEFAULT NOW()
		);`,
		`CREATE TABLE IF NOT EXISTS fused_public_service_insight_outbox (
			report_id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			service_id uuid NOT NULL,
			service_version_id uuid NOT NULL,
			registry_object_kind text NOT NULL,
			registry_object_id uuid NOT NULL,
			direction text NOT NULL,
			transport text NOT NULL,
			outcome text NOT NULL,
			provider_status_class text NOT NULL,
			bucket_start timestamptz NOT NULL,
			bucket_seconds integer NOT NULL DEFAULT 3600,
			call_count bigint NOT NULL,
			total_latency_ms_sum bigint NOT NULL,
			provider_latency_ms_sum bigint NOT NULL,
			latency_histogram jsonb NOT NULL,
			retry_attempts_sum bigint NOT NULL,
			state text NOT NULL DEFAULT 'pending' CHECK (state IN ('pending', 'sent', 'rejected')),
			attempt_count integer NOT NULL DEFAULT 0,
			next_attempt_at timestamptz NOT NULL DEFAULT NOW(),
			last_error_code text NOT NULL DEFAULT '',
			created_at timestamptz NOT NULL DEFAULT NOW(),
			updated_at timestamptz NOT NULL DEFAULT NOW(),
			sent_at timestamptz
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_public_insight_outbox_pending
		ON fused_public_service_insight_outbox(next_attempt_at, created_at) WHERE state = 'pending';`,

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
			public_service_insights_reporting boolean NOT NULL DEFAULT true,
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
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_fused_workspace_services_slug_ci
		ON fused_workspace_services(lower(service_slug)) WHERE service_slug IS NOT NULL;`,

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
			-- The canonical reference preserves the configured key while the
			-- immutable bucket ID prevents delete/recreate of the same name from
			-- silently redirecting webhook verification to another team's bucket.
			secret_ref            text NOT NULL DEFAULT '',
			secret_bucket_id      uuid REFERENCES fused_buckets(id) ON DELETE RESTRICT,
			CHECK ((secret_ref = '' AND secret_bucket_id IS NULL)
				OR (secret_ref <> '' AND secret_bucket_id IS NOT NULL)),
			-- Every registration is owned by exactly one kind: webhook config.
			-- Ownership is immutable through the conflict-qualified batch upsert.
			owning_config_key     text NOT NULL CHECK (owning_config_key <> ''),
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
			owner_subject_id  uuid REFERENCES fused_subjects(id) ON DELETE RESTRICT,
			owner_team_id     uuid REFERENCES fused_teams(id) ON DELETE RESTRICT,
			source_hash       text NOT NULL,
			generation        integer NOT NULL DEFAULT 1 CHECK (generation >= 1),
			desired_state     jsonb NOT NULL DEFAULT '{}'::jsonb,
			managed_resources jsonb NOT NULL DEFAULT '{}'::jsonb,
			latest_resource_id uuid,
			updated_by        uuid,
			created_at        timestamptz DEFAULT NOW(),
			updated_at        timestamptz DEFAULT NOW(),
			UNIQUE(config_key),
			CONSTRAINT chk_fused_config_states_owner CHECK (
				(config_type = 'workspace' AND owner_subject_id IS NULL AND owner_team_id IS NULL) OR
				(config_type IN ('sdk', 'mcp', 'webhook') AND
				 (owner_subject_id IS NOT NULL)::int + (owner_team_id IS NOT NULL)::int = 1)
			)
		);`,
		`CREATE OR REPLACE FUNCTION fused_reject_config_identity_change()
		RETURNS trigger AS $$
		BEGIN
			IF OLD.config_type IS DISTINCT FROM NEW.config_type
			   OR OLD.owner_subject_id IS DISTINCT FROM NEW.owner_subject_id
			   OR OLD.owner_team_id IS DISTINCT FROM NEW.owner_team_id THEN
				RAISE EXCEPTION 'config type and owner are immutable' USING ERRCODE = '23514';
			END IF;
			RETURN NEW;
		END;
		$$ LANGUAGE plpgsql;`,
		`DROP TRIGGER IF EXISTS trg_fused_config_identity_immutable ON fused_config_states;`,
		`CREATE TRIGGER trg_fused_config_identity_immutable
		BEFORE UPDATE ON fused_config_states
		FOR EACH ROW EXECUTE FUNCTION fused_reject_config_identity_change();`,
		`CREATE INDEX IF NOT EXISTS idx_fused_config_states_workspace_type
		ON fused_config_states(config_type);`,

		// Plans are remote, immutable execution receipts except for their actions
		// JSON. The revision lets the CLI/UI refetch when a user switches from a
		// recommended deprecate action to force removal before applying.
		`CREATE TABLE IF NOT EXISTS fused_config_plans (
			id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			config_key       text NOT NULL,
			config_type      text NOT NULL CHECK (config_type IN ('workspace', 'sdk', 'mcp', 'webhook')),
			owner_subject_id uuid REFERENCES fused_subjects(id) ON DELETE RESTRICT,
			owner_team_id    uuid REFERENCES fused_teams(id) ON DELETE RESTRICT,
			source_hash      text NOT NULL,
			base_generation  integer NOT NULL DEFAULT 0 CHECK (base_generation >= 0),
			status           text NOT NULL DEFAULT 'pending'
				CHECK (status IN ('pending', 'applied', 'superseded', 'stale', 'failed')),
			actions          jsonb NOT NULL DEFAULT '[]'::jsonb,
			desired_state    jsonb NOT NULL DEFAULT '{}'::jsonb,
			resolved_payload jsonb NOT NULL DEFAULT '{}'::jsonb,
			blockers         jsonb NOT NULL DEFAULT '[]'::jsonb,
			warnings         jsonb NOT NULL DEFAULT '[]'::jsonb,
			required_permissions jsonb NOT NULL,
			revision         integer NOT NULL DEFAULT 1 CHECK (revision >= 1),
			apply_lease_id   uuid,
			apply_lease_expires_at timestamptz,
			created_by       uuid,
			created_at       timestamptz DEFAULT NOW(),
			applied_at       timestamptz,
			superseded_at    timestamptz,
			CONSTRAINT chk_fused_config_plans_owner CHECK (
				(config_type = 'workspace' AND owner_subject_id IS NULL AND owner_team_id IS NULL) OR
				(config_type IN ('sdk', 'mcp', 'webhook') AND
				 (owner_subject_id IS NOT NULL)::int + (owner_team_id IS NOT NULL)::int = 1)
			)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_config_plans_workspace_key_created
		ON fused_config_plans(config_key, created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_config_plans_owner_status
		ON fused_config_plans(owner_team_id, status, created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_config_plans_subject_owner_status
		ON fused_config_plans(owner_subject_id, status, created_at DESC);`,
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
			response_status integer NOT NULL DEFAULT 200,
			created_at timestamptz NOT NULL DEFAULT NOW(),
			expires_at timestamptz NOT NULL,
			UNIQUE(artifact_id, idempotency_key_hash)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_idempotency_keys_expires
		ON fused_engine_idempotency_keys(expires_at);`,

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
		// The engine operates in a mono-workspace environment. Remove the workspace-level scoping columns.
		`ALTER TABLE fused_buckets DROP CONSTRAINT IF EXISTS uq_workspace_buckets;`,
		`ALTER TABLE fused_buckets DROP COLUMN IF EXISTS workspace_id;`,
		`ALTER TABLE fused_buckets ADD CONSTRAINT uq_workspace_buckets UNIQUE (name);`,
		`ALTER TABLE fused_connect_configs DROP COLUMN IF EXISTS workspace_id;`,
		`ALTER TABLE fused_auth_connections DROP COLUMN IF EXISTS workspace_id;`,
		`ALTER TABLE fused_connect_sessions DROP COLUMN IF EXISTS workspace_id;`,

		// Artifacts map to buckets via the fused_artifact_buckets table. Remove the inline bucket_id column.
		`ALTER TABLE fused_artifact_scopes DROP COLUMN IF EXISTS bucket_id;`,
		// every artifact has one immutable bucket assignment.
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_fused_artifact_buckets_artifact_id ON fused_artifact_buckets(artifact_id);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_fused_artifact_scopes_artifact_identity
		ON fused_artifact_scopes(account_id, kind, name, version)
		WHERE name IS NOT NULL AND version IS NOT NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_fused_artifact_scopes_account_kind ON fused_artifact_scopes(account_id, kind, created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_artifact_scopes_owner_kind ON fused_artifact_scopes(owner_team_id, kind, created_at DESC, artifact_id);`,
		`ALTER TABLE fused_workspace_services ADD COLUMN IF NOT EXISTS service_slug text;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS http_method text;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS request_path text;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS environment_source text;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS provider_host text;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS provider_http_status integer;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS account_id uuid;`,
		`ALTER TABLE fused_engine_execution_events ALTER COLUMN artifact_id DROP NOT NULL;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS direction text NOT NULL DEFAULT 'outbound';`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS operation_id uuid;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS webhook_id uuid;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS external_id text;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS event_name text;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS provider_status_class text;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS failure_category text;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS failure_code text;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS attempt_count integer NOT NULL DEFAULT 1;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS request_bytes bigint NOT NULL DEFAULT 0;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS response_bytes bigint NOT NULL DEFAULT 0;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS verification_status text;`,
		`ALTER TABLE fused_engine_execution_events ADD COLUMN IF NOT EXISTS delivery_status text;`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_direction_started ON fused_engine_execution_events(direction, transport, started_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_operation_started ON fused_engine_execution_events(operation_id, started_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_webhook_started ON fused_engine_execution_events(webhook_id, started_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_started ON fused_engine_execution_events(started_at);`,
		`ALTER TABLE fused_runtime_entitlements ADD COLUMN IF NOT EXISTS public_service_insights_reporting boolean NOT NULL DEFAULT true;`,
		`ALTER TABLE fused_engine_idempotency_keys ADD COLUMN IF NOT EXISTS response_status integer NOT NULL DEFAULT 200;`,
		`DROP TABLE IF EXISTS fused_webhook_events;`,
		`DROP TABLE IF EXISTS fused_mcp_analytics;`,

		// Update connection profile and auth connection constraints to their latest
		// definitions. Unconditionally dropping and re-adding ensures idempotency.
		`ALTER TABLE fused_workspace_connection_profiles
			DROP CONSTRAINT IF EXISTS chk_fused_workspace_connection_profile_baseline_registry_id;`,
		`ALTER TABLE fused_workspace_connection_profiles
			ADD CONSTRAINT chk_fused_workspace_connection_profile_baseline_registry_id
			CHECK (layer <> 'baseline' OR registry_profile_id IS NOT NULL);`,

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

		// Connection profiles are workspace-scoped. Remove the bucket-scoped binding tables.
		`DROP TABLE IF EXISTS fused_bucket_bindings;`,
		`DROP TABLE IF EXISTS fused_bucket_profile_attachments;`,

		// Widen the severity constraint to support non-breaking notifications.
		`ALTER TABLE fused_workspace_notifications DROP CONSTRAINT IF EXISTS fused_workspace_notifications_severity_check;`,
		`ALTER TABLE fused_workspace_notifications ADD CONSTRAINT fused_workspace_notifications_severity_check CHECK (severity IN ('breaking', 'non-breaking'));`,

		// resolved_by records which actor (account) transitioned a notification
		// out of 'pending' (see UpdateWorkspaceNotificationStatus, Phase 4 of
		// plans/plan-service-changelog.md) -- NULL for every row until that first
		// transition happens, same optional/audit-only shape as created_by.
		`ALTER TABLE fused_workspace_notifications ADD COLUMN IF NOT EXISTS resolved_by uuid;`,

		// Ensure the base_url override column exists for execution policies.
		`ALTER TABLE fused_workspace_execution_policies ADD COLUMN IF NOT EXISTS base_url text;`,
	}
}
