package api

import (
	"testing"

	enginev1 "github.com/Usefused/engine/internal/engine/grpc/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// TestValidateUnifiedPaginationIntentsUsesSelectedPublicTargets proves per-target controls use result namespaces rather than service namespaces.
func TestValidateUnifiedPaginationIntentsUsesSelectedPublicTargets(t *testing.T) {
	intents, err := validateUnifiedPaginationIntents(map[string]*enginev1.PaginationIntent{
		"jira": {MaxPages: 1},
	}, []string{"jira"})
	if err != nil {
		t.Fatalf("selected target error = %v", err)
	}
	if intents["jira"] == nil || intents["jira"].MaxPages != 1 {
		t.Fatalf("selected target intent = %#v", intents["jira"])
	}
}

// TestValidateUnifiedPaginationIntentsRejectsUnselectedTarget proves hidden graph targets cannot receive caller controls.
func TestValidateUnifiedPaginationIntentsRejectsUnselectedTarget(t *testing.T) {
	_, err := validateUnifiedPaginationIntents(map[string]*enginev1.PaginationIntent{
		"hidden": {MaxPages: 1},
	}, []string{"jira"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unselected target code = %s, want %s: %v", status.Code(err), codes.InvalidArgument, err)
	}
}

// TestValidateUnifiedPaginationIntentsRejectsNullValue proves a present map key always carries explicit bounded intent.
func TestValidateUnifiedPaginationIntentsRejectsNullValue(t *testing.T) {
	_, err := validateUnifiedPaginationIntents(map[string]*enginev1.PaginationIntent{"jira": nil}, []string{"jira"})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("null target code = %s, want %s: %v", status.Code(err), codes.InvalidArgument, err)
	}
}
