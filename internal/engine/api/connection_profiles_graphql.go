package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/Usefused/engine/internal/engine/sandbox"
	"github.com/Usefused/engine/internal/engine/store"
	"github.com/Usefused/engine/internal/shared/connectionprofile"
	"github.com/Usefused/engine/internal/shared/fusedobject"
	"github.com/google/uuid"
	"github.com/graphql-go/graphql"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
)

var workspaceConnectionBindingGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceConnectionBinding",
	Fields: graphql.Fields{
		"id":                      &graphql.Field{Type: graphql.String},
		"source_kind":             &graphql.Field{Type: graphql.String},
		"literal_value":           &graphql.Field{Type: graphql.String},
		"source_path":             &graphql.Field{Type: graphql.String},
		"target_location":         &graphql.Field{Type: graphql.String},
		"target_name":             &graphql.Field{Type: graphql.String},
		"operation_ids":           &graphql.Field{Type: graphql.NewList(graphql.String)},
		"mode":                    &graphql.Field{Type: graphql.String},
		"provenance":              &graphql.Field{Type: graphql.String},
		"source_profile_revision": &graphql.Field{Type: graphql.Int},
	},
})

// workspaceConnectionProfileGraphQLType projects only the effective profile
// plus safe state (never baseline and override bodies together, never
// secrets/env values/user identifiers/discovery responses), per the plan's
// "Reads" contract.
var workspaceConnectionProfileGraphQLType = graphql.NewObject(graphql.ObjectConfig{
	Name: "WorkspaceConnectionProfile",
	Fields: graphql.Fields{
		"service_id":             &graphql.Field{Type: graphql.String},
		"service_version_id":     &graphql.Field{Type: graphql.String},
		"auth_type":              &graphql.Field{Type: graphql.String},
		"registry_profile_id":    &graphql.Field{Type: graphql.String},
		"profile_revision":       &graphql.Field{Type: graphql.Int},
		"profile_hash":           &graphql.Field{Type: graphql.String},
		"provenance":             &graphql.Field{Type: graphql.String},
		"source":                 &graphql.Field{Type: graphql.String},
		"has_workspace_override": &graphql.Field{Type: graphql.Boolean},
		// is_public reflects whether this profile was published to the Registry
		// via connection_profiles[*].public: true; declarative sync uses it to
		// round-trip that intent back into workspace.yaml.
		"is_public": &graphql.Field{Type: graphql.Boolean},
		"profile":   &graphql.Field{Type: engineJSONType},
		"bindings":  &graphql.Field{Type: graphql.NewList(workspaceConnectionBindingGraphQLType)},
	},
})

// workspaceConnectionProfileGraphQLField returns the effective profile
// (override if present, else baseline) only after account and workspace
// ownership checks at the Engine boundary. There is no bucket dimension:
// every artifact execution in the workspace shares this profile.
func workspaceConnectionProfileGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: workspaceConnectionProfileGraphQLType,
		Args: profileIdentityArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			actor, err := actorFromContext(p.Context)
			if err != nil {
				return nil, err
			}
			identity, err := parseProfileIdentity(p.Args)
			if err != nil {
				return nil, err
			}
			if err := s.VerifyWorkspaceOwner(p.Context, actor.accountID); err != nil {
				return nil, err
			}
			profileStore, err := engineProfileStore(s)
			if err != nil {
				return nil, err
			}
			profile, err := profileStore.GetEffectiveWorkspaceProfile(p.Context, identity.serviceID, identity.versionID, identity.authType)
			if err != nil || profile == nil {
				return nil, err
			}
			bindings, err := profileStore.ListWorkspaceProfileBindings(p.Context, identity.serviceID, identity.versionID, identity.authType)
			if err != nil {
				return nil, err
			}
			return workspaceProfileFields(profile, bindings), nil
		},
	}
}

// setWorkspaceConnectionProfileGraphQLField exposes the shared GraphQL write
// used by UI and CLI while keeping validation and persistence in one
// resolver. Editing an effective profile is always an upsert of the
// workspace override -- callers never choose between create and update.
func setWorkspaceConnectionProfileGraphQLField(s store.Store, verifier ServiceVerifier, registry sandbox.RegistryClient) *graphql.Field {
	return &graphql.Field{
		Type: workspaceConnectionProfileGraphQLType,
		Args: mergeProfileArgs(profileIdentityArgs(), graphql.FieldConfigArgument{
			"version": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
			"profile": &graphql.ArgumentConfig{Type: graphql.NewNonNull(engineJSONType)},
		}),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			return setWorkspaceConnectionProfile(p, s, verifier, registry)
		},
	}
}

// resetWorkspaceConnectionProfileGraphQLField deletes the workspace override
// only; the baseline (if any) survives untouched. This affects dispatch for
// every artifact in the workspace that selects the tuple, so it always
// returns the resulting effective profile (or null) rather than a bare
// boolean, letting the caller show the user what happened.
func resetWorkspaceConnectionProfileGraphQLField(s store.Store) *graphql.Field {
	return &graphql.Field{
		Type: workspaceConnectionProfileGraphQLType,
		Args: profileIdentityArgs(),
		Resolve: func(p graphql.ResolveParams) (interface{}, error) {
			ctx, span := otel.Tracer("engine").Start(p.Context, "engine.connection_profile.workspace_reset")
			defer span.End()
			actor, err := actorFromContext(ctx)
			if err != nil {
				return nil, err
			}
			identity, err := parseProfileIdentity(p.Args)
			if err != nil {
				return nil, err
			}
			if err := s.VerifyWorkspaceOwner(ctx, actor.accountID); err != nil {
				return nil, err
			}
			profileStore, err := engineProfileStore(s)
			if err != nil {
				return nil, err
			}
			if err := profileStore.ResetWorkspaceProfile(ctx, identity.serviceID, identity.versionID, identity.authType); err != nil {
				span.SetAttributes(profileMutationAttrs(identity, "error")...)
				return nil, err
			}
			effective, err := profileStore.GetEffectiveWorkspaceProfile(ctx, identity.serviceID, identity.versionID, identity.authType)
			if err != nil {
				span.SetAttributes(profileMutationAttrs(identity, "error")...)
				return nil, err
			}
			span.SetAttributes(profileMutationAttrs(identity, "reset")...)
			if effective == nil {
				return nil, nil
			}
			bindings, err := profileStore.ListWorkspaceProfileBindings(ctx, identity.serviceID, identity.versionID, identity.authType)
			if err != nil {
				return nil, err
			}
			return workspaceProfileFields(effective, bindings), nil
		},
	}
}

// setWorkspaceConnectionProfile validates against pinned Registry facts before
// atomically replacing the workspace override and its compiled bindings.
func setWorkspaceConnectionProfile(p graphql.ResolveParams, s store.Store, verifier ServiceVerifier, registry sandbox.RegistryClient) (interface{}, error) {
	ctx, span := otel.Tracer("engine").Start(p.Context, "engine.connection_profile.workspace_set")
	defer span.End()
	actor, err := actorFromContext(ctx)
	if err != nil {
		return nil, err
	}
	identity, profile, err := parseWorkspaceProfileArgs(p.Args)
	if err != nil {
		span.SetAttributes(attribute.String("outcome", "invalid"))
		return nil, err
	}
	if err := s.VerifyWorkspaceOwner(ctx, actor.accountID); err != nil {
		return nil, err
	}
	if err := verifyWorkspaceServiceVersionActive(ctx, s, identity.serviceID, identity.versionID); err != nil {
		span.SetAttributes(profileMutationAttrs(identity, "inactive_version")...)
		return nil, err
	}
	version := strings.TrimSpace(fmt.Sprint(p.Args["version"]))
	if err := validateEngineWorkspaceProfile(ctx, verifier, registry, identity, version, profile); err != nil {
		span.SetAttributes(profileMutationAttrs(identity, "validation_failed")...)
		return nil, err
	}
	stored, bindings, err := persistEngineWorkspaceProfile(ctx, s, identity, profile)
	if err != nil {
		span.SetAttributes(profileMutationAttrs(identity, "error")...)
		return nil, err
	}
	// The runtime profile cache (Batch 3) is keyed by non-secret workspace/
	// service/version/auth/revision metadata and is not populated by this
	// store layer today, so there is no separate cache entry to evict here;
	// the store write above is the single source of truth read by dispatch.
	span.SetAttributes(append(profileMutationAttrs(identity, "updated"),
		attribute.Int("profile_revision", stored.ProfileRevision), attribute.Int("binding_count", len(bindings)))...)
	return workspaceProfileFields(stored, bindings), nil
}

// verifyWorkspaceServiceVersionActive requires the exact pinned version to be
// enabled in this workspace before an override can be written for it -- an
// override for a version nobody activated would be dead configuration.
func verifyWorkspaceServiceVersionActive(ctx context.Context, s store.Store, serviceID, versionID uuid.UUID) error {
	statusStore, ok := s.(store.WorkspaceServiceVersionStatusStore)
	if !ok {
		return errors.New("workspace service version status store is unavailable")
	}
	active, err := statusStore.IsWorkspaceServiceVersionActive(ctx, serviceID, versionID)
	if err != nil {
		return err
	}
	if !active {
		return errors.New("service version is not active in this workspace")
	}
	return nil
}

// validateEngineWorkspaceProfile uses the complete pinned contract, preventing
// UI-authored profiles from receiving weaker checks than imported profiles.
func validateEngineWorkspaceProfile(ctx context.Context, verifier ServiceVerifier, registry sandbox.RegistryClient, identity profileIdentity, version string, profile connectionprofile.Profile) error {
	contract, err := engineProfileContract(ctx, verifier, registry, identity, version)
	if err != nil {
		return err
	}
	return connectionprofile.Validate(&profile, contract).Err()
}

// persistEngineWorkspaceProfile snapshots canonical behavior and compiles its
// bindings before a transactional upsert reaches runtime readers.
func persistEngineWorkspaceProfile(ctx context.Context, s store.Store, identity profileIdentity, profile connectionprofile.Profile) (*store.WorkspaceConnectionProfile, []store.WorkspaceConnectionBinding, error) {
	profileStore, err := engineProfileStore(s)
	if err != nil {
		return nil, nil, err
	}
	current, err := profileStore.GetEffectiveWorkspaceProfile(ctx, identity.serviceID, identity.versionID, identity.authType)
	if err != nil {
		return nil, nil, err
	}
	snapshot, hash, err := profileSnapshot(profile)
	if err != nil {
		return nil, nil, err
	}
	desired := store.WorkspaceConnectionProfile{
		ServiceVersionID: identity.versionID, AuthType: identity.authType, Layer: "override",
		ProfileRevision: nextLocalProfileRevision(current), ProfileHash: hash, Provenance: "workspace", ProfileSnapshot: snapshot,
	}
	bindings, err := compileProfileBindings(profile, desired)
	if err != nil {
		return nil, nil, err
	}
	stored, err := profileStore.UpsertWorkspaceProfileOverride(ctx, desired, bindings)
	if err != nil {
		return nil, nil, err
	}
	return stored, bindings, nil
}

type profileIdentity struct {
	serviceID uuid.UUID
	versionID uuid.UUID
	authType  string
}

// profileIdentityArgs centralizes the composite profile identity so query,
// set, and reset operations cannot drift in their required fields.
func profileIdentityArgs() graphql.FieldConfigArgument {
	return graphql.FieldConfigArgument{
		"service_id":         &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		"service_version_id": &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
		"auth_type":          &graphql.ArgumentConfig{Type: graphql.NewNonNull(graphql.String)},
	}
}

// mergeProfileArgs extends a freshly allocated argument map for mutations
// without duplicating the profile identity declaration.
func mergeProfileArgs(left, right graphql.FieldConfigArgument) graphql.FieldConfigArgument {
	for key, value := range right {
		left[key] = value
	}
	return left
}

// parseProfileIdentity validates opaque IDs and canonical auth before any store
// lookup, producing errors that do not echo caller input.
func parseProfileIdentity(args map[string]interface{}) (profileIdentity, error) {
	serviceID, err := uuid.Parse(strings.TrimSpace(fmt.Sprint(args["service_id"])))
	if err != nil {
		return profileIdentity{}, errors.New("service_id is invalid")
	}
	versionID, err := uuid.Parse(strings.TrimSpace(fmt.Sprint(args["service_version_id"])))
	if err != nil {
		return profileIdentity{}, errors.New("service_version_id is invalid")
	}
	authType := connectionprofile.CanonicalAuthType(fmt.Sprint(args["auth_type"]))
	if authType == "" {
		return profileIdentity{}, errors.New("auth_type must be oauth or oidc")
	}
	return profileIdentity{serviceID: serviceID, versionID: versionID, authType: authType}, nil
}

// parseWorkspaceProfileArgs rejects publication controls and normalizes the
// behavior snapshot that will be validated, hashed, and persisted.
func parseWorkspaceProfileArgs(args map[string]interface{}) (profileIdentity, connectionprofile.Profile, error) {
	identity, err := parseProfileIdentity(args)
	if err != nil {
		return profileIdentity{}, connectionprofile.Profile{}, err
	}
	raw, ok := args["profile"].(map[string]interface{})
	if !ok || hasEnginePublicationControl(raw) {
		return profileIdentity{}, connectionprofile.Profile{}, errors.New("profile must be an object without publication controls")
	}
	payload, err := json.Marshal(raw)
	if err != nil {
		return profileIdentity{}, connectionprofile.Profile{}, errors.New("profile is invalid")
	}
	var profile connectionprofile.Profile
	if err := json.Unmarshal(payload, &profile); err != nil {
		return profileIdentity{}, connectionprofile.Profile{}, errors.New("profile is invalid")
	}
	profile.AuthType = connectionprofile.CanonicalAuthType(profile.AuthType)
	if profile.AuthType != identity.authType {
		return profileIdentity{}, connectionprofile.Profile{}, errors.New("profile auth_type must match attachment auth_type")
	}
	connectionprofile.Normalize(&profile)
	return identity, profile, nil
}

// engineProfileContract combines metadata and one bounded operation fetch for
// the exact service version selected by workspace config.
func engineProfileContract(ctx context.Context, verifier ServiceVerifier, registry sandbox.RegistryClient, identity profileIdentity, version string) (connectionprofile.Contract, error) {
	metadata, err := verifier.FetchServiceMetadata(ctx, identity.serviceID, version)
	if err != nil {
		return connectionprofile.Contract{}, err
	}
	if metadata.ServiceVersionID != identity.versionID {
		return connectionprofile.Contract{}, errors.New("service version identity does not match pinned metadata")
	}
	operations, err := registry.FetchServiceOperations(ctx, identity.serviceID, identity.versionID)
	if err != nil {
		return connectionprofile.Contract{}, err
	}
	return connectionprofile.Contract{
		AuthTypes: engineAuthTypes(metadata.AuthConfigs), Servers: engineServerNames(metadata.Servers),
		Operations: engineProfileOperations(operations), Complete: true,
	}, nil
}

// engineAuthTypes projects only auth families needed by profile validation.
func engineAuthTypes(configs fusedobject.AuthConfigs) []string {
	values := make([]string, 0, len(configs))
	for _, config := range configs {
		values = append(values, config.Type)
	}
	return values
}

// engineServerNames keeps only environment identifiers referenced by profiles.
func engineServerNames(servers fusedobject.Servers) []string {
	values := make([]string, 0, len(servers))
	for _, server := range servers {
		if server.Environment != "" {
			values = append(values, server.Environment)
		}
	}
	return values
}

// engineProfileOperations projects endpoint and parameter identity without
// retaining response schemas or other unrelated service metadata.
func engineProfileOperations(endpoints []fusedobject.Endpoint) []connectionprofile.Operation {
	operations := make([]connectionprofile.Operation, 0, len(endpoints))
	for _, endpoint := range endpoints {
		parameters := make([]connectionprofile.Parameter, 0, len(endpoint.Parameters))
		for _, parameter := range endpoint.Parameters {
			parameters = append(parameters, connectionprofile.Parameter{Name: parameter.Name, Location: parameter.In})
		}
		operations = append(operations, connectionprofile.Operation{ID: endpoint.Name, Method: endpoint.Method, Parameters: parameters})
	}
	return operations
}

// compileProfileBindings converts validated expressions to closed runtime
// sources and refuses unresolved environment references at this API boundary.
func compileProfileBindings(profile connectionprofile.Profile, owner store.WorkspaceConnectionProfile) ([]store.WorkspaceConnectionBinding, error) {
	bindings := make([]store.WorkspaceConnectionBinding, 0, len(profile.Bindings))
	for _, configured := range profile.Bindings {
		expression, err := connectionprofile.ParseExpression(configured.Value)
		if err != nil || expression.Kind == connectionprofile.SourceEnvironment {
			return nil, errors.New("profile binding contains an unresolved value")
		}
		binding := store.WorkspaceConnectionBinding{
			ServiceID: owner.ServiceID, ServiceVersionID: owner.ServiceVersionID,
			TargetLocation: configured.Location, TargetName: configured.Name,
			OperationIDs: configured.Operations, Mode: configured.Mode, Provenance: owner.Provenance,
			SourceProfileRevision: &owner.ProfileRevision,
		}
		setCompiledBindingSource(&binding, expression)
		bindings = append(bindings, binding)
	}
	return bindings, nil
}

// setCompiledBindingSource stores either a literal or a resource path so
// dispatch never reparses the profile expression language.
func setCompiledBindingSource(binding *store.WorkspaceConnectionBinding, expression connectionprofile.Expression) {
	if expression.Kind == connectionprofile.SourceLiteral {
		binding.SourceKind = "literal"
		value := expression.Raw
		binding.LiteralValue = &value
		return
	}
	binding.SourceKind = "connection_resource"
	path := expression.SourcePath
	binding.SourcePath = &path
}

// profileSnapshot hashes canonical JSON so profile changes are auditable
// without putting profile content into telemetry.
func profileSnapshot(profile connectionprofile.Profile) ([]byte, string, error) {
	payload, err := json.Marshal(profile)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(payload)
	return payload, hex.EncodeToString(sum[:]), nil
}

// nextLocalProfileRevision gives workspace overrides monotonic local history
// without colliding with Registry-owned publication revisions.
func nextLocalProfileRevision(current *store.WorkspaceConnectionProfile) int {
	if current == nil {
		return 1
	}
	return current.ProfileRevision + 1
}

// workspaceProfileFields projects the safe effective-profile representation
// shared by UI and CLI; compiled rows contain no user connection tokens. The
// source label and has_workspace_override flag let callers render "Provider
// default" / "Fused default" / "Workspace customization" without a second
// query, per the plan's UI information architecture.
func workspaceProfileFields(profile *store.WorkspaceConnectionProfile, bindings []store.WorkspaceConnectionBinding) map[string]interface{} {
	var snapshot interface{}
	_ = json.Unmarshal(profile.ProfileSnapshot, &snapshot)
	items := make([]map[string]interface{}, 0, len(bindings))
	for _, binding := range bindings {
		items = append(items, workspaceBindingFields(binding))
	}
	return map[string]interface{}{
		"service_id": profile.ServiceID.String(), "service_version_id": profile.ServiceVersionID.String(), "auth_type": profile.AuthType,
		"registry_profile_id": optionalUUIDGraphQLValue(profile.RegistryProfileID),
		"profile_revision":    profile.ProfileRevision, "profile_hash": profile.ProfileHash,
		"provenance": profile.Provenance, "source": profile.Provenance,
		"has_workspace_override": profile.Layer == "override",
		"is_public":              profile.IsPublic,
		"profile":                snapshot, "bindings": items,
	}
}

// optionalUUIDGraphQLValue preserves null for workspace-local profiles because an
// empty Registry identity must not be serialized as the all-zero UUID.
func optionalUUIDGraphQLValue(value *uuid.UUID) interface{} {
	if value == nil {
		return nil
	}
	return value.String()
}

// workspaceBindingFields keeps nullable source fields explicit for the
// GraphQL custom object instead of leaking storage implementation details.
func workspaceBindingFields(binding store.WorkspaceConnectionBinding) map[string]interface{} {
	return map[string]interface{}{
		"id": binding.ID.String(), "source_kind": binding.SourceKind,
		"literal_value": optionalStringValue(binding.LiteralValue), "source_path": optionalStringValue(binding.SourcePath),
		"target_location": binding.TargetLocation, "target_name": binding.TargetName,
		"operation_ids": binding.OperationIDs, "mode": binding.Mode, "provenance": binding.Provenance,
		"source_profile_revision": optionalIntValue(binding.SourceProfileRevision),
	}
}

// optionalStringValue preserves GraphQL null semantics for absent sources.
func optionalStringValue(value *string) interface{} {
	if value == nil {
		return nil
	}
	return *value
}

// optionalIntValue preserves GraphQL null semantics for unversioned rows.
func optionalIntValue(value *int) interface{} {
	if value == nil {
		return nil
	}
	return *value
}

// hasEnginePublicationControl prevents workspace authors from asserting public
// or curated status inside a local override.
func hasEnginePublicationControl(config map[string]interface{}) bool {
	for _, key := range []string{"visibility", "provenance", "scope", "public", "owner", "owner_account_id"} {
		if _, ok := config[key]; ok {
			return true
		}
	}
	return false
}

// engineProfileStore keeps the optional store capability explicit for older
// deployments and focused test doubles.
func engineProfileStore(s store.Store) (store.WorkspaceProfileStore, error) {
	profileStore, ok := s.(store.WorkspaceProfileStore)
	if !ok {
		return nil, errors.New("connection profile store is unavailable")
	}
	return profileStore, nil
}

// profileMutationAttrs records non-secret identity and outcome only; snapshots,
// bindings, URLs, and environment values never enter OTEL.
func profileMutationAttrs(identity profileIdentity, outcome string) []attribute.KeyValue {
	return []attribute.KeyValue{
		attribute.String("service_id", identity.serviceID.String()),
		attribute.String("service_version_id", identity.versionID.String()),
		attribute.String("auth_type", identity.authType),
		attribute.String("profile_provenance", "workspace"),
		attribute.String("outcome", outcome),
	}
}
