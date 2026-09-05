package api

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"sync"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/trace"
)

const (
	// Pair fallback uses the largest batch size proven by the live failure profile while avoiding singleton N+1 recovery.
	missingSnapshotFallbackBatchSize = 2
	// Two adjacent generic failures distinguish an outage from one isolated failed partition.
	missingSnapshotConsecutiveFailLimit = 2
	// Two concurrent partitions fit the outage probe bound and shorten healthy fallback without flooding Registry.
	missingSnapshotPartitionConcurrency = 2
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
	ErrorMessage     string `json:"error_message,omitempty"`
}

type missingSnapshotFetchFailure struct {
	version      store.WorkspaceServiceVersion
	code         string
	errorMessage string
}

type missingSnapshotBatchResult struct {
	snapshots     []store.ServiceContractSnapshot
	failures      []missingSnapshotFetchFailure
	unresolved    []store.WorkspaceServiceVersion
	err           error
	partitionable bool
	genericFails  int
}

type refreshServiceContractPath struct {
	serviceID        uuid.UUID
	serviceVersionID uuid.UUID
}

type refreshHTTPError struct {
	status          int
	message         string
	phase           string
	commitState     string
	rejectedVersion uuid.UUID
	rejectedMessage string
}

func (e refreshHTTPError) Error() string {
	return e.message
}

// RefreshServiceContractHandler refreshes one pinned local runtime snapshot and
// reports every failure through the shared structured envelope.
func RefreshServiceContractHandler(s store.Store, fetcher RuntimeContractFetcher) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx := r.Context()
		accountID, err := controlActorAccount(ctx)
		// Authentication fails before any Registry fetch or snapshot mutation.
		if err != nil {
			writeWorkspaceConfigError(w, withWorkspaceConfigErrorMetadata(workspaceConfigHTTPError{
				status: http.StatusUnauthorized, code: "authentication_required", message: "Authentication is required to refresh a runtime contract.", remediation: "Log in or provide a valid Fused credential.",
			}, "runtime_contract_refresh_admission", "", "not_committed"), ctx)
			return
		}
		path, err := parseRefreshServiceContractPath(r)
		// Malformed service identity cannot select a snapshot and is safe to correct locally.
		if err != nil {
			writeRefreshServiceContractError(w, ctx, err)
			return
		}
		// Workspace verification precedes the Registry fetch and local upsert.
		if err := verifyWorkspaceActor(ctx, accountID); err != nil {
			writeRefreshServiceContractError(w, ctx, refreshHTTPError{status: http.StatusInternalServerError, message: "workspace not found"})
			return
		}
		snapshot, err := refreshPinnedServiceContract(ctx, s, fetcher, refreshPinnedServiceContractCall{
			accountID:        accountID,
			serviceID:        path.serviceID,
			serviceVersionID: path.serviceVersionID,
			apiKey:           r.Header.Get("X-API-Key"),
		})
		// Refresh errors are classified only after their typed status is projected to
		// safe public copy and pre-commit mutation metadata.
		if err != nil {
			writeRefreshServiceContractError(w, ctx, err)
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
		// A store write or commit error cannot prove whether the replacement became durable.
		return nil, refreshHTTPError{status: http.StatusInternalServerError, message: "failed to store runtime contract snapshot", phase: "runtime_contract_refresh_commit", commitState: "unknown"}
	}
	span.SetAttributes(attribute.String("outcome", "success"))
	return saved, nil
}

// refreshMissingServiceContracts refreshes every independently admitted snapshot while retaining exact failures in the batch response.
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
	snapshots, failures, err := fetchMissingSnapshots(ctx, batchFetcher, versions, call.apiKey)
	// Infrastructure and ambiguous identity failures still abort before any snapshot write.
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "fetch_failed"))
		// The stable HTTP response stays generic while the trace retains the dependency cause for operators.
		span.SetStatus(codes.Error, "runtime contract fetch failed")
		return nil, err
	}
	writeMissingSnapshots(ctx, s, snapshots, response)
	appendMissingSnapshotFetchFailures(failures, response)
	outcome := "success"
	// A completed batch with exact rejected versions is partial rather than a dependency failure.
	if response.Failed > 0 {
		response.Status = "partial"
		outcome = "partial"
	}
	span.SetAttributes(attribute.String("outcome", outcome), attribute.Int("refreshed", response.Refreshed), attribute.Int("failed", response.Failed))
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

// fetchMissingSnapshots preserves typed rejection isolation and partitions a failed oversized dependency request once.
func fetchMissingSnapshots(ctx context.Context, batchFetcher BatchRuntimeContractFetcher, versions []store.WorkspaceServiceVersion, apiKey string) ([]store.ServiceContractSnapshot, []missingSnapshotFetchFailure, error) {
	// Missing capability is not an empty batch.
	if batchFetcher == nil {
		return nil, nil, refreshHTTPError{status: http.StatusInternalServerError, message: "runtime contract batch fetcher unavailable"}
	}
	result := fetchMissingSnapshotBatch(ctx, batchFetcher, versions, apiKey)
	// One complete response or an all-typed-rejection batch needs no generic recovery.
	if result.err == nil {
		return result.snapshots, result.failures, nil
	}
	recordMissingSnapshotFetchError(ctx, result.err, len(result.unresolved), "initial")
	// Identity errors, caller cancellation, and already-small failures have no safe or useful partition fallback.
	if !result.partitionable || len(result.unresolved) <= missingSnapshotFallbackBatchSize || ctx.Err() != nil {
		return nil, nil, refreshFetchFailure(result.err)
	}
	partitioned := fetchMissingSnapshotPartitions(ctx, batchFetcher, result.unresolved, apiKey)
	// A partition circuit-breaker failure aborts before any accumulated snapshot is written.
	if partitioned.err != nil {
		return nil, nil, refreshFetchFailure(partitioned.err)
	}
	return partitioned.snapshots, append(result.failures, partitioned.failures...), nil
}

// fetchMissingSnapshotBatch removes only exact typed rejections and exposes generic dependency failures for bounded recovery.
func fetchMissingSnapshotBatch(ctx context.Context, batchFetcher BatchRuntimeContractFetcher, versions []store.WorkspaceServiceVersion, apiKey string) missingSnapshotBatchResult {
	remaining := append([]store.WorkspaceServiceVersion(nil), versions...)
	result := missingSnapshotBatchResult{failures: make([]missingSnapshotFetchFailure, 0)}
	for len(remaining) > 0 {
		// Valid peers remain together until Registry names an exact rejected immutable version.
		snapshots, err := batchFetcher.FetchRuntimeContracts(ctx, remaining, apiKey)
		if err == nil {
			// A successful response must account for every exact requested identity before any write is admitted.
			if identityErr := validateMissingSnapshotIdentities(remaining, snapshots); identityErr != nil {
				result.err = identityErr
				return result
			}
			result.snapshots = snapshots
			return result
		}
		rejectedVersionID, rejectedMessage, rejected := sandbox.RuntimeContractRejectionDetails(err)
		// Only an untyped dependency failure is eligible for the separate size-based fallback.
		if !rejected {
			result.unresolved = remaining
			result.err = err
			result.partitionable = true
			result.genericFails = 1
			return result
		}
		var rejectedVersion store.WorkspaceServiceVersion
		remaining, rejectedVersion, err = removeRejectedMissingVersion(remaining, rejectedVersionID)
		// Missing or duplicated rejection identity remains fatal rather than excusing unrelated versions.
		if err != nil {
			result.err = err
			return result
		}
		result.failures = append(result.failures, missingSnapshotFetchFailure{version: rejectedVersion, code: "runtime_contract_rejected", errorMessage: rejectedMessage})
	}
	return result
}

// fetchMissingSnapshotPartitions retries fixed pairs in bounded concurrent waves and stops quickly during an outage.
func fetchMissingSnapshotPartitions(ctx context.Context, batchFetcher BatchRuntimeContractFetcher, versions []store.WorkspaceServiceVersion, apiKey string) missingSnapshotBatchResult {
	result := missingSnapshotBatchResult{
		snapshots: make([]store.ServiceContractSnapshot, 0, len(versions)),
		failures:  make([]missingSnapshotFetchFailure, 0),
	}
	partitions := missingSnapshotVersionPartitions(versions)
	consecutiveFailures := 0
	// Each wave completes before another begins so a broad outage cannot fan out past the two-probe ceiling.
	for waveStart := 0; waveStart < len(partitions); waveStart += missingSnapshotPartitionConcurrency {
		waveEnd := waveStart + missingSnapshotPartitionConcurrency
		// The final wave may have one partition while every earlier wave retains the two-probe ceiling.
		if waveEnd > len(partitions) {
			waveEnd = len(partitions)
		}
		wave := fetchMissingSnapshotPartitionWave(ctx, batchFetcher, partitions[waveStart:waveEnd], apiKey)
		// Source-order processing keeps adjacent-failure and response ordering deterministic despite concurrent completion.
		for _, partition := range wave {
			partition = splitFailedMissingSnapshotPair(ctx, batchFetcher, partition, apiKey)
			var partitionErr error
			consecutiveFailures, partitionErr = mergeMissingSnapshotPartition(ctx, &result, partition, consecutiveFailures)
			// A fatal or circuit-breaking partition stops before the caller persists any accumulated snapshots.
			if partitionErr != nil {
				result.err = partitionErr
				return result
			}
		}
	}
	return result
}

// splitFailedMissingSnapshotPair resolves a generic pair failure with at most two concurrent exact singleton probes.
func splitFailedMissingSnapshotPair(ctx context.Context, batchFetcher BatchRuntimeContractFetcher, partition missingSnapshotBatchResult, apiKey string) missingSnapshotBatchResult {
	// Successful, terminal, caller-canceled, and already-singleton results have no safe or useful narrower boundary.
	if partition.err == nil || !partition.partitionable || len(partition.unresolved) != missingSnapshotFallbackBatchSize || ctx.Err() != nil {
		return partition
	}
	recordMissingSnapshotFetchError(ctx, partition.err, len(partition.unresolved), "partition_pair")
	requests := [][]store.WorkspaceServiceVersion{{partition.unresolved[0]}, {partition.unresolved[1]}}
	probes := fetchMissingSnapshotPartitionWave(ctx, batchFetcher, requests, apiKey)
	resolved := missingSnapshotBatchResult{
		snapshots: make([]store.ServiceContractSnapshot, 0, len(partition.unresolved)),
		failures:  append([]missingSnapshotFetchFailure(nil), partition.failures...),
	}
	var lastGenericErr error
	// Source-order merging keeps recovered snapshots and exact failures deterministic despite concurrent probes.
	for _, probe := range probes {
		// A terminal singleton retains its original classification instead of being projected as an exact retryable failure.
		if mergeMissingSnapshotSingletonProbe(ctx, &resolved, probe) {
			return probe
		}
		// Only a generic probe error contributes an outage cause; resolved probes leave the prior cause unchanged.
		if probe.err != nil {
			lastGenericErr = probe.err
		}
	}
	// Two failed singleton probes establish the same adjacent-failure outage boundary as two failed pair partitions.
	if resolved.genericFails >= missingSnapshotConsecutiveFailLimit {
		resolved.err = lastGenericErr
		resolved.partitionable = true
		return resolved
	}
	return resolved
}

// mergeMissingSnapshotSingletonProbe adds one exact probe result and reports whether its failure is terminal.
func mergeMissingSnapshotSingletonProbe(ctx context.Context, resolved *missingSnapshotBatchResult, probe missingSnapshotBatchResult) bool {
	resolved.failures = append(resolved.failures, probe.failures...)
	// A validated snapshot or exact typed rejection proves this singleton was resolved without generic failure.
	if probe.err == nil {
		resolved.snapshots = append(resolved.snapshots, probe.snapshots...)
		return false
	}
	recordMissingSnapshotFetchError(ctx, probe.err, len(probe.unresolved), "partition_singleton")
	// Identity ambiguity and caller cancellation remain fatal instead of becoming an exact singleton failure.
	if !probe.partitionable || ctx.Err() != nil {
		return true
	}
	resolved.genericFails++
	resolved.unresolved = append(resolved.unresolved, probe.unresolved...)
	// A singleton generic result can name only its one requested immutable version.
	for _, unresolved := range probe.unresolved {
		resolved.failures = append(resolved.failures, missingSnapshotFetchFailure{version: unresolved, code: "runtime_contract_fetch_failed", errorMessage: "The Engine could not fetch this runtime contract from Registry."})
	}
	return false
}

// mergeMissingSnapshotPartition preserves source-order results and advances the bounded outage circuit for one completed partition.
func mergeMissingSnapshotPartition(ctx context.Context, result *missingSnapshotBatchResult, partition missingSnapshotBatchResult, consecutiveFailures int) (int, error) {
	result.failures = append(result.failures, partition.failures...)
	// Exact identity success resets the outage circuit and admits only this partition's validated snapshots.
	if partition.err == nil {
		result.snapshots = append(result.snapshots, partition.snapshots...)
		return 0, nil
	}
	recordMissingSnapshotFetchError(ctx, partition.err, len(partition.unresolved), "partition")
	// Identity ambiguity and caller cancellation must stop rather than becoming per-version failure output.
	if !partition.partitionable || ctx.Err() != nil {
		return consecutiveFailures, partition.err
	}
	failureCount := partition.genericFails
	// Legacy or independently constructed generic results still consume one outage-circuit failure.
	if failureCount < 1 {
		failureCount = 1
	}
	consecutiveFailures += failureCount
	// Two adjacent dependency failures are treated as an outage, bounding fallback amplification to one concurrent wave.
	if consecutiveFailures >= missingSnapshotConsecutiveFailLimit {
		return consecutiveFailures, partition.err
	}
	for _, unresolved := range partition.unresolved {
		// A failed partition reports no content; its exact requested identities remain safe to retry later.
		result.failures = append(result.failures, missingSnapshotFetchFailure{version: unresolved, code: "runtime_contract_fetch_failed", errorMessage: "The Engine could not fetch this runtime contract from Registry."})
	}
	return consecutiveFailures, nil
}

// missingSnapshotVersionPartitions retains source order while grouping fallback requests into proven two-version batches.
func missingSnapshotVersionPartitions(versions []store.WorkspaceServiceVersion) [][]store.WorkspaceServiceVersion {
	partitions := make([][]store.WorkspaceServiceVersion, 0, (len(versions)+missingSnapshotFallbackBatchSize-1)/missingSnapshotFallbackBatchSize)
	for start := 0; start < len(versions); start += missingSnapshotFallbackBatchSize {
		end := start + missingSnapshotFallbackBatchSize
		// The final partition may contain one version while every earlier request retains pair batching.
		if end > len(versions) {
			end = len(versions)
		}
		partitions = append(partitions, versions[start:end])
	}
	return partitions
}

// fetchMissingSnapshotPartitionWave runs at most two independent Registry partitions and returns results in source order.
func fetchMissingSnapshotPartitionWave(ctx context.Context, batchFetcher BatchRuntimeContractFetcher, partitions [][]store.WorkspaceServiceVersion, apiKey string) []missingSnapshotBatchResult {
	results := make([]missingSnapshotBatchResult, len(partitions))
	var wait sync.WaitGroup
	// The caller supplies at most one bounded wave, so this loop cannot exceed the shared concurrency ceiling.
	for index, partition := range partitions {
		wait.Add(1)
		// Each goroutine owns one result slot, so completion order cannot reorder response or circuit-breaker semantics.
		go func(resultIndex int, requested []store.WorkspaceServiceVersion) {
			defer wait.Done()
			results[resultIndex] = fetchMissingSnapshotBatch(ctx, batchFetcher, requested, apiKey)
		}(index, partition)
	}
	// Joining the bounded wave prevents writes and failure classification before every in-flight request has a stable result.
	wait.Wait()
	return results
}

// recordMissingSnapshotFetchError retains the private dependency cause with bounded request context in OpenTelemetry.
func recordMissingSnapshotFetchError(ctx context.Context, err error, batchSize int, phase string) {
	// Missing errors have no diagnostic value and should not create synthetic exception events.
	if err == nil {
		return
	}
	trace.SpanFromContext(ctx).RecordError(err, trace.WithAttributes(
		attribute.String("runtime_contract.fetch_phase", phase),
		attribute.Int("runtime_contract.batch_size", batchSize),
	))
}

// removeRejectedMissingVersion consumes exactly one Registry-identified version without changing peer order.
func removeRejectedMissingVersion(versions []store.WorkspaceServiceVersion, rejectedVersionID uuid.UUID) ([]store.WorkspaceServiceVersion, store.WorkspaceServiceVersion, error) {
	remaining := make([]store.WorkspaceServiceVersion, 0, len(versions)-1)
	var rejected store.WorkspaceServiceVersion
	matches := 0
	for _, version := range versions {
		// Only the exact immutable version named by the typed rejection leaves the next batch.
		if version.ServiceVersionID == rejectedVersionID {
			rejected = version
			matches++
			continue
		}
		remaining = append(remaining, version)
	}
	// One-to-one identity prevents missing and duplicate local pins from becoming silent failures.
	if matches != 1 {
		return nil, store.WorkspaceServiceVersion{}, errors.New("runtime contract rejection identity does not match exactly one requested version")
	}
	return remaining, rejected, nil
}

// validateMissingSnapshotIdentities requires a complete disjoint response for the final successful batch.
func validateMissingSnapshotIdentities(versions []store.WorkspaceServiceVersion, snapshots []store.ServiceContractSnapshot) error {
	expected := make(map[uuid.UUID]uuid.UUID, len(versions))
	for _, version := range versions {
		// Empty or duplicate local identities cannot authorize a Registry snapshot write.
		if version.ServiceID == uuid.Nil || version.ServiceVersionID == uuid.Nil {
			return errors.New("missing runtime contract request identity")
		}
		// A repeated version ID would let one returned snapshot satisfy multiple local rows.
		if _, duplicate := expected[version.ServiceVersionID]; duplicate {
			return errors.New("duplicate runtime contract request identity")
		}
		expected[version.ServiceVersionID] = version.ServiceID
	}
	for _, snapshot := range snapshots {
		serviceID, found := expected[snapshot.ServiceVersionID]
		// Missing, substituted, and duplicate response identities all fail before persistence.
		if !found || serviceID != snapshot.ServiceID {
			return errors.New("runtime contract response identity mismatch")
		}
		delete(expected, snapshot.ServiceVersionID)
	}
	// Every requested version needs one exact accepted snapshot.
	if len(expected) != 0 {
		return errors.New("runtime contract response is incomplete")
	}
	return nil
}

// appendMissingSnapshotFetchFailures projects stable codes and reviewed public messages beside exact requested identities.
func appendMissingSnapshotFetchFailures(failures []missingSnapshotFetchFailure, response *refreshMissingContractsResponse) {
	for _, failure := range failures {
		response.Failed++
		response.Results = append(response.Results, refreshServiceContractResult{
			ServiceID: failure.version.ServiceID.String(), ServiceVersionID: failure.version.ServiceVersionID.String(),
			Version: failure.version.Version, Error: failure.code, ErrorMessage: failure.errorMessage,
		})
	}
}

// writeMissingSnapshots persists independently admitted contracts and reports stable write failures per immutable version.
func writeMissingSnapshots(ctx context.Context, s store.Store, snapshots []store.ServiceContractSnapshot, response *refreshMissingContractsResponse) {
	writer, err := refreshSnapshotWriter(s)
	// Missing storage capability fails each admitted snapshot without exposing implementation details.
	if err != nil {
		appendMissingSnapshotError(snapshots, response)
		return
	}
	for _, snapshot := range snapshots {
		saved, err := writer.UpsertServiceContractSnapshot(ctx, snapshot)
		// A per-version write failure leaves peers eligible for independent persistence.
		if err != nil {
			appendMissingSnapshotError([]store.ServiceContractSnapshot{snapshot}, response)
			continue
		}
		response.Refreshed++
		response.Results = append(response.Results, refreshResultFromSnapshot(saved, "", ""))
	}
}

// appendMissingSnapshotError reports a stable storage category without returning raw persistence errors.
func appendMissingSnapshotError(snapshots []store.ServiceContractSnapshot, response *refreshMissingContractsResponse) {
	for _, snapshot := range snapshots {
		response.Failed++
		response.Results = append(response.Results, refreshResultFromSnapshot(&snapshot, "runtime_contract_store_failed", "The Engine could not store this runtime contract snapshot."))
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

// refreshResultFromSnapshot projects one persisted or failed snapshot through stable public error fields.
func refreshResultFromSnapshot(snapshot *store.ServiceContractSnapshot, errorCode, errorMessage string) refreshServiceContractResult {
	return refreshServiceContractResult{
		ServiceID:        snapshot.ServiceID.String(),
		ServiceVersionID: snapshot.ServiceVersionID.String(),
		Version:          snapshot.Version,
		ContractHash:     snapshot.ContractHash,
		Error:            errorCode,
		ErrorMessage:     errorMessage,
	}
}

func refreshSnapshotWriter(s store.Store) (runtimeContractSnapshotWriter, error) {
	writer, ok := s.(runtimeContractSnapshotWriter)
	if !ok {
		return nil, refreshHTTPError{status: http.StatusInternalServerError, message: "runtime contract snapshot store unavailable"}
	}
	return writer, nil
}

// fetchRefreshSnapshot retains the canonical rejection category before any snapshot replacement.
func fetchRefreshSnapshot(ctx context.Context, fetcher RuntimeContractFetcher, call refreshPinnedServiceContractCall, version string) (*store.ServiceContractSnapshot, error) {
	// A missing fetcher cannot prove that the pinned contract is valid.
	if fetcher == nil {
		return nil, refreshHTTPError{status: http.StatusInternalServerError, message: "runtime contract fetcher unavailable"}
	}
	// Why fetch before writing: refresh must preserve the last good local
	// snapshot when Registry is unavailable or returns an invalid projection.
	snapshot, err := fetcher.FetchRuntimeContract(ctx, call.serviceID, call.serviceVersionID, version, call.apiKey)
	// Do not replace a last-good snapshot when fetch or admission fails.
	if err != nil {
		slog.WarnContext(ctx, "refresh runtime contract fetch failed",
			slog.String("service_id", call.serviceID.String()),
			slog.String("service_version_id", call.serviceVersionID.String()),
			slog.Any("error", err))
		return nil, refreshFetchFailure(err)
	}
	return snapshot, nil
}

// refreshFetchFailure distinguishes authoritative content rejection from a retryable Registry outage.
func refreshFetchFailure(err error) refreshHTTPError {
	// Only the canonical typed decoder may supply source-repair classification.
	if version, reason, rejected := sandbox.RuntimeContractRejectionDetails(err); rejected {
		return refreshHTTPError{status: http.StatusUnprocessableEntity, message: "runtime_contract_rejected", rejectedVersion: version, rejectedMessage: reason}
	}
	return refreshHTTPError{status: http.StatusBadGateway, message: "failed to fetch runtime contract from registry"}
}

// writeRefreshServiceContractError converts refresh-local failures into one
// stable public contract without exposing persistence or Registry prose.
func writeRefreshServiceContractError(w http.ResponseWriter, ctx context.Context, err error) {
	var httpErr refreshHTTPError
	// Unknown failures receive the same generic internal classification as typed
	// store failures, while their underlying prose remains private.
	if !errors.As(err, &httpErr) {
		httpErr = refreshHTTPError{status: http.StatusInternalServerError, message: "refresh failed"}
	}
	publicErr := refreshWorkspaceConfigError(httpErr)
	phase, commitState := httpErr.phase, httpErr.commitState
	// Admission, lookup, and dependency failures occur before the snapshot store write.
	if phase == "" {
		phase = "runtime_contract_refresh"
	}
	if commitState == "" {
		commitState = "not_committed"
	}
	writeWorkspaceConfigError(w, withWorkspaceConfigErrorMetadata(publicErr, phase, "", commitState), ctx)
}

// refreshWorkspaceConfigError maps refresh-local status and safe validation
// text to stable automation codes while replacing every 5xx message.
func refreshWorkspaceConfigError(err refreshHTTPError) workspaceConfigHTTPError {
	// A known rejected version needs repair/re-import, never outage retries.
	if err.rejectedVersion != uuid.Nil {
		return workspaceConfigHTTPError{status: http.StatusUnprocessableEntity, code: "runtime_contract_rejected", category: "validation", message: "Registry rejected the runtime contract for this service version.", remediation: "Repair and re-import the rejected service version, then refresh its runtime contract.", details: map[string]any{"service_version_id": err.rejectedVersion.String(), "server_detail": err.rejectedMessage}}
	}
	// Client-correctable path validation retains the precise field diagnosis.
	if err.status == http.StatusBadRequest {
		// Only the two local route-parser diagnostics are safe to preserve verbatim.
		switch err.message {
		case "service id must be a valid UUID":
			return workspaceConfigHTTPError{status: err.status, code: "invalid_service_id", message: err.message, remediation: "Use the exact service ID shown by workspace service commands."}
		case "service_version_id must be a valid UUID":
			return workspaceConfigHTTPError{status: err.status, code: "invalid_service_version_id", message: err.message, remediation: "Use the exact service-version ID shown by workspace service commands."}
		default:
			return workspaceConfigHTTPError{status: err.status, code: "invalid_runtime_contract_refresh_request", message: "The runtime contract refresh request is invalid.", remediation: "Use exact service and service-version IDs from workspace service commands."}
		}
	}
	// An absent active pin is authoritative and requires workspace correction, not retry.
	if err.status == http.StatusNotFound {
		return workspaceConfigHTTPError{status: err.status, code: "runtime_contract_not_active", message: "The selected workspace service version is not active.", remediation: "Activate that exact service version before refreshing its runtime contract."}
	}
	// Registry dependency failures are retryable but never expose downstream prose.
	if err.status == http.StatusBadGateway {
		return workspaceConfigHTTPError{status: err.status, code: "runtime_contract_dependency_unavailable", message: "The Engine could not fetch the runtime contract.", category: "dependency", retryable: true, remediation: "Retry and check Registry availability if the problem continues."}
	}
	return workspaceConfigHTTPError{status: http.StatusInternalServerError, code: "runtime_contract_refresh_failed", message: "The Engine could not refresh the runtime contract.", category: "internal", retryable: true, remediation: "Retry and check Engine logs if the problem continues."}
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
