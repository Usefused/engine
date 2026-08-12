package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
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
const maxQuotaSignalBodyBytes = 1 << 20

func (d *Dispatcher) syncProviderRateLimitResponse(ctx context.Context, srv *models.Service, obj *models.IntegrationObject, response *http.Response, bodyValues ...map[string]any) error {
	if srv.RateLimit == nil || d.rateLimits == nil || response == nil {
		return nil
	}
	request, err := providerRateLimitRequest(ctx, srv, obj)
	if err != nil {
		return err
	}
	observations, outcome := providerHeaderObservations(srv.RateLimit, request.Policies, response.Header, firstBodyValues(bodyValues), time.Now())
	cooldown := providerCooldownV3(srv.RateLimit.Cooldown, response.StatusCode, response.Header, time.Now())
	// Most successful responses carry no quota signal. Avoiding a no-op UPDATE
	// keeps that common path from competing with the next acquisition solely to
	// rewrite updated_at; actionable clamps and cooldowns remain fail-closed.
	if !providerRateLimitSyncRequired(observations, cooldown) {
		recordProviderHeaderSync(ctx, outcome, nil)
		return nil
	}
	err = d.rateLimits.SyncProviderRateLimit(ctx, ratelimitpolicy.SyncRequest{
		AccountID: request.AccountID, ServiceVersionID: request.ServiceVersionID,
		CooldownUntil: cooldown, Observations: observations,
	})
	recordProviderHeaderSync(ctx, outcome, err)
	return err
}

func providerRateLimitSyncRequired(observations []ratelimitpolicy.ResponseObservation, cooldown *time.Time) bool {
	if cooldown != nil {
		return true
	}
	for _, observation := range observations {
		if observation.Limit != nil || observation.Remaining != nil || observation.ResetAt != nil || observation.Cost != nil {
			return true
		}
	}
	return false
}

func providerHeaderObservations(config *ratelimitpolicy.Config, resolved []ratelimitpolicy.ResolvedPolicy, headers http.Header, body map[string]any, now time.Time) ([]ratelimitpolicy.ResponseObservation, string) {
	byName := make(map[string]ratelimitpolicy.Policy, len(config.Policies))
	for _, policy := range config.Policies {
		byName[policy.Name] = policy
	}
	observations := make([]ratelimitpolicy.ResponseObservation, 0, len(resolved))
	outcome := "none"
	for _, policy := range resolved {
		observation := responseObservation(policy)
		configured := byName[policy.Name]
		if configured.ResponseSignals != nil {
			parsed, parsedOutcome := parsePolicySignals(configured.ResponseSignals, headers, body, now)
			observation.Limit, observation.Remaining, observation.ResetAt, observation.Cost = parsed.Limit, parsed.Remaining, parsed.ResetAt, parsed.Cost
			observation.LocalCost = policy.Cost
			outcome = combineHeaderOutcome(outcome, parsedOutcome)
		}
		observations = append(observations, observation)
	}
	return observations, outcome
}

func firstBodyValues(values []map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	return values[0]
}

func parsePolicySignals(config *ratelimitpolicy.ResponseSignals, headers http.Header, body map[string]any, now time.Time) (ratelimitpolicy.ResponseObservation, string) {
	result := ratelimitpolicy.ResponseObservation{}
	outcome := "none"
	invalid := false
	result.Limit, outcome, invalid = parseSignalInteger(config.Limit, headers, body, outcome, invalid)
	result.Remaining, outcome, invalid = parseSignalInteger(config.Remaining, headers, body, outcome, invalid)
	result.Cost, outcome, invalid = parseSignalInteger(config.Cost, headers, body, outcome, invalid)
	if config.Reset != nil {
		raw, exists := signalValue(config.Reset.Signal, headers, body)
		if exists {
			parsed, err := parseResetHeader(raw, string(config.Reset.Format), now)
			if err != nil {
				invalid = true
			} else {
				result.ResetAt, outcome = &parsed, "applied"
			}
		}
	}
	if invalid {
		outcome = "invalid"
	}
	return result, outcome
}

func parseSignalInteger(signal *ratelimitpolicy.ResponseSignal, headers http.Header, body map[string]any, outcome string, invalid bool) (*int64, string, bool) {
	if signal == nil {
		return nil, outcome, invalid
	}
	raw, exists := signalValue(*signal, headers, body)
	if !exists {
		return nil, outcome, invalid
	}
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value < 0 || value > maxRuntimeHeaderValue {
		return nil, outcome, true
	}
	return &value, "applied", invalid
}

func signalValue(signal ratelimitpolicy.ResponseSignal, headers http.Header, body map[string]any) (string, bool) {
	if signal.Source == ratelimitpolicy.ResponseSignalHeader {
		value := headers.Get(signal.Name)
		return value, value != ""
	}
	value, ok := body[signal.Path]
	if !ok {
		return "", false
	}
	return fmt.Sprint(value), true
}

func responseObservation(policy ratelimitpolicy.ResolvedPolicy) ratelimitpolicy.ResponseObservation {
	limit := policy.Limit
	if policy.Algorithm == "token_bucket" {
		limit = policy.Capacity
	}
	duration := policy.DurationMs
	if policy.Algorithm == "rolling_window" {
		limit, duration = policy.RollingLimit, policy.RollingDurationMs
	}
	return ratelimitpolicy.ResponseObservation{
		PolicyName: policy.Name, ScopeKind: policy.ScopeKind, ScopeID: uuid.MustParse(policy.ScopeID),
		Algorithm: policy.Algorithm, LocalLimit: limit, DurationMs: duration,
	}
}

func parseResetHeader(raw, format string, now time.Time) (time.Time, error) {
	reset, err := parseResetValue(raw, format, now)
	if err != nil {
		return time.Time{}, err
	}
	if reset.After(now.Add(maxResponseResetHorizon)) {
		return time.Time{}, errors.New("reset header exceeds bounded horizon")
	}
	return reset, nil
}

func parseResetValue(raw, format string, now time.Time) (time.Time, error) {
	switch format {
	case "delta_seconds":
		return parseDeltaReset(raw, now, time.Second)
	case "delta_milliseconds":
		return parseDeltaReset(raw, now, time.Millisecond)
	case "unix_seconds":
		return parseUnixReset(raw, time.Second)
	case "unix_milliseconds":
		return parseUnixReset(raw, time.Millisecond)
	case "rfc3339":
		return time.Parse(time.RFC3339, strings.TrimSpace(raw))
	case "http_date":
		return http.ParseTime(strings.TrimSpace(raw))
	default:
		return time.Time{}, errors.New("unsupported reset header format")
	}
}

func parseDeltaReset(raw string, now time.Time, unit time.Duration) (time.Time, error) {
	value, err := parseNonnegativeInt(raw)
	if err != nil || value > int64(maxResponseResetHorizon/unit) {
		return time.Time{}, errors.New("reset header exceeds bounded horizon")
	}
	return now.Add(time.Duration(value) * unit), nil
}

func parseUnixReset(raw string, unit time.Duration) (time.Time, error) {
	value, err := parseNonnegativeInt(raw)
	if err != nil {
		return time.Time{}, err
	}
	if unit == time.Second {
		return time.Unix(value, 0), nil
	}
	return time.UnixMilli(value), nil
}

func parseNonnegativeInt(raw string) (int64, error) {
	value, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || value < 0 {
		return 0, errors.New("header is not a nonnegative integer")
	}
	return value, nil
}

func providerCooldownV3(config *ratelimitpolicy.Cooldown, status int, headers http.Header, now time.Time) *time.Time {
	if config == nil || !statusInRanges(status, config.Statuses) {
		return nil
	}
	for _, candidate := range config.Headers {
		raw := headers.Get(candidate.Name)
		for _, format := range candidate.Formats {
			when, err := parseResetHeader(raw, string(format), now)
			if raw == "" || err != nil {
				continue
			}
			maximum := now.Add(time.Duration(candidate.MaxDelayMs) * time.Millisecond)
			if when.After(maximum) {
				when = maximum
			}
			return &when
		}
	}
	return nil
}

func statusInRanges(status int, ranges []ratelimitpolicy.StatusRange) bool {
	for _, item := range ranges {
		if status >= item.Min && status <= item.Max {
			return true
		}
	}
	return false
}

func hasBodyRateLimitSignals(config *ratelimitpolicy.Config) bool {
	if config == nil || config.Version != ratelimitpolicy.Version {
		return false
	}
	for _, policy := range config.Policies {
		if policySignalsUseBody(policy.ResponseSignals) {
			return true
		}
	}
	return false
}

func policySignalsUseBody(signals *ratelimitpolicy.ResponseSignals) bool {
	if signals == nil {
		return false
	}
	for _, signal := range []*ratelimitpolicy.ResponseSignal{signals.Limit, signals.Remaining, signals.Cost} {
		if signal != nil && signal.Source == ratelimitpolicy.ResponseSignalBody {
			return true
		}
	}
	return signals.Reset != nil && signals.Reset.Signal.Source == ratelimitpolicy.ResponseSignalBody
}

func captureQuotaSignalBody(config *ratelimitpolicy.Config, response *http.Response) (map[string]any, error) {
	if !hasBodyRateLimitSignals(config) {
		return nil, nil
	}
	paths := quotaBodyPaths(config)
	wanted := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		wanted[path] = struct{}{}
	}
	temporary, err := os.CreateTemp("", "fused-quota-body-*")
	if err != nil {
		return nil, errors.New("create bounded quota signal spool")
	}
	cleanup := func() { temporary.Close(); os.Remove(temporary.Name()) }
	written, copyErr := io.Copy(temporary, io.LimitReader(response.Body, maxQuotaSignalBodyBytes+1))
	response.Body.Close()
	if copyErr != nil || written > maxQuotaSignalBodyBytes {
		cleanup()
		return nil, errors.New("provider quota signal body exceeds safe bound")
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, errors.New("rewind provider quota signal body")
	}
	decoder := json.NewDecoder(temporary)
	decoder.UseNumber()
	values := make(map[string]any, len(wanted))
	if err := extractQuotaScalars(decoder, nil, wanted, values, 0); err != nil {
		cleanup()
		return nil, errors.New("provider quota signal body is invalid JSON")
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		cleanup()
		return nil, errors.New("provider quota signal body has trailing data")
	}
	if _, err := temporary.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, errors.New("rewind provider quota signal body")
	}
	response.Body = &quotaSignalSpool{File: temporary, path: temporary.Name()}
	return values, nil
}

type quotaSignalSpool struct {
	*os.File
	path string
}

func (s *quotaSignalSpool) Close() error {
	err := s.File.Close()
	_ = os.Remove(s.path)
	return err
}

func quotaBodyPaths(config *ratelimitpolicy.Config) []string {
	paths := make([]string, 0)
	for _, policy := range config.Policies {
		signals := policy.ResponseSignals
		if signals == nil {
			continue
		}
		for _, signal := range []*ratelimitpolicy.ResponseSignal{signals.Limit, signals.Remaining, signals.Cost} {
			if signal != nil && signal.Source == ratelimitpolicy.ResponseSignalBody {
				paths = append(paths, signal.Path)
			}
		}
		if signals.Reset != nil && signals.Reset.Signal.Source == ratelimitpolicy.ResponseSignalBody {
			paths = append(paths, signals.Reset.Signal.Path)
		}
	}
	return paths
}

func extractQuotaScalars(decoder *json.Decoder, path []string, wanted map[string]struct{}, values map[string]any, depth int) error {
	if depth > 32 {
		return errors.New("provider quota signal body nesting exceeds bound")
	}
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, nested := token.(json.Delim)
	if !nested {
		key := "$." + strings.Join(path, ".")
		if _, ok := wanted[key]; ok {
			values[key] = token
		}
		return nil
	}
	if delimiter == '{' {
		return extractQuotaObject(decoder, path, wanted, values, depth+1)
	}
	return skipQuotaArray(decoder, path, wanted, values, depth+1)
}

func extractQuotaObject(decoder *json.Decoder, path []string, wanted map[string]struct{}, values map[string]any, depth int) error {
	for decoder.More() {
		name, err := decoder.Token()
		if err != nil {
			return err
		}
		if err := extractQuotaScalars(decoder, append(path, name.(string)), wanted, values, depth); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	return err
}

func skipQuotaArray(decoder *json.Decoder, path []string, wanted map[string]struct{}, values map[string]any, depth int) error {
	none := map[string]struct{}{}
	for decoder.More() {
		if err := extractQuotaScalars(decoder, path, none, values, depth); err != nil {
			return err
		}
	}
	_, err := decoder.Token()
	return err
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
