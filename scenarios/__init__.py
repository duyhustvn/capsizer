from __future__ import annotations

from .base import Scenario
from .chat_sse import ChatSSEScenario
from .rest import RestScenario

REGISTRY: dict[str, type[Scenario]] = {
    "chat-sse": ChatSSEScenario,
    "rest": RestScenario,
}

__all__ = ["Scenario", "ChatSSEScenario", "RestScenario", "REGISTRY"]
