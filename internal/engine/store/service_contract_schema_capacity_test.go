package store

import (
	"strings"
	"testing"

	"github.com/Usefused/engine/internal/shared/canonicaljson"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
)

// TestServiceContractHashAdmitsLargeSchemaProfile exercises consumer normalization beyond ordinary request limits.
func TestServiceContractHashAdmitsLargeSchemaProfile(t *testing.T) {
	raw := []byte(`{"description":"` + strings.Repeat("x", canonicaljson.MaxInputBytes) + `","type":"string"}`)
	hash, err := canonicaljson.HexSchemaSHA256(raw)
	// The input must carry a verified digest under the schema-specific profile.
	if err != nil {
		t.Fatal(err)
	}
	id := uuid.New()
	first := ServiceContractSnapshot{
		ExecutionContractEnvelope: fusedobject.EngineExecutionContractSupport(),
		Endpoints:                 []fusedobject.Endpoint{hashFixtureEndpoint(id, raw, hash, nil)},
	}
	want, err := serviceContractHash(first)
	// Store hashing must not accidentally reapply the ordinary JSON byte ceiling.
	if err != nil {
		t.Fatal(err)
	}
	reordered := []byte(`{"type":"string","description":"` + strings.Repeat("x", canonicaljson.MaxInputBytes) + `"}`)
	second := first
	second.Endpoints = []fusedobject.Endpoint{hashFixtureEndpoint(id, reordered, hash, nil)}
	got, err := serviceContractHash(second)
	// Database-style reserialization must preserve the aggregate service identity too.
	if err != nil || got != want {
		t.Fatalf("large schema service identity changed: %v", err)
	}
}
