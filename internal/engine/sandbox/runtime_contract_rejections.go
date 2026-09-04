package sandbox

import (
	"errors"
	"fmt"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

type runtimeContractGraphQLError struct {
	Message    string `json:"message"`
	Extensions struct {
		Code             string    `json:"code"`
		ServiceVersionID uuid.UUID `json:"service_version_id"`
	} `json:"extensions"`
}

// OwnedServiceRejection identifies a non-executable version without exposing contract bytes.
type OwnedServiceRejection struct {
	ServiceID         uuid.UUID
	ServiceVersionID  uuid.UUID
	BlockingVersionID uuid.UUID
	cause             error
}

// runtimeContractRejections is private so only explicit recovery can consume its validated subset.
type runtimeContractRejections struct {
	failures []OwnedServiceRejection
	accepted []store.ServiceContractSnapshot
}

// RuntimeContractRejectionVersion exposes only typed source-repair identity, never Registry payload or error prose.
func RuntimeContractRejectionVersion(err error) (uuid.UUID, bool) {
	var rejection *runtimeContractRejections
	// Unknown transport errors and prose lookalikes cannot claim a permanent contract rejection.
	if !errors.As(err, &rejection) || len(rejection.failures) == 0 {
		return uuid.Nil, false
	}
	return rejection.failures[0].BlockingVersionID, true
}

// Error retains diagnostics for strict callers without converting the operation into success.
func (e *runtimeContractRejections) Error() string {
	return fmt.Sprintf("runtime contract validation rejected %d service version(s): %v", len(e.failures), errors.Join(e.Unwrap()...))
}

// Unwrap preserves compatibility errors for callers offering an Engine upgrade or source repair.
func (e *runtimeContractRejections) Unwrap() []error {
	causes := make([]error, 0, len(e.failures))
	for _, failure := range e.failures {
		causes = append(causes, failure.cause)
	}
	return causes
}

// ownedServiceRejection binds a rejected payload to the originally requested identity.
func ownedServiceRejection(version store.WorkspaceServiceVersion, cause error) OwnedServiceRejection {
	return OwnedServiceRejection{ServiceID: version.ServiceID, ServiceVersionID: version.ServiceVersionID, BlockingVersionID: version.ServiceVersionID, cause: cause}
}

// indexRuntimeContractBatch rejects incomplete or ambiguous identities before any recovery writes.
func indexRuntimeContractBatch(items []runtimeContractBatchItem, versions []store.WorkspaceServiceVersion) (map[uuid.UUID]runtimeContractBatchItem, error) {
	// Reject duplicate requests before equal slice lengths can conceal an unrelated response.
	if _, err := requestedRuntimeContractVersions(versions); err != nil {
		return nil, err
	}
	byVersion := make(map[uuid.UUID]runtimeContractBatchItem, len(items))
	for _, item := range items {
		// Duplicate IDs could otherwise replace a validated contract through response ordering.
		if _, exists := byVersion[item.ServiceVersionID]; exists {
			return nil, errors.New("duplicate runtime contract identity")
		}
		byVersion[item.ServiceVersionID] = item
	}
	// Extra results are not licensed by the requested batch.
	if len(items) != len(versions) {
		return nil, errors.New("runtime contract batch identity mismatch")
	}
	for _, version := range versions {
		item := byVersion[version.ServiceVersionID]
		// Both outer and nested service identities must match the exact requested version.
		if item.Service == nil || item.ServiceID != version.ServiceID || item.ServiceVersionID != version.ServiceVersionID || item.Service.ID != version.ServiceID {
			return nil, fmt.Errorf("FetchRuntimeContracts: service %s version %s not found or identity mismatch", version.ServiceID, version.ServiceVersionID)
		}
	}
	return byVersion, nil
}

// requestedRuntimeContractVersions shares exact request-identity admission across success and GraphQL failure paths.
func requestedRuntimeContractVersions(versions []store.WorkspaceServiceVersion) (map[uuid.UUID]bool, error) {
	requested := make(map[uuid.UUID]bool, len(versions))
	for _, version := range versions {
		// Empty and duplicate identities provide no safe service-level recovery boundary.
		if version.ServiceID == uuid.Nil || version.ServiceVersionID == uuid.Nil || requested[version.ServiceVersionID] {
			return nil, errors.New("invalid or duplicate requested runtime contract identity")
		}
		requested[version.ServiceVersionID] = true
	}
	return requested, nil
}

// admittedRuntimeContractSnapshot reuses the canonical validator rather than weakening startup admission.
func admittedRuntimeContractSnapshot(item runtimeContractBatchItem, requested store.WorkspaceServiceVersion) (*store.ServiceContractSnapshot, error) {
	// Unsupported execution semantics stay non-executable even during recovery.
	if err := fusedobject.ValidateExecutionContractEnvelope(item.ExecutionContractEnvelope); err != nil {
		return nil, err
	}
	return runtimeContractSnapshotFromBatchItem(item, requested)
}

// classifyRuntimeContractGraphQLErrors defers a rejected batch without per-service retry queries.
func classifyRuntimeContractGraphQLErrors(failures []runtimeContractGraphQLError, versions []store.WorkspaceServiceVersion) error {
	requested, err := requestedRuntimeContractVersions(versions)
	// A malformed request cannot receive a trustworthy per-version rejection.
	if err != nil {
		return err
	}
	for _, failure := range failures {
		// Auth, identity, SQL, unknown and mixed failures retain their fatal boundary.
		if failure.Extensions.Code != "runtime_contract_rejected" || !requested[failure.Extensions.ServiceVersionID] {
			return fmt.Errorf("FetchRuntimeContracts: graphql error: %s", failure.Message)
		}
	}
	rejected := &runtimeContractRejections{}
	for _, version := range versions {
		failure := ownedServiceRejection(version, errors.New("Registry rejected the runtime contract batch; repair the reported version before retrying"))
		failure.BlockingVersionID = failures[0].Extensions.ServiceVersionID
		rejected.failures = append(rejected.failures, failure)
	}
	return rejected
}
