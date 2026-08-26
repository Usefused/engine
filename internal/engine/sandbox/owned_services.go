package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

const ownedServicesPageLimit = 100

// OwnedRegistryService is the exact Registry identity Engine needs to restore
// one owned service after its local workspace database has been recreated.
type OwnedRegistryService struct {
	ServiceID        uuid.UUID
	Name             string
	Slug             string
	Version          string
	ServiceVersionID uuid.UUID
}

type ownedServicesPage struct {
	Data []struct {
		ID                    string `json:"id"`
		Name                  string `json:"name"`
		Slug                  string `json:"slug"`
		CurrentServiceVersion string `json:"current_service_version"`
		ServiceVersions       []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"service_versions"`
	} `json:"data"`
	Total int `json:"total"`
	Page  int `json:"page"`
	Limit int `json:"limit"`
}

// FetchOwnedServices lists the services owned by the licensed Registry
// account. The Registry's services root is account-scoped, so public services
// owned by other providers are deliberately excluded from workspace bootstrap.
func (c *HTTPRegistryClient) FetchOwnedServices(ctx context.Context) ([]OwnedRegistryService, error) {
	services := make([]OwnedRegistryService, 0)
	for page := 1; ; page++ {
		result, err := c.fetchOwnedServicesPage(ctx, page)
		if err != nil {
			return nil, err
		}
		for _, service := range result.Data {
			serviceID, err := uuid.Parse(service.ID)
			if err != nil {
				return nil, fmt.Errorf("FetchOwnedServices: invalid service id %q: %w", service.ID, err)
			}
			versionID := currentServiceVersionID(service.CurrentServiceVersion, service.ServiceVersions)
			// A definition without a concrete current version can be edited in the
			// Registry, but it cannot be pinned into the Engine runtime yet.
			if service.CurrentServiceVersion == "" || versionID == uuid.Nil {
				continue
			}
			services = append(services, OwnedRegistryService{
				ServiceID:        serviceID,
				Name:             service.Name,
				Slug:             service.Slug,
				Version:          service.CurrentServiceVersion,
				ServiceVersionID: versionID,
			})
		}
		if result.Page*result.Limit >= result.Total || len(result.Data) == 0 {
			break
		}
	}
	return services, nil
}

func (c *HTTPRegistryClient) fetchOwnedServicesPage(ctx context.Context, page int) (ownedServicesPage, error) {
	req, err := c.newGraphQLRequest(ctx, graphqlQuery{
		Query: `
			query OwnedServices($page: Int!, $limit: Int!) {
				services(page: $page, limit: $limit) {
					data {
						id
						name
						slug
						current_service_version
						service_versions { id name }
					}
					total
					page
					limit
				}
			}
		`,
		Variables: map[string]interface{}{"page": page, "limit": ownedServicesPageLimit},
	})
	if err != nil {
		return ownedServicesPage{}, fmt.Errorf("FetchOwnedServices: create request: %w", err)
	}
	response, err := c.do(req)
	if err != nil {
		return ownedServicesPage{}, fmt.Errorf("FetchOwnedServices: request failed: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return ownedServicesPage{}, fmt.Errorf("FetchOwnedServices: registry returned %d: %s", response.StatusCode, string(body))
	}
	var decoded struct {
		Data struct {
			Services ownedServicesPage `json:"services"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return ownedServicesPage{}, fmt.Errorf("FetchOwnedServices: decode response: %w", err)
	}
	if len(decoded.Errors) > 0 {
		return ownedServicesPage{}, fmt.Errorf("FetchOwnedServices: graphql error: %s", decoded.Errors[0].Message)
	}
	return decoded.Data.Services, nil
}

type ownedServiceRegistry interface {
	FetchOwnedServices(ctx context.Context) ([]OwnedRegistryService, error)
	FetchRuntimeContracts(ctx context.Context, versions []store.WorkspaceServiceVersion, apiKey string) ([]store.ServiceContractSnapshot, error)
}

// OwnedServiceReconcileResult describes the idempotent startup reconciliation.
type OwnedServiceReconcileResult struct {
	Discovered    int
	AlreadyActive int
	Activated     int
	Deferred      []OwnedServiceRejection
}

// ReconcileOwnedServices restores Registry-owned services that are absent from
// this Engine workspace. Existing services keep their current local pins.
func ReconcileOwnedServices(ctx context.Context, workspace store.Store, registry ownedServiceRegistry, accountID uuid.UUID, apiKey string) (OwnedServiceReconcileResult, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.owned_services.reconcile")
	defer span.End()
	result, err := reconcileOwnedServices(ctx, workspace, registry, accountID, apiKey)
	outcome := "complete"
	// Deferred contracts must remain visible even though the control plane can safely start.
	if len(result.Deferred) > 0 {
		outcome = "partial"
	}
	// Infrastructure and trust failures must not masquerade as recoverable service content.
	if err != nil {
		outcome = "failed"
	}
	span.SetAttributes(attribute.String("outcome", outcome),
		attribute.Int("service.discovered_count", boundedPassiveCount(result.Discovered)),
		attribute.Int("service.activated_count", boundedPassiveCount(result.Activated)),
		attribute.Int("service.deferred_count", boundedPassiveCount(len(result.Deferred))))
	for _, failure := range result.Deferred {
		slog.WarnContext(ctx, "Owned service recovery deferred; repair the Registry contract, then add the exact version to the workspace",
			"service_id", failure.ServiceID, "service_version_id", failure.ServiceVersionID,
			"blocking_service_version_id", failure.BlockingVersionID, "failure_code", "runtime_contract_rejected")
	}
	return result, err
}

// reconcileOwnedServices isolates validated content failures without changing identity or persistence policy.
func reconcileOwnedServices(ctx context.Context, workspace store.Store, registry ownedServiceRegistry, accountID uuid.UUID, apiKey string) (OwnedServiceReconcileResult, error) {
	result := OwnedServiceReconcileResult{}
	owned, err := registry.FetchOwnedServices(ctx)
	// A failed licensed listing cannot be interpreted as an empty workspace.
	if err != nil {
		return result, err
	}
	result.Discovered = len(owned)

	snapshotStore, ok := workspace.(store.OwnedServiceRecoveryStore)
	// Recovery requires both SQL membership selection and canonical snapshot persistence.
	if !ok {
		return result, errors.New("owned service reconciliation requires runtime contract storage")
	}

	missing, err := missingOwnedServices(ctx, snapshotStore, owned)
	// Membership errors leave local pins untouched and remain startup failures.
	if err != nil {
		return result, err
	}
	result.AlreadyActive = len(owned) - len(missing)
	// All existing pins are authoritative; startup must not float them to Registry latest.
	if len(missing) == 0 {
		return result, nil
	}

	versions := make([]store.WorkspaceServiceVersion, 0, len(missing))
	for _, service := range missing {
		versions = append(versions, store.WorkspaceServiceVersion{
			ServiceID:        service.ServiceID,
			ServiceVersionID: service.ServiceVersionID,
			Version:          service.Version,
			Status:           "enabled",
		})
	}
	snapshots, err := registry.FetchRuntimeContracts(ctx, versions, apiKey)
	// Only canonical content rejection can supply a validated recovery subset.
	if err != nil {
		var rejected *runtimeContractRejections
		// Transport, authorization, identity and unknown failures still stop startup.
		if !errors.As(err, &rejected) {
			return result, fmt.Errorf("fetch owned service runtime contracts: %w", err)
		}
		snapshots = rejected.accepted
		result.Deferred = rejected.failures
	}
	// Every requested identity needs exactly one accepted or deferred result before any writes begin.
	if err := validateOwnedRecoverySet(missing, snapshots, result.Deferred); err != nil {
		return result, err
	}
	result.Activated, err = restoreOwnedSnapshots(ctx, workspace, snapshotStore, missing, snapshots, accountID)
	return result, err
}

// validateOwnedRecoverySet prevents partial or duplicate fetcher responses from appearing fully recovered.
func validateOwnedRecoverySet(missing []OwnedRegistryService, snapshots []store.ServiceContractSnapshot, failures []OwnedServiceRejection) error {
	pending := make(map[uuid.UUID]uuid.UUID, len(missing))
	for _, service := range missing {
		pending[service.ServiceVersionID] = service.ServiceID
	}
	for _, snapshot := range snapshots {
		// Accepted data must consume one exact requested identity.
		if err := consumeOwnedRecoveryIdentity(pending, snapshot.ServiceID, snapshot.ServiceVersionID); err != nil {
			return err
		}
	}
	for _, failure := range failures {
		// A rejection cannot excuse a missing or duplicated unrelated service.
		if err := consumeOwnedRecoveryIdentity(pending, failure.ServiceID, failure.ServiceVersionID); err != nil {
			return err
		}
	}
	// An incomplete response is not a successful empty recovery.
	if len(pending) != 0 {
		return errors.New("owned service recovery response is incomplete")
	}
	return nil
}

// consumeOwnedRecoveryIdentity enforces one-to-one admission before persistence starts.
func consumeOwnedRecoveryIdentity(pending map[uuid.UUID]uuid.UUID, serviceID, versionID uuid.UUID) error {
	expected, ok := pending[versionID]
	// Absence includes duplicates because the first admitted result consumes the identity.
	if !ok || expected != serviceID {
		return errors.New("owned service recovery identity mismatch")
	}
	delete(pending, versionID)
	return nil
}

// missingOwnedServices maps only SQL-selected missing IDs back to the already bounded Registry metadata.
func missingOwnedServices(ctx context.Context, persistence store.OwnedServiceRecoveryStore, owned []OwnedRegistryService) ([]OwnedRegistryService, error) {
	ids := make([]uuid.UUID, 0, len(owned))
	byID := make(map[uuid.UUID]OwnedRegistryService, len(owned))
	for _, service := range owned {
		ids = append(ids, service.ServiceID)
		byID[service.ServiceID] = service
	}
	missingIDs, err := persistence.MissingOwnedServiceIDs(ctx, ids)
	// No fallback may turn a SQL failure into apparent absence.
	if err != nil {
		return nil, err
	}
	missing := make([]OwnedRegistryService, 0, len(missingIDs))
	for _, id := range missingIDs {
		missing = append(missing, byID[id])
	}
	return missing, nil
}

// restoreOwnedSnapshots writes each independently admitted service; rejected versions never reach storage or activation.
func restoreOwnedSnapshots(ctx context.Context, workspace store.Store, persistence store.OwnedServiceRecoveryStore, missing []OwnedRegistryService, snapshots []store.ServiceContractSnapshot, accountID uuid.UUID) (int, error) {
	byVersion := make(map[uuid.UUID]OwnedRegistryService, len(missing))
	for _, service := range missing {
		byVersion[service.ServiceVersionID] = service
	}
	activated := 0
	for _, snapshot := range snapshots {
		// validateOwnedRecoverySet admitted the complete tuple set before any writes began.
		service := byVersion[snapshot.ServiceVersionID]
		// Persistence failure is not a content rejection and must remain visible to startup.
		if _, err := persistence.UpsertServiceContractSnapshot(ctx, snapshot); err != nil {
			return activated, fmt.Errorf("store owned service %s runtime contract: %w", service.ServiceID, err)
		}
		// Membership is created only after canonical snapshot validation and persistence succeed.
		if err := workspace.AddWorkspaceServiceVersion(ctx, service.ServiceID, service.Slug, service.Version, service.ServiceVersionID, service.Name, accountID); err != nil {
			return activated, fmt.Errorf("activate owned service %s: %w", service.ServiceID, err)
		}
		activated++
	}
	return activated, nil
}
