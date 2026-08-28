package store

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/google/uuid"
)

type appCatalogScanFixture struct {
	values []any
}

// Scan projects one deterministic row into the repository's stable column order.
func (fixture appCatalogScanFixture) Scan(dest ...any) error {
	*(dest[0].(*uuid.UUID)) = fixture.values[0].(uuid.UUID)
	*(dest[1].(*uuid.UUID)) = fixture.values[1].(uuid.UUID)
	*(dest[2].(*uuid.UUID)) = fixture.values[2].(uuid.UUID)
	*(dest[3].(*string)) = fixture.values[3].(string)
	*(dest[4].(*string)) = fixture.values[4].(string)
	*(dest[5].(*string)) = fixture.values[5].(string)
	*(dest[6].(*AppKind)) = AppKind(fixture.values[6].(string))
	*(dest[7].(*AppStatus)) = AppStatus(fixture.values[7].(string))
	*(dest[8].(*time.Time)) = fixture.values[8].(time.Time)
	*(dest[9].(*string)) = fixture.values[9].(string)
	*(dest[10].(*string)) = fixture.values[10].(string)
	*(dest[11].(*string)) = fixture.values[11].(string)
	*(dest[12].(*json.RawMessage)) = fixture.values[12].([]byte)
	*(dest[13].(**time.Time)) = fixture.values[13].(*time.Time)
	return nil
}

// TestScanAppCatalogItemPreservesExactVersionIdentityAndSelections verifies stable-family metadata does not shift immutable fields.
func TestScanAppCatalogItemPreservesExactVersionIdentityAndSelections(t *testing.T) {
	familyID, appID, serviceID := uuid.New(), uuid.New(), uuid.New()
	createdAt, deactivationAt := time.Now(), time.Now().Add(24*time.Hour)
	item, err := scanAppCatalogItem(appCatalogScanFixture{values: []any{
		familyID, appID, uuid.Nil, "Support", "Internal support SDK", "2.0.0", "sdk", "deprecated",
		createdAt, "typescript", "generator-v2", "# Support", []byte(`[{"service_id":"` + serviceID.String() + `","endpoint_ids":[],"webhook_ids":[]}]`), &deactivationAt,
	}})
	if err != nil {
		t.Fatalf("scanAppCatalogItem: %v", err)
	}
	if item.AppFamilyID != familyID || item.AppID != appID || item.Version != "2.0.0" || len(item.Selections) != 1 || item.Selections[0].ServiceID != serviceID {
		t.Fatalf("unexpected app catalogue item: %#v", item)
	}
}
