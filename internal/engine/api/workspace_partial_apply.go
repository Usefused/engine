package api

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/engine/workspaceplan"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"
)

// workspacePartialApplyError carries confirmed per-service outcomes through the existing apply endpoint.
type workspacePartialApplyError struct {
	Results []store.WorkspaceApplyProgress
}

// Error keeps partial apply failures safe for legacy clients and logs.
func (e *workspacePartialApplyError) Error() string {
	return "workspace partially applied; retry the same plan to resume eligible unfinished services"
}

// executeWorkspacePartialApply uses the reviewed plan as the recovery identity and keeps independent service commits separate.
func executeWorkspacePartialApply(ctx context.Context, configStore store.ConfigRepository, s store.Store, verifier ServiceVerifier, call workspaceApplyCall, progress store.WorkspaceApplyProgressStore) ([]appliedWorkspaceWebhook, error) {
	plan, err := configStore.GetConfigPlan(ctx, call.planID)
	// Lost final responses can be recovered without replaying an already completed plan.
	if err == nil && plan.Status == store.ConfigPlanStatusApplied && plan.Revision == call.planRevision && plan.SourceHash == call.sourceHash {
		return nil, nil
	}
	plan, current, err := loadWorkspacePlanForApply(ctx, configStore, call)
	// Admission errors precede all service writes.
	if err != nil {
		return nil, withWorkspaceConfigErrorMetadata(err, "apply_admission", call.planID.String(), "not_committed")
	}
	// Old pending plans may have partially executed without receipts; do not infer their history after upgrade.
	if plan.WorkspaceApplyVersion != 1 {
		return nil, withWorkspaceConfigErrorMetadata(workspaceConfigHTTPError{status: http.StatusConflict, message: "workspace plan predates resumable apply; create a new plan"}, "apply_admission", plan.ID.String(), "not_committed")
	}
	lease, err := configStore.ReserveConfigPlanApply(ctx, plan.ID, call.planRevision)
	// Only one holder of this exact reviewed revision may execute groups.
	if err != nil {
		return nil, withWorkspaceConfigErrorMetadata(configPlanApplyReservationHTTPError(err), "apply_admission", plan.ID.String(), "not_committed")
	}
	defer releaseWorkspaceApplyLease(configStore, plan.ID, call.planRevision, lease.ID)
	applyCtx, stop := workspaceApplyLeaseContext(ctx, configStore, plan.ID, call.planRevision, lease.ID)
	defer stop()
	desired, previous, err := workspaceApplyInputs(plan, current)
	// Existing destructive review checks remain whole-plan admission requirements.
	if err != nil {
		return nil, err
	}
	// Explicit destructive decisions remain prerequisites for every partial execution.
	if err := validateWorkspaceRemovalDecisions(plan); err != nil {
		return nil, err
	}
	// Revalidate exact removal scope before any independent service can change.
	if _, err := prepareExplicitWorkspaceRemovals(applyCtx, s, desired, plan.Actions); err != nil {
		return nil, err
	}
	receipts, err := progress.WorkspaceApplyProgress(applyCtx, plan.ID, call.planRevision)
	// Without receipts there is no safe distinction between fresh and completed work.
	if err != nil {
		return nil, err
	}
	states := map[string]store.WorkspaceApplyProgress{}
	for _, item := range receipts {
		states[item.Key] = item
	}
	// Include unattempted services so an early stop never hides the unfinished suffix.
	for id := range desired.Services {
		key := "service:" + id.String()
		// An absent receipt must remain visibly unattempted rather than disappear from results.
		if _, exists := states[key]; !exists {
			states[key] = store.WorkspaceApplyProgress{Key: key, Status: "pending"}
		}
	}
	// An external dispatch without its completion receipt is never automatically replayed.
	if states["registry"].Status == "running" {
		return nil, workspacePartialResults(states)
	}
	base := store.WorkspaceApplyStep{PlanID: plan.ID, Revision: call.planRevision, LeaseID: lease.ID}
	// Generic secrets are shared prerequisites and commit once before service groups.
	if len(desired.BucketSecrets) > 0 && states["buckets"].Status != "succeeded" {
		secrets, prepErr := prepareWorkspaceBucketSecrets(applyCtx, s, desired, call.bucketSecretMats, call.masterKey)
		// Shared secret material must be valid before any dependent service is attempted.
		if prepErr != nil {
			return nil, prepErr
		}
		step := base
		step.Key = "buckets"
		// The shared prerequisite and its receipt share the same commit boundary.
		err = progress.RunWorkspaceApplyStep(applyCtx, step, func(txCtx context.Context, txStore store.Store, _ store.ConfigRepository, state *store.ConfigState) (*store.UpsertConfigStateParams, error) {
			// The prerequisite receipt must roll back with a failed secret write.
			if err := upsertWorkspaceBucketSecrets(txCtx, txStore, secrets); err != nil {
				return nil, err
			}
			return workspacePartialState(plan, state, desired, nil, call.accountID, true)
		})
		// A failed prerequisite blocks every consumer until a later explicit retry.
		if err != nil {
			return nil, err
		}
		states[step.Key] = store.WorkspaceApplyProgress{Key: step.Key, Status: "succeeded"}
	}
	for _, svc := range sortedDesiredServices(desired) {
		key := "service:" + svc.ServiceID.String()
		// Confirmed services are never prepared or written again by this plan.
		if states[key].Status == "succeeded" {
			continue
		}
		// Losing the execution lease or database context stops new scheduling.
		if applyCtx.Err() != nil {
			return nil, workspacePartialResults(states)
		}
		subset := workspaceServiceSubset(desired, map[uuid.UUID]bool{svc.ServiceID: true})
		step := base
		step.Key = key
		err = applyWorkspaceServiceStep(applyCtx, progress, s, verifier, call, plan, subset, step)
		// Uncertain commits and stale ownership require reconciliation before any further work.
		if errors.Is(err, store.ErrWorkspaceApplyOutcomeUnknown) || errors.Is(err, store.ErrConfigPlanRevisionMismatch) || errors.Is(err, store.ErrWorkspaceApplyStale) {
			states[key] = store.WorkspaceApplyProgress{Key: key, Status: "running", ErrorCode: "needs_reconciliation"}
			return nil, workspacePartialResults(states)
		}
		// Proven local rollback affects only this independent service.
		if err != nil {
			slog.WarnContext(applyCtx, "workspace service apply failed", "service_id", svc.ServiceID, "error", err)
			states[key] = store.WorkspaceApplyProgress{Key: key, Status: "failed", ErrorCode: "service_apply_failed"}
			// Lost receipt storage stops scheduling because subsequent progress could not be reported reliably.
			if recordErr := progress.RecordWorkspaceApplyStep(applyCtx, step, "failed", "service_apply_failed"); recordErr != nil {
				return nil, workspacePartialResults(states)
			}
			continue
		}
		states[key] = store.WorkspaceApplyProgress{Key: key, Status: "succeeded"}
	}
	// Dependent whole-service removals retain their existing composite transaction semantics.
	removalIDs := map[uuid.UUID]bool{}
	for id := range previous {
		// Only services absent from the desired set belong to the composite removal group.
		if _, keep := desired.Services[id]; !keep {
			removalIDs[id] = true
		}
	}
	actions, err := parseWorkspacePlanActions(plan.Actions)
	// A failed decode or lookup cannot supply safe input to the next apply stage.
	if err != nil {
		return nil, err
	}
	for _, action := range actions {
		id, _ := uuid.Parse(action.ServiceID)
		// Explicit unmanaged removals are approved targets, even without prior managed state.
		if action.ExplicitRemoval {
			// Only services absent from the desired set belong to the composite removal group.
			if _, keep := desired.Services[id]; !keep {
				removalIDs[id] = true
			}
		}
	}
	// Composite removal is attempted once and then skipped by its durable success receipt.
	if len(removalIDs) > 0 && states["removals"].Status != "succeeded" {
		step := base
		step.Key = "removals"
		subset := workspaceServiceSubset(desired, removalIDs)
		err = applyWorkspaceServiceStep(applyCtx, progress, s, verifier, call, plan, subset, step)
		// Composite removals cannot be resumed after an uncertain commit without receipt inspection.
		if err != nil {
			states[step.Key] = store.WorkspaceApplyProgress{Key: step.Key, Status: "failed", ErrorCode: "removal_apply_failed"}
			// A lost commit response cannot be reported as a proven rollback.
			if errors.Is(err, store.ErrWorkspaceApplyOutcomeUnknown) {
				states[step.Key] = store.WorkspaceApplyProgress{Key: step.Key, Status: "running", ErrorCode: "needs_reconciliation"}
			}
			return nil, workspacePartialResults(states)
		}
		states[step.Key] = store.WorkspaceApplyProgress{Key: step.Key, Status: "succeeded"}
	}
	// Failed local groups must not publish desired Registry changes that never became active locally.
	for _, state := range states {
		// External publication must wait for all required local groups to complete.
		if state.Status != "succeeded" {
			return nil, workspacePartialResults(states)
		}
	}
	// Local-only plans need no external dispatch, and confirmed publication must never repeat.
	if workspaceHasRegistryActions(actions) && states["registry"].Status != "succeeded" {
		step := base
		step.Key = "registry"
		// Persist uncertainty before the first external write so a crash cannot make replay look safe.
		if err := progress.RecordWorkspaceApplyStep(applyCtx, step, "running", "needs_reconciliation"); err != nil {
			return nil, err
		}
		states[step.Key] = store.WorkspaceApplyProgress{Key: step.Key, Status: "running", ErrorCode: "needs_reconciliation"}
		// Registry writes are intentionally outside local transactions and are never automatically replayed.
		if err := applyWorkspaceExternalSteps(applyCtx, s, verifier, call, plan, current, desired); err != nil {
			return nil, workspacePartialResults(states)
		}
		// An unrecorded external success remains uncertain and blocks automatic replay.
		if err := progress.RecordWorkspaceApplyStep(applyCtx, step, "succeeded", ""); err != nil {
			return nil, workspacePartialResults(states)
		}
		states[step.Key] = store.WorkspaceApplyProgress{Key: step.Key, Status: "succeeded"}
	}
	// The whole desired source becomes applied only after every required group is confirmed.
	if err := persistWorkspaceConfigApply(applyCtx, configStore, call, plan, desired, previous, lease.ID); err != nil {
		return nil, err
	}
	return nil, nil
}

// workspacePartialResults keeps output deterministic and distinguishes uncertainty from a proven rollback.
func workspacePartialResults(states map[string]store.WorkspaceApplyProgress) error {
	result := &workspacePartialApplyError{}
	for _, item := range states {
		result.Results = append(result.Results, item)
	}
	sort.Slice(result.Results, func(i, j int) bool { return result.Results[i].Key < result.Results[j].Key })
	return result
}

// workspaceServiceSubset carries exact service scope without changing the public desired-state contract.
func workspaceServiceSubset(desired workspaceDesiredState, ids map[uuid.UUID]bool) workspaceDesiredState {
	subset := desired
	subset.Services = map[uuid.UUID]workspaceDesiredService{}
	for id, svc := range desired.Services {
		// A service transaction may reconcile only its explicitly assigned identities.
		if ids[id] {
			subset.Services[id] = svc
		}
	}
	// Scope is carried through deprecations separately; callers filter managed resources and actions by IDs.
	subset.applyServiceIDs = ids
	subset.BucketSecrets = nil
	return subset
}

// applyWorkspaceServiceStep prepares network-dependent facts first, then commits all related local writes with their receipt.
func applyWorkspaceServiceStep(ctx context.Context, progress store.WorkspaceApplyProgressStore, s store.Store, verifier ServiceVerifier, call workspaceApplyCall, plan *store.ConfigPlan, desired workspaceDesiredState, step store.WorkspaceApplyStep) error {
	for id, svc := range desired.Services {
		name, err := verifiedWorkspaceServiceName(ctx, verifier, svc, call.apiKey)
		// A Registry read failure cannot leave half an activated service.
		if err != nil {
			return err
		}
		svc.ServiceName = name
		desired.Services[id] = svc
	}
	profiles, err := prepareWorkspaceProfilePlan(ctx, s, verifier, call.apiKey, desired, call.profileMats)
	// Profile validation precedes the enclosing service transaction.
	if err != nil {
		return err
	}
	scopedPlan := *plan
	scopedPlan.Actions, err = workspaceScopedActions(plan.Actions, desired.applyServiceIDs)
	// A failed decode or lookup cannot supply safe input to the next apply stage.
	if err != nil {
		return err
	}
	// Every local write below uses a transaction-bound store, including nested savepoints.
	return progress.RunWorkspaceApplyStep(ctx, step, func(txCtx context.Context, txStore store.Store, txConfig store.ConfigRepository, current *store.ConfigState) (*store.UpsertConfigStateParams, error) {
		previous, err := parseManagedWorkspaceResources(current)
		// A failed decode or lookup cannot supply safe input to the next apply stage.
		if err != nil {
			return nil, err
		}
		for id := range previous {
			// Exclude unrelated managed state before invoking existing removal reconciliation.
			if !desired.applyServiceIDs[id] {
				delete(previous, id)
			}
		}
		// Capacity is rechecked inside the service transaction, after prior independent commits.
		if err := checkWorkspaceServiceLimit(txCtx, trace.SpanFromContext(txCtx), txStore, desired, previous); err != nil {
			return nil, err
		}
		// A failed profile write must roll back activation and every other local change in this group.
		if err := reconcileWorkspaceProfilePlan(txCtx, txStore, profiles); err != nil {
			return nil, err
		}
		// Names were verified before the transaction; membership failures roll back the whole service.
		if _, err := upsertDesiredWorkspaceServices(txCtx, txStore, nil, call.apiKey, call.accountID, desired); err != nil {
			return nil, err
		}
		// Version replacement cannot commit its additions while leaving a failed local removal behind.
		if err := removePreviouslyManagedWorkspaceResources(txCtx, txStore, desired, previous); err != nil {
			return nil, err
		}
		// Explicit removals remain part of the same service commit boundary.
		if err := removeExplicitWorkspaceResources(txCtx, txStore, desired, scopedPlan.Actions); err != nil {
			return nil, err
		}
		// Do not leave the service enabled with a failed required default policy.
		if err := applyWorkspaceExecutionPolicyLocalActions(txCtx, txStore, &scopedPlan, desired); err != nil {
			return nil, err
		}
		// Pinned versions and their local policy overrides must commit together.
		if err := applyWorkspaceVersionExecutionPolicyLocalActions(txCtx, txStore, &scopedPlan, desired); err != nil {
			return nil, err
		}
		// Removal notices and their underlying changes must agree after rollback or retry.
		if err := createWorkspaceRemovalNotifications(txCtx, txConfig, call, &scopedPlan); err != nil {
			return nil, err
		}
		return workspacePartialState(plan, current, desired, previous, call.accountID, false)
	})
}

// workspaceScopedActions preserves approved action payloads while excluding every unrelated service.
func workspaceScopedActions(raw json.RawMessage, ids map[uuid.UUID]bool) (json.RawMessage, error) {
	var actions []workspacePlanAction
	// Malformed approved actions cannot be replaced by an empty action set.
	if err := json.Unmarshal(raw, &actions); err != nil {
		return nil, err
	}
	selected := []workspacePlanAction{}
	for _, action := range actions {
		id, err := uuid.Parse(action.ServiceID)
		// Only exact service identities authorize actions within this transaction group.
		if err == nil && ids[id] {
			selected = append(selected, action)
		}
	}
	return json.Marshal(selected)
}

// workspacePartialState merges only committed service groups; unfinished source intent remains in the saved plan.
func workspacePartialState(plan *store.ConfigPlan, current *store.ConfigState, subset workspaceDesiredState, previous map[uuid.UUID]workspaceManagedService, actor uuid.UUID, buckets bool) (*store.UpsertConfigStateParams, error) {
	var target, actual workspaceConfigDocument
	// An unreadable reviewed target cannot produce a trustworthy applied-state projection.
	if err := json.Unmarshal(plan.ResolvedPayload, &target); err != nil {
		return nil, err
	}
	actual = workspaceConfigDocument{Kind: target.Kind, Version: target.Version, Services: map[string]workspaceConfigService{}}
	// Retain previously committed services when applying another independent group.
	if current != nil {
		// Corrupt persisted state must not be replaced by an incomplete reconstruction.
		if err := json.Unmarshal(current.DesiredState, &actual); err != nil {
			return nil, err
		}
	}
	// An initially empty projection still needs a map for the first committed service.
	if actual.Services == nil {
		actual.Services = map[string]workspaceConfigService{}
	}
	managed, err := parseManagedWorkspaceResources(current)
	// Invalid applied-state metadata must roll back this service rather than record incomplete state.
	if err != nil {
		return nil, err
	}
	// Shared material updates only the bucket projection; service receipts own service state.
	if buckets {
		actual.Buckets = target.Buckets
	} else {
		replacements := managedResourcesAfterApply(subset, previous)
		retained := map[uuid.UUID]bool{}
		for _, svc := range replacements.Services {
			id := uuid.MustParse(svc.ServiceID)
			managed[id] = svc
			retained[id] = true
		}
		for id := range subset.applyServiceIDs {
			// Remove managed identity only after its actual removal committed in this transaction.
			if !retained[id] {
				delete(managed, id)
			}
			for key, svc := range actual.Services {
				// Match immutable service identity even if a source uses a different display key.
				if svc.ServiceID == id.String() {
					// Advisory deprecation retains the prior enabled configuration until its actual removal.
					if _, exists := subset.Services[id]; exists || !retained[id] {
						delete(actual.Services, key)
					}
				}
			}
		}
		for key, svc := range target.Services {
			id, _ := uuid.Parse(svc.ServiceID)
			// Unfinished source entries must remain solely in the plan, outside applied state.
			if subset.applyServiceIDs[id] {
				actual.Services[key] = svc
			}
		}
	}
	encoded, err := json.Marshal(actual)
	// Invalid applied-state metadata must roll back this service rather than record incomplete state.
	if err != nil {
		return nil, err
	}
	resources := workspaceManagedResources{}
	for _, svc := range managed {
		resources.Services = append(resources.Services, svc)
	}
	sort.Slice(resources.Services, func(i, j int) bool { return resources.Services[i].ServiceID < resources.Services[j].ServiceID })
	managedJSON, err := json.Marshal(resources)
	// Invalid applied-state metadata must roll back this service rather than record incomplete state.
	if err != nil {
		return nil, err
	}
	digest := sha256.Sum256(encoded)
	return &store.UpsertConfigStateParams{ConfigKey: plan.ConfigKey, ConfigType: store.ConfigTypeWorkspace, SourceHash: fmt.Sprintf("sha256:%x", digest), DesiredState: encoded, ManagedResources: managedJSON, UpdatedBy: actor}, nil
}

// workspaceHasRegistryActions avoids an ambiguous external marker for local-only plans.
func workspaceHasRegistryActions(actions map[string]workspacePlanAction) bool {
	for _, action := range actions {
		// These existing action types are the only external mutation stages in workspace apply.
		switch action.Type {
		case workspaceplan.ActionDeprecateService, workspaceplan.ActionDeprecateVersion, workspaceplan.ActionPublishServiceExecutionPolicy, workspaceplan.ActionPublishServiceVersionExecutionPolicy, workspaceplan.ActionPublishConnectionProfile, workspaceplan.ActionSetServicePublic, workspaceplan.ActionSetServicePrivate, workspaceplan.ActionSetServiceVersionPublic, workspaceplan.ActionSetServiceVersionPrivate:
			return true
		case workspaceplan.ActionRemoveService:
			// Removal performs a Registry ownership check and may archive the owned service.
			return true
		}
	}
	return false
}

// applyWorkspaceExternalSteps retains the existing Registry ordering without repeating already committed local policy writes.
func applyWorkspaceExternalSteps(ctx context.Context, s store.Store, verifier ServiceVerifier, call workspaceApplyCall, plan *store.ConfigPlan, current *store.ConfigState, desired workspaceDesiredState) error {
	// Stop after the first uncertain external stage without replaying earlier mutations.
	if err := applyWorkspaceRegistryActions(ctx, verifier, call, plan, current); err != nil {
		return err
	}
	// Publication failures retain the pre-dispatch uncertainty marker for reconciliation.
	if err := applyWorkspaceExecutionPolicyPublishActions(ctx, verifier, call, plan, desired); err != nil {
		return err
	}
	// Version publication must be confirmed before later external stages run.
	if err := applyWorkspaceVersionExecutionPolicyPublishActions(ctx, verifier, call, plan, desired); err != nil {
		return err
	}
	return applyWorkspaceConnectionProfilePublishActions(ctx, verifier, s, call, plan, desired)
}

// writeWorkspacePartialApply returns a non-success HTTP status so older clients cannot report partial commits as complete.
func writeWorkspacePartialApply(w http.ResponseWriter, planID uuid.UUID, result *workspacePartialApplyError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{"status": "partially_applied", "plan_id": planID.String(), "services": result.Results, "error": map[string]any{"code": "workspace_partially_applied", "message": result.Error(), "commit_state": "unknown"}})
}
