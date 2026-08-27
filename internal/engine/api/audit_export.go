package api

import (
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

const defaultAuditExportLimit = 1000

// AuditExportHandler returns a bounded credential-free audit projection and
// emits structured control-plane failures for every rejected export request.
func AuditExportHandler(s store.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.audit.export_http")
		defer span.End()
		actor, ok := accesscontrol.ActorFromContext(ctx)
		// Missing actor context uses the shared authorization envelope with request correlation.
		if !ok {
			accesscontrol.WriteAuthorizationError(w, accesscontrol.ErrAuthenticationRequired, ctx)
			return
		}
		format := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("format")))
		if format == "" {
			format = "csv"
		}
		query, err := auditExportQueryFromRequest(r, actor.SubjectID)
		// Format and filter validation complete before repository access so a caller
		// receives one precise, non-mutating request diagnostic.
		if err != nil || (format != "csv" && format != "jsonl") {
			message, remediation := auditExportRequestDiagnostic(format, err)
			writeControlAPIError(w, ctx, http.StatusBadRequest, "invalid_audit_export", message, remediation)
			return
		}
		repository, ok := s.(store.AuditRepository)
		// An Engine without the audit repository capability cannot safely synthesize
		// an export from another data path.
		if !ok {
			writeControlAPIError(w, ctx, http.StatusInternalServerError, "audit_export_unavailable", "The Engine could not export audit events.", "Retry and check Engine logs if the problem continues.")
			return
		}
		rows, err := repository.ExportAuditEvents(ctx, query)
		// Repository failures remain in logs and traces; the public 5xx response uses
		// fixed prose plus request correlation.
		if err != nil {
			span.RecordError(err)
			writeControlAPIError(w, ctx, http.StatusInternalServerError, "audit_export_unavailable", "The Engine could not export audit events.", "Retry and check Engine logs if the problem continues.")
			return
		}
		span.SetAttributes(attribute.Int("audit.export_rows", len(rows)), attribute.String("audit.export_format", format))
		writeAuditExport(w, format, rows)
	}
}

// auditExportRequestDiagnostic maps only local parser errors to precise safe
// 4xx copy without echoing any query value.
func auditExportRequestDiagnostic(format string, err error) (string, string) {
	// Unsupported formats have a closed, explicit correction independent of filter parsing.
	if format != "csv" && format != "jsonl" {
		return "Audit export format must be csv or jsonl.", "Set format=csv or format=jsonl."
	}
	// Parser errors are Engine-owned sentinels containing field names but no supplied values.
	if err != nil {
		switch err.Error() {
		case "invalid limit":
			return "Audit export limit must be an integer from 1 to 10000.", "Provide a limit from 1 to 10000."
		case "invalid outcome":
			return "Audit export outcome contains an unsupported value.", "Use an audit outcome accepted by the audit activity filters."
		case "invalid actor_subject_id":
			return "Audit export actor_subject_id must be a valid UUID.", "Use an actor subject ID from audit activity."
		case "invalid from":
			return "Audit export from must be an RFC3339 timestamp.", "Provide from as an RFC3339 timestamp."
		case "invalid to":
			return "Audit export to must be an RFC3339 timestamp.", "Provide to as an RFC3339 timestamp."
		}
	}
	return "The audit export parameters are invalid.", "Provide valid audit export filters and retry."
}

func auditExportQueryFromRequest(r *http.Request, requester uuid.UUID) (store.AuditExportQuery, error) {
	actorSubjectID, err := optionalQueryUUID(r, "actor_subject_id")
	if err != nil {
		return store.AuditExportQuery{}, err
	}
	from, err := optionalQueryTime(r, "from")
	if err != nil {
		return store.AuditExportQuery{}, err
	}
	to, err := optionalQueryTime(r, "to")
	if err != nil {
		return store.AuditExportQuery{}, err
	}
	limit := defaultAuditExportLimit
	if value := strings.TrimSpace(r.URL.Query().Get("limit")); value != "" {
		limit, err = strconv.Atoi(value)
		if err != nil || limit < 1 || limit > 10000 {
			return store.AuditExportQuery{}, errors.New("invalid limit")
		}
	}
	outcomes, err := queryAuditOutcomes(r.URL.Query()["outcome"])
	if err != nil {
		return store.AuditExportQuery{}, err
	}
	return store.AuditExportQuery{
		RequesterSubjectID: requester, ActorSubjectID: actorSubjectID, Actions: cleanQueryValues(r.URL.Query()["action"]),
		Outcomes: outcomes, From: from, To: to, Limit: limit,
	}, nil
}

func optionalQueryUUID(r *http.Request, name string) (*uuid.UUID, error) {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return nil, nil
	}
	id, err := uuid.Parse(value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s", name)
	}
	return &id, nil
}

func optionalQueryTime(r *http.Request, name string) (*time.Time, error) {
	value := strings.TrimSpace(r.URL.Query().Get(name))
	if value == "" {
		return nil, nil
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return nil, fmt.Errorf("invalid %s", name)
	}
	return &parsed, nil
}

func queryAuditOutcomes(values []string) ([]accesscontrol.AuditOutcome, error) {
	outcomes := make([]accesscontrol.AuditOutcome, 0, len(values))
	for _, value := range cleanQueryValues(values) {
		outcome := accesscontrol.AuditOutcome(value)
		switch outcome {
		case accesscontrol.AuditAttempted, accesscontrol.AuditAllowed, accesscontrol.AuditDenied, accesscontrol.AuditSucceeded, accesscontrol.AuditFailed, accesscontrol.AuditRolledBack, accesscontrol.AuditCancelled:
			outcomes = append(outcomes, outcome)
		default:
			return nil, errors.New("invalid outcome")
		}
	}
	return outcomes, nil
}

func cleanQueryValues(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		for _, item := range strings.Split(value, ",") {
			if item = strings.TrimSpace(item); item != "" {
				result = append(result, item)
			}
		}
	}
	return result
}

func writeAuditExport(w http.ResponseWriter, format string, rows []store.AuditExportRow) {
	w.Header().Set("Content-Disposition", `attachment; filename="fused-audit.`+format+`"`)
	if format == "jsonl" {
		w.Header().Set("Content-Type", "application/x-ndjson")
		writeAuditJSONL(w, rows)
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	writeAuditCSV(w, rows)
}

func writeAuditJSONL(w http.ResponseWriter, rows []store.AuditExportRow) {
	encoder := json.NewEncoder(w)
	for _, row := range rows {
		_ = encoder.Encode(projectAuditExportRow(row))
	}
}

func writeAuditCSV(w http.ResponseWriter, rows []store.AuditExportRow) {
	writer := csv.NewWriter(w)
	_ = writer.Write([]string{"id", "occurred_at", "actor_subject_id", "actor_credential_id", "action", "permission", "resource_type", "resource_id", "request_id", "trace_id", "method", "path", "outcome", "status_code", "reason_code", "missing_requirements"})
	for _, row := range rows {
		_ = writer.Write(auditExportCSVRow(row))
	}
	writer.Flush()
}

func auditExportCSVRow(row store.AuditExportRow) []string {
	projected := projectAuditExportRow(row)
	missing, _ := json.Marshal(projected["missing_requirements"])
	cells := []string{
		projected["id"].(string), projected["occurred_at"].(string), projected["actor_subject_id"].(string), projected["actor_credential_id"].(string),
		projected["action"].(string), projected["permission"].(string), projected["resource_type"].(string), projected["resource_id"].(string),
		projected["request_id"].(string), projected["trace_id"].(string), projected["method"].(string), projected["path"].(string),
		projected["outcome"].(string), strconv.Itoa(row.StatusCode), projected["reason_code"].(string), string(missing),
	}
	for index := range cells {
		cells[index] = safeAuditCSVCell(cells[index])
	}
	return cells
}

func safeAuditCSVCell(value string) string {
	if value == "" {
		return value
	}
	// Spreadsheet programs interpret these prefixes as formulas. A leading
	// apostrophe preserves the audit text while preventing evaluation.
	switch value[0] {
	case '=', '+', '-', '@', '\t', '\r':
		return "'" + value
	default:
		return value
	}
}

func projectAuditExportRow(row store.AuditExportRow) map[string]interface{} {
	missing := make([]map[string]interface{}, 0, len(row.MissingRequirements))
	for _, requirement := range row.MissingRequirements {
		missing = append(missing, projectAccessRequirement(requirement))
	}
	return map[string]interface{}{
		"id": row.ID.String(), "occurred_at": row.OccurredAt.UTC().Format(time.RFC3339Nano),
		"actor_subject_id": nullableUUIDString(row.ActorSubjectID), "actor_credential_id": nullableUUIDString(row.ActorCredentialID),
		"action": row.Action, "permission": string(row.Permission), "resource_type": string(row.Resource.Type), "resource_id": nullableResourceID(row.Resource),
		"request_id": row.RequestID, "trace_id": row.TraceID, "method": row.Method, "path": row.Path,
		"outcome": string(row.Outcome), "status_code": row.StatusCode, "reason_code": row.ReasonCode, "missing_requirements": missing,
	}
}

func nullableUUIDString(value *uuid.UUID) string {
	if value == nil {
		return ""
	}
	return value.String()
}

func nullableResourceID(resource accesscontrol.ResourceRef) string {
	if resource.ID == uuid.Nil {
		return ""
	}
	return resource.ID.String()
}
