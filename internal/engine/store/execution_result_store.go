package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"go.opentelemetry.io/otel"
)

const (
	MaxExecutionDataBytes   = 512 * 1024
	maxExecutionSearchTerms = 8
	maxExecutionSearchLimit = 100
)

var (
	ErrExecutionResultNotFound            = errors.New("execution result not found")
	ErrExecutionResultInvalid             = errors.New("execution result is invalid")
	ErrExecutionResultDataTooLarge        = errors.New("execution data exceeds 512 KiB")
	ErrExecutionResultTransition          = errors.New("execution result state transition is invalid")
	ErrExecutionResultIdempotencyConflict = errors.New("rerun idempotency key conflicts with a different request")
	executionPathSegment                  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// ExecutionResult is the durable record for one exact unified app version.
// Input and read-handle hashes are deliberately omitted from JSON projections.
type ExecutionResult struct {
	ID                   uuid.UUID       `json:"executionId"`
	AccountID            uuid.UUID       `json:"-"`
	AppFamilyID          uuid.UUID       `json:"-"`
	AppID                uuid.UUID       `json:"appId"`
	AppVersion           string          `json:"version"`
	AppTokenID           uuid.UUID       `json:"-"`
	ReadHandleHash       string          `json:"-"`
	IdempotencyKeyHash   string          `json:"-"`
	Status               string          `json:"status"`
	Input                json.RawMessage `json:"-"`
	Output               json.RawMessage `json:"output,omitempty"`
	Data                 json.RawMessage `json:"data"`
	ErrorCode            string          `json:"-"`
	ErrorMessage         string          `json:"-"`
	SourceExecutionID    *uuid.UUID      `json:"sourceExecutionId,omitempty"`
	SourceWebhookEventID string          `json:"sourceWebhookEventId,omitempty"`
	Mode                 string          `json:"mode"`
	CreatedAt            time.Time       `json:"createdAt"`
	UpdatedAt            time.Time       `json:"updatedAt"`
	CompletedAt          *time.Time      `json:"completedAt,omitempty"`
	ExpiresAt            *time.Time      `json:"expiresAt,omitempty"`
}

// ExecutionResultSearch scopes query terms to one immutable app version and its admitted data paths.
type ExecutionResultSearch struct {
	AccountID        uuid.UUID
	AppID            uuid.UUID
	AllowedDataPaths []string
	Where            map[string]json.RawMessage
	Limit            int
}

// ExecutionResultStore is narrow so execution persistence does not expand every control-plane store fixture.
type ExecutionResultStore interface {
	CreateExecutionResult(context.Context, ExecutionResult) error
	CreateOrGetRerunExecutionResult(context.Context, ExecutionResult) (uuid.UUID, bool, error)
	StartExecutionResult(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) error
	CompleteExecutionResult(context.Context, uuid.UUID, uuid.UUID, uuid.UUID, string, json.RawMessage, json.RawMessage, string, string) error
	GetExecutionResult(context.Context, uuid.UUID, uuid.UUID, uuid.UUID) (*ExecutionResult, error)
	SearchExecutionResults(context.Context, ExecutionResultSearch) ([]ExecutionResult, error)
	DeleteExpiredExecutionResults(context.Context, time.Time, int) (int64, error)
}

// canonicalExecutionJSON validates the same byte representation used by the JSONB size guard.
func canonicalExecutionJSON(raw json.RawMessage, required bool) (json.RawMessage, error) {
	// Nil is valid only for optional output; an explicit JSON null remains a value.
	if len(raw) == 0 {
		if required {
			return nil, ErrExecutionResultInvalid
		}
		return nil, nil
	}
	canonical, err := canonicaljson.Canonicalize(raw)
	if err != nil {
		return nil, ErrExecutionResultInvalid
	}
	// The bound applies to UTF-8 bytes after canonical serialization, including keys.
	if len(canonical) > MaxExecutionDataBytes {
		return nil, ErrExecutionResultDataTooLarge
	}
	return canonical, nil
}

// validExecutionStatus admits only lifecycle states that the schema can persist.
func validExecutionStatus(status string) bool {
	switch status {
	case "queued", "running", "succeeded", "failed", "indeterminate":
		return true
	default:
		return false
	}
}

// validExecutionMode prevents unrecognized execution history from entering durable records.
func validExecutionMode(mode string) bool {
	switch mode {
	case "live", "replay", "rerun":
		return true
	default:
		return false
	}
}

// validateNewExecutionResult keeps untrusted identities and handle digests out of durable rows.
func validateNewExecutionResult(record ExecutionResult) error {
	for _, id := range []uuid.UUID{record.ID, record.AccountID, record.AppFamilyID, record.AppID} {
		// Every row must have complete ownership before a worker can start.
		if id == uuid.Nil {
			return ErrExecutionResultInvalid
		}
	}
	// Trigger attribution is checked independently of shared app and mode identity.
	if err := validateExecutionTrigger(record); err != nil {
		return err
	}
	// The exact app version is the authored operation identity; no second capability name is needed.
	if !boundedExecutionName(record.AppVersion) {
		return ErrExecutionResultInvalid
	}
	// Mode-specific ownership and handle rules remain shared across every trigger.
	if !validNewExecutionModeFields(record) {
		return ErrExecutionResultInvalid
	}
	return nil
}

// validateExecutionTrigger distinguishes Engine-owned events from authenticated caller invocations.
func validateExecutionTrigger(record ExecutionResult) error {
	// Only an Engine-owned webhook admission may omit caller-token attribution.
	if record.AppTokenID == uuid.Nil && record.SourceWebhookEventID == "" {
		return ErrExecutionResultInvalid
	}
	// Event provenance cannot be injected into token-backed calls, reruns, or replay records.
	if record.SourceWebhookEventID != "" && (len(record.SourceWebhookEventID) > 256 || record.Mode != "live" || record.AppTokenID != uuid.Nil) {
		return ErrExecutionResultInvalid
	}
	return nil
}

// validNewExecutionModeFields binds optional idempotency to a declared execution mode.
func validNewExecutionModeFields(record ExecutionResult) bool {
	if record.Status != "queued" || !validExecutionMode(record.Mode) || !validExecutionReadHash(record.ReadHandleHash) {
		return false
	}
	// Rerun needs a linked source and a hashed key before any provider effect can occur.
	if record.Mode == "rerun" && (record.SourceExecutionID == nil || !validExecutionReadHash(record.IdempotencyKeyHash)) {
		return false
	}
	// Optional hashes on other modes must retain the same unambiguous encoding.
	if record.IdempotencyKeyHash != "" && !validExecutionReadHash(record.IdempotencyKeyHash) {
		return false
	}
	return true
}

// boundedExecutionName keeps authored identifiers compact in records and indexes.
func boundedExecutionName(name string) bool {
	return len(name) > 0 && len(name) <= 256
}

// validExecutionReadHash accepts only one lowercase SHA-256 digest.
func validExecutionReadHash(hash string) bool {
	// Fixed length avoids ambiguous encodings and timing differences in read checks.
	if len(hash) != 64 {
		return false
	}
	for _, char := range hash {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}
	return true
}

// CreateExecutionResult records an accepted call before dispatch or worker startup.
func (s *postgresStore) CreateExecutionResult(ctx context.Context, record ExecutionResult) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.execution_result.create")
	defer span.End()
	// All triggers must persist complete ownership before effects begin.
	if err := validateNewExecutionResult(record); err != nil {
		return err
	}
	input, err := canonicalExecutionJSON(record.Input, true)
	// Malformed or oversized input cannot become a durable accepted execution.
	if err != nil {
		return err
	}
	// A new execution owns an empty document; rerun and replay never inherit durable data.
	_, err = s.db.Exec(ctx, `INSERT INTO fused_unified_app_results
		(id, account_id, app_family_id, app_id, app_version, app_token_id, read_handle_hash, idempotency_key_hash, status, input, data, source_execution_id, mode, source_webhook_event_id)
		VALUES ($1,$2,$3,$4,$5,$6,$7,NULLIF($8,''),'queued',$9,'null'::jsonb,$10,$11,NULLIF($12,''))`,
		record.ID, record.AccountID, record.AppFamilyID, record.AppID, record.AppVersion,
		record.AppTokenID, record.ReadHandleHash, record.IdempotencyKeyHash, input, record.SourceExecutionID, record.Mode, record.SourceWebhookEventID)
	return err
}

// CreateOrGetRerunExecutionResult reserves one live rerun and returns its first ID on an identical retry.
func (s *postgresStore) CreateOrGetRerunExecutionResult(ctx context.Context, record ExecutionResult) (uuid.UUID, bool, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.execution_result.rerun.reserve")
	defer span.End()
	// This method cannot be used to weaken the ordinary live execution admission path.
	if record.Mode != "rerun" || record.SourceExecutionID == nil || !validExecutionReadHash(record.IdempotencyKeyHash) {
		return uuid.Nil, false, ErrExecutionResultInvalid
	}
	err := s.CreateExecutionResult(ctx, record)
	if err == nil {
		return record.ID, true, nil
	}
	var pgError *pgconn.PgError
	// Only the dedicated rerun uniqueness conflict is an idempotent retry.
	if !errors.As(err, &pgError) || pgError.ConstraintName != "uq_fused_unified_app_results_rerun" {
		return uuid.Nil, false, err
	}
	existingID, err := s.matchExistingRerun(ctx, record)
	if err != nil {
		return uuid.Nil, false, err
	}
	return existingID, false, nil
}

// matchExistingRerun reads one SQL-unique reservation and verifies the pinned request before deduplication.
func (s *postgresStore) matchExistingRerun(ctx context.Context, record ExecutionResult) (uuid.UUID, error) {
	var existingID uuid.UUID
	var existingVersion string
	var existingInput []byte
	err := s.db.QueryRow(ctx, `SELECT id, app_version, input FROM fused_unified_app_results
		WHERE account_id=$1 AND app_id=$2 AND app_token_id=$3 AND mode='rerun'
			AND source_execution_id=$4 AND idempotency_key_hash=$5`,
		record.AccountID, record.AppID, record.AppTokenID, record.SourceExecutionID, record.IdempotencyKeyHash).
		Scan(&existingID, &existingVersion, &existingInput)
	if err != nil {
		return uuid.Nil, err
	}
	input, err := canonicalExecutionJSON(record.Input, true)
	if err != nil {
		return uuid.Nil, err
	}
	storedInput, err := canonicalExecutionJSON(existingInput, true)
	if err != nil {
		return uuid.Nil, err
	}
	// A reused key may return the first ID only when it still describes the same pinned request.
	if existingVersion != record.AppVersion || !bytes.Equal(storedInput, input) {
		return uuid.Nil, ErrExecutionResultIdempotencyConflict
	}
	return existingID, nil
}

// StartExecutionResult moves only an accepted queued call into worker execution.
func (s *postgresStore) StartExecutionResult(ctx context.Context, accountID, appID, executionID uuid.UUID) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.execution_result.start")
	defer span.End()
	tag, err := s.db.Exec(ctx, `UPDATE fused_unified_app_results SET status='running', updated_at=NOW()
		WHERE id=$1 AND account_id=$2 AND app_id=$3 AND status='queued'`, executionID, accountID, appID)
	if err != nil {
		return err
	}
	// Zero rows means a duplicate, expired, or invalid transition and must not restart work.
	if tag.RowsAffected() != 1 {
		return ErrExecutionResultTransition
	}
	return nil
}

// CompleteExecutionResult commits the final JSONB document, authored output, and status in one row update.
func (s *postgresStore) CompleteExecutionResult(ctx context.Context, accountID, appID, executionID uuid.UUID, status string, rawOutput, rawData json.RawMessage, errorCode, errorMessage string) error {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.execution_result.complete")
	defer span.End()
	// Only terminal states are valid here; pending states use the separate start transition.
	if status != "succeeded" && status != "failed" && status != "indeterminate" {
		return ErrExecutionResultInvalid
	}
	output, err := canonicalExecutionJSON(rawOutput, status == "succeeded")
	if err != nil {
		return err
	}
	data, err := canonicalExecutionJSON(rawData, true)
	if err != nil {
		return err
	}
	// Retained authored messages share the diagnostic text bound; stacks and response payloads are stored separately.
	if len(errorCode) > 128 || len(errorMessage) > 65536 {
		return ErrExecutionResultInvalid
	}
	tag, err := s.db.Exec(ctx, `UPDATE fused_unified_app_results
		SET status=$4, output=$5::jsonb, data=$6::jsonb, error_code=$7, error_message=$8, updated_at=NOW(),
			completed_at=NOW(), expires_at=NOW()+INTERVAL '24 hours'
		WHERE id=$1 AND account_id=$2 AND app_id=$3 AND status IN ('queued','running')`,
		executionID, accountID, appID, status, output, data, errorCode, errorMessage)
	if err != nil {
		return err
	}
	// Terminal states are immutable; an uncertain result cannot be silently overwritten.
	if tag.RowsAffected() != 1 {
		return ErrExecutionResultTransition
	}
	return nil
}

// scanExecutionResult keeps every read on one explicit column projection.
func scanExecutionResult(row pgx.Row) (*ExecutionResult, error) {
	var record ExecutionResult
	var input, output, data []byte
	err := row.Scan(&record.ID, &record.AccountID, &record.AppFamilyID, &record.AppID, &record.AppVersion,
		&record.AppTokenID, &record.ReadHandleHash, &record.Status,
		&input, &output, &data, &record.ErrorCode, &record.ErrorMessage,
		&record.SourceExecutionID, &record.Mode, &record.CreatedAt, &record.UpdatedAt, &record.CompletedAt, &record.ExpiresAt, &record.SourceWebhookEventID)
	// A partial row cannot be exposed as a valid retained result.
	if err != nil {
		return nil, err
	}
	record.Input, record.Output, record.Data = input, output, data
	return &record, nil
}

const executionResultColumns = `id, account_id, app_family_id, app_id, app_version,
	app_token_id, read_handle_hash, status, input, output, data, error_code, error_message,
	source_execution_id, mode, created_at, updated_at, completed_at, expires_at, COALESCE(source_webhook_event_id,'')`

const executionSearchColumns = `id, app_id, app_version, status, output, data,
	error_code, error_message, source_execution_id, mode, created_at, completed_at, COALESCE(source_webhook_event_id,'')`

// scanExecutionSearchResult omits retained input and handle hashes from multi-row reads.
func scanExecutionSearchResult(row pgx.Row) (*ExecutionResult, error) {
	var record ExecutionResult
	var output, data []byte
	err := row.Scan(&record.ID, &record.AppID, &record.AppVersion, &record.Status,
		&output, &data, &record.ErrorCode, &record.ErrorMessage, &record.SourceExecutionID,
		&record.Mode, &record.CreatedAt, &record.CompletedAt, &record.SourceWebhookEventID)
	// Incomplete search rows must not drop provenance while appearing to be valid execution results.
	if err != nil {
		return nil, err
	}
	record.Output, record.Data = output, data
	return &record, nil
}

// GetExecutionResult resolves only an unexpired row within the exact account and app version.
func (s *postgresStore) GetExecutionResult(ctx context.Context, accountID, appID, executionID uuid.UUID) (*ExecutionResult, error) {
	row := s.db.QueryRow(ctx, `SELECT `+executionResultColumns+` FROM fused_unified_app_results
		WHERE id=$1 AND account_id=$2 AND app_id=$3 AND (expires_at IS NULL OR expires_at>NOW())`, executionID, accountID, appID)
	record, err := scanExecutionResult(row)
	// Missing, expired, and cross-tenant IDs share the same public not-found result.
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrExecutionResultNotFound
	}
	return record, err
}

// ValidExecutionDataPath bounds searchable JSON paths to simple object keys.
func ValidExecutionDataPath(path string) bool {
	segments := strings.Split(path, ".")
	// Nested paths remain shallow so search plans cannot be used for arbitrary JSON traversal.
	if len(segments) == 0 || len(segments) > 4 {
		return false
	}
	for _, segment := range segments {
		if !executionPathSegment.MatchString(segment) {
			return false
		}
	}
	return true
}

// executionSearchValue validates a scalar filter and returns its concrete JSON value.
func executionSearchValue(raw json.RawMessage) (any, error) {
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, ErrExecutionResultInvalid
	}
	// Objects and arrays are excluded because the builder admits scalar property paths only.
	switch value.(type) {
	case string, float64, bool, nil:
		return value, nil
	default:
		return nil, ErrExecutionResultInvalid
	}
}

// nestedExecutionSearchJSON makes a GIN-indexable containment term for one admitted path.
func nestedExecutionSearchJSON(path string, value any) ([]byte, error) {
	parts := strings.Split(path, ".")
	var nested any = value
	for index := len(parts) - 1; index >= 0; index-- {
		nested = map[string]any{parts[index]: nested}
	}
	return json.Marshal(nested)
}

// validateExecutionSearchFilter applies request bounds and checks every immutable searchable path.
func validateExecutionSearchFilter(filter ExecutionResultSearch) (map[string]struct{}, error) {
	// Exact app version identity already bounds the search to one authored operation.
	if filter.AccountID == uuid.Nil || filter.AppID == uuid.Nil {
		return nil, ErrExecutionResultInvalid
	}
	if filter.Limit < 1 || filter.Limit > maxExecutionSearchLimit || len(filter.Where) > maxExecutionSearchTerms || len(filter.AllowedDataPaths) > 32 {
		return nil, ErrExecutionResultInvalid
	}
	allowed := make(map[string]struct{}, len(filter.AllowedDataPaths))
	for _, path := range filter.AllowedDataPaths {
		// A malformed bundle declaration must fail closed before SQL construction.
		if !ValidExecutionDataPath(path) {
			return nil, ErrExecutionResultInvalid
		}
		allowed[path] = struct{}{}
	}
	return allowed, nil
}

// executionSearchTerm converts one validated field into a parameterized SQL predicate.
func executionSearchTerm(name string, raw json.RawMessage, allowed map[string]struct{}, position int) (string, any, error) {
	value, err := executionSearchValue(raw)
	if err != nil {
		return "", nil, err
	}
	switch name {
	case "executionId":
		id, err := uuid.Parse(fmt.Sprint(value))
		if err != nil {
			return "", nil, ErrExecutionResultInvalid
		}
		return fmt.Sprintf("id=$%d", position), id, nil
	case "status":
		status, ok := value.(string)
		if !ok || !validExecutionStatus(status) {
			return "", nil, ErrExecutionResultInvalid
		}
		return fmt.Sprintf("status=$%d", position), status, nil
	case "createdAt":
		stamp, ok := value.(string)
		if !ok {
			return "", nil, ErrExecutionResultInvalid
		}
		parsed, err := time.Parse(time.RFC3339Nano, stamp)
		if err != nil {
			return "", nil, ErrExecutionResultInvalid
		}
		return fmt.Sprintf("created_at=$%d", position), parsed, nil
	default:
		return executionDataSearchTerm(name, value, allowed, position)
	}
}

// executionDataSearchTerm uses containment so an admitted nested field can use the JSONB GIN index.
func executionDataSearchTerm(name string, value any, allowed map[string]struct{}, position int) (string, any, error) {
	// Caller-supplied paths never become SQL identifiers and must match the bundle allowlist.
	if !strings.HasPrefix(name, "data.") {
		return "", nil, ErrExecutionResultInvalid
	}
	path := strings.TrimPrefix(name, "data.")
	if _, ok := allowed[path]; !ok {
		return "", nil, ErrExecutionResultInvalid
	}
	containment, err := nestedExecutionSearchJSON(path, value)
	if err != nil {
		return "", nil, err
	}
	return fmt.Sprintf("data @> $%d::jsonb", position), containment, nil
}

// SearchExecutionResults applies exact-version scope and an immutable data-path allowlist before SQL.
func (s *postgresStore) SearchExecutionResults(ctx context.Context, filter ExecutionResultSearch) ([]ExecutionResult, error) {
	allowed, err := validateExecutionSearchFilter(filter)
	if err != nil {
		return nil, err
	}
	clauses := []string{"account_id=$1", "app_id=$2", "(expires_at IS NULL OR expires_at>NOW())"}
	args := []any{filter.AccountID, filter.AppID}
	for name, raw := range filter.Where {
		clause, arg, err := executionSearchTerm(name, raw, allowed, len(args)+1)
		if err != nil {
			return nil, err
		}
		args = append(args, arg)
		clauses = append(clauses, clause)
	}
	args = append(args, filter.Limit)
	query := `SELECT ` + executionSearchColumns + ` FROM fused_unified_app_results WHERE ` + strings.Join(clauses, " AND ") + fmt.Sprintf(` ORDER BY created_at DESC, id DESC LIMIT $%d`, len(args))
	rows, err := s.db.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := make([]ExecutionResult, 0, filter.Limit)
	for rows.Next() {
		record, err := scanExecutionSearchResult(rows)
		if err != nil {
			return nil, err
		}
		results = append(results, *record)
	}
	return results, rows.Err()
}

// DeleteExpiredExecutionResults removes only terminal rows past both their retention deadline and the caller's cutoff.
func (s *postgresStore) DeleteExpiredExecutionResults(ctx context.Context, before time.Time, limit int) (int64, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.execution_result.expire")
	defer span.End()
	// A bounded batch prevents maintenance from monopolizing the execution table.
	if limit < 1 || limit > 1000 {
		return 0, ErrExecutionResultInvalid
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM fused_unified_app_results WHERE id IN (
		SELECT id FROM fused_unified_app_results
		WHERE expires_at IS NOT NULL AND expires_at<LEAST($1,NOW())
		ORDER BY expires_at, id LIMIT $2
	)`, before, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// executionResultDelegate keeps cached Store wrappers transparent to the result persistence path.
func (s *cachedStore) executionResultDelegate() (ExecutionResultStore, error) {
	repository, ok := s.Store.(ExecutionResultStore)
	// The runtime must fail closed if a replacement Store lacks durable records.
	if !ok {
		return nil, errors.New("execution result store unavailable")
	}
	return repository, nil
}

// CreateExecutionResult delegates durable admission without caching caller data.
func (s *cachedStore) CreateExecutionResult(ctx context.Context, record ExecutionResult) error {
	repository, err := s.executionResultDelegate()
	if err != nil {
		return err
	}
	return repository.CreateExecutionResult(ctx, record)
}

// CreateOrGetRerunExecutionResult delegates the unique reservation to PostgreSQL.
func (s *cachedStore) CreateOrGetRerunExecutionResult(ctx context.Context, record ExecutionResult) (uuid.UUID, bool, error) {
	repository, err := s.executionResultDelegate()
	if err != nil {
		return uuid.Nil, false, err
	}
	return repository.CreateOrGetRerunExecutionResult(ctx, record)
}

// StartExecutionResult delegates the single queued-to-running transition.
func (s *cachedStore) StartExecutionResult(ctx context.Context, accountID, appID, executionID uuid.UUID) error {
	repository, err := s.executionResultDelegate()
	if err != nil {
		return err
	}
	return repository.StartExecutionResult(ctx, accountID, appID, executionID)
}

// CompleteExecutionResult delegates terminal commits without a stale cache window.
func (s *cachedStore) CompleteExecutionResult(ctx context.Context, accountID, appID, executionID uuid.UUID, status string, output, data json.RawMessage, errorCode, errorMessage string) error {
	repository, err := s.executionResultDelegate()
	if err != nil {
		return err
	}
	return repository.CompleteExecutionResult(ctx, accountID, appID, executionID, status, output, data, errorCode, errorMessage)
}

// GetExecutionResult delegates exact reads so expiry and handle checks see current state.
func (s *cachedStore) GetExecutionResult(ctx context.Context, accountID, appID, executionID uuid.UUID) (*ExecutionResult, error) {
	repository, err := s.executionResultDelegate()
	if err != nil {
		return nil, err
	}
	return repository.GetExecutionResult(ctx, accountID, appID, executionID)
}

// SearchExecutionResults delegates indexed search without Go-side filtering.
func (s *cachedStore) SearchExecutionResults(ctx context.Context, filter ExecutionResultSearch) ([]ExecutionResult, error) {
	repository, err := s.executionResultDelegate()
	if err != nil {
		return nil, err
	}
	return repository.SearchExecutionResults(ctx, filter)
}

// DeleteExpiredExecutionResults delegates bounded retention cleanup to SQL.
func (s *cachedStore) DeleteExpiredExecutionResults(ctx context.Context, before time.Time, limit int) (int64, error) {
	repository, err := s.executionResultDelegate()
	if err != nil {
		return 0, err
	}
	return repository.DeleteExpiredExecutionResults(ctx, before, limit)
}

var _ ExecutionResultStore = (*postgresStore)(nil)
var _ ExecutionResultStore = (*cachedStore)(nil)
