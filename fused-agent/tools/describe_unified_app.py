from typing import Any
from harnest.agent import client_tool


@client_tool
def describe_unified_app(goal: str) -> dict[str, Any]:
    """Use the open Unified App creation form's Describe flow to select exact service operations and generate an unsaved TypeScript draft. Supply the user's goal and explicit service/publisher choices. Does not activate services, create credentials, save, deploy, or execute provider operations. Read the resulting page before editing or compiling."""
    ...
