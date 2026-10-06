from typing import Any
from harnest.agent import client_tool

@client_tool
def validate_form() -> dict[str, Any]:
    """Check native form constraints on the current page without submitting any form."""
    ...
