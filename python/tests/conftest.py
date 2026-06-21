"""Shared test fixtures.

A ``FakeLlm`` (the BaseLlm analogue of Go's ``fakeLLM``) yields scripted text and
records the requests it received, so we can test agent wiring and deterministic logic
without a real model. We never assert on real LLM output content.
"""

from __future__ import annotations

import pytest
from google.adk.models import BaseLlm, LlmRequest, LlmResponse
from google.genai import types
from pydantic import PrivateAttr


class FakeLlm(BaseLlm):
    """A deterministic BaseLlm that yields fixed text responses in order."""

    _texts: list[str] = PrivateAttr(default_factory=list)
    _idx: int = PrivateAttr(default=0)
    _requests: list[LlmRequest] = PrivateAttr(default_factory=list)

    def __init__(self, *texts: str) -> None:
        super().__init__(model="fake")
        self._texts = list(texts) or [""]
        self._idx = 0
        self._requests = []

    async def generate_content_async(self, llm_request: LlmRequest, stream: bool = False):  # type: ignore[override]
        self._requests.append(llm_request)
        text = self._texts[min(self._idx, len(self._texts) - 1)]
        self._idx += 1
        yield LlmResponse(
            content=types.Content(role="model", parts=[types.Part.from_text(text=text)]),
            turn_complete=True,
        )

    @property
    def requests(self) -> list[LlmRequest]:
        return self._requests


@pytest.fixture
def fake_llm():
    """Factory: ``fake_llm("a", "b")`` -> a FakeLlm yielding "a" then "b"."""

    def _make(*texts: str) -> FakeLlm:
        return FakeLlm(*texts)

    return _make
