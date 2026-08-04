package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
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

type ownedServiceSnapshotWriter interface {
	UpsertServiceContractSnapshot(ctx context.Context, snapshot store.ServiceContractSnapshot) (*store.ServiceContractSnapshot, error)
}

// OwnedServiceReconcileResult describes the idempotent startup reconciliation.
type OwnedServiceReconcileResult struct {
	Discovered    int
	AlreadyActive int
	Activated     int
}

// ReconcileOwnedServices restores Registry-owned services that are absent from
// this Engine workspace. Existing services keep their current local pins.
func ReconcileOwnedServices(ctx context.Context, workspace store.Store, registry ownedServiceRegistry, accountID uuid.UUID, apiKey string) (OwnedServiceReconcileResult, error) {
	result := OwnedServiceReconcileResult{}
	owned, err := registry.FetchOwnedServices(ctx)
	if err != nil {
		return result, err
	}
	result.Discovered = len(owned)

	snapshotStore, ok := workspace.(ownedServiceSnapshotWriter)
	if !ok {
		return result, errors.New("owned service reconciliation requires runtime contract storage")
	}

	var failures []error
	missing := make([]OwnedRegistryService, 0, len(owned))
	for _, service := range owned {
		enabled, err := workspace.IsWorkspaceServiceEnabled(ctx, service.ServiceID)
		if err != nil {
			failures = append(failures, fmt.Errorf("check owned service %s: %w", service.ServiceID, err))
			continue
		}
		if enabled {
			result.AlreadyActive++
			continue
		}
		missing = append(missing, service)
	}
	if len(failures) > 0 {
		return result, errors.Join(failures...)
	}
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
	if err != nil {
		return result, fmt.Errorf("fetch owned service runtime contracts: %w", err)
	}
	snapshotsByVersion := make(map[uuid.UUID]store.ServiceContractSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		snapshotsByVersion[snapshot.ServiceVersionID] = snapshot
	}

	for _, service := range missing {
		snapshot, ok := snapshotsByVersion[service.ServiceVersionID]
		if !ok {
			failures = append(failures, fmt.Errorf("owned service %s runtime contract was not returned", service.ServiceID))
			continue
		}
		if _, err := snapshotStore.UpsertServiceContractSnapshot(ctx, snapshot); err != nil {
			failures = append(failures, fmt.Errorf("store owned service %s runtime contract: %w", service.ServiceID, err))
			continue
		}
		if err := workspace.AddWorkspaceServiceVersion(ctx, service.ServiceID, service.Slug, service.Version, service.ServiceVersionID, service.Name, accountID); err != nil {
			failures = append(failures, fmt.Errorf("activate owned service %s: %w", service.ServiceID, err))
			continue
		}
		result.Activated++
	}
	return result, errors.Join(failures...)
}
