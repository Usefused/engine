package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
)

func (s *postgresStore) AcquireProviderRateLimit(ctx context.Context, request ratelimitpolicy.AcquireRequest) (ratelimitpolicy.Decision, error) {
	if request.AccountID == uuid.Nil || request.ServiceVersionID == uuid.Nil || len(request.Policies) == 0 {
		return ratelimitpolicy.Decision{}, errors.New("provider rate-limit acquisition identity is incomplete")
	}
	payload, err := json.Marshal(request.Policies)
	if err != nil {
		return ratelimitpolicy.Decision{}, fmt.Errorf("marshal provider rate-limit policies: %w", err)
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return ratelimitpolicy.Decision{}, fmt.Errorf("begin provider rate-limit acquisition: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	// PostgreSQL does not reliably update a row twice through separate CTEs in
	// one statement. Initializing/configuring first and consuming second inside
	// the same transaction keeps the decision atomic across Engine instances.
	if _, err := tx.Exec(ctx, initializeProviderRateLimitSQL, request.AccountID, request.ServiceVersionID, payload); err != nil {
		return ratelimitpolicy.Decision{}, fmt.Errorf("initialize provider rate limit: %w", err)
	}
	var allowed bool
	var retryAfterMS int64
	err = tx.QueryRow(ctx, acquireProviderRateLimitSQL, request.AccountID, request.ServiceVersionID, payload).Scan(&allowed, &retryAfterMS)
	if err != nil {
		return ratelimitpolicy.Decision{}, fmt.Errorf("acquire provider rate limit: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ratelimitpolicy.Decision{}, fmt.Errorf("commit provider rate-limit acquisition: %w", err)
	}
	return ratelimitpolicy.Decision{Allowed: allowed, RetryAfter: time.Duration(retryAfterMS) * time.Millisecond}, nil
}

func (s *postgresStore) SyncProviderRateLimit(ctx context.Context, request ratelimitpolicy.SyncRequest) error {
	if request.AccountID == uuid.Nil || request.ServiceVersionID == uuid.Nil || len(request.Observations) == 0 {
		return errors.New("provider rate-limit synchronization identity is incomplete")
	}
	payload, err := json.Marshal(request.Observations)
	if err != nil {
		return fmt.Errorf("marshal provider rate-limit observations: %w", err)
	}
	_, err = s.db.Exec(ctx, syncProviderRateLimitSQL, request.AccountID, request.ServiceVersionID, payload, request.CooldownUntil)
	if err != nil {
		return fmt.Errorf("sync provider rate limit: %w", err)
	}
	return nil
}

const initializeProviderRateLimitSQL = `
WITH input AS MATERIALIZED (
	SELECT *
	FROM jsonb_to_recordset($3::jsonb) AS policy(
		name text, unit text, scope_kind text, scope_id text, cost bigint,
		algorithm text, config_hash text, "limit" bigint, duration_ms bigint,
		capacity bigint, refill_units bigint, refill_interval_ms bigint
	)
)
INSERT INTO fused_provider_rate_limit_states AS current (
		account_id, service_version_id, policy_name, scope_kind, scope_id,
		config_hash, algorithm, fixed_window_started_at, fixed_window_used,
		tokens, token_refilled_at, updated_at
	)
	SELECT $1, $2, name, scope_kind, scope_id::uuid, config_hash, algorithm,
		CASE WHEN algorithm = 'fixed_window' THEN statement_timestamp() END,
		0,
		CASE WHEN algorithm = 'token_bucket' THEN capacity END,
		CASE WHEN algorithm = 'token_bucket' THEN statement_timestamp() END,
		statement_timestamp()
	FROM input
	ORDER BY name, scope_kind, scope_id
	ON CONFLICT (account_id, service_version_id, policy_name, scope_kind, scope_id)
	DO UPDATE SET
		config_hash = EXCLUDED.config_hash,
		algorithm = EXCLUDED.algorithm,
		fixed_window_started_at = CASE
			WHEN EXCLUDED.algorithm <> 'fixed_window' THEN NULL
			WHEN current.algorithm <> 'fixed_window' OR current.config_hash <> EXCLUDED.config_hash THEN statement_timestamp()
			ELSE current.fixed_window_started_at
		END,
		fixed_window_used = CASE
			WHEN EXCLUDED.algorithm <> 'fixed_window' THEN 0
			WHEN current.algorithm <> 'fixed_window' OR current.config_hash <> EXCLUDED.config_hash THEN 0
			ELSE LEAST(current.fixed_window_used, (SELECT "limit" FROM input WHERE name = EXCLUDED.policy_name))
		END,
		tokens = CASE
			WHEN EXCLUDED.algorithm <> 'token_bucket' THEN NULL
			WHEN current.algorithm <> 'token_bucket' OR current.config_hash <> EXCLUDED.config_hash THEN EXCLUDED.tokens
			ELSE LEAST(current.tokens, EXCLUDED.tokens)
		END,
		token_refilled_at = CASE
			WHEN EXCLUDED.algorithm <> 'token_bucket' THEN NULL
			WHEN current.algorithm <> 'token_bucket' OR current.config_hash <> EXCLUDED.config_hash THEN statement_timestamp()
			ELSE current.token_refilled_at
		END,
		updated_at = statement_timestamp()
	`

const acquireProviderRateLimitSQL = `
WITH input AS MATERIALIZED (
	SELECT *
	FROM jsonb_to_recordset($3::jsonb) AS policy(
		name text, unit text, scope_kind text, scope_id text, cost bigint,
		algorithm text, config_hash text, "limit" bigint, duration_ms bigint,
		capacity bigint, refill_units bigint, refill_interval_ms bigint
	)
), locked AS MATERIALIZED (
	SELECT state.*, input.cost, input."limit", input.duration_ms,
		input.capacity, input.refill_units, input.refill_interval_ms
	FROM fused_provider_rate_limit_states state
	JOIN input ON input.name = state.policy_name
		AND input.scope_kind = state.scope_kind
		AND input.scope_id::uuid = state.scope_id
	WHERE state.account_id = $1 AND state.service_version_id = $2
	ORDER BY state.policy_name, state.scope_kind, state.scope_id
	FOR UPDATE OF state
), refills AS MATERIALIZED (
	SELECT locked.*,
		CASE WHEN algorithm = 'token_bucket' THEN GREATEST(0, FLOOR(
			EXTRACT(EPOCH FROM (statement_timestamp() - token_refilled_at)) * 1000 / refill_interval_ms
		)::bigint) ELSE 0 END AS refill_steps
	FROM locked
), ready AS MATERIALIZED (
	SELECT refills.*,
		CASE WHEN algorithm = 'fixed_window'
			AND statement_timestamp() >= fixed_window_started_at + make_interval(secs => duration_ms::double precision / 1000)
			THEN statement_timestamp() ELSE fixed_window_started_at END AS ready_window_started_at,
		CASE WHEN algorithm = 'fixed_window'
			AND statement_timestamp() >= fixed_window_started_at + make_interval(secs => duration_ms::double precision / 1000)
			THEN 0 ELSE fixed_window_used END AS ready_window_used,
		CASE WHEN algorithm = 'token_bucket' THEN LEAST(
			capacity::numeric,
			tokens::numeric + refill_steps::numeric * refill_units::numeric
		)::bigint ELSE NULL END AS ready_tokens,
		CASE WHEN algorithm = 'token_bucket' THEN
			token_refilled_at + make_interval(secs => (refill_steps * refill_interval_ms)::double precision / 1000)
		ELSE NULL END AS ready_refilled_at
	FROM refills
), assessed AS MATERIALIZED (
	SELECT ready.*,
		(cooldown_until IS NULL OR cooldown_until <= statement_timestamp()) AND
		CASE algorithm
			WHEN 'fixed_window' THEN cost <= "limit" - ready_window_used
			WHEN 'token_bucket' THEN cost <= ready_tokens
		END AS row_allowed,
		CASE
			WHEN cooldown_until > statement_timestamp() THEN cooldown_until
			WHEN algorithm = 'fixed_window' AND cost <= "limit" THEN
				ready_window_started_at + make_interval(secs => duration_ms::double precision / 1000)
			WHEN algorithm = 'token_bucket' AND cost <= capacity THEN
				ready_refilled_at + make_interval(secs => (
					CEIL(GREATEST(0, cost - ready_tokens)::numeric / refill_units) * refill_interval_ms
				)::double precision / 1000)
		END AS row_retry_at
	FROM ready
), decision AS MATERIALIZED (
	SELECT BOOL_AND(row_allowed) AS allowed,
		CASE
			WHEN BOOL_AND(row_allowed) THEN 0
			WHEN BOOL_OR(row_retry_at IS NULL) FILTER (WHERE NOT row_allowed) THEN 0
			ELSE GREATEST(0, CEIL(EXTRACT(EPOCH FROM (MAX(row_retry_at) FILTER (WHERE NOT row_allowed) - statement_timestamp())) * 1000))::bigint
		END AS retry_after_ms
	FROM assessed
), consumed AS (
	UPDATE fused_provider_rate_limit_states state
	SET fixed_window_started_at = CASE WHEN assessed.algorithm = 'fixed_window' THEN assessed.ready_window_started_at END,
		fixed_window_used = CASE WHEN assessed.algorithm = 'fixed_window' THEN assessed.ready_window_used + assessed.cost ELSE 0 END,
		tokens = CASE WHEN assessed.algorithm = 'token_bucket' THEN assessed.ready_tokens - assessed.cost END,
		token_refilled_at = CASE WHEN assessed.algorithm = 'token_bucket' THEN assessed.ready_refilled_at END,
		updated_at = statement_timestamp()
	FROM assessed, decision
	WHERE decision.allowed
		AND state.account_id = assessed.account_id
		AND state.service_version_id = assessed.service_version_id
		AND state.policy_name = assessed.policy_name
		AND state.scope_kind = assessed.scope_kind
		AND state.scope_id = assessed.scope_id
	RETURNING state.policy_name
)
SELECT decision.allowed, decision.retry_after_ms
FROM decision
CROSS JOIN (SELECT COUNT(*) FROM consumed) consumption
`

const syncProviderRateLimitSQL = `
WITH input AS MATERIALIZED (
	SELECT *
	FROM jsonb_to_recordset($3::jsonb) AS observation(
		policy_name text, scope_kind text, scope_id uuid, algorithm text,
		local_limit bigint, duration_ms bigint, "limit" bigint,
		remaining bigint, reset_at timestamptz
	)
), locked AS MATERIALIZED (
	SELECT state.account_id, state.service_version_id, state.policy_name,
		state.scope_kind, state.scope_id
	FROM fused_provider_rate_limit_states state
	JOIN input ON input.policy_name = state.policy_name
		AND input.scope_kind = state.scope_kind AND input.scope_id = state.scope_id
	WHERE state.account_id = $1 AND state.service_version_id = $2
	ORDER BY state.policy_name, state.scope_kind, state.scope_id
	FOR UPDATE OF state
)
UPDATE fused_provider_rate_limit_states state
SET fixed_window_started_at = CASE
		WHEN input.algorithm = 'fixed_window' AND input.reset_at IS NOT NULL THEN GREATEST(
			state.fixed_window_started_at,
			input.reset_at - make_interval(secs => input.duration_ms::double precision / 1000)
		)
		ELSE state.fixed_window_started_at
	END,
	fixed_window_used = CASE
		WHEN input.algorithm = 'fixed_window' AND input.remaining IS NOT NULL THEN GREATEST(
			state.fixed_window_used,
			input.local_limit - LEAST(input.local_limit, input.remaining, COALESCE(input."limit", input.local_limit))
		)
		ELSE state.fixed_window_used
	END,
	tokens = CASE
		WHEN input.algorithm = 'token_bucket' AND input.remaining IS NOT NULL THEN LEAST(
			state.tokens, input.local_limit, input.remaining, COALESCE(input."limit", input.local_limit)
		)
		ELSE state.tokens
	END,
	cooldown_until = GREATEST(state.cooldown_until, $4::timestamptz),
	updated_at = statement_timestamp()
FROM input, locked
WHERE state.account_id = locked.account_id
	AND state.service_version_id = locked.service_version_id
	AND state.policy_name = locked.policy_name
	AND state.scope_kind = locked.scope_kind
	AND state.scope_id = locked.scope_id
	AND input.policy_name = locked.policy_name
	AND input.scope_kind = locked.scope_kind
	AND input.scope_id = locked.scope_id
`
