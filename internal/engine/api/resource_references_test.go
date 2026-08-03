package api

import (
	"testing"

	"github.com/Usefused/engine/internal/engine/store"
)

func TestResourceReferenceGraphQLErrorPublishesSafeCategory(t *testing.T) {
	tests := []struct {
		name string
		err  error
		code string
	}{
		{name: "not found", err: store.ErrResourceReferenceNotFound, code: graphQLCodeResourceNotFound},
		{name: "ambiguous", err: store.ErrResourceReferenceAmbiguous, code: graphQLCodeResourceAmbiguous},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := resourceReferenceGraphQLError(test.err)
			extended, ok := err.(interface{ Extensions() map[string]interface{} })
			if !ok {
				t.Fatalf("error %T does not expose safe GraphQL extensions", err)
			}
			if code := extended.Extensions()["code"]; code != test.code {
				t.Fatalf("code = %v, want %q", code, test.code)
			}
		})
	}
}
