from typing import Any
from harnest.agent import client_tool


@client_tool
def prepare_service_import(expected_revision: int, target_type: str, source_url: str = "", source_content: str = "", source_mode: str = "spec") -> dict[str, Any]:
    """Prepare a reviewed endpoint or webhook specification import for the service version currently open in the UI. Use target_type endpoints or webhooks and exactly one public specification URL or credential-free OpenAPI document. Uses the same import plan API as fused-cli; opens a review with additions, changes and removals. Does not apply, save, register a receiving URL, or create secrets. Requires fresh page context with serviceImport.available. For a webhook documentation website, set source_mode docs and source_url: the shared Registry discovery API crawls, extracts cited event schemas, preserves existing events and settings, and opens a review. Never fabricate schemas from an unread URL. Website discovery supports documented JSON POST events."""
    ...
