package api

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
)

const executionSearchGrant = "execution:read"

type executionResultView struct {
	ExecutionID       uuid.UUID               `json:"executionId"`
	AppID             uuid.UUID               `json:"appId"`
	Version           string                  `json:"version"`
	Status            string                  `json:"status"`
	Output            json.RawMessage         `json:"output,omitempty"`
	Error             *restExecutionErrorBody `json:"error,omitempty"`
	Mode              string                  `json:"mode"`
	SourceExecutionID *uuid.UUID              `json:"sourceExecutionId,omitempty"`
	CreatedAt         string                  `json:"createdAt"`
	CompletedAt       string                  `json:"completedAt,omitempty"`
}

// MountExecutionResultRoutes exposes durable reads only after the shared app POST issues read handles.
func MountExecutionResultRoutes(router chi.Router, server *EngineGRPCServer) {
	// A missing server must not publish unauthenticated result routes.
	if router == nil || server == nil {
		return
	}
	router.Get("/v1/apps/{app_id}/executions/{execution_id}", server.handleExecutionResultGet)
	router.Get("/v1/apps/{app_id}/executions", server.handleExecutionResultSearch)
}

// publicExecutionResult projects the authored output without exposing stored search data, input, or read-handle hashes.
func publicExecutionResult(record *store.ExecutionResult) executionResultView {
	view := executionResultView{
		ExecutionID: record.ID, AppID: record.AppID, Version: record.AppVersion,
		Status: record.Status, Output: record.Output,
		Mode: record.Mode, SourceExecutionID: record.SourceExecutionID,
		CreatedAt: record.CreatedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
	}
	// A terminal error contains only the bounded public code and message saved by Engine.
	if record.ErrorCode != "" {
		view.Error = &restExecutionErrorBody{Code: record.ErrorCode, Message: record.ErrorMessage}
	}
	if record.CompletedAt != nil {
		view.CompletedAt = record.CompletedAt.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	}
	return view
}

// executionReadHandleDigest accepts exactly one caller-held credential without logging it.
func executionReadHandleDigest(request *http.Request) (string, bool) {
	values := request.Header.Values("X-Execution-Read-Handle")
	// A fixed-size random handle avoids accepting ambiguous or low-entropy credentials.
	if len(values) != 1 || len(values[0]) != 64 {
		return "", false
	}
	for _, char := range values[0] {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return "", false
		}
	}
	digest := sha256.Sum256([]byte(values[0]))
	return hex.EncodeToString(digest[:]), true
}

// handleExecutionResultGet requires both exact-app bearer authorization and the execution's read handle.
func (s *EngineGRPCServer) handleExecutionResultGet(writer http.ResponseWriter, request *http.Request) {
	ctx, span := otel.Tracer("engine").Start(request.Context(), "engine.execution_result.get")
	defer span.End()
	appID, requestErr := parseRESTAppID(chi.URLParam(request, "app_id"))
	if requestErr != nil {
		writeRESTExecutionError(writer, requestErr)
		return
	}
	identityScope, _, requestErr := s.authenticateRESTApp(request.WithContext(ctx), appID)
	if requestErr != nil {
		writeRESTExecutionError(writer, requestErr)
		return
	}
	executionID, err := uuid.Parse(chi.URLParam(request, "execution_id"))
	if err != nil || executionID == uuid.Nil {
		writeRESTExecutionError(writer, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "execution_id must be a UUID"))
		return
	}
	record, requestErr := s.authorizedExecutionResult(ctx, request, identityScope.AccountID, appID, executionID)
	if requestErr != nil {
		writeRESTExecutionError(writer, requestErr)
		return
	}
	writeRESTExecutionJSON(writer, http.StatusOK, publicExecutionResult(record))
}

// authorizedExecutionResult combines an exact-app lookup with the caller-held read credential.
func (s *EngineGRPCServer) authorizedExecutionResult(ctx context.Context, request *http.Request, accountID, appID, executionID uuid.UUID) (*store.ExecutionResult, *restExecutionError) {
	hash, valid := executionReadHandleDigest(request)
	// Missing and malformed handles reveal no execution existence.
	if !valid {
		return nil, newRESTExecutionError(http.StatusNotFound, "execution_not_found", "execution not found")
	}
	repository, ok := s.store.(store.ExecutionResultStore)
	if !ok {
		return nil, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "execution results are unavailable")
	}
	record, err := repository.GetExecutionResult(ctx, accountID, appID, executionID)
	// Missing results and mismatched handles share the same response to avoid ID enumeration.
	if errors.Is(err, store.ErrExecutionResultNotFound) || (err == nil && subtle.ConstantTimeCompare([]byte(hash), []byte(record.ReadHandleHash)) != 1) {
		return nil, newRESTExecutionError(http.StatusNotFound, "execution_not_found", "execution not found")
	}
	if err != nil {
		return nil, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "execution results are unavailable")
	}
	return record, nil
}

// executionSearchRequest reads one bounded first-page query from URL parameters.
func executionSearchRequest(request *http.Request) (map[string]json.RawMessage, int, *restExecutionError) {
	query := request.URL.Query()
	// The exact app version owns one execute contract, so no secondary name may alter the search scope.
	if _, supplied := query["capability"]; supplied {
		return nil, 0, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "capability is not a search parameter")
	}
	limit, requestErr := executionSearchLimit(query["limit"])
	if requestErr != nil {
		return nil, 0, requestErr
	}
	where, requestErr := executionSearchWhere(query["where"])
	if requestErr != nil {
		return nil, 0, requestErr
	}
	return where, limit, nil
}

// executionSearchLimit applies a default and caps one page before querying storage.
func executionSearchLimit(values []string) (int, *restExecutionError) {
	if len(values) == 0 {
		return 20, nil
	}
	if len(values) != 1 {
		return 0, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "limit is invalid")
	}
	parsed, err := strconv.Atoi(values[0])
	if err != nil || parsed < 1 || parsed > 100 {
		return 0, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "limit must be between 1 and 100")
	}
	return parsed, nil
}

// executionSearchWhere accepts only one small JSON object of scalar predicates.
func executionSearchWhere(values []string) (map[string]json.RawMessage, *restExecutionError) {
	if len(values) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	var where map[string]json.RawMessage
	// URL predicates are capped independently of the execution input limit.
	if len(values) != 1 || len(values[0]) > 4096 {
		return nil, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "where must be a bounded JSON object")
	}
	canonical, err := canonicaljson.Canonicalize([]byte(values[0]))
	// Canonical parsing rejects duplicate field names before an allowlist is evaluated.
	if err != nil || json.Unmarshal(canonical, &where) != nil || where == nil {
		return nil, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "where must be a bounded JSON object")
	}
	return where, nil
}

// searchablePathsForUnifiedApp extracts policy only from the persisted immutable bundle.
func searchablePathsForUnifiedApp(manifest json.RawMessage) ([]string, error) {
	decoded, err := parseUnifiedAppManifest(manifest)
	if err != nil {
		return nil, err
	}
	return decoded.Searchable, nil
}

// handleExecutionResultSearch authorizes a first page and applies the bundle's immutable filter policy.
func (s *EngineGRPCServer) handleExecutionResultSearch(writer http.ResponseWriter, request *http.Request) {
	ctx, span := otel.Tracer("engine").Start(request.Context(), "engine.execution_result.search")
	defer span.End()
	appID, requestErr := parseRESTAppID(chi.URLParam(request, "app_id"))
	if requestErr != nil {
		writeRESTExecutionError(writer, requestErr)
		return
	}
	scope, identity, requestErr := s.authenticateRESTApp(request.WithContext(ctx), appID)
	if requestErr != nil {
		writeRESTExecutionError(writer, requestErr)
		return
	}
	// Search spans multiple executions, so a read handle cannot substitute for an app-level grant.
	if !identity.TokenPolicy.AllowsOperation(executionSearchGrant) {
		writeRESTExecutionError(writer, newRESTExecutionError(http.StatusForbidden, "access_denied", "execution search is not allowed"))
		return
	}
	where, limit, requestErr := executionSearchRequest(request)
	if requestErr != nil {
		writeRESTExecutionError(writer, requestErr)
		return
	}
	items, requestErr := s.searchExecutionResults(ctx, scope.AccountID, appID, where, limit)
	if requestErr != nil {
		writeRESTExecutionError(writer, requestErr)
		return
	}
	views := make([]executionResultView, 0, len(items))
	for index := range items {
		views = append(views, publicExecutionResult(&items[index]))
	}
	writeRESTExecutionJSON(writer, http.StatusOK, map[string]any{"items": views})
}

// searchExecutionResults loads the immutable bundle policy before delegating predicates to SQL.
func (s *EngineGRPCServer) searchExecutionResults(ctx context.Context, accountID, appID uuid.UUID, where map[string]json.RawMessage, limit int) ([]store.ExecutionResult, *restExecutionError) {
	results, ok := s.store.(store.ExecutionResultStore)
	if !ok {
		return nil, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "execution results are unavailable")
	}
	bundles, ok := s.store.(store.UnifiedAppBundleStore)
	if !ok {
		return nil, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "execution bundle is unavailable")
	}
	bundle, err := bundles.GetUnifiedAppBundle(ctx, appID)
	if err != nil {
		return nil, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "execution bundle is unavailable")
	}
	paths, err := searchablePathsForUnifiedApp(bundle.Manifest)
	if err != nil {
		return nil, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "unified app is unavailable for search")
	}
	items, err := results.SearchExecutionResults(ctx, store.ExecutionResultSearch{
		AccountID: accountID, AppID: appID,
		AllowedDataPaths: paths, Where: where, Limit: limit,
	})
	if errors.Is(err, store.ErrExecutionResultInvalid) {
		return nil, newRESTExecutionError(http.StatusBadRequest, "invalid_request", "search filter is invalid")
	}
	if err != nil {
		return nil, newRESTExecutionError(http.StatusServiceUnavailable, "runtime_unavailable", "execution search is unavailable")
	}
	return items, nil
}
