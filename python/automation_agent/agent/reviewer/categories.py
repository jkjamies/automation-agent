"""The consolidated category set + category selection (UI-only gating).

Each category is one consolidated review agent bundling related dimensions; it emits
per-dimension-tagged findings over the whole filtered diff. The glue/synthesis pass
(architectural alignment, testability, test coverage) is built separately — it runs after these
and needs their findings.
"""

from __future__ import annotations

import posixpath
from dataclasses import dataclass
from enum import Enum

from automation_agent.githubapi import PRFile


class Tier(Enum):
    """Selects which model a category runs on: the code-reasoning model for the lenses that need
    it, the base model for the lighter ones (model-size-split)."""

    BASE = 0  # OLLAMA_MODEL (base reasoning)
    CODE = 1  # OLLAMA_CODE_MODEL (code reasoning)


@dataclass(frozen=True)
class Category:
    """One consolidated review agent."""

    name: str  # unique ADK sub-agent name + state-key suffix
    title: str  # human label
    prompt_name: str  # prompts/<prompt_name>.md
    tier: Tier
    ui_only: bool = False  # accessibility runs only when the diff touches UI/markup files
    other: bool = False  # the catch-all: its findings are forced to nitpick


# The consolidated agent set. The glue/synthesis pass is built separately.
CATEGORIES: list[Category] = [
    Category(name="safety", title="Safety", prompt_name="safety", tier=Tier.CODE),
    Category(name="security", title="Security", prompt_name="security", tier=Tier.CODE),
    Category(name="performance", title="Performance", prompt_name="performance", tier=Tier.BASE),
    Category(name="code_quality", title="Code quality", prompt_name="code_quality", tier=Tier.CODE),
    Category(
        name="accessibility",
        title="Accessibility",
        prompt_name="accessibility",
        tier=Tier.BASE,
        ui_only=True,
    ),
    Category(name="other", title="Other", prompt_name="other", tier=Tier.BASE, other=True),
]


def select_categories(files: list[PRFile]) -> list[Category]:
    """Return the categories that apply to a changed-file set: all of them, minus the UI-only
    lens (accessibility) when no UI/markup file changed."""
    ui = has_ui_files(files)
    return [c for c in CATEGORIES if not (c.ui_only and not ui)]


# The file types that warrant an accessibility lens (markup/templates/styles and component
# files).
_UI_EXTENSIONS = frozenset(
    {
        ".html",
        ".htm",
        ".xhtml",
        ".css",
        ".scss",
        ".sass",
        ".less",
        ".jsx",
        ".tsx",
        ".vue",
        ".svelte",
        ".astro",
    }
)


def has_ui_files(files: list[PRFile]) -> bool:
    """Report whether any changed file is UI/markup, by extension."""
    return any(posixpath.splitext(f.path)[1].lower() in _UI_EXTENSIONS for f in files)
