from typing import Any
from harnest.agent import client_tool

@client_tool
def get_page_context() -> dict[str, Any]:
    """Read the current Fused page, non-sensitive form fields, links and connected editor metadata."""
    ...
