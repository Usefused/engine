package ratelimitcoordinator

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"sort"
	"strings"

	"github.com/Usefused/engine/internal/shared/ratelimitpolicy"
	"github.com/google/uuid"
)

func validateAcquireRequest(request ratelimitpolicy.AcquireRequest) ([]ratelimitpolicy.ResolvedPolicy, string, error) {
	if request.AccountID == uuid.Nil || request.ServiceVersionID == uuid.Nil || len(request.Policies) == 0 {
		return nil, "", errors.New("provider rate-limit acquisition identity is incomplete")
	}
	policies := append([]ratelimitpolicy.ResolvedPolicy(nil), request.Policies...)
	sortPolicies(policies)
	identities := make([]policyIdentity, len(policies))
	for index, policy := range policies {
		identity, err := validatedPolicyIdentity(policy)
		if err != nil {
			return nil, "", err
		}
		if index > 0 && identity == identities[index-1] {
			return nil, "", errors.New("provider rate-limit policy identity is duplicated")
		}
		identities[index] = identity
	}
	return policies, stateKey(request.AccountID, request.ServiceVersionID, identities), nil
}

func validateSyncRequest(request ratelimitpolicy.SyncRequest) (string, error) {
	if request.AccountID == uuid.Nil || request.ServiceVersionID == uuid.Nil || len(request.Observations) == 0 {
		return "", errors.New("provider rate-limit synchronization identity is incomplete")
	}
	identities := make([]policyIdentity, len(request.Observations))
	for index, observation := range request.Observations {
		if observation.PolicyName == "" || observation.ScopeKind == "" || observation.ScopeID == uuid.Nil {
			return "", errors.New("provider rate-limit observation identity is incomplete")
		}
		identities[index] = policyIdentity{observation.PolicyName, observation.ScopeKind, observation.ScopeID}
	}
	sort.Slice(identities, func(i, j int) bool { return identities[i].less(identities[j]) })
	for index := 1; index < len(identities); index++ {
		if identities[index] == identities[index-1] {
			return "", errors.New("provider rate-limit observation identity is duplicated")
		}
	}
	return stateKey(request.AccountID, request.ServiceVersionID, identities), nil
}

type policyIdentity struct {
	name      string
	scopeKind string
	scopeID   uuid.UUID
}

func (p policyIdentity) less(other policyIdentity) bool {
	if p.name != other.name {
		return p.name < other.name
	}
	if p.scopeKind != other.scopeKind {
		return p.scopeKind < other.scopeKind
	}
	return p.scopeID.String() < other.scopeID.String()
}

func validatedPolicyIdentity(policy ratelimitpolicy.ResolvedPolicy) (policyIdentity, error) {
	scopeID, err := uuid.Parse(policy.ScopeID)
	if err != nil || policy.Name == "" || policy.ScopeKind == "" || policy.ConfigHash == "" || policy.Cost < 1 {
		return policyIdentity{}, errors.New("provider rate-limit policy is incomplete")
	}
	return policyIdentity{policy.Name, policy.ScopeKind, scopeID}, nil
}

func sortPolicies(policies []ratelimitpolicy.ResolvedPolicy) {
	sort.Slice(policies, func(i, j int) bool {
		left, _ := validatedPolicyIdentity(policies[i])
		right, _ := validatedPolicyIdentity(policies[j])
		return left.less(right)
	})
}

func stateKey(accountID, serviceVersionID uuid.UUID, identities []policyIdentity) string {
	var input strings.Builder
	input.WriteString(accountID.String())
	input.WriteByte('|')
	input.WriteString(serviceVersionID.String())
	for _, identity := range identities {
		input.WriteByte('|')
		input.WriteString(identity.name)
		input.WriteByte('|')
		input.WriteString(identity.scopeKind)
		input.WriteByte('|')
		input.WriteString(identity.scopeID.String())
	}
	sum := sha256.Sum256([]byte(input.String()))
	return "v1." + hex.EncodeToString(sum[:])
}
