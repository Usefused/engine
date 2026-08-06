package api

import (
	"context"
	"testing"

	"github.com/google/uuid"
	"github.com/graphql-go/graphql"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
	"github.com/Usefused/engine/internal/engine/store"
)

type appAccessGraphQLStore struct {
	store.Store
	selectorPage  store.AppSelectorPage
	teamPage      store.AppOwningTeamPage
	selectorCalls int
	teamCalls     int
	selectorQuery store.AppSelectorQuery
	teamQuery     store.ActorTeamSelectorQuery
	decision      store.AppOwnershipDecision
	preflight     store.AppOwnershipPreflight
	preflightErr  error
	teamBySlug    store.Team
	teamBySlugErr error
	resolvedID    uuid.UUID
	referenceCall store.AppOwningTeamReferenceQuery
	referenceErr  error
	apps          map[uuid.UUID]store.App
}

func (s *appAccessGraphQLStore) GetApp(_ context.Context, appID uuid.UUID) (*store.App, error) {
	if app, ok := s.apps[appID]; ok {
		copy := app
		return &copy, nil
	}
	return nil, store.ErrAppNotFound
}

func (s *appAccessGraphQLStore) GetTeamBySlug(context.Context, string) (store.Team, error) {
	return s.teamBySlug, s.teamBySlugErr
}

func (s *appAccessGraphQLStore) ResolveAppOwningTeamReference(_ context.Context, query store.AppOwningTeamReferenceQuery) (uuid.UUID, error) {
	s.referenceCall = query
	return s.resolvedID, s.referenceErr
}

// Existing handler fixtures predate app ownership. Their Owner actor is
// fully authorized, so this shared default keeps unrelated plan tests focused
// while dedicated Sprint 5 tests exercise denial decisions explicitly.
func (s *workspaceTestStore) PreflightAppOwnership(context.Context, store.AppOwnershipPreflight) (store.AppOwnershipDecision, error) {
	return store.AppOwnershipDecision{Allowed: true, MembershipAllowed: true}, nil
}

func (s *workspaceTestStore) ListAppBuildSelectors(context.Context, store.AppSelectorQuery) (store.AppSelectorPage, error) {
	return store.AppSelectorPage{}, nil
}

func (s *workspaceTestStore) ListAppOwningTeams(context.Context, store.ActorTeamSelectorQuery) (store.AppOwningTeamPage, error) {
	return store.AppOwningTeamPage{}, nil
}

func (s *workspaceTestStore) ResolveAppOwningTeamReference(context.Context, store.AppOwningTeamReferenceQuery) (uuid.UUID, error) {
	return uuid.Nil, store.ErrResourceReferenceNotFound
}

func (s *appAccessGraphQLStore) PreflightAppOwnership(_ context.Context, input store.AppOwnershipPreflight) (store.AppOwnershipDecision, error) {
	s.preflight = input
	return s.decision, s.preflightErr
}

func (s *appAccessGraphQLStore) ListAppBuildSelectors(_ context.Context, query store.AppSelectorQuery) (store.AppSelectorPage, error) {
	s.selectorCalls++
	s.selectorQuery = query
	return s.selectorPage, nil
}

func (s *appAccessGraphQLStore) ListAppOwningTeams(_ context.Context, query store.ActorTeamSelectorQuery) (store.AppOwningTeamPage, error) {
	s.teamCalls++
	s.teamQuery = query
	return s.teamPage, nil
}

func TestAppAccessGraphQLSelectorsUseOneRepositoryCallPerAggregate(t *testing.T) {
	actor := controlTestOwnerActor(uuid.New())
	teamID, serviceID := uuid.New(), uuid.New()
	repository := &appAccessGraphQLStore{
		resolvedID: teamID,
		selectorPage: store.AppSelectorPage{Items: []store.AppBuildSelector{{
			Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceService, ID: serviceID}, DisplayName: "GitHub",
		}}, Total: 1},
		teamPage: store.AppOwningTeamPage{Items: []store.AppOwningTeam{{ID: teamID, Name: "Platform", Slug: "platform"}}, Total: 1},
	}
	schema, err := newMCPGraphQLSchema(nil, repository, nil, nil, nil)
	if err != nil {
		t.Fatalf("new schema: %v", err)
	}
	result := graphql.Do(graphql.Params{
		Schema: schema,
		RequestString: `query Selectors($team:ID!){
			appOwningTeams(search:"plat",limit:10,offset:0){total items{id name slug}}
			appBuildSelectors(owner_team_id:$team,resource_type:SERVICE,search:"git",limit:10,offset:0){total items{resource_type resource_id display_name}}
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

func TestAppAccessGraphQLSelectorsDefaultToPersonalOwner(t *testing.T) {
	actor := controlTestOwnerActor(uuid.New())
	repository := &appAccessGraphQLStore{selectorPage: store.AppSelectorPage{Items: []store.AppBuildSelector{}, Total: 0}}
	schema, err := newMCPGraphQLSchema(nil, repository, nil, nil, nil)
	if err != nil {
		t.Fatalf("new schema: %v", err)
	}
	result := graphql.Do(graphql.Params{
		Schema: schema, RequestString: `{ appBuildSelectors(resource_type:SERVICE,limit:10,offset:0){total} }`,
		Context: accesscontrol.ContextWithActor(context.Background(), actor),
	})
	if len(result.Errors) != 0 {
		t.Fatalf("GraphQL errors: %#v", result.Errors)
	}
	if repository.selectorCalls != 1 || repository.selectorQuery.ActorSubjectID != actor.SubjectID || repository.selectorQuery.OwnerTeamID != uuid.Nil {
		t.Fatalf("personal selector query = %#v", repository.selectorQuery)
	}
}

func TestAppAccessGraphQLSelectorsResolveOwnerTeamSlug(t *testing.T) {
	actor := controlTestOwnerActor(uuid.New())
	teamID := uuid.New()
	repository := &appAccessGraphQLStore{resolvedID: teamID, selectorPage: store.AppSelectorPage{Items: []store.AppBuildSelector{}}}
	schema, err := newMCPGraphQLSchema(nil, repository, nil, nil, nil)
	if err != nil {
		t.Fatalf("new schema: %v", err)
	}
	result := graphql.Do(graphql.Params{
		Schema: schema, RequestString: `{ appBuildSelectors(owner_team_id:"platform",resource_type:BUCKET){total} }`,
		Context: accesscontrol.ContextWithActor(context.Background(), actor),
	})
	if len(result.Errors) != 0 {
		t.Fatalf("GraphQL errors: %#v", result.Errors)
	}
	if repository.referenceCall.ActorSubjectID != actor.SubjectID || repository.referenceCall.Reference != "platform" {
		t.Fatalf("reference query = %#v", repository.referenceCall)
	}
	if repository.selectorCalls != 1 || repository.selectorQuery.OwnerTeamID != teamID {
		t.Fatalf("selector query = %#v", repository.selectorQuery)
	}
}

func TestAppAccessGraphQLSelectorsDoNotRevealIneligibleTeamSlugs(t *testing.T) {
	actor := controlTestOwnerActor(uuid.New())
	repository := &appAccessGraphQLStore{referenceErr: store.ErrResourceReferenceNotFound}
	schema, err := newMCPGraphQLSchema(nil, repository, nil, nil, nil)
	if err != nil {
		t.Fatalf("new schema: %v", err)
	}
	result := graphql.Do(graphql.Params{
		Schema: schema, RequestString: `{ appBuildSelectors(owner_team_id:"private-team",resource_type:SERVICE){total} }`,
		Context: accesscontrol.ContextWithActor(context.Background(), actor),
	})
	if len(result.Errors) != 1 || result.Errors[0].Message != "resource was not found; use its name, slug, email, or full UUID" {
		t.Fatalf("GraphQL errors = %#v", result.Errors)
	}
	if repository.selectorCalls != 0 {
		t.Fatalf("selector calls = %d, want 0", repository.selectorCalls)
	}
}

func TestAppAccessGraphQLPolicyTraversesFragmentsAndMultipleRoots(t *testing.T) {
	workspaceID := uuid.New()
	schema, err := newMCPGraphQLSchema(nil, &appAccessGraphQLStore{}, nil, nil, nil)
	if err != nil {
		t.Fatalf("new schema: %v", err)
	}
	body := []byte(`{"query":"query Access($team:ID!){ ...Selectors appOwningTeams { total } } fragment Selectors on EngineQuery { appBuildSelectors(owner_team_id:$team,resource_type:BUCKET){ total } }","variables":{"team":"` + uuid.NewString() + `"}}`)
	plan, err := buildGraphQLAuthorizationPlan(&schema, body, workspaceID)
	if err != nil {
		t.Fatalf("build plan: %v", err)
	}
	want := accesscontrol.Requirement{Permission: accesscontrol.PermissionAppCreate, Resource: accesscontrol.ResourceRef{Type: accesscontrol.ResourceWorkspace, ID: workspaceID}}
	if len(plan.requirements) != 1 || plan.requirements[0] != want || plan.rootFields != 2 {
		t.Fatalf("plan = requirements %#v/root fields %d", plan.requirements, plan.rootFields)
	}
}
