from typing import Any
from harnest.agent import client_tool

@client_tool
def update_form_field(expected_revision: int, field_id: str, value: str | bool) -> dict[str, Any]:
    """Update one visible non-private form field, using its current page revision. Does not submit or save."""
    ...
