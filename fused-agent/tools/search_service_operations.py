from typing import Any
from harnest.agent import client_tool

@client_tool
def search_service_operations(service_id: str, version: str = "", query: str = "", offset: int = 0) -> dict[str, Any]:
    """Browse an observed service's versions when version is empty; otherwise search endpoints in that exact version. Empty query lists endpoints. Returns 20 per page. Preserve an existing app's pinned version."""
    ...
