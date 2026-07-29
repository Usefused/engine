package requestbinding

import (
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
	"github.com/google/uuid"
)

func TestResolveLiteralAndResourceBindings(t *testing.T) {
	literal := "2026-01"
	baseURL := "base_url"
	accountID := "metadata.account_id"
	bindings := []store.WorkspaceConnectionBinding{
		{SourceKind: "literal", LiteralValue: &literal, TargetLocation: "header", TargetName: "X-Version", Mode: "default"},
		{SourceKind: "connection_resource", SourcePath: &baseURL, TargetLocation: "base_url", Mode: "force"},
		{SourceKind: "connection_resource", SourcePath: &accountID, TargetLocation: "header", TargetName: "X-Account-ID", Mode: "force"},
	}
	values, err := Resolve(bindings, Resource{BaseURL: "https://tenant.example.com", MetadataJSON: []byte(`{"account_id":"acct-1"}`)}, uuid.New())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(values) != 3 || values[0].Value != literal || values[1].Value != "https://tenant.example.com" || values[2].Value != "acct-1" {
		t.Fatalf("Resolve values = %#v", values)
	}
}

func TestResolveRejectsObjectMetadata(t *testing.T) {
	path := "metadata.account"
	_, err := Resolve([]store.WorkspaceConnectionBinding{{SourceKind: "connection_resource", SourcePath: &path}}, Resource{MetadataJSON: []byte(`{"account":{"id":"secret"}}`)}, uuid.New())
	if err == nil {
		t.Fatal("expected object metadata binding to fail")
	}
}

func TestResolvePreservesLargeNumericMetadata(t *testing.T) {
	path := "metadata.portal_id"
	values, err := Resolve([]store.WorkspaceConnectionBinding{{SourceKind: "connection_resource", SourcePath: &path}}, Resource{MetadataJSON: []byte(`{"portal_id":9007199254740993}`)}, uuid.New())
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(values) != 1 || values[0].Value != "9007199254740993" {
		t.Fatalf("metadata value lost precision: %#v", values)
	}
}

func TestHasDynamicSource(t *testing.T) {
	literal := "x"
	if HasDynamicSource([]store.WorkspaceConnectionBinding{{SourceKind: "literal", LiteralValue: &literal}}) {
		t.Fatal("literal binding reported as dynamic")
	}
	if !HasDynamicSource([]store.WorkspaceConnectionBinding{{SourceKind: "connection_resource"}}) {
		t.Fatal("dynamic binding was not detected")
	}
}
