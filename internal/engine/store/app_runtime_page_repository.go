package store

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

var ErrInvalidAppKind = errors.New("app kind must be sdk or mcp")

type AppRuntimePageRepository interface {
	ListAuthorizedAppRuntimesByAccount(context.Context, uuid.UUID, accesscontrol.AuthorizedScope, string, int, int) ([]AppRuntime, int, error)
}

func normalizeAppKind(kind string) (string, bool) {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case "":
		return "", true
	case "sdk":
		return "sdk", true
	case "mcp":
		return "mcp", true
	default:
		return "", false
	}
}
