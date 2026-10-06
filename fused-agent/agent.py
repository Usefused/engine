from harnest.agent import Agent
from harnest.lib.ai_gateway import configured_model


# Harnest discovers project instructions, skills and tools, and retains follow-ups in the authenticated session.
root_agent = Agent(
    name="fused_agent",
    history="session",
    model=configured_model(),
    description=(
        "Helps teams inspect Fused workspace data and edit drafts using "
        "authenticated, permission-aware tools."
    ),
)
