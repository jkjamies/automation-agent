"""Public API for the root dispatcher package."""

from __future__ import annotations

from automation_agent.agent.root.agents_setup import (
    Deps,
    build_root_dispatcher,
    summary_handler,
)
from automation_agent.agent.root.root import Dispatcher, Handler

__all__ = [
    "Deps",
    "Dispatcher",
    "Handler",
    "build_root_dispatcher",
    "summary_handler",
]
