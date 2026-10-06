"""Verify the Engine-authenticated actor before Harnest can read sessions or resume tools."""
from dataclasses import dataclass
import os
from typing import Any, Mapping

import jwt


@dataclass(frozen=True, slots=True)
class VerifiedIdentity:
    """Only verified, non-secret identity facts may enter the Harnest principal."""
    user_id: str
    claims: Mapping[str, Any]


async def verify_fused_bearer(authorization: str) -> VerifiedIdentity | None:
    """Adapt the Engine identity boundary to Fused's short-lived, agent-only bearer tokens."""
    # Bound malformed headers before parsing; only the Engine proxy issues this private runtime credential.
    if not isinstance(authorization, str) or len(authorization) > 8192:
        return None
    parts = authorization.split()
    # Do not accept cookies, arbitrary browser JWTs or another authentication scheme.
    if len(parts) != 2 or parts[0].casefold() != "bearer":
        return None
    try:
        claims = jwt.decode(parts[1], os.environ["FUSED_AGENT_IDENTITY_KEY"], algorithms=["HS256"],
                            audience="fused-agent", issuer="fused-engine",
                            options={"require": ["exp", "iat", "sub", "aud", "iss", "workspace_id"]})
        subject, workspace = claims["sub"], claims["workspace_id"]
        # Both stable actor and workspace scope are mandatory for conversation isolation.
        if not isinstance(subject, str) or not subject.strip() or not isinstance(workspace, str) or not workspace.strip():
            return None
        return VerifiedIdentity(user_id=subject, claims={"workspace_id": workspace})
    except (ValueError, KeyError, jwt.PyJWTError):
        return None
