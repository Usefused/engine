"""Adapt the shared Harnest gateway lifecycle to Registry-owned Fused inference."""
from dataclasses import dataclass, field
import os

import httpx
from openai import AsyncOpenAI
from harnest.model import LiteLLMModel, LiteLLMLifecycle


@dataclass(frozen=True)
class GatewaySettings:
    """Engine resolves configuration; this project never reads unrelated Engine secrets."""
    base_url: str
    model: str
    api_key: str = field(repr=False)
    registry: bool = True


def load_gateway() -> GatewaySettings:
    """Use Engine's explicit child configuration instead of inheriting ambient provider defaults."""
    endpoint = os.environ.get("FUSED_AGENT_GATEWAY_URL", "").strip()
    key = os.environ.get("FUSED_AGENT_GATEWAY_KEY", "").strip()
    model = os.environ.get("FUSED_AGENT_MODEL", "fused-agent").strip()
    # Missing configuration must fail before the SDK can select another model or credential.
    if not endpoint or not key or not model:
        raise ValueError("Fused agent gateway URL, credential and model are required")
    return GatewaySettings(endpoint, model, key, os.environ.get("FUSED_AGENT_REGISTRY_GATEWAY") == "true")


class GatewayLifecycle(LiteLLMLifecycle):
    """Own a per-model transport as in the shared agent; Registry is the default model authority."""

    def __init__(self, settings: GatewaySettings):
        """Keep transport configuration isolated from other agents and process-global SDK settings."""
        self.settings = settings

    async def create_transport(self, context):
        """Send server credentials only to the configured gateway, without redirects or ambient proxies."""
        # The Registry license header is inappropriate for custom model providers.
        headers = {"X-API-Key": self.settings.api_key} if self.settings.registry else {}
        http = httpx.AsyncClient(trust_env=False, follow_redirects=False, timeout=180)
        return AsyncOpenAI(base_url=self.settings.base_url, api_key=self.settings.api_key,
                           organization="", project="", max_retries=0,
                           default_headers=headers, http_client=http)

    async def close(self, context):
        """Release this model's pooled connections when its lifecycle ends."""
        # Startup can fail before a transport exists.
        if context.transport is not None:
            await context.transport.close()


def configured_model() -> LiteLLMModel:
    """Preserve the gateway lifecycle and serialize page tools across browser suspension boundaries."""
    settings = load_gateway()
    return LiteLLMModel(model=f"openai/{settings.model}", api_base=settings.base_url,
                        api_key=settings.api_key, parallel_tool_calls=False, lifecycle=GatewayLifecycle(settings))
