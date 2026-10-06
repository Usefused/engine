from typing import Any
from harnest.agent import client_tool

@client_tool
def read_selected_contracts(path: str = "", offset: int = 0) -> dict[str, Any]:
    """Read the exact authorized provider contracts selected in the connected Unified App editor."""
    ...
