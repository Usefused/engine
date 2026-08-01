package accesscontrol

import (
	"context"
	"strconv"
	"testing"

	"github.com/google/uuid"
)

type benchmarkPrincipalLoader struct {
	principal ControlPrincipal
}

func (loader benchmarkPrincipalLoader) LoadControlPrincipal(context.Context, string) (ControlPrincipal, error) {
	return loader.principal, nil
}

func BenchmarkAuthenticatorAndAuthorization(b *testing.B) {
	for _, grantCount := range []int{1, 10, 50} {
		b.Run("cold_grants_"+strconv.Itoa(grantCount), func(b *testing.B) {
			benchmarkColdAuthentication(b, grantCount)
		})
	}
	b.Run("cache_hit", benchmarkCachedAuthentication)
}

func benchmarkColdAuthentication(b *testing.B, grantCount int) {
	principal, requirement := benchmarkPrincipal(grantCount)
	authenticator, err := NewAuthenticator(benchmarkPrincipalLoader{principal: principal}, 1, AuthenticatorOptions{CacheEntries: 128})
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for index := range b.N {
		actor, authErr := authenticator.AuthenticateControlCredential(context.Background(), "benchmark-"+strconv.Itoa(index))
		if authErr != nil {
			b.Fatal(authErr)
		}
		if authErr = (SnapshotAuthorizer{}).CheckAll(context.Background(), actor, requirement); authErr != nil {
			b.Fatal(authErr)
		}
	}
}

func benchmarkCachedAuthentication(b *testing.B) {
	principal, requirement := benchmarkPrincipal(50)
	authenticator, err := NewAuthenticator(benchmarkPrincipalLoader{principal: principal}, 1, AuthenticatorOptions{})
	if err != nil {
		b.Fatal(err)
	}
	if _, err = authenticator.AuthenticateControlCredential(context.Background(), "cached"); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		actor, authErr := authenticator.AuthenticateControlCredential(context.Background(), "cached")
		if authErr != nil {
			b.Fatal(authErr)
		}
		if authErr = (SnapshotAuthorizer{}).CheckAll(context.Background(), actor, requirement); authErr != nil {
			b.Fatal(authErr)
		}
	}
}

func benchmarkPrincipal(grantCount int) (ControlPrincipal, Requirement) {
	workspaceID := uuid.New()
	grants := make([]Grant, grantCount)
	for index := range grants {
		grants[index] = Grant{
			Permission: PermissionServiceConsume,
			Resource:   ResourceRef{Type: ResourceService, ID: uuid.New()},
		}
	}
	return ControlPrincipal{
		AccountID: uuid.New(), WorkspaceID: workspaceID, SubjectID: uuid.New(),
		CredentialID: uuid.New(), Kind: SubjectUser, Revision: 1, EffectiveGrants: grants,
	}, Requirement{Permission: grants[0].Permission, Resource: grants[0].Resource}
}
