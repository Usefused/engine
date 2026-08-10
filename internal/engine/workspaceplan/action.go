// Package workspaceplan owns the immutable action vocabulary shared by plan
// production, apply, and authorization.
package workspaceplan

type ActionType string

const (
	ActionAddService                              ActionType = "add_service"
	ActionEnableServiceVersion                    ActionType = "enable_service_version"
	ActionRemoveService                           ActionType = "remove_service"
	ActionDisableServiceVersion                   ActionType = "disable_service_version"
	ActionDeprecateService                        ActionType = "deprecate_service"
	ActionDeprecateVersion                        ActionType = "deprecate_version"
	ActionPublishServiceExecutionPolicy           ActionType = "publish_service_execution_policy"
	ActionPublishServiceVersionExecutionPolicy    ActionType = "publish_service_version_execution_policy"
	ActionAttachConnectionProfile                 ActionType = "attach_connection_profile"
	ActionDetachConnectionProfile                 ActionType = "detach_connection_profile"
	ActionPublishConnectionProfile                ActionType = "publish_connection_profile"
	ActionSetServicePublic                        ActionType = "set_service_public"
	ActionSetServicePrivate                       ActionType = "set_service_private"
	ActionSetServiceVersionPublic                 ActionType = "set_service_version_public"
	ActionSetServiceVersionPrivate                ActionType = "set_service_version_private"
	ActionSetLocalExecutionPolicy                 ActionType = "set_local_execution_policy"
	ActionResetLocalExecutionPolicy               ActionType = "reset_local_execution_policy"
	ActionSetLocalServiceVersionExecutionPolicy   ActionType = "set_local_service_version_execution_policy"
	ActionResetLocalServiceVersionExecutionPolicy ActionType = "reset_local_service_version_execution_policy"
	ActionCreateBucketBinding                     ActionType = "create_bucket_binding"
)

type AuthorizationClass uint8

const (
	AuthorizationUnknown AuthorizationClass = iota
	AuthorizationServiceManage
	AuthorizationCredentialsManage
)

// actionAuthorization is deliberately the canonical action registry. New
// actions must declare their authorization class here before any producer can
// treat them as valid, preventing plan/apply and authorization drift.
var actionAuthorization = map[ActionType]AuthorizationClass{
	ActionAddService:                              AuthorizationServiceManage,
	ActionEnableServiceVersion:                    AuthorizationServiceManage,
	ActionRemoveService:                           AuthorizationServiceManage,
	ActionDisableServiceVersion:                   AuthorizationServiceManage,
	ActionDeprecateService:                        AuthorizationServiceManage,
	ActionDeprecateVersion:                        AuthorizationServiceManage,
	ActionPublishServiceExecutionPolicy:           AuthorizationServiceManage,
	ActionPublishServiceVersionExecutionPolicy:    AuthorizationServiceManage,
	ActionAttachConnectionProfile:                 AuthorizationServiceManage,
	ActionDetachConnectionProfile:                 AuthorizationServiceManage,
	ActionPublishConnectionProfile:                AuthorizationServiceManage,
	ActionSetServicePublic:                        AuthorizationServiceManage,
	ActionSetServicePrivate:                       AuthorizationServiceManage,
	ActionSetServiceVersionPublic:                 AuthorizationServiceManage,
	ActionSetServiceVersionPrivate:                AuthorizationServiceManage,
	ActionSetLocalExecutionPolicy:                 AuthorizationServiceManage,
	ActionResetLocalExecutionPolicy:               AuthorizationServiceManage,
	ActionSetLocalServiceVersionExecutionPolicy:   AuthorizationServiceManage,
	ActionResetLocalServiceVersionExecutionPolicy: AuthorizationServiceManage,
	ActionCreateBucketBinding:                     AuthorizationCredentialsManage,
}

func (action ActionType) AuthorizationClass() (AuthorizationClass, bool) {
	class, valid := actionAuthorization[action]
	return class, valid
}

func (action ActionType) Valid() bool {
	_, valid := action.AuthorizationClass()
	return valid
}

func (action ActionType) String() string {
	return string(action)
}

// ActionTypes exposes a copy of the registered vocabulary for contract tests
// without maintaining a second list that can drift from authorization.
func ActionTypes() []ActionType {
	actions := make([]ActionType, 0, len(actionAuthorization))
	for action := range actionAuthorization {
		actions = append(actions, action)
	}
	return actions
}
