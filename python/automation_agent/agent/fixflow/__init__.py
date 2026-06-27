"""fixflow — the reusable event-driven PR-fix engine (kickoff -> suspend -> CI resume).

Concrete agents (lintfixer, covfixer) supply a :class:`Spec`; the engine owns the loop,
the apply mechanics, and attempt counting. The suspended runs live in an injected
``setup.ParkStore`` backend (memory | sqlite | firestore), owned by the Driver.
"""

from automation_agent.agent.fixflow.analyze import EditFunc, parallel_analyze
from automation_agent.agent.fixflow.applyfix import (
    ApplyConfig,
    ApplyResult,
    FileEdit,
    GitHub,
    apply_fix,
    commit,
    open_repo,
)
from automation_agent.agent.fixflow.engine import (
    AnalyzeFunc,
    AnalyzeInput,
    Deps,
    Engine,
    FileWork,
    NoWorkError,
    ResumeInput,
    Spec,
    TriageFunc,
    new_engine,
)
from automation_agent.agent.fixflow.envelope import Kickoff, parse_kickoff
from automation_agent.agent.fixflow.explore import explore
from automation_agent.agent.fixflow.files import read_file
from automation_agent.agent.fixflow.tools import repo_tools
from automation_agent.agent.fixflow.util import (
    extract_json_array,
    extract_json_object,
    strip_fences,
)

__all__ = [
    "AnalyzeFunc",
    "AnalyzeInput",
    "ApplyConfig",
    "ApplyResult",
    "Deps",
    "EditFunc",
    "Engine",
    "FileEdit",
    "FileWork",
    "GitHub",
    "Kickoff",
    "NoWorkError",
    "ResumeInput",
    "Spec",
    "TriageFunc",
    "apply_fix",
    "commit",
    "explore",
    "extract_json_array",
    "extract_json_object",
    "new_engine",
    "open_repo",
    "parallel_analyze",
    "parse_kickoff",
    "read_file",
    "repo_tools",
    "strip_fences",
]
