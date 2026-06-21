"""setup — shared agent-building utilities (LLM switch, prompts, runner, longrun).

The only package permitted to import provider SDKs (LiteLlm / Gemini / genai),
enforced by the arch tests.
"""

from automation_agent.agent.setup.events import (
    assistant_text,
    content_text,
    last_text,
    state_string,
    text_event,
    user_text,
)
from automation_agent.agent.setup.generate import generate_text
from automation_agent.agent.setup.llm import build_code_llm, build_llm
from automation_agent.agent.setup.longrun import (
    DriveResult,
    LongRunDriver,
    Sequencer,
)
from automation_agent.agent.setup.prompt import Prompts
from automation_agent.agent.setup.runner import (
    drive,
    drive_collect_state,
    drive_text,
    new_runner,
)

__all__ = [
    "DriveResult",
    "LongRunDriver",
    "Prompts",
    "Sequencer",
    "assistant_text",
    "build_code_llm",
    "build_llm",
    "content_text",
    "drive",
    "drive_collect_state",
    "drive_text",
    "generate_text",
    "last_text",
    "new_runner",
    "state_string",
    "text_event",
    "user_text",
]
