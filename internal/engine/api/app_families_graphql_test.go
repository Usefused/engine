package api

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

type appFamilyGraphQLTestStore struct {
	*artifactReferenceGraphQLTestStore
	family        store.AppFamilyCatalogItem
	limit, offset int
	kind          string
}

// ListAuthorizedAppFamilies records the paginated family query and returns one storage-owned aggregate.
func (s *appFamilyGraphQLTestStore) ListAuthorizedAppFamilies(_ context.Context, accountID uuid.UUID, scope accesscontrol.AuthorizedScope, kind, _ string, limit, offset int) ([]store.AppFamilyCatalogItem, int, error) {
	// Family discovery must retain the authenticated actor's account and app.read scope.
	if accountID != s.accountID || !scope.All {
		return nil, 0, fmt.Errorf("unexpected catalogue authorization scope")
	}
	s.kind, s.limit, s.offset = kind, limit, offset
	return []store.AppFamilyCatalogItem{s.family}, 1, nil
}

// ListAuthorizedAppsByFamily retains immutable sibling rows for the existing UI version selector.
func (s *appFamilyGraphQLTestStore) ListAuthorizedAppsByFamily(_ context.Context, accountID, familyID uuid.UUID, _ accesscontrol.AuthorizedScope) ([]store.AppCatalogItem, error) {
	items := []store.AppCatalogItem{}
	for _, scope := range s.mockScopes {
		// The selector must remain confined to its exact account and logical family.
		if scope.AccountID == accountID && scope.AppFamilyID == familyID {
			items = append(items, appCatalogItemFromTestScope(scope))
		}
	}
	return items, nil
}

// TestAppFamiliesPreserveUIVersionQueries exercises grouped discovery beside the exact queries used by UI lists and details.
func TestAppFamiliesPreserveUIVersionQueries(t *testing.T) {
	for _, kind := range []store.AppKind{store.AppKindSDK, store.AppKindMCP} {
		// Both adapters must keep existing immutable-version UI behavior when family discovery is added.
		t.Run(string(kind), func(t *testing.T) {
			account, family, oldID, newID := uuid.New(), uuid.New(), uuid.New(), uuid.New()
			latestCreatedAt := time.Now().UTC()
			fixture := &workspaceTestStore{accountID: account, mockScopes: map[uuid.UUID]*store.AppRuntime{
				oldID: {AccountID: account, AppFamilyID: family, AppID: oldID, StableAppID: oldID, Kind: kind, Name: "Support:Tools", Version: "1", CreatedAt: latestCreatedAt.Add(-time.Minute)},
				newID: {AccountID: account, AppFamilyID: family, AppID: newID, StableAppID: oldID, Kind: kind, Name: "Support:Tools", Version: "2", CreatedAt: latestCreatedAt},
			}}
			s := &appFamilyGraphQLTestStore{artifactReferenceGraphQLTestStore: &artifactReferenceGraphQLTestStore{workspaceTestStore: fixture}, family: store.AppFamilyCatalogItem{
				AppFamilyID: family, Name: "Support:Tools", Kind: kind, VersionCount: 2,
				LatestAppID: newID, LatestVersion: "2", LatestStatus: store.AppStatusActive, LatestCreatedAt: &latestCreatedAt,
				StableAppID: oldID, StableVersion: "1",
			}}
			h := mountMCPGraphQLTestHandler(t, s)
			data := doMCPGraphQLRequest(t, h, fmt.Sprintf(`query {
    appFamilies(kind:%q,limit:1,offset:0) { total items { app_family_id name version_count latest_version latest_version_id latest_status latest_created_at stable_version stable_version_id transport_urls { streamable_http sse } } }
    apps(kind:%q,limit:20,offset:0) { total items { app_id app_family_id version } }
    app(app_id:%q) { app_id version }
    appVersions(app_family_id:%q) { app_id version }
   }`, kind, kind, oldID.String(), family.String()))
			grouped := data["appFamilies"].(map[string]any)
			items := grouped["items"].([]any)
			// The GraphQL layer must forward the requested family page without changing its grouped total.
			if grouped["total"] != float64(1) || len(items) != 1 || s.kind != string(kind) || s.limit != 1 || s.offset != 0 {
				t.Fatalf("grouped page: %#v", grouped)
			}
			item := items[0].(map[string]any)
			// Grouping preserves logical identity and deterministic latest-version presentation without selecting execution implicitly.
			if item["app_family_id"] != family.String() || item["version_count"] != float64(2) || item["latest_version_id"] != newID.String() || item["latest_version"] != "2" {
				t.Fatalf("family: %#v", item)
			}
			// Only the MCP adapter can advertise explicitly promoted runtime routes.
			if kind == store.AppKindMCP {
				urls := item["transport_urls"].(map[string]any)
				// An older explicit promotion must not float to the newest version's route.
				if item["stable_version_id"] != oldID.String() || urls["streamable_http"] == "" {
					t.Fatalf("promotion: %#v", item)
				}
			} else {
				// SDK discovery may identify the newest row for navigation but must not imply a stable executable route.
				if item["stable_version_id"] != nil || item["transport_urls"] != nil {
					t.Fatalf("SDK has implicit default: %#v", item)
				}
			}
			versions := data["apps"].(map[string]any)
			// UI version lists and sibling selectors must retain both versions, not the grouped row.
			if versions["total"] != float64(2) || len(versions["items"].([]any)) != 2 || len(data["appVersions"].([]any)) != 2 {
				t.Fatalf("UI version queries changed: %#v", data)
			}
			exact := data["app"].(map[string]any)
			// UI detail navigation must remain bound to the exact version ID selected by the user.
			if exact["app_id"] != oldID.String() || exact["version"] != "1" {
				t.Fatalf("UI detail changed: %#v", exact)
			}
		})
	}
}

// TestUnpromotedAppFamilyHasNoTransport protects retained MCP identities after their promoted version is removed.
func TestUnpromotedAppFamilyHasNoTransport(t *testing.T) {
	result := appFamilySummaryFields(nil, store.AppFamilyCatalogItem{AppFamilyID: uuid.New(), Name: "support", Kind: store.AppKindMCP, VersionCount: 1}, nil, false)
	// A remaining version does not authorize an implicit stable promotion.
	if result["transport_urls"] != nil || result["stable_version_id"] != nil {
		t.Fatalf("unpromoted MCP advertised a route: %#v", result)
	}
}

// TestSDKAppFamilyProjectsLatestVersion verifies family navigation and package evidence remain bound to one exact version.
func TestSDKAppFamilyProjectsLatestVersion(t *testing.T) {
	appID := uuid.New()
	createdAt := time.Now().UTC()
	result := appFamilySummaryFields(nil, store.AppFamilyCatalogItem{
		AppFamilyID: uuid.New(), Name: "support", Kind: store.AppKindSDK, TargetLanguage: "typescript", VersionCount: 2,
		LatestAppID: appID, LatestVersion: "2.0.0", LatestStatus: store.AppStatusActive, LatestCreatedAt: &createdAt,
	}, map[uuid.UUID]int64{appID: 7}, true)
	// Latest metadata must expose the same exact identity used for the optional Registry download count.
	if result["latest_version_id"] != appID.String() || result["latest_version"] != "2.0.0" || result["downloads"] != "7" || result["latest_created_at"] == "" {
		t.Fatalf("latest SDK projection: %#v", result)
	}
}
