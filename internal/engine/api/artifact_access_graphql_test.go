package api

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

type artifactAccessGraphQLStore struct {
	store.Store
	selectorPage  store.ArtifactSelectorPage
	teamPage      store.ArtifactOwningTeamPage
	selectorCalls int
	teamCalls     int
	selectorQuery store.ArtifactSelectorQuery
	teamQuery     store.ActorTeamSelectorQuery
	decision      store.ArtifactOwnershipDecision
	preflight     store.ArtifactOwnershipPreflight
	preflightErr  error
}

// Existing handler fixtures predate artifact ownership. Their Owner actor is
// fully authorized, so this shared default keeps unrelated plan tests focused
// while dedicated Sprint 5 tests exercise denial decisions explicitly.
func (s *workspaceTestStore) PreflightArtifactOwnership(context.Context, store.ArtifactOwnershipPreflight) (store.ArtifactOwnershipDecision, error) {
	return store.ArtifactOwnershipDecision{Allowed: true, MembershipAllowed: true}, nil
}

func (s *workspaceTestStore) ListArtifactBuildSelectors(context.Context, store.ArtifactSelectorQuery) (store.ArtifactSelectorPage, error) {
	return store.ArtifactSelectorPage{}, nil
}

func (s *workspaceTestStore) ListArtifactOwningTeams(context.Context, store.ActorTeamSelectorQuery) (store.ArtifactOwningTeamPage, error) {
	return store.ArtifactOwningTeamPage{}, nil
}

func (s *artifactAccessGraphQLStore) PreflightArtifactOwnership(_ context.Context, input store.ArtifactOwnershipPreflight) (store.ArtifactOwnershipDecision, error) {
	s.preflight = input
	return s.decision, s.preflightErr
}

func (s *artifactAccessGraphQLStore) ListArtifactBuildSelectors(_ context.Context, query store.ArtifactSelectorQuery) (store.ArtifactSelectorPage, error) {
	s.selectorCalls++
	s.selectorQuery = query
	return s.selectorPage, nil
}

func (s *artifactAccessGraphQLStore) ListArtifactOwningTeams(_ context.Context, query store.ActorTeamSelectorQuery) (store.ArtifactOwningTeamPage, error) {
	s.teamCalls++
	s.teamQuery = query
	return s.teamPage, nil
}

func TestArtifactAccessGraphQLSelectorsUseOneRepositoryCallPerAggregate(t *testing.T) {
	actor := controlTestOwnerActor(uuid.New())
	teamID, serviceID := uuid.New(), uuid.New()
	repository := &artifactAccessGraphQLStore{
		selectorPage: store.ArtifactSelectorPage{Items: []store.ArtifactBuildSelector{{
			Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}, DisplayName: "GitHub",
		}}, Total: 1},
		teamPage: store.ArtifactOwningTeamPage{Items: []store.ArtifactOwningTeam{{ID: teamID, Name: "Platform", Slug: "platform"}}, Total: 1},
	}
	schema, err := newMCPGraphQLSchema(nil, repository, nil, nil, nil)
	if err != nil {
		t.Fatalf("new schema: %v", err)
	}
	result := graphql.Do(graphql.Params{
		Schema: schema,
		RequestString: `query Selectors($team:ID!){
			artifactOwningTeams(search:"plat",limit:10,offset:0){total items{id name slug}}
			artifactBuildSelectors(owner_team_id:$team,resource_type:SERVICE,search:"git",limit:10,offset:0){total items{resource_type resource_id display_name}}
		}`,
		VariableValues: map[string]interface{}{"team": teamID.String()},
		Context:        accesscontrol.ContextWithActor(context.Background(), actor),
	})
	if len(result.Errors) != 0 {
		t.Fatalf("GraphQL errors: %#v", result.Errors)
	}
	if repository.selectorCalls != 1 || repository.teamCalls != 1 {
		t.Fatalf("repository calls = selector %d/team %d, want 1/1", repository.selectorCalls, repository.teamCalls)
	}
	if repository.selectorQuery.ActorSubjectID != actor.SubjectID || repository.selectorQuery.OwnerTeamID != teamID || repository.selectorQuery.ResourceType != accesscontrol.ResourceService {
		t.Fatalf("selector query = %#v", repository.selectorQuery)
	}
	if repository.teamQuery.ActorSubjectID != actor.SubjectID {
		t.Fatalf("team query actor = %s, want %s", repository.teamQuery.ActorSubjectID, actor.SubjectID)
	}
}

func TestArtifactAccessGraphQLPolicyTraversesFragmentsAndMultipleRoots(t *testing.T) {
	workspaceID := uuid.New()
	schema, err := newMCPGraphQLSchema(nil, &artifactAccessGraphQLStore{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("new schema: %v", err)
	}
	body := []byte(`{"query":"query Access($team:ID!){ ...Selectors artifactOwningTeams { total } } fragment Selectors on EngineQuery { artifactBuildSelectors(owner_team_id:$team,resource_type:BUCKET){ total } }","variables":{"team":"` + uuid.NewString() + `"}}`)
	plan, err := buildGraphQLAuthorizationPlan(&schema, body, workspaceID)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	want := accesscontrol.Requirement{Permission: accesscontrol.PermissionArtifactCreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}}
	if len(plan.requirements) != 1 || plan.requirements[0] != want || plan.rootFields != 2 {
		t.Fatalf("plan = requirements %#v/root fields %d", plan.requirements, plan.rootFields)
	}
}
