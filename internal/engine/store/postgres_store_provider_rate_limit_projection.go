package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// BatchUpsertProviderRateLimitStates projects a JetStream batch in one SQL
// statement. Sequence and timestamp guards make delayed or redelivered KV
// revisions idempotent without a read-before-write query.
func (s *postgresStore) BatchUpsertProviderRateLimitStates(ctx context.Context, states []ratelimitpolicy.StateEnvelope) error {
	if len(states) == 0 {
		return nil
	}
	payload, err := json.Marshal(states)
	if err != nil {
		return fmt.Errorf("encode provider rate-limit projection: %w", err)
	}
	if _, err := s.db.Exec(ctx, upsertProviderRateLimitProjectionSQL, payload); err != nil {
		return fmt.Errorf("persist provider rate-limit projection: %w", err)
	}
	return nil
}

// LoadProviderRateLimitState is a cold recovery lookup used only when the
// authoritative JetStream key is absent. Filtering remains in SQL and all
// matching policy rows are loaded in one query.
func (s *postgresStore) LoadProviderRateLimitState(ctx context.Context, request ratelimitpolicy.AcquireRequest) (*ratelimitpolicy.StateEnvelope, error) {
	if request.AccountID == uuid.Nil || request.ServiceVersionID == uuid.Nil || len(request.Policies) == 0 {
		return nil, errors.New("provider rate-limit recovery identity is incomplete")
	}
	payload, err := json.Marshal(request.Policies)
	if err != nil {
		return nil, fmt.Errorf("encode provider rate-limit recovery policies: %w", err)
	}
	rows, err := s.db.Query(ctx, loadProviderRateLimitProjectionSQL, request.AccountID, request.ServiceVersionID, payload)
	if err != nil {
		return nil, fmt.Errorf("load provider rate-limit projection: %w", err)
	}
	defer rows.Close()
	state, err := scanProviderRateLimitProjection(rows, request)
	if err != nil {
		return nil, err
	}
	if len(state.Policies) != len(request.Policies) {
		return nil, nil
	}
	return &state, nil
}

func scanProviderRateLimitProjection(rows pgx.Rows, request ratelimitpolicy.AcquireRequest) (ratelimitpolicy.StateEnvelope, error) {
	state := ratelimitpolicy.StateEnvelope{
		SchemaVersion: ratelimitpolicy.ProviderRateLimitStateSchemaVersion,
		AccountID:     request.AccountID, ServiceVersionID: request.ServiceVersionID,
	}
	for rows.Next() {
		policy, cooldown, sequence, updatedAt, err := scanProviderRateLimitProjectionRow(rows)
		if err != nil {
			return state, err
		}
		state.Policies = append(state.Policies, policy)
		state.CooldownUntil = latestTimestamp(state.CooldownUntil, cooldown)
		if sequence > state.Sequence {
			state.Sequence = sequence
		}
		if updatedAt.After(state.UpdatedAt) {
			state.UpdatedAt = updatedAt
		}
	}
	if err := rows.Err(); err != nil {
		return state, fmt.Errorf("iterate provider rate-limit projection: %w", err)
	}
	return state, nil
}

func scanProviderRateLimitProjectionRow(row pgx.Row) (ratelimitpolicy.PolicyState, *time.Time, uint64, time.Time, error) {
	var state ratelimitpolicy.PolicyState
	var cooldown *time.Time
	var sequence int64
	var updatedAt time.Time
	err := row.Scan(
		&state.Name, &state.ScopeKind, &state.ScopeID, &state.ConfigHash, &state.Algorithm,
		&state.FixedWindowStartedAt, &state.FixedWindowUsed, &state.Tokens,
		&state.TokenRefilledAt, &state.RollingUsage, &state.ConcurrencyUsed,
		&state.ConcurrencyHolders, &cooldown, &sequence, &updatedAt,
	)
	if err != nil {
		return state, nil, 0, time.Time{}, fmt.Errorf("scan provider rate-limit projection: %w", err)
	}
	if sequence < 0 {
		return state, nil, 0, time.Time{}, errors.New("provider rate-limit projection sequence is invalid")
	}
	return state, cooldown, uint64(sequence), updatedAt, nil
}

func latestTimestamp(current, candidate *time.Time) *time.Time {
	if candidate == nil || (current != nil && !candidate.After(*current)) {
		return current
	}
	copy := *candidate
	return &copy
}

const upsertProviderRateLimitProjectionSQL = `
WITH envelopes AS MATERIALIZED (
	SELECT *
	FROM jsonb_to_recordset($1::jsonb) AS envelope(
		account_id uuid, service_version_id uuid, policies jsonb,
		cooldown_until timestamptz, sequence bigint, updated_at timestamptz
	)
), expanded AS MATERIALIZED (
	SELECT envelope.account_id, envelope.service_version_id,
		policy.name AS policy_name, policy.scope_kind, policy.scope_id,
		policy.config_hash, policy.algorithm,
		policy.fixed_window_started_at, policy.fixed_window_used,
		policy.tokens, policy.token_refilled_at,
		-- Empty algorithm state is omitted by JSON but remains an explicit,
		-- non-null collection in the relational recovery contract.
		COALESCE(policy.rolling_usage, '[]'::jsonb) AS rolling_usage,
		policy.concurrency_used,
		COALESCE(policy.concurrency_holders, '{}'::jsonb) AS concurrency_holders,
		envelope.cooldown_until, envelope.sequence AS state_sequence, envelope.updated_at
	FROM envelopes envelope
	CROSS JOIN LATERAL jsonb_to_recordset(envelope.policies) AS policy(
		name text, scope_kind text, scope_id uuid, config_hash text, algorithm text,
		fixed_window_started_at timestamptz, fixed_window_used bigint,
		tokens bigint, token_refilled_at timestamptz, rolling_usage jsonb,
		concurrency_used bigint, concurrency_holders jsonb
	)
), latest AS (
	SELECT DISTINCT ON (account_id, service_version_id, policy_name, scope_kind, scope_id) *
	FROM expanded
	ORDER BY account_id, service_version_id, policy_name, scope_kind, scope_id, state_sequence DESC, updated_at DESC
)
INSERT INTO fused_provider_rate_limit_states AS current (
	account_id, service_version_id, policy_name, scope_kind, scope_id,
	config_hash, algorithm, fixed_window_started_at, fixed_window_used,
	tokens, token_refilled_at, rolling_usage, concurrency_used,
	concurrency_holders, cooldown_until, state_sequence, updated_at
)
SELECT account_id, service_version_id, policy_name, scope_kind, scope_id,
	config_hash, algorithm, fixed_window_started_at, fixed_window_used,
	tokens, token_refilled_at, rolling_usage, concurrency_used,
	concurrency_holders, cooldown_until, state_sequence, updated_at
FROM latest
ON CONFLICT (account_id, service_version_id, policy_name, scope_kind, scope_id)
DO UPDATE SET
	config_hash = EXCLUDED.config_hash,
	algorithm = EXCLUDED.algorithm,
	fixed_window_started_at = EXCLUDED.fixed_window_started_at,
	fixed_window_used = EXCLUDED.fixed_window_used,
	tokens = EXCLUDED.tokens,
	token_refilled_at = EXCLUDED.token_refilled_at,
	rolling_usage = EXCLUDED.rolling_usage,
	concurrency_used = EXCLUDED.concurrency_used,
	concurrency_holders = EXCLUDED.concurrency_holders,
	cooldown_until = EXCLUDED.cooldown_until,
	state_sequence = EXCLUDED.state_sequence,
	updated_at = EXCLUDED.updated_at
WHERE EXCLUDED.state_sequence > current.state_sequence
	OR (EXCLUDED.state_sequence = current.state_sequence AND EXCLUDED.updated_at >= current.updated_at)
`

const loadProviderRateLimitProjectionSQL = `
WITH requested AS MATERIALIZED (
	SELECT *
	FROM jsonb_to_recordset($3::jsonb) AS policy(
		name text, scope_kind text, scope_id uuid
	)
)
SELECT state.policy_name, state.scope_kind, state.scope_id,
	state.config_hash, state.algorithm, state.fixed_window_started_at,
	state.fixed_window_used, state.tokens, state.token_refilled_at,
	state.rolling_usage, state.concurrency_used, state.concurrency_holders,
	state.cooldown_until, state.state_sequence, state.updated_at
FROM fused_provider_rate_limit_states state
JOIN requested ON requested.name = state.policy_name
	AND requested.scope_kind = state.scope_kind
	AND requested.scope_id = state.scope_id
WHERE state.account_id = $1 AND state.service_version_id = $2
ORDER BY state.policy_name, state.scope_kind, state.scope_id
`
