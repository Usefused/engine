package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/Usefused/engine/internal/engine/store"
)

// isSDKGeneratePath reports whether r is the Registry's ungated direct SDK
// generation endpoint (Task 6, engine_workspace_registration_plan.md): the
// UI's SDK Builder page calls this directly (Fact 13), and unlike the
// Engine's own config-as-code path it has no workspace-activation concept
// at all today (Fact 12). This is the one REST-proxied request that needs a
// workspace check *before* forwarding -- rejecting outright, not just
// reacting to what comes back -- so it gets its own branch in
// RESTProxyHandler instead of the normal uniform forward.
func isSDKGeneratePath(method, path string) bool {
	return method == http.MethodPost && path == "/sdks/generate"
}

// sdkGenerateRequestBody mirrors just the piece of the Registry
// GenerateSDKRequest contract this gate needs. Keeping the shape local avoids
// importing Registry implementation code into Engine.
type sdkGenerateRequestBody struct {
	Selections []sdkGenerateSelection `json:"selections"`
}

type sdkGenerateSelection struct {
	ServiceID        uuid.UUID `json:"service_id"`
	ServiceName      string    `json:"service_name,omitempty"`
	ServiceSlug      string    `json:"service_slug,omitempty"`
	ServiceVersionID uuid.UUID `json:"service_version_id,omitempty"`
	BlockReason      string    `json:"-"`
}

// forwardSDKGenerateWithWorkspaceGate mirrors forwardRESTMutationWithSpan's
// span setup, but reads the request body first to check every selected
// service's workspace-activation state before deciding whether to forward
// at all.
func forwardSDKGenerateWithWorkspaceGate(proxy Forwarder, s store.Store, w http.ResponseWriter, r *http.Request, accountID uuid.UUID) {
	ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.proxy.rest_mutation", trace.WithAttributes(
		attribute.String("user_action", "rest."+r.Method),
		attribute.String("account_id", accountID.String()),
		attribute.String("path", r.URL.Path),
	))
	defer span.End()

	body, err := io.ReadAll(r.Body)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "bad_request"))
		http.Error(w, `{"error":"failed to read request body"}`, http.StatusBadRequest)
		return
	}
	// The Registry still needs to see the exact same bytes if this request
	// ends up forwarded -- restore the body after reading it once here.
	r.Body = io.NopCloser(bytes.NewReader(body))

	unactivated, blocked, checkErr := firstUnactivatedSelection(ctx, s, accountID, body)
	if checkErr != nil {
		span.SetAttributes(attribute.String("outcome", "error"))
		slog.ErrorContext(ctx, "sdk generate gate: activation check failed", slog.Any("error", checkErr))
		http.Error(w, `{"error":"failed to verify workspace activation"}`, http.StatusInternalServerError)
		return
	}
	if blocked {
		attrs := []attribute.KeyValue{
			attribute.String("outcome", "forbidden"),
			attribute.String("blocked_service_id", unactivated.ServiceID.String()),
			attribute.String("blocked_reason", unactivated.BlockReason),
		}
		if unactivated.ServiceVersionID != uuid.Nil {
			attrs = append(attrs, attribute.String("blocked_service_version_id", unactivated.ServiceVersionID.String()))
		}
		span.SetAttributes(attrs...)
		slog.WarnContext(ctx, "sdk generate: rejected selection for an unactivated service",
			slog.String("service_id", unactivated.ServiceID.String()), slog.String("account_id", accountID.String()))
		msg := workspaceActivationRequiredMessage(unactivated)
		http.Error(w, fmt.Sprintf(`{"error":%q}`, msg), http.StatusForbidden)
		return
	}

	rec := newStatusRecorder(w)
	proxy.Forward(rec, r.WithContext(ctx), "")
	span.SetAttributes(
		attribute.Int("http_status_code", rec.status),
		attribute.String("outcome", outcomeLabel(rec.status)),
	)
}

// firstUnactivatedSelection decodes body's selections and reports the first
// requested service ID that isn't activated in the caller's workspace, if
// any.
//
// Malformed JSON or an empty selection list is not this gate's concern to
// report -- it returns "not blocked" so the request reaches the Registry,
// whose own request validation produces the real error, exactly as it would
// without this gate in place at all.
//
// An account with no workspace at all (GetWorkspaceIDForAccount errors)
// means nothing has ever been activated, so the first requested service ID
// is reported as the blocker without needing an IsWorkspaceServiceEnabled call.
//
// An IsWorkspaceServiceEnabled failure is returned before forwarding. This
// security gate cannot defer failure into post-publication recovery because a
// request admitted here would already have bypassed its workspace boundary.
func firstUnactivatedSelection(ctx context.Context, s store.Store, accountID uuid.UUID, body []byte) (selection sdkGenerateSelection, blocked bool, err error) {
	var req sdkGenerateRequestBody
	if decErr := json.Unmarshal(body, &req); decErr != nil || len(req.Selections) == 0 {
		return sdkGenerateSelection{}, false, nil
	}

	wsErr := verifyWorkspaceActor(ctx, accountID)
	if wsErr != nil {
		return req.Selections[0], true, nil
	}

	allowedVersions, versionsErr := s.ListWorkspaceServiceVersionsForServices(ctx, sdkGateServiceIDs(req.Selections))
	if versionsErr != nil {
		return sdkGenerateSelection{}, false, versionsErr
	}
	for _, sel := range req.Selections {
		blockedSelection, blocked, blockErr := workspaceSDKSelectionBlock(ctx, s, allowedVersions, sel)
		if blockErr != nil || blocked {
			return blockedSelection, blocked, blockErr
		}
	}
	return sdkGenerateSelection{}, false, nil
}

func workspaceSDKSelectionBlock(ctx context.Context, s store.Store, allowedVersions map[uuid.UUID][]store.WorkspaceServiceVersion, sel sdkGenerateSelection) (sdkGenerateSelection, bool, error) {
	activated, err := s.IsWorkspaceServiceEnabled(ctx, sel.ServiceID)
	if err != nil {
		return sdkGenerateSelection{}, false, err
	}
	if !activated {
		sel.BlockReason = "service_not_activated"
		return sel, true, nil
	}
	if sel.ServiceVersionID == uuid.Nil {
		sel.BlockReason = "service_version_required"
		return sel, true, nil
	}
	if !activationVersionExistsByUUID(allowedVersions[sel.ServiceID], sel.ServiceVersionID) {
		sel.BlockReason = "service_version_not_enabled"
		return sel, true, nil
	}
	return sdkGenerateSelection{}, false, nil
}

func workspaceActivationRequiredMessage(selection sdkGenerateSelection) string {
	if selection.BlockReason == "service_version_required" {
		return fmt.Sprintf("service %s requires service_version_id for SDK generation", blockedServiceLabel(selection))
	}
	if selection.BlockReason == "service_version_not_enabled" && selection.ServiceVersionID != uuid.Nil {
		return fmt.Sprintf("service version %s for service %s is not enabled in this workspace", selection.ServiceVersionID.String(), blockedServiceLabel(selection))
	}
	return fmt.Sprintf("service %s is not activated in this workspace. Run 'fused-cli workspace service add %s' to activate it.",
		blockedServiceLabel(selection), shellArg(workspaceServiceAddTarget(selection)))
}

func sdkGateServiceIDs(selections []sdkGenerateSelection) []uuid.UUID {
	ids := make([]uuid.UUID, 0, len(selections))
	seen := map[uuid.UUID]bool{}
	for _, selection := range selections {
		if selection.ServiceID == uuid.Nil || seen[selection.ServiceID] {
			continue
		}
		seen[selection.ServiceID] = true
		ids = append(ids, selection.ServiceID)
	}
	return ids
}

func blockedServiceLabel(selection sdkGenerateSelection) string {
	if name := strings.TrimSpace(selection.ServiceName); name != "" {
		return name
	}
	if slug := strings.TrimSpace(selection.ServiceSlug); slug != "" {
		return slug
	}
	return selection.ServiceID.String()
}

func workspaceServiceAddTarget(selection sdkGenerateSelection) string {
	if slug := strings.TrimSpace(selection.ServiceSlug); slug != "" {
		return slug
	}
	if name := strings.TrimSpace(selection.ServiceName); name != "" {
		return name
	}
	return selection.ServiceID.String()
}

func shellArg(value string) string {
	if strings.ContainsAny(value, " \t\n'\"\\$`") {
		return strconv.Quote(value)
	}
	return value
}
