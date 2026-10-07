from typing import Any
from harnest.agent import client_tool


@client_tool
def set_unified_app_view(view: str, expected_revision: int) -> dict[str, Any]:
    """Open the connected Unified App's yaml or typescript editor with a fresh page revision. Returns visible config/source fields for reading and update_form_field. YAML contains configuration and credential references, not secret values. Repair invalid YAML before switching back. This only changes the editor tab; never saves or deploys."""
    ...
