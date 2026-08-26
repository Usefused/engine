package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

const (
	// Import replay and workspace activation may outlast the ordinary server
	// write deadline; retain a finite operation budget matching CLI imports.
	importApplyExecutionTimeout = 20 * time.Minute
	// Durable control audit completes after the proxy handler returns, so its
	// final response needs a small window beyond the work deadline.
	importApplyResponseGrace = 10 * time.Second
)

// prepareImportApplyResponseWindow changes only this admitted apply request,
// retaining caller cancellation and any earlier request-owned deadline.
func prepareImportApplyResponseWindow(w http.ResponseWriter, r *http.Request) (*http.Request, context.CancelFunc, error) {
	ctx, cancel := context.WithTimeout(r.Context(), importApplyExecutionTimeout)
	deadline, _ := ctx.Deadline()
	err := http.NewResponseController(w).SetWriteDeadline(deadline.Add(importApplyResponseGrace))
	// In-memory response writers have no socket deadline to replace. Production
	// HTTP writers support the controller through middleware Unwrap methods.
	if errors.Is(err, http.ErrNotSupported) {
		err = nil
	}
	return r.WithContext(ctx), cancel, err
}

// writeImportResponseWindowFailure makes response admission fail safely without
// guessing the outcome of an earlier attempt represented by the same receipt.
func writeImportResponseWindowFailure(w http.ResponseWriter, ctx context.Context) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusServiceUnavailable)
	_ = json.NewEncoder(w).Encode(workspaceConfigErrorResponse{Error: workspaceConfigErrorBody{
		Code: "import_response_deadline_unavailable", Message: "Engine could not prepare the import response window; this request was not forwarded.",
		Category: "unavailable", Phase: "request_admission", RequestID: chimiddleware.GetReqID(ctx),
		CommitState: "unknown", Recovery: "fused-cli import status --help",
		Remediation: "Check any earlier apply using its operation ID before retrying with the original review receipt.",
	}})
}
