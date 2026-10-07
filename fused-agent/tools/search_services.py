from typing import Any
from harnest.agent import client_tool

@client_tool
def search_services(query: str, offset: int = 0) -> dict[str, Any]:
    """Search authorized services by name or keyword. Returns 20 names, canonical refs and internal IDs per page; use next_offset to continue. Never enables a service."""
    ...
