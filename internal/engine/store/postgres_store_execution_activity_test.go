package store

import (
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
)

func TestEngineExecutionWhereClauseScopesArtifactInSQL(t *testing.T) {
	accountID := uuid.New()
	artifactID := uuid.New()
	whereClause, args := engineExecutionWhereClause(EngineExecutionFilter{
		AccountID: accountID, ArtifactID: artifactID, Transport: "sdk", Status: "failed",
	})

	if !strings.HasPrefix(whereClause, "WHERE account_id = $1 AND artifact_id = $2") {
		t.Fatalf("where clause does not enforce account and artifact scope: %s", whereClause)
	}
	wantArgs := []any{accountID, artifactID, "sdk", "failed"}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("args = %#v, want %#v", args, wantArgs)
	}
}

func TestEngineExecutionWhereClauseDefaultsToServiceScope(t *testing.T) {
	serviceID := uuid.New()
	whereClause, args := engineExecutionWhereClause(EngineExecutionFilter{
		AccountID: uuid.New(), ServiceID: serviceID,
	})

	if !strings.Contains(whereClause, "service_id = $2") || strings.Contains(whereClause, "artifact_id") {
		t.Fatalf("where clause = %s, want service scope", whereClause)
	}
	if args[1] != serviceID {
		t.Fatalf("scope arg = %v, want %s", args[1], serviceID)
	}
}
