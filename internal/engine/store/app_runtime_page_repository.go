package store

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

var ErrInvalidAppKind = errors.New("app kind must be sdk, mcp, or execution")

type AppRuntimePageRepository interface {
	ListAuthorizedAppRuntimesByAccount(context.Context, uuid.UUID, accesscontrol.AuthorizedScope, string, int, int) ([]AppRuntime, int, error)
}

// normalizeAppKind validates read filters before they can widen an authorized runtime page.
func normalizeAppKind(kind string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "":
		return "", true
	case string(AppKindSDK):
		return AppKindSDK.String(), true
	case string(AppKindMCP):
		return AppKindMCP.String(), true
	case string(AppKindUnifiedApp):
		return AppKindUnifiedApp.String(), true
	default:
		return "", false
	}
}
