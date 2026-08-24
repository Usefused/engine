package models

import (
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestDecodeAppSelectionsRequiresExactCurrentSchema(t *testing.T) {
	valid := SDKSelection{
		ServiceID: uuid.New(), ServiceVersionID: uuid.New(),
		SchemaVersion: AppSelectionSchemaVersion,
	}
	tests := []struct {
		name      string
		scope     int
		selection SDKSelection
		wantError bool
	}{
		{name: "current", scope: AppScopeSchemaVersion, selection: valid},
		{name: "missing", scope: AppScopeSchemaVersion, selection: SDKSelection{ServiceID: valid.ServiceID, ServiceVersionID: valid.ServiceVersionID}, wantError: true},
		{name: "old", scope: AppScopeSchemaVersion, selection: SDKSelection{ServiceID: valid.ServiceID, ServiceVersionID: valid.ServiceVersionID, SchemaVersion: AppSelectionSchemaVersion - 1}, wantError: true},
		{name: "future", scope: AppScopeSchemaVersion, selection: SDKSelection{ServiceID: valid.ServiceID, ServiceVersionID: valid.ServiceVersionID, SchemaVersion: AppSelectionSchemaVersion + 1}, wantError: true},
		{name: "old scope", scope: AppScopeSchemaVersion - 1, selection: valid, wantError: true},
		{name: "missing service", scope: AppScopeSchemaVersion, selection: SDKSelection{ServiceVersionID: valid.ServiceVersionID, SchemaVersion: AppSelectionSchemaVersion}, wantError: true},
		{name: "missing service version", scope: AppScopeSchemaVersion, selection: SDKSelection{ServiceID: valid.ServiceID, SchemaVersion: AppSelectionSchemaVersion}, wantError: true},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			payload, err := json.Marshal([]SDKSelection{test.selection})
			if err != nil {
				t.Fatalf("marshal selection: %v", err)
			}
			_, err = DecodeAppSelections(test.scope, payload)
			if (err != nil) != test.wantError {
				t.Fatalf("DecodeAppSelections() error = %v, wantError %v", err, test.wantError)
			}
		})
	}
}

func TestDecodeAppSelectionsRejectsRemovedFieldAlias(t *testing.T) {
	serviceID, serviceVersionID := uuid.New(), uuid.New()
	payload := []byte(`[{"service_id":"` + serviceID.String() + `","service_version_id":"` + serviceVersionID.String() + `","schema_version":3,"definition_schema_version":3}]`)
	if _, err := DecodeAppSelections(AppScopeSchemaVersion, payload); err == nil {
		t.Fatal("removed definition schema field was accepted as an alias")
	}
}
