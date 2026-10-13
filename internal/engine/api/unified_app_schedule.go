package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

// UnifiedAppSchedulesHandler keeps private saved input and all schedule mutations on the management plane.
func UnifiedAppSchedulesHandler(s store.Store, key []byte) http.HandlerFunc {
	// Every request must recheck family ownership and management authority before accessing saved inputs.
	return func(w http.ResponseWriter, r *http.Request) {
		ctx, span := otel.Tracer("engine").Start(r.Context(), "engine.unified_app.schedule")
		defer span.End()
		actor, app, err := lifecycleActorAndApp(ctx, s, r)
		// Foreign or missing immutable identities cannot authorize a family schedule.
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		err = (accesscontrol.SnapshotAuthorizer{}).CheckAll(ctx, actor, accesscontrol.Requirement{Permission: accesscontrol.PermissionAppUnifiedAppManage, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceApp, ID: app.AppFamilyID}})
		// Schedule reads expose saved input, so ordinary app readers are deliberately insufficient.
		if err != nil {
			writeAppLifecycleError(w, span, err)
			return
		}
		repository, ok := s.(store.UnifiedAppScheduleStore)
		// Only hosted apps with durable scheduling support can use this control surface.
		if !ok {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: 400, message: "schedules require a Unified App"})
			return
		}
		family, err := s.GetAppFamily(ctx, app.AppFamilyID)
		// Adapter identity comes from the persisted family, never from a version label.
		if err != nil || family.Kind != store.AppKindUnifiedApp {
			writeSDKConfigError(w, workspaceConfigHTTPError{status: 400, message: "schedules require a Unified App"})
			return
		}
		span.SetAttributes(attribute.String("app.family_id", app.AppFamilyID.String()), attribute.String("actor.type", string(actor.Kind)))
		err = handleUnifiedAppSchedule(ctx, w, r, s, repository, key, actor, app)
		// Conflicts remain distinguishable from transient storage errors without leaking stored input.
		if err != nil {
			writeSDKConfigError(w, scheduleHTTPError(err))
			return
		}
		span.SetAttributes(attribute.String("outcome", "succeeded"))
	}
}

// handleUnifiedAppSchedule separates private listing from bounded mutation decoding.
func handleUnifiedAppSchedule(ctx context.Context, w http.ResponseWriter, r *http.Request, s store.Store, repository store.UnifiedAppScheduleStore, key []byte, actor accesscontrol.Actor, app *store.App) error {
	// Listing never changes timing or execution state.
	if r.Method == http.MethodGet {
		items, err := repository.ListUnifiedAppSchedules(ctx, actor.AccountID, app.AppFamilyID, key)
		if err != nil {
			return err
		}
		writeJSON(w, map[string]any{"schedules": items})
		return nil
	}
	// Deletion is optimistic and cannot race an admitted occurrence.
	if r.Method == http.MethodDelete {
		return deleteUnifiedAppSchedule(ctx, w, r, repository, actor.AccountID, app.AppFamilyID)
	}
	item, err := decodeUnifiedAppSchedule(w, r)
	// The server supplies ownership and creator identity instead of accepting them from the request.
	if err != nil {
		return err
	}
	item.AccountID, item.FamilyID, item.CreatedBy = actor.AccountID, app.AppFamilyID, actor.SubjectID
	if err := store.ValidateSchedule(&item, time.Now()); err != nil {
		return workspaceConfigHTTPError{status: 400, message: err.Error()}
	}
	// Pausing must remain possible even when a promoted app no longer accepts the old saved input.
	if item.Enabled {
		if err := validateScheduledInput(ctx, s, item); err != nil {
			return err
		}
	}
	if err := repository.SaveUnifiedAppSchedule(ctx, item, key); err != nil {
		return err
	}
	writeJSON(w, map[string]any{"id": item.ID, "saved": true})
	return nil
}

// deleteUnifiedAppSchedule requires the exact reviewed revision before removing future automation.
func deleteUnifiedAppSchedule(ctx context.Context, w http.ResponseWriter, r *http.Request, repository store.UnifiedAppScheduleStore, account, family uuid.UUID) error {
	id, err := uuid.Parse(chi.URLParam(r, "schedule_id"))
	revision, revisionErr := strconv.ParseInt(r.URL.Query().Get("revision"), 10, 64)
	if err != nil || revisionErr != nil || revision < 1 {
		return workspaceConfigHTTPError{status: 400, message: "schedule ID and revision are required"}
	}
	if err := repository.DeleteUnifiedAppSchedule(ctx, account, family, id, revision); err != nil {
		return err
	}
	writeJSON(w, map[string]bool{"deleted": true})
	return nil
}

// decodeUnifiedAppSchedule accepts only authored fields, never internal execution ownership or status.
func decodeUnifiedAppSchedule(w http.ResponseWriter, r *http.Request) (store.UnifiedAppSchedule, error) {
	var body struct {
		ID       uuid.UUID       `json:"id"`
		Name     string          `json:"name"`
		Input    json.RawMessage `json:"input"`
		Cron     string          `json:"cron"`
		Timezone string          `json:"timezone"`
		RunAt    *time.Time      `json:"run_at"`
		Enabled  bool            `json:"enabled"`
		Revision int64           `json:"revision"`
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, store.MaxExecutionDataBytes+4096))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&body)
	var trailing any
	// One bounded document keeps mutation identity and input unambiguous.
	if err != nil || decoder.Decode(&trailing) != io.EOF {
		return store.UnifiedAppSchedule{}, workspaceConfigHTTPError{status: 400, message: "invalid schedule document"}
	}
	return store.UnifiedAppSchedule{ID: body.ID, Name: body.Name, Input: body.Input, Cron: body.Cron, Timezone: body.Timezone, RunAt: body.RunAt, Enabled: body.Enabled, Revision: body.Revision}, nil
}

// validateScheduledInput checks the current public input contract without running authored code or fetching external schemas.
func validateScheduledInput(ctx context.Context, s store.Store, item store.UnifiedAppSchedule) error {
	targets, ok := s.(store.UnifiedAppPromotionStore)
	// A versionless family cannot accept new runnable automation.
	if !ok {
		return errors.New("schedule target unavailable")
	}
	target, err := targets.UnifiedAppTrafficTarget(ctx, item.FamilyID)
	if err != nil || target == uuid.Nil {
		return workspaceConfigHTTPError{status: 409, message: "Unified App has no active version"}
	}
	bundles, ok := s.(store.UnifiedAppBundleStore)
	if !ok {
		return errors.New("schedule bundle unavailable")
	}
	bundle, err := bundles.GetUnifiedAppBundle(ctx, target)
	if err != nil {
		return err
	}
	manifest, err := parseUnifiedAppManifest(bundle.Manifest)
	if err != nil {
		return err
	}
	return validateScheduleSchema(manifest.InputSchema, item.Input)
}

// validateScheduleSchema uses only bounded local documents and emits no caller values in errors.
func validateScheduleSchema(rawSchema, rawInput json.RawMessage) error {
	var schema jsonschema.Schema
	var input any
	// The manifest and input are local bounded documents; validation never loads remote references.
	if json.Unmarshal(rawSchema, &schema) != nil || json.Unmarshal(rawInput, &input) != nil {
		return workspaceConfigHTTPError{status: 400, message: "invalid scheduled input"}
	}
	resolved, err := schema.Resolve(nil)
	if err != nil {
		return workspaceConfigHTTPError{status: 400, message: "input schema cannot be resolved"}
	}
	// Schema failures omit authored values from public errors and telemetry.
	if resolved.Validate(input) != nil {
		return workspaceConfigHTTPError{status: 400, message: "input does not match the active Unified App schema"}
	}
	return nil
}

// scheduleHTTPError preserves safe validation messages while keeping database and encryption details private.
func scheduleHTTPError(err error) error {
	var requestErr workspaceConfigHTTPError
	// Only deliberately public errors may cross the control boundary unchanged.
	if errors.As(err, &requestErr) {
		return requestErr
	}
	if errors.Is(err, store.ErrScheduleConflict) {
		return workspaceConfigHTTPError{status: 409, message: err.Error()}
	}
	return workspaceConfigHTTPError{status: 503, message: "schedule storage is unavailable"}
}
