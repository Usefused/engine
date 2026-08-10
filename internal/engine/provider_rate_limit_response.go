package engine

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/shared/models"
	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
)

const maxRuntimeHeaderValue = int64(1_000_000_000_000)
const maxResponseResetHorizon = 2_678_400_000 * time.Millisecond

func (d *Dispatcher) syncProviderRateLimitResponse(ctx context.Context, srv *models.Service, obj *models.IntegrationObject, response *http.Response) error {
	if srv.RateLimit == nil || d.rateLimits == nil || response == nil {
		return nil
	}
	request, err := providerRateLimitRequest(ctx, srv, obj)
	if err != nil {
		return err
	}
	observations, outcome := providerHeaderObservations(srv.RateLimit, request.Policies, response.Header, time.Now())
	cooldown := providerCooldown(srv.RateLimit, response.StatusCode, response.Header.Get("Retry-After"), time.Now())
	err = d.rateLimits.SyncProviderRateLimit(ctx, ratelimitpolicy.SyncRequest{
		AccountID: request.AccountID, ServiceVersionID: request.ServiceVersionID,
		CooldownUntil: cooldown, Observations: observations,
	})
	recordProviderHeaderSync(ctx, outcome, err)
	return err
}

func providerHeaderObservations(config *ratelimitpolicy.Config, resolved []ratelimitpolicy.ResolvedPolicy, headers http.Header, now time.Time) ([]ratelimitpolicy.ResponseObservation, string) {
	byName := make(map[string]ratelimitpolicy.Policy, len(config.Policies))
	for _, policy := range config.Policies {
		byName[policy.Name] = policy
	}
	observations := make([]ratelimitpolicy.ResponseObservation, 0, len(resolved))
	outcome := "none"
	for _, policy := range resolved {
		observation := responseObservation(policy)
		configured := byName[policy.Name]
		if configured.ResponseHeaders != nil {
			parsed, parsedOutcome := parsePolicyHeaders(configured.ResponseHeaders, headers, now)
			observation.Limit, observation.Remaining, observation.ResetAt = parsed.Limit, parsed.Remaining, parsed.ResetAt
			outcome = combineHeaderOutcome(outcome, parsedOutcome)
		}
		observations = append(observations, observation)
	}
	return observations, outcome
}

func responseObservation(policy ratelimitpolicy.ResolvedPolicy) ratelimitpolicy.ResponseObservation {
	limit := policy.Limit
	if policy.Algorithm == "token_bucket" {
		limit = policy.Capacity
	}
	return ratelimitpolicy.ResponseObservation{
		PolicyName: policy.Name, ScopeKind: policy.ScopeKind, ScopeID: uuid.MustParse(policy.ScopeID),
		Algorithm: policy.Algorithm, LocalLimit: limit, DurationMS: policy.DurationMS,
	}
}

func parsePolicyHeaders(config *ratelimitpolicy.ResponseHeaders, headers http.Header, now time.Time) (ratelimitpolicy.ResponseObservation, string) {
	observation := ratelimitpolicy.ResponseObservation{}
	outcome := "none"
	var invalid bool
	observation.Limit, outcome, invalid = parseBoundedHeader(headers, config.Limit, outcome, invalid)
	observation.Remaining, outcome, invalid = parseBoundedHeader(headers, config.Remaining, outcome, invalid)
	if config.Reset != nil {
		value := headers.Get(config.Reset.Name)
		if value != "" {
			reset, err := parseResetHeader(value, config.Reset.Format, now)
			if err != nil {
				invalid = true
			} else {
				observation.ResetAt = &reset
				outcome = "applied"
			}
		}
	}
	if invalid {
		outcome = "invalid"
	}
	return observation, outcome
}

func parseBoundedHeader(headers http.Header, name, outcome string, invalid bool) (*int64, string, bool) {
	if name == "" || headers.Get(name) == "" {
		return nil, outcome, invalid
	}
	value, err := strconv.ParseInt(strings.TrimSpace(headers.Get(name)), 10, 64)
	if err != nil || value < 0 || value > maxRuntimeHeaderValue {
		return nil, outcome, true
	}
	return &value, "applied", invalid
}

func parseResetHeader(raw, format string, now time.Time) (time.Time, error) {
	var reset time.Time
	var err error
	switch format {
	case "delta_seconds":
		var seconds int64
		seconds, err = parseNonnegativeInt(raw)
		if err == nil && seconds <= int64(maxResponseResetHorizon/time.Second) {
			reset = now.Add(time.Duration(seconds) * time.Second)
		} else if err == nil {
			err = errors.New("reset header exceeds bounded horizon")
		}
	case "unix_seconds":
		var seconds int64
		seconds, err = parseNonnegativeInt(raw)
		reset = time.Unix(seconds, 0)
	case "unix_milliseconds":
		var milliseconds int64
		milliseconds, err = parseNonnegativeInt(raw)
		reset = time.UnixMilli(milliseconds)
	case "rfc3339":
		reset, err = time.Parse(time.RFC3339, strings.TrimSpace(raw))
	default:
		return time.Time{}, errors.New("unsupported reset header format")
	}
	if err != nil {
		return time.Time{}, err
	}
	if reset.After(now.Add(maxResponseResetHorizon)) {
		return time.Time{}, errors.New("reset header exceeds bounded horizon")
	}
	return reset, nil
}

func parseNonnegativeInt(raw string) (int64, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value < 0 {
		return 0, errors.New("header is not a nonnegative integer")
	}
	return value, nil
}

func providerCooldown(config *ratelimitpolicy.Config, status int, raw string, now time.Time) *time.Time {
	if status != http.StatusTooManyRequests || config.RetryAfter == nil || raw == "" {
		return nil
	}
	delay, err := parseStandardRetryAfter(raw, now)
	if err != nil {
		return nil
	}
	maximum := time.Duration(config.RetryAfter.MaxDelayMS) * time.Millisecond
	if delay > maximum {
		delay = maximum
	}
	cooldown := now.Add(delay)
	return &cooldown
}

func parseStandardRetryAfter(raw string, now time.Time) (time.Duration, error) {
	if seconds, err := parseNonnegativeInt(raw); err == nil {
		return time.Duration(seconds) * time.Second, nil
	}
	when, err := http.ParseTime(strings.TrimSpace(raw))
	if err != nil {
		return 0, err
	}
	if when.Before(now) {
		return 0, nil
	}
	return when.Sub(now), nil
}

func combineHeaderOutcome(current, next string) string {
	if current == "invalid" || next == "invalid" {
		return "invalid"
	}
	if current == "applied" || next == "applied" {
		return "applied"
	}
	return "none"
}

func recordProviderHeaderSync(ctx context.Context, outcome string, err error) {
	if err != nil {
		outcome = "error"
	}
	RecordRateLimitHeaderOutcome(ctx, outcome)
	_, span := otel.Tracer("engine").Start(ctx, "engine.provider_rate_limit.header_sync")
	defer span.End()
	span.SetAttributes(attribute.String("rate_limit.header_outcome", outcome))
	if err != nil {
		span.SetStatus(codes.Error, "sync_failed")
	}
}
