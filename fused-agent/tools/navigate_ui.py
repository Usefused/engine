from typing import Any
from harnest.agent import client_tool

@client_tool
def navigate_ui(path: str) -> dict[str, Any]:
    """Open a Fused page whose path was returned by the current page context."""
    ...
