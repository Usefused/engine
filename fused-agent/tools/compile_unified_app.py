from typing import Any
from harnest.agent import client_tool

@client_tool
def compile_unified_app(expected_revision: int) -> dict[str, Any]:
    """Validate and compile the current Unified App draft when its editor is connected. Does not deploy."""
    ...
