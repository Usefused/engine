from typing import Any
from harnest.agent import client_tool


@client_tool
def revise_unified_app(goal: str, expected_revision: int) -> dict[str, Any]:
    """Revise the open Unified App draft using Describe's intent, service discovery, operation classification and source APIs. Use for missing operations, additional services or changed business logic. Pass the current page revision. Preserves existing source, service versions, aliases and settings while adding discovered capabilities and revising TypeScript together. Produces an unsaved draft only; never activates services, changes credentials, saves, deploys or executes providers. Read the resulting page and compile."""
    ...
