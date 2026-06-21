"""The lint-fixer's prompt loader (its own ``prompts/`` dir, reviewable next to it)."""

from __future__ import annotations

from automation_agent.agent import setup

prompts = setup.Prompts("automation_agent.agent.lintfixer")
