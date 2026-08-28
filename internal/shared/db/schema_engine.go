package db

import (
	"context"
	"fmt"
	"log"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	unifiedEmptySetHash = "sha256:4f53cda18c2baa0c0354bb5f9a3ecbe5ed12ab4d8e11ba873c2f11161202b945"
)

// mcpSessionEndReasonConstraint keeps every durable session termination cause on the clean schema.
const mcpSessionEndReasonConstraint = `CONSTRAINT chk_fused_mcp_sessions_end_reason CHECK (
	end_reason IS NULL OR end_reason IN (
		'client_terminated', 'client_disconnected', 'idle_timeout', 'max_lifetime', 'token_expired',
		'token_revoked', 'app_deactivated', 'engine_shutdown', 'runtime_failed', 'tool_call_timeout'
	)
)`

// initEngineSchema reconciles the supported clean schema directly and does not
// attempt to upgrade databases from retired pre-baseline layouts.
func initEngineSchema(ctx context.Context, pool *pgxpool.Pool) error {
	for _, q := range engineSchemaQueries() {
		if _, err := pool.Exec(ctx, q); err != nil {
			return fmt.Errorf("failed to execute query %q: %w", q, err)
		}
	}

	log.Println("Engine database schema initialization complete.")
	return nil
}

// engineSchemaQueries returns the canonical schema plus unversioned live-table convergence.
func engineSchemaQueries() []string {
	queries := []string{
		// Workspaces
		`CREATE TABLE IF NOT EXISTS fused_workspaces (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			account_id uuid NOT NULL,
			name text NOT NULL,
			slug text NOT NULL UNIQUE,
			connect_display_name text NOT NULL DEFAULT 'Fused',
			connect_logo_url text NOT NULL DEFAULT '',
			connect_primary_color text NOT NULL DEFAULT '#6941ff',
			connect_primary_color_customized boolean NOT NULL DEFAULT false,
			connect_support_url text NOT NULL DEFAULT '',
			connect_privacy_url text NOT NULL DEFAULT '',
			singleton_key smallint NOT NULL DEFAULT 1 UNIQUE,
			created_at timestamptz DEFAULT NOW(),
			updated_at timestamptz DEFAULT NOW(),
			CHECK (singleton_key = 1)
		);`,

		// Installation identity belongs to the database, not a process. Every
		// Engine replica sharing this database therefore reports one stable
		// installation while retaining a distinct runtime identity in memory.
		`CREATE TABLE IF NOT EXISTS fused_engine_installation (
			singleton_key smallint PRIMARY KEY DEFAULT 1,
			installation_id uuid NOT NULL UNIQUE DEFAULT gen_random_uuid(),
			created_at timestamptz NOT NULL DEFAULT NOW(),
			CHECK (singleton_key = 1)
		);`,
		`INSERT INTO fused_engine_installation (singleton_key)
		VALUES (1)
		ON CONFLICT (singleton_key) DO NOTHING;`,
		// Sensitive migrations run after master-key loading and keep a permanent immutable ledger.
		`CREATE TABLE IF NOT EXISTS fused_sensitive_data_migrations (
			version bigint PRIMARY KEY,
			name text NOT NULL UNIQUE,
			rows_migrated bigint NOT NULL DEFAULT 0,
			completed_at timestamptz NOT NULL DEFAULT NOW()
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
				CHECK (kind IN ('bootstrap', 'user', 'service_account', 'app')),
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

		// External subjects are durable Engine-local bindings. Registry proves the
		// identity, but only this database decides which invited user receives the
		// existing local roles and grants.
		`CREATE TABLE IF NOT EXISTS fused_external_identities (
			issuer               text NOT NULL,
			external_subject     text NOT NULL,
			provider             text NOT NULL,
			user_subject_id      uuid NOT NULL REFERENCES fused_users(subject_id) ON DELETE CASCADE,
			created_at           timestamptz NOT NULL DEFAULT NOW(),
			last_authenticated_at timestamptz NOT NULL DEFAULT NOW(),
			PRIMARY KEY (issuer, external_subject),
			CONSTRAINT uq_fused_external_identity_user UNIQUE (user_subject_id),
			CONSTRAINT chk_fused_external_identity_values CHECK (
				issuer <> '' AND external_subject <> '' AND provider <> ''
			)
		);`,

		// The Registry verifier is encrypted for cross-process completion; poll
		// tokens are reduced to hashes. Verified assertion fields exist only until
		// the local binding transaction consumes and clears them.
		`CREATE TABLE IF NOT EXISTS fused_managed_login_transactions (
			id                           uuid PRIMARY KEY,
			registry_transaction_id      uuid NOT NULL UNIQUE,
			account_id                   uuid NOT NULL,
			installation_id              uuid NOT NULL,
			purpose                      text NOT NULL CHECK (purpose = 'browser_login'),
			poll_secret_hash             text NOT NULL UNIQUE,
			enrollment_ref               text NOT NULL,
			encrypted_dek                text NOT NULL,
			encrypted_registry_verifier  text,
			state                        text NOT NULL DEFAULT 'pending' CHECK (
				state IN ('pending', 'exchanging', 'verified', 'consumed', 'expired')
			),
			exchange_started_at          timestamptz,
			provider                     text,
			issuer                       text,
			external_subject             text,
			verified_email               text,
			display_name                 text,
			auth_method                  text,
			authenticated_at             timestamptz,
			logout_encrypted_dek         text,
			encrypted_logout_token       text,
			logout_expires_at            timestamptz,
			expires_at                   timestamptz NOT NULL,
			created_at                   timestamptz NOT NULL DEFAULT NOW(),
			consumed_at                  timestamptz,
			CONSTRAINT chk_fused_managed_login_enrollment CHECK (enrollment_ref <> '')
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_managed_login_transactions_expiry
		ON fused_managed_login_transactions(expires_at, id)
		WHERE state <> 'consumed';`,

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
			source       text NOT NULL DEFAULT 'api_key',
			auth_method  text NOT NULL DEFAULT 'api_key',
			expires_at   timestamptz,
			last_used_at timestamptz,
			revoked_at   timestamptz,
			created_at   timestamptz NOT NULL DEFAULT NOW(),
			CONSTRAINT chk_fused_control_credential_metadata CHECK (source <> '' AND auth_method <> '')
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_control_credentials_active_hash
		ON fused_control_credentials(key_hash) WHERE revoked_at IS NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_fused_control_credentials_subject_recent
		ON fused_control_credentials(subject_id, created_at DESC, id);`,
		`CREATE UNIQUE INDEX IF NOT EXISTS uq_fused_control_credentials_active_name_ci
		ON fused_control_credentials(subject_id, lower(name)) WHERE revoked_at IS NULL;`,

		// A managed browser credential owns exactly one opaque Registry logout
		// capability. Deleting it during local revocation makes provider logout
		// one-shot while keeping Logto tokens out of Engine storage.
		`CREATE TABLE IF NOT EXISTS fused_browser_logout_contexts (
			credential_id          uuid PRIMARY KEY REFERENCES fused_control_credentials(id) ON DELETE CASCADE,
			encrypted_dek          text NOT NULL,
			encrypted_logout_token text NOT NULL,
			expires_at             timestamptz NOT NULL,
			created_at             timestamptz NOT NULL DEFAULT NOW(),
			CONSTRAINT chk_fused_browser_logout_context_values CHECK (
				encrypted_dek <> '' AND encrypted_logout_token <> ''
			)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_browser_logout_contexts_expiry
		ON fused_browser_logout_contexts(expires_at, credential_id);`,

		// CLI login precommits a credential hash before browser approval. Keeping
		// the rendezvous in PostgreSQL lets any Engine node complete the flow while
		// ensuring the browser and polling capabilities are independent and one-time.
		`CREATE TABLE IF NOT EXISTS fused_cli_login_transactions (
			id                       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			poll_secret_hash         text NOT NULL UNIQUE,
			browser_secret_hash      text NOT NULL UNIQUE,
			credential_hash          text NOT NULL UNIQUE,
			credential_prefix        text NOT NULL,
			state                    text NOT NULL DEFAULT 'pending',
			approved_subject_id      uuid REFERENCES fused_subjects(id) ON DELETE RESTRICT,
			approved_by_credential_id uuid REFERENCES fused_control_credentials(id) ON DELETE RESTRICT,
			auth_method              text,
			expires_at               timestamptz NOT NULL,
			credential_expires_at    timestamptz NOT NULL,
			credential_id            uuid REFERENCES fused_control_credentials(id) ON DELETE RESTRICT,
			created_at                timestamptz NOT NULL DEFAULT NOW(),
			approved_at               timestamptz,
			consumed_at               timestamptz,
			CONSTRAINT chk_fused_cli_login_state
				CHECK (state IN ('pending', 'approved', 'consumed')),
			CONSTRAINT chk_fused_cli_login_prefix CHECK (credential_prefix <> '')
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_cli_login_transactions_expiry
		ON fused_cli_login_transactions(expires_at, id)
		WHERE state <> 'consumed';`,

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
				CHECK (scope_type IN ('workspace', 'service', 'bucket', 'app'))
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
				CHECK (resource_type IN ('workspace', 'service', 'bucket', 'app')),
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
				CHECK (outcome IN ('attempted', 'allowed', 'denied', 'succeeded', 'failed', 'rolled_back', 'cancelled')),
			CONSTRAINT chk_fused_audit_events_resource_type
				CHECK (resource_type IS NULL OR resource_type IN ('workspace', 'service', 'bucket', 'app')),
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
			auth_name                text NOT NULL,
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
		// workspace secrets. The unique key intentionally excludes app_id: SDKs
		// linked to the same bucket should share connected users.
		`CREATE TABLE IF NOT EXISTS fused_auth_connections (
			id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			bucket_id          uuid NOT NULL REFERENCES fused_buckets(id) ON DELETE CASCADE,
			service_id         uuid NOT NULL,
			service_version_id uuid NOT NULL,
			end_user_ref       text NOT NULL,
			created_by_app_id       uuid,
			auth_type          text NOT NULL,
			auth_name          text NOT NULL,
			credential_source_service_id uuid NOT NULL,
			credential_source_auth_type text NOT NULL,
			credential_source_auth_name text NOT NULL,
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
			last_refresh_attempt_at timestamptz,
			last_refreshed_at  timestamptz,
			refresh_retry_not_before timestamptz,
			refresh_lease_token uuid,
			refresh_lease_expires_at timestamptz,
			refresh_state      text NOT NULL DEFAULT 'ok',
			-- Failure diagnostics contain stable codes and trace
			-- correlation only; provider bodies, user references, and
			-- credentials never enter these fields.
			last_failure_code  text NOT NULL DEFAULT '',
			last_failure_at    timestamptz,
			last_failure_trace_id text NOT NULL DEFAULT '',
			created_at         timestamptz DEFAULT NOW(),
			updated_at         timestamptz DEFAULT NOW(),
			CONSTRAINT uq_fused_auth_connections UNIQUE (bucket_id, service_id, end_user_ref, auth_name),
			CONSTRAINT chk_fused_auth_connections_refresh_state
				CHECK (refresh_state IN ('ok', 'failed', 'expired', 'reconnect_required')),
			CONSTRAINT chk_fused_auth_connections_refresh_lease
				CHECK ((refresh_lease_token IS NULL) = (refresh_lease_expires_at IS NULL))
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_auth_connections_bucket_service
		ON fused_auth_connections(bucket_id, service_id);`,
		// Refresh admission mirrors the worker's exact due-time ordering and normalized OAuth/OIDC family predicate.
		`CREATE INDEX IF NOT EXISTS idx_fused_auth_connections_refresh
		ON fused_auth_connections(
			COALESCE(LEAST(expires_at, refresh_token_expires_at), expires_at, refresh_token_expires_at), id
		)
		WHERE refresh_state IN ('ok', 'failed', 'expired')
		  AND service_version_id IS NOT NULL
		  AND lower(replace(btrim(auth_type), '-', '_')) IN
		      ('oauth', 'oauth2', 'oauth2_authorization_code', 'oidc', 'openidconnect', 'open_id_connect');`,
		// The clean-schema boundary rejects retained unversioned grants instead of preserving an ambiguous refresh path.
		`ALTER TABLE fused_auth_connections ALTER COLUMN service_version_id SET NOT NULL;`,

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
				service_version_id uuid NOT NULL,
				auth_type          text NOT NULL,
				auth_name          text NOT NULL,
				credential_source_service_id uuid NOT NULL,
				credential_source_auth_type text NOT NULL,
				credential_source_auth_name text NOT NULL,
				end_user_ref       text NOT NULL,
				state_hash         text NOT NULL UNIQUE,
				nonce_hash         text NOT NULL DEFAULT '',
				encrypted_dek      text NOT NULL DEFAULT '',
				pkce_verifier      text NOT NULL DEFAULT '',
				redirect_uri       text NOT NULL DEFAULT '',
				created_by_app_id       uuid,
				return_url         text NOT NULL DEFAULT '',
				resource_input     jsonb NOT NULL DEFAULT '{}'::jsonb,
				requested_scopes   text[] NOT NULL DEFAULT '{}',
				expires_at         timestamptz NOT NULL,
				used_at            timestamptz,
				created_at         timestamptz DEFAULT NOW()
			);`,
		// Exact contract identity is mandatory; pre-baseline NULL sessions are unsupported.
		`ALTER TABLE fused_connect_sessions ALTER COLUMN service_version_id SET NOT NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_fused_connect_sessions_state_hash
			ON fused_connect_sessions(state_hash);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_connect_sessions_expires
			ON fused_connect_sessions(expires_at);`,

		// Input sessions exist only when an SDK/CLI caller omits a required
		// provider-routing field. Keeping them separate ensures OAuth state,
		// nonce, and PKCE material are not created until the browser form has
		// produced a complete validated input set. Only a hash of the one-time
		// browser token is stored.
		`CREATE TABLE IF NOT EXISTS fused_connect_input_sessions (
			id                 uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			bucket_id          uuid NOT NULL REFERENCES fused_buckets(id) ON DELETE CASCADE,
			service_id         uuid NOT NULL,
			auth_type          text NOT NULL,
			auth_name          text NOT NULL,
			credential_source_service_id uuid NOT NULL,
			credential_source_auth_type text NOT NULL,
			credential_source_auth_name text NOT NULL,
			contract_hash      text NOT NULL CHECK (contract_hash ~ '^sha256:[0-9a-f]{64}$'),
			end_user_ref       text NOT NULL,
			token_hash         text NOT NULL UNIQUE,
			created_by_app_id  uuid,
			return_url         text NOT NULL DEFAULT '',
			resource_input     jsonb NOT NULL DEFAULT '{}'::jsonb,
			requested_scopes   text[] NOT NULL DEFAULT '{}',
			expires_at         timestamptz NOT NULL,
			used_at            timestamptz,
			created_at         timestamptz NOT NULL DEFAULT NOW()
		);`,
		// The UNIQUE token_hash constraint already supplies the exact lookup
		// index, so only expiry needs a separate cleanup index.
		`CREATE INDEX IF NOT EXISTS idx_fused_connect_input_sessions_expires
			ON fused_connect_input_sessions(expires_at);`,

		// Engine execution receipts are compact product/audit records. OTEL owns
		// rich step-level detail; this table keeps user history queryable even
		// when an observability backend is disabled or has shorter retention.
		`CREATE TABLE IF NOT EXISTS fused_engine_execution_events (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			trace_id text,
			span_id text,
			account_id uuid,
			app_family_id uuid,
			app_id uuid,
			app_token_id uuid,
			app_version text,
			transport text NOT NULL,
			provider_protocol text,
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
			auth_scheme_names text[] NOT NULL DEFAULT '{}',
			auth_scheme_types text[] NOT NULL DEFAULT '{}',
			auth_scheme_count bigint NOT NULL DEFAULT 0,
			auth_selection_outcome text,
			pagination_type text,
			pagination_page_count bigint NOT NULL DEFAULT 0,
			pagination_item_count bigint NOT NULL DEFAULT 0,
			pagination_byte_count bigint NOT NULL DEFAULT 0,
			pagination_stop_reason text,
			rate_limit_decision text,
			rate_limit_policy_count bigint NOT NULL DEFAULT 0,
			rate_limit_scope_kinds text[] NOT NULL DEFAULT '{}',
			rate_limit_units text[] NOT NULL DEFAULT '{}',
			rate_limit_unit_totals bigint[] NOT NULL DEFAULT '{}',
			rate_limit_retry_outcome text,
			rate_limit_header_outcome text,
			request_bytes bigint NOT NULL DEFAULT 0,
			response_bytes bigint NOT NULL DEFAULT 0,
			verification_status text,
			delivery_status text,
			idempotency_key_hash text,
			request_body_hash text,
			timings jsonb,
			execution_kind text NOT NULL DEFAULT 'physical' CHECK (execution_kind IN ('physical','unified')),
			parent_execution_id uuid,
			unified_target text NOT NULL DEFAULT '',
			execution_phase text NOT NULL DEFAULT '',
			unified_steps jsonb NOT NULL DEFAULT '[]',
			started_at timestamp with time zone NOT NULL,
			ended_at timestamp with time zone NOT NULL,
			created_at timestamp with time zone DEFAULT NOW(),
			idempotency_replayed boolean NOT NULL DEFAULT false,
			CONSTRAINT chk_fused_execution_app_identity CHECK (
				transport NOT IN ('sdk', 'mcp', 'rest') OR (
					app_family_id IS NOT NULL AND app_id IS NOT NULL
					AND NULLIF(BTRIM(app_version), '') IS NOT NULL
				)
			),
			CONSTRAINT chk_fused_execution_app_version_length CHECK (
				app_version IS NULL OR CHAR_LENGTH(app_version) <= 128
			)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_service_version
		ON fused_engine_execution_events(service_id, service_version_id);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_service_started
		ON fused_engine_execution_events(service_id, started_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_started
		ON fused_engine_execution_events(started_at);`,
		// Workspace Activity always scopes date-range aggregation by account; this
		// order lets PostgreSQL bound the tenant slice before grouping dimensions.
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_account_started
		ON fused_engine_execution_events(account_id, started_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_direction_started
		ON fused_engine_execution_events(direction, transport, started_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_operation_started
		ON fused_engine_execution_events(operation_id, started_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_webhook_started
		ON fused_engine_execution_events(webhook_id, started_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_family_started
		ON fused_engine_execution_events(account_id, app_family_id, started_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_app_started
		ON fused_engine_execution_events(account_id, app_id, started_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_endpoint
		ON fused_engine_execution_events(app_id, endpoint_name, started_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_execution_events_token_started
		ON fused_engine_execution_events(app_token_id, started_at DESC)
		WHERE app_token_id IS NOT NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_fused_execution_parent
		ON fused_engine_execution_events(account_id, app_family_id, parent_execution_id, started_at, id)
		WHERE parent_execution_id IS NOT NULL;`,
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
			entitlement_revision text NOT NULL DEFAULT '',
			plan text NOT NULL,
			heartbeat_required boolean NOT NULL,
			usage_reporting text NOT NULL,
			public_service_insights_enabled boolean NOT NULL DEFAULT false,
			heartbeat_interval_seconds integer NOT NULL CHECK (heartbeat_interval_seconds > 0),
			heartbeat_stale_after_seconds integer NOT NULL CHECK (heartbeat_stale_after_seconds > 0),
			refreshed_at timestamptz NOT NULL,
			updated_at timestamptz NOT NULL DEFAULT NOW(),
			-- Capability limits: negative = unlimited, 0 = not allowed
			max_buckets integer NOT NULL DEFAULT -1,
			max_sdk_families integer NOT NULL DEFAULT -1,
			max_mcp_families integer NOT NULL DEFAULT -1,
			max_services integer NOT NULL DEFAULT -1,
			max_sandbox_concurrency integer NOT NULL DEFAULT -1,
			drift_monitoring_enabled boolean NOT NULL DEFAULT false,
			webhook_ingestion_enabled boolean NOT NULL DEFAULT false,
			sso_enabled boolean NOT NULL DEFAULT false,
			execution_retention_days integer NOT NULL DEFAULT 30,
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
			contract_version   integer NOT NULL,
			required_capabilities text[] NOT NULL,
			revision           integer NOT NULL DEFAULT 0,
			source_hash        text NOT NULL DEFAULT '',
			generation_contract_hash text NOT NULL DEFAULT '' CHECK (generation_contract_hash = '' OR generation_contract_hash ~ '^sha256:[0-9a-f]{64}$'),
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
		// The changelog matcher queries this cache rather than calling the
		// Registry live, so it needs to exist whether or not a workspace
		// happens to be actively syncing when a change lands. registry_changelog_id is UNIQUE so a
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

		// Webhook registrations are workspace-local so ingress resolves a request
		// with one indexed read. auth_type/auth_location/
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
			signature_policy      jsonb,
			callback_url          text NOT NULL DEFAULT '',
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

		// Workspace Secrets
		`CREATE TABLE IF NOT EXISTS fused_workspace_secrets (
			id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			bucket_id        uuid NOT NULL REFERENCES fused_buckets(id) ON DELETE CASCADE,
			service_id       uuid NOT NULL,
			key_name         text NOT NULL,
			credential_type  text NOT NULL,
			encrypted_dek    text NOT NULL,
			encrypted_value  text NOT NULL,
			last_used_at     timestamptz,
			expires_at       timestamptz,
			created_at       timestamptz DEFAULT NOW(),
			updated_at       timestamptz DEFAULT NOW(),
			CONSTRAINT uq_workspace_secrets UNIQUE (bucket_id, service_id, key_name)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_workspace_secrets_lookup
		ON fused_workspace_secrets(bucket_id, service_id);`,

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
		// auth_type, and every app execution in the workspace shares it
		// regardless of which bucket the app selected. Baseline and
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
			timeout_ms              integer,
			pagination              jsonb,
			event_extraction_path   text,
			incoming_webhook_config jsonb,
			-- base_url is this workspace's own local override/workaround for a
			-- wrong or missing spec-derived base_url -- takes effect here
			-- immediately on apply regardless of publish, same as every other
			-- column on this table (see LocalObjectCache.applyExecutionPolicyOverride).
			base_url                text,
			server_variables        jsonb,
			created_at timestamptz NOT NULL DEFAULT NOW(),
			updated_at timestamptz NOT NULL DEFAULT NOW(),
			CHECK (timeout_ms IS NULL OR timeout_ms BETWEEN 1 AND 86400000)
		);`,
		// Exactly one service-default override row per service.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_fused_workspace_execution_policies_service_default
		ON fused_workspace_execution_policies(service_id) WHERE service_version_id IS NULL;`,
		// Exactly one version-override row per version.
		`CREATE UNIQUE INDEX IF NOT EXISTS idx_fused_workspace_execution_policies_version_override
		ON fused_workspace_execution_policies(service_version_id) WHERE service_version_id IS NOT NULL;`,

		// JetStream coordinates live provider limits. This table is its eventual
		// projection for recovery and audit queries; the complete scope key keeps
		// connection-scoped policies separated without joining on execution data.
		`CREATE TABLE IF NOT EXISTS fused_provider_rate_limit_states (
			account_id                uuid NOT NULL,
			service_version_id        uuid NOT NULL,
			policy_name               text NOT NULL,
			scope_kind                text NOT NULL,
			scope_id                  uuid NOT NULL,
			config_hash               text NOT NULL,
			algorithm                 text NOT NULL,
			fixed_window_started_at   timestamptz,
			fixed_window_used         bigint NOT NULL DEFAULT 0,
			tokens                     bigint,
			token_refilled_at          timestamptz,
			rolling_usage              jsonb NOT NULL DEFAULT '[]'::jsonb,
			concurrency_used            bigint NOT NULL DEFAULT 0,
			concurrency_holders         jsonb NOT NULL DEFAULT '{}'::jsonb,
			cooldown_until             timestamptz,
			state_sequence             bigint NOT NULL DEFAULT 0,
			updated_at                 timestamptz NOT NULL DEFAULT NOW(),
			PRIMARY KEY (account_id, service_version_id, policy_name, scope_kind, scope_id),
			CHECK (length(scope_kind) BETWEEN 1 AND 256),
			CHECK (algorithm IN ('fixed_window', 'rolling_window', 'token_bucket', 'concurrency')),
			CHECK (fixed_window_used >= 0),
			CHECK (tokens IS NULL OR tokens >= 0)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_provider_rate_limit_states_updated
		ON fused_provider_rate_limit_states(updated_at);`,

		// --- App-family tables ---

		// App families are the stable authorization and configuration boundary
		// for SDK and MCP applications. One family represents one logical SDK
		// or MCP across all its versions. Tokens, ownership, team access, and
		// credential-bucket configuration belong to the family, not to
		// individual versions.
		`CREATE TABLE IF NOT EXISTS fused_app_families (
			app_family_id       uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			account_id          uuid NOT NULL,
			kind                text NOT NULL CHECK (kind IN ('sdk', 'mcp')),
			canonical_name      text NOT NULL,
			display_name        text NOT NULL,
			target_language     text,
			owner_subject_id    uuid REFERENCES fused_subjects(id) ON DELETE RESTRICT,
			owner_team_id       uuid REFERENCES fused_teams(id) ON DELETE RESTRICT,
			created_at          timestamptz NOT NULL DEFAULT NOW(),
			updated_at          timestamptz NOT NULL DEFAULT NOW(),
			CONSTRAINT chk_fused_app_families_owner CHECK (
				(owner_subject_id IS NOT NULL)::int +
				(owner_team_id IS NOT NULL)::int = 1
			),
			CONSTRAINT chk_fused_app_families_language CHECK (
				(kind = 'sdk' AND target_language IS NOT NULL)
				OR (kind = 'mcp' AND target_language IS NULL)
			),
			UNIQUE (account_id, kind, canonical_name),
			UNIQUE (app_family_id, account_id)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_app_families_account_kind
			ON fused_app_families(account_id, kind, created_at DESC);`,

		// Each app is one immutable version and its exact execution scope.
		// Kind and target_language are read through the family to avoid
		// duplicating them on every version row.
		`CREATE TABLE IF NOT EXISTS fused_apps (
			app_id                 uuid PRIMARY KEY,
			app_family_id          uuid NOT NULL,
			account_id             uuid NOT NULL,
			version                text NOT NULL,
			config_key             text NOT NULL,
			source_hash            text NOT NULL DEFAULT '',
			capability_hash        text NOT NULL DEFAULT '',
			scope_schema_version   integer NOT NULL DEFAULT 1,
			selections             jsonb NOT NULL DEFAULT '[]'::jsonb,
			unified_definition_schema_version integer NOT NULL DEFAULT 3,
			unified_definitions    jsonb NOT NULL DEFAULT '[]'::jsonb,
			unified_definition_hash text NOT NULL DEFAULT '` + unifiedEmptySetHash + `',
			unified_codegen_descriptor_hash text NOT NULL DEFAULT '` + unifiedEmptySetHash + `',
			generator_version      text,
			status                 text NOT NULL
			                       CHECK (status IN ('building', 'active', 'deprecated')),
			deprecation_message    text,
			deprecated_at          timestamptz,
			planned_deactivation_at timestamptz,
			created_by             uuid REFERENCES fused_subjects(id) ON DELETE RESTRICT,
			created_at             timestamptz NOT NULL DEFAULT NOW(),
			activated_at           timestamptz,
			UNIQUE (app_family_id, version),
			UNIQUE (account_id, config_key),
			FOREIGN KEY (app_family_id, account_id)
				REFERENCES fused_app_families(app_family_id, account_id)
				ON DELETE RESTRICT,
			CONSTRAINT chk_fused_apps_unified_definition_shape CHECK (
				unified_definition_schema_version = 3
				AND jsonb_typeof(unified_definitions) = 'array'
				AND octet_length(unified_definitions::text) <= 1048576
			),
			CONSTRAINT chk_fused_apps_unified_hashes CHECK (
				unified_definition_hash ~ '^sha256:[0-9a-f]{64}$'
				AND unified_codegen_descriptor_hash ~ '^sha256:[0-9a-f]{64}$'
			)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_apps_family_status
			ON fused_apps(app_family_id, status, created_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_apps_account_status
			ON fused_apps(account_id, status, created_at DESC);`,
		// Package retention walks only runnable SDK versions by app_id. This
		// partial index keeps each keyset page bounded as version history grows.
		`CREATE INDEX IF NOT EXISTS idx_fused_apps_package_lease
			ON fused_apps(app_id, app_family_id)
			WHERE status IN ('active', 'deprecated');`,
		`CREATE TABLE IF NOT EXISTS fused_app_capabilities (
			app_id          uuid NOT NULL REFERENCES fused_apps(app_id) ON DELETE CASCADE,
			capability_key  text NOT NULL CHECK (capability_key <> ''),
			PRIMARY KEY (app_id, capability_key)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_app_capabilities_key
			ON fused_app_capabilities(capability_key, app_id);`,

		// Tombstones prevent a deactivated (family, version) pair from being
		// reused. They contain only identity fields — no selections,
		// credentials, or runtime configuration.
		`CREATE TABLE IF NOT EXISTS fused_app_tombstones (
			app_id             uuid PRIMARY KEY,
			app_family_id      uuid NOT NULL,
			account_id         uuid NOT NULL,
			version            text NOT NULL,
			source_hash        text NOT NULL,
			deactivated_by     uuid REFERENCES fused_subjects(id) ON DELETE RESTRICT,
			deactivated_at     timestamptz NOT NULL DEFAULT NOW(),
			UNIQUE (app_family_id, version),
			FOREIGN KEY (app_family_id, account_id)
				REFERENCES fused_app_families(app_family_id, account_id)
				ON DELETE RESTRICT
		);`,

		// All versions in a family resolve through the same connection
		// profile/credential bucket. Reconfiguration changes the mapping
		// once without cloning version scopes.
		`CREATE TABLE IF NOT EXISTS fused_app_family_buckets (
			app_family_id uuid PRIMARY KEY
			              REFERENCES fused_app_families(app_family_id)
			              ON DELETE CASCADE,
			bucket_id     uuid NOT NULL REFERENCES fused_buckets(id) ON DELETE RESTRICT,
			created_at    timestamptz NOT NULL DEFAULT NOW(),
			updated_at    timestamptz NOT NULL DEFAULT NOW()
		);`,

		// Token history is credential-free and outlives the active hash. Activity
		// can therefore explain who issued an expired token without retaining a
		// credential that could ever become executable again.
		`CREATE TABLE IF NOT EXISTS fused_app_token_history (
			id                    uuid PRIMARY KEY,
			app_family_id         uuid NOT NULL
			                      REFERENCES fused_app_families(app_family_id)
			                      ON DELETE CASCADE,
			name                  text NOT NULL,
			allow_all             boolean NOT NULL DEFAULT true,
			allowed_operations    text[] NOT NULL DEFAULT '{}',
			binding_mode          text NOT NULL DEFAULT 'dynamic',
			expires_at            timestamptz,
			issued_by_subject_id  uuid,
			issued_by_credential_id uuid,
			status                text NOT NULL DEFAULT 'active',
			terminated_at         timestamptz,
			termination_reason     text,
			terminated_by_subject_id uuid,
			terminated_by_credential_id uuid,
			created_at            timestamptz NOT NULL DEFAULT NOW(),
			CONSTRAINT chk_fused_app_token_history_allow CHECK (
				(allow_all AND cardinality(allowed_operations) = 0)
				OR (NOT allow_all AND cardinality(allowed_operations) > 0 AND NOT ('*' = ANY(allowed_operations)))
			),
			CONSTRAINT chk_fused_app_token_history_binding_mode
				CHECK (binding_mode IN ('dynamic', 'fixed')),
			CONSTRAINT chk_fused_app_token_history_status
				CHECK (status IN ('active', 'expired', 'revoked')),
			CONSTRAINT chk_fused_app_token_history_terminal CHECK (
				(status = 'active' AND terminated_at IS NULL)
				OR (status <> 'active' AND terminated_at IS NOT NULL)
			),
			CONSTRAINT chk_fused_app_token_history_reason CHECK (
				(status = 'active' AND termination_reason IS NULL)
				OR (status = 'expired' AND termination_reason = 'expired')
				OR (status = 'revoked' AND termination_reason = 'revoked')
			)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_app_token_history_family
			ON fused_app_token_history(app_family_id, created_at DESC);`,

		// Family-level execution tokens authorize all active and deprecated
		// versions without duplicating credentials for each immutable app. The
		// active row is disposable: plaintext is one-time output and only the
		// hash remains until expiry or revocation removes it.
		`CREATE TABLE IF NOT EXISTS fused_app_tokens (
			id              uuid PRIMARY KEY,
			app_family_id   uuid NOT NULL
			                REFERENCES fused_app_families(app_family_id)
			                ON DELETE CASCADE,
			token_hash      text NOT NULL UNIQUE,
			name            text NOT NULL,
			allow_all       boolean NOT NULL DEFAULT true,
			allowed_operations text[] NOT NULL DEFAULT '{}',
			binding_mode    text NOT NULL DEFAULT 'dynamic',
			expires_at      timestamptz,
			last_used_at    timestamptz,
			created_at      timestamptz NOT NULL DEFAULT NOW(),
			CONSTRAINT chk_fused_app_tokens_allow CHECK (
				(allow_all AND cardinality(allowed_operations) = 0)
				OR (NOT allow_all AND cardinality(allowed_operations) > 0 AND NOT ('*' = ANY(allowed_operations)))
			),
			CONSTRAINT chk_fused_app_tokens_binding_mode
				CHECK (binding_mode IN ('dynamic', 'fixed')),
			CONSTRAINT fk_fused_app_tokens_history
				FOREIGN KEY (id) REFERENCES fused_app_token_history(id) ON DELETE RESTRICT,
			UNIQUE (app_family_id, name)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_app_tokens_expiry
			ON fused_app_tokens(expires_at, id) WHERE expires_at IS NOT NULL;`,
		// Fixed bindings resolve user-authored selectors once at issue time. The
		// runtime receives only Engine-owned connection identities and deletion
		// of the active token removes the complete executable binding set.
		`CREATE TABLE IF NOT EXISTS fused_app_token_bindings (
			token_id           uuid NOT NULL REFERENCES fused_app_tokens(id) ON DELETE CASCADE,
			service_id         uuid NOT NULL,
			auth_name          text NOT NULL DEFAULT '',
			auth_connection_id uuid NOT NULL REFERENCES fused_auth_connections(id) ON DELETE CASCADE,
			resource_id        uuid REFERENCES fused_connection_resources(id) ON DELETE CASCADE,
			created_at         timestamptz NOT NULL DEFAULT NOW(),
			PRIMARY KEY (token_id, service_id, auth_name)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_app_token_bindings_connection
			ON fused_app_token_bindings(auth_connection_id);`,
		// Idempotency rows bind directly to one immutable app version and never preserve removed artifact identity.
		`CREATE TABLE IF NOT EXISTS fused_engine_idempotency_keys (
			id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
			app_id uuid NOT NULL REFERENCES fused_apps(app_id) ON DELETE CASCADE,
			idempotency_key_hash text NOT NULL,
			request_body_hash text,
			environment text,
			response_body bytea,
			response_status integer NOT NULL DEFAULT 200,
			response_media_family text NOT NULL DEFAULT 'unknown',
			created_at timestamptz NOT NULL DEFAULT NOW(),
			expires_at timestamptz NOT NULL,
			CONSTRAINT chk_fused_engine_idempotency_response_media_family CHECK (response_media_family IN ('sse', 'json', 'binary', 'xml', 'text', 'other', 'unknown')),
			UNIQUE(app_id, idempotency_key_hash)
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_engine_idempotency_keys_expires
			ON fused_engine_idempotency_keys(expires_at);`,
		// MCP sessions are declared after app/token identity so both foreign keys are direct clean-schema constraints.
		`CREATE TABLE IF NOT EXISTS fused_mcp_sessions (
			id uuid PRIMARY KEY,
			app_id uuid NOT NULL REFERENCES fused_apps(app_id) ON DELETE CASCADE,
			app_token_id uuid REFERENCES fused_app_token_history(id) ON DELETE SET NULL,
			session_id text,
			protocol_version text NOT NULL DEFAULT '2024-11-05',
			started_at timestamp with time zone DEFAULT NOW(),
			ended_at timestamp with time zone,
			last_activity_at timestamp with time zone DEFAULT NOW(),
			end_reason text,
			client_name text NOT NULL DEFAULT '' CHECK (octet_length(client_name) <= 128),
			client_version text NOT NULL DEFAULT '' CHECK (octet_length(client_version) <= 128),
			initial_client_ip inet,
			` + mcpSessionEndReasonConstraint + `
		);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_mcp_sessions_app_started
			ON fused_mcp_sessions(app_id, started_at DESC);`,
		`CREATE INDEX IF NOT EXISTS idx_fused_mcp_sessions_token_started
			ON fused_mcp_sessions(app_token_id, started_at DESC) WHERE app_token_id IS NOT NULL;`,
		`CREATE INDEX IF NOT EXISTS idx_fused_mcp_sessions_cursor
			ON fused_mcp_sessions(app_id, started_at DESC, id DESC);`,
	}
	return append(queries, unifiedSchemaConvergenceQueries()...)
}

// unifiedSchemaConvergenceQueries makes v3 the only writable Unified shape.
// Earlier immutable rows remain inert history until their SDKs are reapplied;
// they are never decoded, relabeled, or rewritten as the current contract.
func unifiedSchemaConvergenceQueries() []string {
	return []string{
		`ALTER TABLE fused_apps ADD COLUMN IF NOT EXISTS unified_definition_schema_version integer NOT NULL DEFAULT 3;`,
		`ALTER TABLE fused_apps ADD COLUMN IF NOT EXISTS unified_definitions jsonb NOT NULL DEFAULT '[]'::jsonb;`,
		`ALTER TABLE fused_apps ADD COLUMN IF NOT EXISTS unified_definition_hash text NOT NULL DEFAULT '` + unifiedEmptySetHash + `';`,
		`ALTER TABLE fused_apps ADD COLUMN IF NOT EXISTS unified_codegen_descriptor_hash text NOT NULL DEFAULT '` + unifiedEmptySetHash + `';`,
		`ALTER TABLE fused_apps ALTER COLUMN unified_definition_schema_version SET DEFAULT 3;`,
		`ALTER TABLE fused_apps ALTER COLUMN unified_definitions SET DEFAULT '[]'::jsonb;`,
		`ALTER TABLE fused_apps ALTER COLUMN unified_definition_hash SET DEFAULT '` + unifiedEmptySetHash + `';`,
		`ALTER TABLE fused_apps ALTER COLUMN unified_codegen_descriptor_hash SET DEFAULT '` + unifiedEmptySetHash + `';`,
		`ALTER TABLE fused_apps ALTER COLUMN unified_definition_schema_version SET NOT NULL;`,
		`ALTER TABLE fused_apps ALTER COLUMN unified_definitions SET NOT NULL;`,
		`ALTER TABLE fused_apps ALTER COLUMN unified_definition_hash SET NOT NULL;`,
		`ALTER TABLE fused_apps ALTER COLUMN unified_codegen_descriptor_hash SET NOT NULL;`,
		`DO $$
		BEGIN
			-- The lock serializes constraint replacement across concurrent Engine startups.
			LOCK TABLE fused_apps IN SHARE ROW EXCLUSIVE MODE;
			-- Replace any earlier definition-shape constraint with the single v3 write contract.
			IF EXISTS (
				SELECT 1
				FROM pg_constraint
				WHERE conrelid = 'fused_apps'::regclass
				  AND conname = 'chk_fused_apps_unified_definition_shape'
				  AND pg_get_constraintdef(oid) NOT LIKE '%unified_definition_schema_version = 3%'
			) THEN
				ALTER TABLE fused_apps DROP CONSTRAINT chk_fused_apps_unified_definition_shape;
			END IF;
			-- NOT VALID preserves immutable history while enforcing v3 on every new write.
			IF NOT EXISTS (
				SELECT 1
				FROM pg_constraint
				WHERE conrelid = 'fused_apps'::regclass
				  AND conname = 'chk_fused_apps_unified_definition_shape'
			) THEN
				ALTER TABLE fused_apps ADD CONSTRAINT chk_fused_apps_unified_definition_shape CHECK (
					unified_definition_schema_version = 3
					AND jsonb_typeof(unified_definitions) = 'array'
					AND octet_length(unified_definitions::text) <= 1048576
				) NOT VALID;
			END IF;
			-- A clean or pre-Unified database can validate immediately; older immutable
			-- rows keep the constraint unvalidated until their app versions are removed.
			IF NOT EXISTS (
				SELECT 1 FROM fused_apps
				WHERE unified_definition_schema_version <> 3
			) THEN
				ALTER TABLE fused_apps VALIDATE CONSTRAINT chk_fused_apps_unified_definition_shape;
			END IF;
			-- Existing hash constraints stay untouched to avoid repeated table validation.
			IF NOT EXISTS (
				SELECT 1
				FROM pg_constraint
				WHERE conrelid = 'fused_apps'::regclass
				  AND conname = 'chk_fused_apps_unified_hashes'
			) THEN
				ALTER TABLE fused_apps ADD CONSTRAINT chk_fused_apps_unified_hashes CHECK (
					unified_definition_hash ~ '^sha256:[0-9a-f]{64}$'
					AND unified_codegen_descriptor_hash ~ '^sha256:[0-9a-f]{64}$'
				);
			END IF;
		END
		$$;`,
	}
}
