package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

type refreshServiceContractResponse struct {
	Status           string `json:"status"`
	ServiceID        string `json:"service_id"`
	ServiceVersionID string `json:"service_version_id"`
	Version          string `json:"version"`
	ContractHash     string `json:"contract_hash"`
}

type refreshMissingContractsResponse struct {
	Status    string                         `json:"status"`
	Missing   int                            `json:"missing"`
	Refreshed int                            `json:"refreshed"`
	Failed    int                            `json:"failed"`
	Results   []refreshServiceContractResult `json:"results"`
}

type refreshServiceContractResult struct {
	ServiceID        string `json:"service_id"`
	ServiceVersionID string `json:"service_version_id"`
	Version          string `json:"version"`
	ContractHash     string `json:"contract_hash,omitempty"`
	Error            string `json:"error,omitempty"`
}

type refreshServiceContractPath struct {
	serviceID        uuid.UUID
	serviceVersionID uuid.UUID
}

type refreshHTTPError struct {
	status  int
	message string
}

func (e refreshHTTPError) Error() string {
	return e.message
}

func RefreshServiceContractHandler(s store.Store, fetcher RuntimeContractFetcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		accountID, err := controlActorAccount(r.Context())
		if err != nil {
			http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
			return
		}
		path, err := parseRefreshServiceContractPath(r)
		if err != nil {
			writeRefreshServiceContractError(w, err)
			return
		}
		if err := verifyWorkspaceActor(r.Context(), accountID); err != nil {
			writeRefreshServiceContractError(w, refreshHTTPError{status: http.StatusInternalServerError, message: "workspace not found"})
			return
		}
		snapshot, err := refreshPinnedServiceContract(r.Context(), s, fetcher, refreshPinnedServiceContractCall{
			accountID:        accountID,
			serviceID:        path.serviceID,
			serviceVersionID: path.serviceVersionID,
			apiKey:           r.Header.Get("X-API-Key"),
		})
		if err != nil {
			writeRefreshServiceContractError(w, err)
			return
		}
		writeRefreshServiceContractResponse(w, snapshot)
	}
}

func parseRefreshServiceContractPath(r *http.Request) (refreshServiceContractPath, error) {
	serviceID, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		return refreshServiceContractPath{}, refreshHTTPError{status: http.StatusBadRequest, message: "service id must be a valid UUID"}
	}
	versionID, err := uuid.Parse(chi.URLParam(r, "version_id"))
	if err != nil {
		return refreshServiceContractPath{}, refreshHTTPError{status: http.StatusBadRequest, message: "service_version_id must be a valid UUID"}
	}
	return refreshServiceContractPath{serviceID: serviceID, serviceVersionID: versionID}, nil
}

type refreshPinnedServiceContractCall struct {
	accountID        uuid.UUID
	serviceID        uuid.UUID
	serviceVersionID uuid.UUID
	apiKey           string
}

type refreshMissingContractsCall struct {
	accountID uuid.UUID
	apiKey    string
	limit     int
}

func refreshPinnedServiceContract(ctx context.Context, s store.Store, fetcher RuntimeContractFetcher, call refreshPinnedServiceContractCall) (*store.ServiceContractSnapshot, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.workspace.refresh_runtime_contract")
	defer span.End()
	span.SetAttributes(refreshPinnedServiceContractAttributes(call)...)

	version, err := getRefreshWorkspaceServiceVersion(ctx, s, call)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "version_not_active"))
		return nil, err
	}
	writer, err := refreshSnapshotWriter(s)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "unsupported"))
		return nil, err
	}
	snapshot, err := fetchRefreshSnapshot(ctx, fetcher, call, version.Version)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "fetch_failed"))
		return nil, err
	}
	saved, err := writer.UpsertServiceContractSnapshot(ctx, *snapshot)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "write_failed"))
		return nil, refreshHTTPError{status: http.StatusInternalServerError, message: "failed to store runtime contract snapshot"}
	}
	span.SetAttributes(attribute.String("outcome", "success"))
	return saved, nil
}

func refreshMissingServiceContracts(ctx context.Context, s store.Store, batchFetcher BatchRuntimeContractFetcher, call refreshMissingContractsCall) (*refreshMissingContractsResponse, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.workspace.refresh_missing_runtime_contracts")
	defer span.End()
	span.SetAttributes(
		attribute.String("user_action", "workspace.refresh_missing_runtime_contracts"),
		attribute.String("account_id", call.accountID.String()),
		attribute.Int("limit", call.limit),
	)

	versions, err := listMissingContractVersions(ctx, s, call.limit)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "lookup_failed"))
		return nil, err
	}
	response := &refreshMissingContractsResponse{Status: "ok", Missing: len(versions)}
	if len(versions) == 0 {
		span.SetAttributes(attribute.String("outcome", "nothing_missing"))
		return response, nil
	}
	snapshots, err := fetchMissingSnapshots(ctx, batchFetcher, versions, call.apiKey)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "fetch_failed"))
		return nil, err
	}
	writeMissingSnapshots(ctx, s, snapshots, response)
	span.SetAttributes(attribute.String("outcome", "success"), attribute.Int("refreshed", response.Refreshed), attribute.Int("failed", response.Failed))
	return response, nil
}

func refreshPinnedServiceContractAttributes(call refreshPinnedServiceContractCall) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("user_action", "workspace.refresh_runtime_contract"),
		attribute.String("account_id", call.accountID.String()),
		attribute.String("service_id", call.serviceID.String()),
		attribute.String("service_version_id", call.serviceVersionID.String()),
	}
}

func listMissingContractVersions(ctx context.Context, s store.Store, limit int) ([]store.WorkspaceServiceVersion, error) {
	backfillStore, ok := s.(store.WorkspaceServiceVersionContractBackfillStore)
	if !ok {
		return nil, refreshHTTPError{status: http.StatusInternalServerError, message: "contract backfill lookup unavailable"}
	}
	// Why the store owns the anti-join: missing snapshot detection must stay a
	// bounded SQL query, not a full workspace activation scan filtered in Go.
	versions, err := backfillStore.ListWorkspaceServiceVersionsMissingContractSnapshots(ctx, limit)
	if err != nil {
		return nil, refreshHTTPError{status: http.StatusInternalServerError, message: "failed to list missing runtime contract snapshots"}
	}
	return versions, nil
}

func fetchMissingSnapshots(ctx context.Context, batchFetcher BatchRuntimeContractFetcher, versions []store.WorkspaceServiceVersion, apiKey string) ([]store.ServiceContractSnapshot, error) {
	if batchFetcher == nil {
		return nil, refreshHTTPError{status: http.StatusInternalServerError, message: "runtime contract batch fetcher unavailable"}
	}
	// Why batch is required here: this endpoint can cover many old activations,
	// so falling back to one Registry request per row would turn rollout into an
	// N+1 network path.
	snapshots, err := batchFetcher.FetchRuntimeContracts(ctx, versions, apiKey)
	if err != nil {
		return nil, refreshHTTPError{status: http.StatusBadGateway, message: "failed to fetch runtime contracts from registry"}
	}
	return snapshots, nil
}

func writeMissingSnapshots(ctx context.Context, s store.Store, snapshots []store.ServiceContractSnapshot, response *refreshMissingContractsResponse) {
	writer, err := refreshSnapshotWriter(s)
	if err != nil {
		appendMissingSnapshotError(snapshots, response, err)
		return
	}
	for _, snapshot := range snapshots {
		saved, err := writer.UpsertServiceContractSnapshot(ctx, snapshot)
		if err != nil {
			appendMissingSnapshotError([]store.ServiceContractSnapshot{snapshot}, response, err)
			continue
		}
		response.Refreshed++
		response.Results = append(response.Results, refreshResultFromSnapshot(saved, ""))
	}
}

func appendMissingSnapshotError(snapshots []store.ServiceContractSnapshot, response *refreshMissingContractsResponse, err error) {
	for _, snapshot := range snapshots {
		response.Failed++
		response.Results = append(response.Results, refreshResultFromSnapshot(&snapshot, err.Error()))
	}
}

func getRefreshWorkspaceServiceVersion(ctx context.Context, s store.Store, call refreshPinnedServiceContractCall) (*store.WorkspaceServiceVersion, error) {
	lookup, ok := s.(store.WorkspaceServiceVersionLookupStore)
	if !ok {
		return nil, refreshHTTPError{status: http.StatusInternalServerError, message: "workspace service version lookup unavailable"}
	}
	version, err := lookup.GetWorkspaceServiceVersion(ctx, call.serviceID, call.serviceVersionID)
	if errors.Is(err, store.ErrWorkspaceServiceVersionNotFound) {
		return nil, refreshHTTPError{status: http.StatusNotFound, message: "workspace service version is not active"}
	}
	if err != nil {
		return nil, refreshHTTPError{status: http.StatusInternalServerError, message: "failed to load workspace service version"}
	}
	return version, nil
}

func refreshResultFromSnapshot(snapshot *store.ServiceContractSnapshot, err string) refreshServiceContractResult {
	return refreshServiceContractResult{
		ServiceID:        snapshot.ServiceID.String(),
		ServiceVersionID: snapshot.ServiceVersionID.String(),
		Version:          snapshot.Version,
		ContractHash:     snapshot.ContractHash,
		Error:            err,
	}
}

func refreshSnapshotWriter(s store.Store) (runtimeContractSnapshotWriter, error) {
	writer, ok := s.(runtimeContractSnapshotWriter)
	if !ok {
		return nil, refreshHTTPError{status: http.StatusInternalServerError, message: "runtime contract snapshot store unavailable"}
	}
	return writer, nil
}

func fetchRefreshSnapshot(ctx context.Context, fetcher RuntimeContractFetcher, call refreshPinnedServiceContractCall, version string) (*store.ServiceContractSnapshot, error) {
	if fetcher == nil {
		return nil, refreshHTTPError{status: http.StatusInternalServerError, message: "runtime contract fetcher unavailable"}
	}
	// Why fetch before writing: refresh must preserve the last good local
	// snapshot when Registry is unavailable or returns an invalid projection.
	snapshot, err := fetcher.FetchRuntimeContract(ctx, call.serviceID, call.serviceVersionID, version, call.apiKey)
	if err != nil {
		slog.WarnContext(ctx, "refresh runtime contract fetch failed",
			slog.String("service_id", call.serviceID.String()),
			slog.String("service_version_id", call.serviceVersionID.String()),
			slog.Any("error", err))
		return nil, refreshHTTPError{status: http.StatusBadGateway, message: "failed to fetch runtime contract from registry"}
	}
	return snapshot, nil
}

func writeRefreshServiceContractError(w http.ResponseWriter, err error) {
	var httpErr refreshHTTPError
	if !errors.As(err, &httpErr) {
		httpErr = refreshHTTPError{status: http.StatusInternalServerError, message: "refresh failed"}
	}
	http.Error(w, fmt.Sprintf(`{"error":%q}`, httpErr.message), httpErr.status)
}

func writeRefreshServiceContractResponse(w http.ResponseWriter, snapshot *store.ServiceContractSnapshot) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(refreshServiceContractResponse{
		Status:           "refreshed",
		ServiceID:        snapshot.ServiceID.String(),
		ServiceVersionID: snapshot.ServiceVersionID.String(),
		Version:          snapshot.Version,
		ContractHash:     snapshot.ContractHash,
	})
}
