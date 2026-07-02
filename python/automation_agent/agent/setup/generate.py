"""Single-shot non-streaming text completion helper.

Lets callers outside ``setup`` use a model without importing genai directly.
"""

from __future__ import annotations

from google.adk.models import BaseLlm, LlmRequest
from google.genai import types

from automation_agent.agent.setup.events import content_text, user_text


def json_config() -> types.GenerateContentConfig:
    """Request JSON-formatted model output. On the local Ollama path this switches the
    adapter into JSON mode (so the response is at least syntactically valid JSON); it does
    not enforce a schema. Lives here so callers need not import the provider SDK directly."""
    return types.GenerateContentConfig(response_mime_type="application/json")


async def generate_text(llm: BaseLlm, system: str, user: str) -> str:
    """Run one completion (``system`` instruction + ``user`` prompt) and return text."""
    req = LlmRequest(
        contents=[user_text(user)],
        config=types.GenerateContentConfig(system_instruction=system),
    )
    parts: list[str] = []
    async for resp in llm.generate_content_async(req, stream=False):
        if resp.content is not None:
            parts.append(content_text(resp.content))
    return "".join(parts)
