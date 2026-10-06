from harnest.lib.identity import verify_fused_bearer
from harnest import lifecycle
from harnest.runtime_auth import AuthPrincipal, AuthenticationError


@lifecycle.authenticate
async def authenticate(connection, principal):
    """Bind Harnest requests to the Engine-verified actor using the shared identity adapter."""
    identity = await verify_fused_bearer(connection.headers.get("authorization", ""))
    # A prior principal cannot bypass this runtime's Engine identity boundary.
    if identity is None:
        raise AuthenticationError("A verified Fused Engine identity is required")
    return AuthPrincipal(user_id=identity.user_id, claims=identity.claims)
