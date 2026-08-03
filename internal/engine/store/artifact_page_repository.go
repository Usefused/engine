package store

import (
	"context"
	"errors"
	"strings"

	"github.com/google/uuid"

	"github.com/Usefused/engine/internal/engine/accesscontrol"
)

var ErrInvalidArtifactKind = errors.New("artifact kind must be sdk or mcp")

type ArtifactPageRepository interface {
	ListAuthorizedArtifactScopesByAccount(context.Context, uuid.UUID, accesscontrol.AuthorizedScope, string, int, int) ([]ArtifactScope, int, error)
}

func normalizeArtifactKind(kind string) (string, bool) {
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
