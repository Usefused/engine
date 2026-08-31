package sandbox

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

const (
	// MaxImportPreflightRequestBytes bounds the immutable plan receipt passed
	// through Engine before Registry replays the separately stored source.
	MaxImportPreflightRequestBytes = 16 << 10
	// Registry error envelopes are diagnostic metadata rather than contracts.
	maxImportPreflightErrorBytes = 64 << 10
)

var (
	errImportPreflightResponseLimit = errors.New("import_preflight_response_limit_exceeded")
	// ErrImportRuntimeContractRejected distinguishes deterministic Engine
	// admission from Registry transport or availability failures.
	ErrImportRuntimeContractRejected = errors.New("import_runtime_contract_rejected")
)

// ImportContractPreflight is the validated, non-persisted Engine admission
// result that authorizes forwarding the matching Registry apply.
type ImportContractPreflight struct {
	OperationID  uuid.UUID
	ContractHash string
	Snapshot     store.ServiceContractSnapshot
}

// ImportPreflightHTTPError retains only Registry's bounded structured error so
// Engine can preserve the exact pre-commit recovery contract for the caller.
type ImportPreflightHTTPError struct {
	StatusCode int
	Body       []byte
}

// Error keeps logs useful without copying a potentially sensitive upstream body.
func (e *ImportPreflightHTTPError) Error() string {
	return fmt.Sprintf("Registry import preflight returned HTTP %d", e.StatusCode)
}

type importPreflightResponse struct {
	OperationID  string          `json:"operation_id"`
	Phase        string          `json:"phase"`
	CommitState  string          `json:"commit_state"`
	ContractHash string          `json:"contract_hash"`
	Contract     json.RawMessage `json:"contract"`
}

// PreflightImport asks Registry to replay the reviewed plan without mutation,
// verifies its proof, and runs the ordinary Engine runtime admission pipeline.
func (c *HTTPRegistryClient) PreflightImport(ctx context.Context, applyBody []byte) (*ImportContractPreflight, error) {
	// The public apply shape is intentionally tiny; accepting more would create
	// a second unbounded source-ingestion route through Engine.
	if len(applyBody) == 0 || len(applyBody) > MaxImportPreflightRequestBytes {
		return nil, errors.New("import preflight request is empty or exceeds 16 KiB")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, c.registryBaseURL()+"/integrations/import/preflight", bytes.NewReader(applyBody))
	// Request construction must fail before either preflight or publication reaches Registry.
	if err != nil {
		return nil, fmt.Errorf("create import preflight request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := c.doWithCallerDeadline(request)
	// The enclosing import context owns the long-running replay deadline; a
	// transport failure never falls through to the mutating apply request.
	if err != nil {
		return nil, fmt.Errorf("run import preflight request: %w", err)
	}
	defer response.Body.Close()
	// Registry owns typed plan/provenance failures, so preserve its bounded body
	// instead of collapsing them into an Engine-generated generic message.
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		body, readErr := readBoundedImportPreflightBody(response.Body, maxImportPreflightErrorBytes)
		// An incomplete error envelope cannot safely claim a known recovery state.
		if readErr != nil {
			return nil, readErr
		}
		return nil, &ImportPreflightHTTPError{StatusCode: response.StatusCode, Body: body}
	}
	return decodeImportPreflightResponse(response.Body)
}

// decodeImportPreflightResponse validates proof bytes before interpreting any
// provider contract as executable Engine state.
func decodeImportPreflightResponse(body io.Reader) (*ImportContractPreflight, error) {
	payload, err := readBoundedImportPreflightBody(body, maxRuntimeContractsResponseBytes)
	// Oversized or truncated candidates have not passed Engine admission.
	if err != nil {
		return nil, err
	}
	var response importPreflightResponse
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	// Strict decoding keeps this internal protocol from acquiring silent aliases.
	if err := decoder.Decode(&response); err != nil {
		return nil, errors.New("Registry returned an invalid import preflight response")
	}
	// A second JSON value cannot be ignored because the proof covers one exact candidate.
	if err := decoder.Decode(new(any)); err != io.EOF {
		return nil, errors.New("Registry returned trailing import preflight data")
	}
	return admitImportPreflightResponse(response)
}

// admitImportPreflightResponse binds the Registry proof to the exact candidate
// before reusing normal runtime mapping and semantic validation.
func admitImportPreflightResponse(response importPreflightResponse) (*ImportContractPreflight, error) {
	operationID, err := uuid.Parse(response.OperationID)
	// Only the durable plan identity may correlate a later apply or recovery attempt.
	if err != nil || operationID == uuid.Nil {
		return nil, errors.New("Registry import preflight omitted a valid operation_id")
	}
	// Preflight must never claim a Registry commit before Engine admits the candidate.
	if response.Phase != "engine_preflight" || response.CommitState != "not_committed" {
		return nil, errors.New("Registry import preflight returned an invalid phase or commit state")
	}
	if err := verifyImportPreflightHash(response.ContractHash, response.Contract); err != nil {
		return nil, err
	}
	item, err := decodeImportRuntimeContract(response.Contract)
	// No unknown candidate surface may be covered by the proof but skipped by Engine admission.
	if err != nil {
		return nil, err
	}
	requested := store.WorkspaceServiceVersion{ServiceID: item.ServiceID, ServiceVersionID: item.ServiceVersionID, Version: item.Version}
	indexed, err := indexRuntimeContractBatch([]runtimeContractBatchItem{item}, []store.WorkspaceServiceVersion{requested})
	// Reusing batch identity admission keeps nested service IDs from bypassing
	// the exact ownership checks applied to ordinary Registry snapshot fetches.
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrImportRuntimeContractRejected, err)
	}
	snapshot, err := admittedRuntimeContractSnapshot(indexed[item.ServiceVersionID], requested)
	// Existing runtime admission owns every capability, policy, schema, and transport decision.
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrImportRuntimeContractRejected, err)
	}
	return &ImportContractPreflight{OperationID: operationID, ContractHash: response.ContractHash, Snapshot: *snapshot}, nil
}

// decodeImportRuntimeContract strictly decodes the same top-level DTO used by
// ordinary runtime fetches so every proof-bound field reaches Engine admission.
func decodeImportRuntimeContract(contract json.RawMessage) (runtimeContractBatchItem, error) {
	var item runtimeContractBatchItem
	decoder := json.NewDecoder(bytes.NewReader(contract))
	decoder.DisallowUnknownFields()
	// Preserve the decoder cause for owner-visible Engine logs without exposing the contract body.
	if err := decoder.Decode(&item); err != nil {
		return runtimeContractBatchItem{}, fmt.Errorf("Registry import preflight returned an invalid runtime contract: %w", err)
	}
	// The raw contract must contain exactly one proof-bound object.
	if err := decoder.Decode(new(any)); err != io.EOF {
		return runtimeContractBatchItem{}, errors.New("Registry import preflight returned trailing runtime contract data")
	}
	return item, nil
}

// verifyImportPreflightHash recomputes the compact candidate digest so the
// proof cannot describe different bytes from those Engine actually validates.
func verifyImportPreflightHash(contractHash string, contract json.RawMessage) error {
	if !store.ValidGenerationContractHash(contractHash) {
		return errors.New("Registry import preflight returned an invalid contract hash")
	}
	var compact bytes.Buffer
	// Compacting removes response formatting only; object ordering remains the
	// deterministic Registry wire order covered by its original json.Marshal.
	if err := json.Compact(&compact, contract); err != nil {
		return errors.New("Registry import preflight returned malformed contract JSON")
	}
	digest := sha256.Sum256(compact.Bytes())
	actual := "sha256:" + hex.EncodeToString(digest[:])
	// Hashes are not secrets, but exact comparison is required to bind apply to this candidate.
	if !strings.EqualFold(actual, contractHash) {
		return errors.New("Registry import preflight contract hash does not match its candidate")
	}
	return nil
}

// readBoundedImportPreflightBody reads one complete response while preserving
// the runtime contract ceiling shared with ordinary Registry snapshot fetches.
func readBoundedImportPreflightBody(body io.Reader, limit int64) ([]byte, error) {
	reader := &io.LimitedReader{R: body, N: limit + 1}
	payload, err := io.ReadAll(reader)
	// The sentinel byte distinguishes an exact-limit payload from an overrun.
	if reader.N == 0 {
		return nil, fmt.Errorf("%w: Registry response exceeds %d bytes", errImportPreflightResponseLimit, limit)
	}
	// A partial transport read cannot prove either candidate or commit state.
	if err != nil {
		return nil, errors.New("Registry import preflight response could not be read completely")
	}
	return payload, nil
}
