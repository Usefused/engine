package api

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Usefused/engine/internal/engine/auth"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/models"
	"github.com/google/uuid"
)

// TestUnifiedAppCallAncestry rejects cycles while permitting independent siblings of the same target.
func TestUnifiedAppCallAncestry(t *testing.T) {
	root, child := uuid.New(), uuid.New()
	ctx := withUnifiedAppCallRoot(context.Background(), root)
	nested, err := descendUnifiedAppCall(ctx, root, child)
	// A selected child starts a new ancestry without modifying the parent's slice.
	if err != nil {
		t.Fatal(err)
	}
	// Returning to the parent would retain an interpreter waiting on itself.
	if _, err := descendUnifiedAppCall(nested, child, root); err == nil {
		t.Fatal("recursive call accepted")
	}
	// Calling a child twice sequentially is composition, not recursion.
	if _, err := descendUnifiedAppCall(ctx, root, child); err != nil {
		t.Fatal(err)
	}
	for i := 2; i < maxUnifiedAppCallDepth; i++ {
		nested, err = descendUnifiedAppCall(nested, child, uuid.New())
		// Every level up to the bounded depth remains callable.
		if err != nil {
			t.Fatal(err)
		}
	}
	// Extra depth must fail before any target runtime is read or started.
	if _, err := descendUnifiedAppCall(nested, child, uuid.New()); err == nil {
		t.Fatal("unbounded depth")
	}
}

// TestUnifiedAppCallBudget bounds the whole tree even when authored code launches parallel branches.
func TestUnifiedAppCallBudget(t *testing.T) {
	root := uuid.New()
	ctx := withUnifiedAppCallRoot(context.Background(), root)
	var accepted atomic.Int32
	var workers sync.WaitGroup
	for i := 0; i < maxCapabilityFetchCalls*2; i++ {
		workers.Add(1)
		// All sibling calls race on one root-owned budget, never per-worker copies.
		go func() {
			defer workers.Done()
			// Count only admitted calls so races cannot oversubscribe the tree.
			if _, err := descendUnifiedAppCall(ctx, root, uuid.New()); err == nil {
				accepted.Add(1)
			}
		}()
	}
	workers.Wait()
	// Exactly the configured finite budget may cross the nested boundary.
	if accepted.Load() != maxCapabilityFetchCalls {
		t.Fatalf("accepted %d", accepted.Load())
	}
}

// TestUnifiedAppCallHostBoundary proves raw worker requests cannot bypass alias grants or add provider controls.
func TestUnifiedAppCallHostBoundary(t *testing.T) {
	family := uuid.New()
	fixture := &attachmentRuntimeFixture{bindings: []models.UnifiedAppBinding{{Alias: "self", AppFamilyID: family}}}
	host := &executionCapabilityHost{apps: &EngineGRPCServer{store: fixture}, identity: auth.RuntimeIdentity{
		Kind: store.AppKindUnifiedApp, AppFamilyID: family, TokenPolicy: store.AppTokenPolicy{AllowedOperations: []string{"execute"}},
	}}
	for _, request := range []string{
		`{"unifiedApp":"self","input":{},"selector":{"endUserRef":"other"}}`,
		`{"unifiedApp":"self","input":[]} `,
		`{"unifiedApp":"self","input":{}} {}`,
		`{"unifiedApp":"self","input":{}}`,
	} {
		// Invalid shape or missing token authority must stop before the attachment store is consulted.
		if _, err := host.Fetch(context.Background(), json.RawMessage(request)); err == nil {
			t.Fatal("invalid call accepted")
		}
	}
	// Even a well-formed call needs its own alias grant.
	if fixture.reads != 0 {
		t.Fatal("denied call read attachment metadata")
	}
	host.identity.TokenPolicy = store.AppTokenPolicy{AllowAll: true}
	_, err := host.Fetch(context.Background(), json.RawMessage(`{"unifiedApp":"self","input":{}}`))
	// Self-delegation is denied before target lookup, which this fixture deliberately does not implement.
	if err == nil || !strings.Contains(err.Error(), "recursive") {
		t.Fatalf("self call: %v", err)
	}
}

// TestUnifiedAppReferenceOnlyAdmission removes the need for a dummy provider while keeping empty apps invalid.
func TestUnifiedAppReferenceOnlyAdmission(t *testing.T) {
	doc := sdkConfigDocument{APIVersion: "fused/v1", Kind: "unified_app", Name: "Parent", Version: "1.0.0", Bucket: "default", Source: "export default {};",
		UnifiedApps: map[string]models.UnifiedAppReference{"child": {Name: "Child", Version: "1.0.0"}},
	}
	// The full Engine validator, not just attachment parsing, must admit reference-only scope.
	if err := validateUnifiedAppConfigDocument(doc); err != nil {
		t.Fatal(err)
	}
	doc.UnifiedApps = nil
	// Authored source alone must not create undeclared runtime authority.
	if validateUnifiedAppConfigDocument(doc) == nil {
		t.Fatal("empty app accepted")
	}
}
