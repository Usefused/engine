package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

const ownedArtifactsPageLimit = 100

type ownedArtifactsPage struct {
	Items []struct {
		ID                 string                `json:"id"`
		Name               string                `json:"name"`
		Description        string                `json:"description"`
		Version            string                `json:"version"`
		TargetType         string                `json:"target_type"`
		TargetLanguage     string                `json:"target_language"`
		Readme             string                `json:"readme"`
		CreatedAt          string                `json:"created_at"`
		DetailedSelections []models.SDKSelection `json:"detailed_selections"`
	} `json:"items"`
	Total int `json:"total"`
}

// FetchOwnedArtifactSnapshots returns only the licensed account's generated
// SDK definitions. Runtime credentials never cross this restore boundary.
func (c *HTTPRegistryClient) FetchOwnedArtifactSnapshots(ctx context.Context, accountID uuid.UUID) ([]store.ArtifactSnapshot, error) {
	items := make([]store.ArtifactSnapshot, 0)
	for offset := 0; ; offset += ownedArtifactsPageLimit {
		page, err := c.fetchOwnedArtifactsPage(ctx, offset)
		if err != nil {
			return nil, err
		}
		for _, item := range page.Items {
			id, parseErr := uuid.Parse(item.ID)
			if parseErr != nil {
				return nil, fmt.Errorf("FetchOwnedArtifactSnapshots: invalid id: %w", parseErr)
			}
			var created *time.Time
			if value, parseErr := time.Parse(time.RFC3339, item.CreatedAt); parseErr == nil {
				created = &value
			}
			items = append(items, store.ArtifactSnapshot{ArtifactID: id, AccountID: accountID, Kind: "sdk",
				Name: item.Name, Description: item.Description, Version: item.Version, TargetLanguage: item.TargetLanguage,
				Readme: item.Readme, Selections: artifactSnapshotSelections(item.DetailedSelections), ScopeSchemaVersion: models.ArtifactScopeSchemaVersion,
				RegistryCreatedAt: created})
		}
		if offset+len(page.Items) >= page.Total || len(page.Items) == 0 {
			break
		}
	}
	return items, nil
}

func (c *HTTPRegistryClient) fetchOwnedArtifactsPage(ctx context.Context, offset int) (ownedArtifactsPage, error) {
	req, err := c.newGraphQLRequest(ctx, graphqlQuery{Query: `query OwnedArtifacts($limit: Int!, $offset: Int!) {
		sdks(limit: $limit, offset: $offset, target_type: "sdk", latest_only: true) {
			items {
				id name description version target_type target_language readme created_at
					detailed_selections {
						service_id service_version_id endpoint_ids webhook_ids select_all webhook_select_all
						operation_names webhook_names auth_type auth_name connect_scopes
						injections { location name value mode }
					}
			}
			total
		}
	}`, Variables: map[string]interface{}{"limit": ownedArtifactsPageLimit, "offset": offset}})
	if err != nil {
		return ownedArtifactsPage{}, err
	}
	response, err := c.do(req)
	if err != nil {
		return ownedArtifactsPage{}, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(response.Body)
		return ownedArtifactsPage{}, fmt.Errorf("FetchOwnedArtifactSnapshots: registry returned %d: %s", response.StatusCode, string(body))
	}
	var decoded struct {
		Data struct {
			SDKs ownedArtifactsPage `json:"sdks"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return ownedArtifactsPage{}, err
	}
	if len(decoded.Errors) > 0 {
		return ownedArtifactsPage{}, fmt.Errorf("FetchOwnedArtifactSnapshots: Registry contract mismatch: %s", decoded.Errors[0].Message)
	}
	return decoded.Data.SDKs, nil
}

func artifactSnapshotSelections(selections []models.SDKSelection) json.RawMessage {
	payload, err := json.Marshal(selections)
	if err != nil {
		return json.RawMessage("[]")
	}
	return payload
}

type ownedArtifactRegistry interface {
	FetchOwnedArtifactSnapshots(context.Context, uuid.UUID) ([]store.ArtifactSnapshot, error)
}

func ReconcileOwnedArtifacts(ctx context.Context, destination store.ArtifactSnapshotStore, registry ownedArtifactRegistry, accountID uuid.UUID) (int, error) {
	ctx, span := otel.Tracer("engine").Start(ctx, "engine.artifact_snapshots.reconcile")
	defer span.End()
	snapshots, err := registry.FetchOwnedArtifactSnapshots(ctx, accountID)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "fetch_failed"))
		return 0, err
	}
	if err := destination.UpsertArtifactSnapshots(ctx, snapshots); err != nil {
		span.SetAttributes(attribute.String("outcome", "persist_failed"))
		return 0, err
	}
	span.SetAttributes(attribute.String("outcome", "success"), attribute.Int("artifact_count", len(snapshots)))
	return len(snapshots), nil
}
