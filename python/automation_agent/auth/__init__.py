"""GitHub authentication seam: PAT vs GitHub App installation tokens."""

from __future__ import annotations

from automation_agent.auth.auth import (
    AppProvider,
    IdentityResolver,
    StaticProvider,
    TokenProvider,
    new_app_provider,
)

__all__ = [
    "AppProvider",
    "IdentityResolver",
    "StaticProvider",
    "TokenProvider",
    "new_app_provider",
]
