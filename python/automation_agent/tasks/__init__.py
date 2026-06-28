"""tasks — the execution transport between webhook ingress and the dispatcher.

In-process (default, local) or Cloud Tasks (production, in-request). See
``specs/20260626-workflow-execution-transport.md``.
"""

from automation_agent.tasks.cloudtasks import (
    MAX_TASK_BYTES,
    CloudTasks,
    Submitter,
    new_cloud_tasks,
)
from automation_agent.tasks.inprocess import (
    DEFAULT_MAX_CONCURRENT,
    DRAIN_TIMEOUT,
    InProcess,
)
from automation_agent.tasks.transport import DispatchFunc, Transport

__all__ = [
    "DEFAULT_MAX_CONCURRENT",
    "DRAIN_TIMEOUT",
    "MAX_TASK_BYTES",
    "CloudTasks",
    "DispatchFunc",
    "InProcess",
    "Submitter",
    "Transport",
    "new_cloud_tasks",
]
