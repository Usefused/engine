from typing import Any
from harnest.agent import client_tool

@client_tool
def read_service_contract(service_id: str, version: str, operation: str, path: str = "", offset: int = 0) -> dict[str, Any]:
    """Read a discovered endpoint's authorized request/response contract without selecting or executing it. Use returned paths and offsets for bounded details. Never reads credentials."""
    ...
