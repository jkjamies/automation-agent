"""The CI-wait suspend/resume loop on ADK long-running tools.

Port of ``fixflow/driver.go``. The Driver owns the long-run agent, the in-memory
parked-run registry, and each session's run params. All policy — retry vs give up,
attempt counting, the per-run timeout — lives here; the agent's Sequencer model only
emits a fixed apply_fix -> await_ci sequence.

Lifecycle: kickoff applies a fix and parks on await_ci (registered in the registry). A
check_run webhook drives resume, which atomically claims the parked run and either
notifies success, resumes for another attempt, or gives up at max_iter. If CI never
reports, the registry's per-run timer fires on_timeout, which frees the run and asks for
human review. There is no durable store: a process restart strands parked runs.

Tool error convention: Python ADK propagates a raised tool exception, so the apply_fix
tool callable wraps its work in try/except and returns ``{"error": str(e)}`` on failure
— the Sequencer's apply-error branch checks ``"error" in response`` and concludes.
"""

from __future__ import annotations

import logging
from dataclasses import dataclass
from typing import TYPE_CHECKING, Any

from google.adk.agents import LlmAgent
from google.adk.tools import FunctionTool, LongRunningFunctionTool

from automation_agent.agent import setup
from automation_agent.agent.fixflow.registry import ParkedRun, RunRegistry

if TYPE_CHECKING:
    from automation_agent.agent.fixflow.engine import Engine, ResumeInput
    from automation_agent.agent.fixflow.envelope import Kickoff

TOOL_APPLY_FIX = "apply_fix"
TOOL_AWAIT_CI = "await_ci"


@dataclass
class RunParams:
    """Per-run inputs the apply_fix tool needs. Owned by the Driver (keyed by session
    id) and never model-controlled, so a misbehaving model cannot redirect which repo or
    branch is edited."""

    owner: str
    repo: str
    full_repo: str
    base: str
    report: str
    feedback: str = ""  # previous attempt's CI failure, on retry
    new_branch: bool = True  # True on kickoff (create from base); False on retry


class Driver:
    """Runs a Spec's CI-wait loop on ADK long-running suspend/resume."""

    def __init__(self, engine: Engine) -> None:
        self.engine = engine
        self.reg = RunRegistry()
        self.timeout = engine.d.ci_timeout
        self._runs: dict[str, RunParams] = {}
        self._seq = 0

        seq_model = setup.Sequencer(
            action=TOOL_APPLY_FIX,
            wait=TOOL_AWAIT_CI,
            # The Driver only resumes a run when it has already decided to retry, so a
            # resumed failure always means "apply again". (success/timeout never resume.)
            retry_when=lambda resp: str(resp.get("conclusion")) == "failure",
        )
        # The Sequencer emits tool calls by name ("apply_fix"/"await_ci"), and ADK
        # derives a FunctionTool's name from the callable's __name__ — so the tool
        # callables must carry exactly those names. Bound methods can't be renamed,
        # so wrap them in correctly-named closures (tool_context is injected by name).
        driver = self

        async def apply_fix(tool_context: Any = None) -> dict[str, Any]:
            return await driver._apply_fix_tool(tool_context)

        def await_ci(pr_number: int = 0, head_sha: str = "") -> dict[str, Any]:
            return driver._await_ci_tool(pr_number, head_sha)

        fixer = LlmAgent(
            name="fixer_" + engine.spec.name,
            model=seq_model,
            instruction="Apply the fix, then wait for CI. If CI fails, apply again.",
            tools=[
                FunctionTool(apply_fix),
                LongRunningFunctionTool(await_ci),
            ],
        )
        self.lr = setup.LongRunDriver("fixflow-" + engine.spec.name, "fixer", fixer)

    # --- tools -------------------------------------------------------------

    async def _apply_fix_tool(self, tool_context: Any = None) -> dict[str, Any]:
        """Run one fix attempt for the calling session. The run params are looked up by
        session id (Driver-owned), so the model's args cannot influence the target.

        Wraps the work in try/except returning ``{"error": ...}`` so the Sequencer's
        apply-error branch can conclude (ADK propagates raised exceptions otherwise)."""
        try:
            sid = _session_id(tool_context)
            rp = self._runs.get(sid)
            if rp is None:
                raise ValueError(f"apply_fix: no run params for session {sid!r}")
            res = await self.engine.attempt_once(rp)
            return {"pr_number": res.pr.number, "head_sha": res.head_sha}
        except Exception as exc:  # noqa: BLE001
            return {"error": str(exc)}

    def _await_ci_tool(self, pr_number: int = 0, head_sha: str = "") -> dict[str, Any]:
        """The long-running park point: record that the run is waiting and return
        immediately with a pending status. The real CI result is fed back via resume."""
        return {"status": "pending"}

    # --- lifecycle ---------------------------------------------------------

    async def kickoff(self, k: Kickoff) -> None:
        """Start a new suspended run: apply the fix, then park awaiting CI."""
        sid = self._new_session_id()
        self._runs[sid] = RunParams(
            owner=k.owner(),
            repo=k.name(),
            full_repo=k.repo,
            base=k.base,
            report=k.report_text(),
            new_branch=True,
        )
        try:
            res = await self.lr.start(sid, "Apply the fix and wait for CI.")
        except Exception:
            self._clear(sid)
            raise
        self._after_drive(sid, k.repo, res, 1)

    async def resume(self, in_: ResumeInput) -> None:
        """React to a CI conclusion for a parked run."""
        if in_.pr_number == 0:
            raise ValueError("resume: missing PR number")
        # Only success/failure are actionable. Leave the run parked otherwise.
        if in_.conclusion not in ("success", "failure"):
            self._log(
                logging.INFO,
                "ignoring non-actionable conclusion",
                repo=in_.full_repo,
                conclusion=in_.conclusion,
            )
            return

        key = _pr_key(in_.full_repo, in_.pr_number)
        run = self.reg.resolve(key)
        if run is None:
            # Late, duplicate, raced with the timeout, or after a restart — nothing to do.
            self._log(
                logging.INFO, "resume: no parked run", pr=key, conclusion=in_.conclusion
            )
            return
        link = _pull_url(in_.full_repo, in_.pr_number)

        if in_.conclusion == "success":
            self._clear(run.session_id)
            self._log(
                logging.INFO, "fix succeeded", repo=in_.full_repo, pr=in_.pr_number
            )
            self.engine.notify(
                self.engine.spec.success_title,
                f"{in_.full_repo}: {self.engine.spec.name} passed CI.",
                link,
            )
            return

        # failure
        if run.attempts >= self.engine.d.max_iter:
            self._clear(run.session_id)
            self._log(
                logging.WARNING,
                "fix exhausted attempts",
                repo=in_.full_repo,
                pr=in_.pr_number,
                attempts=run.attempts,
            )
            self.engine.notify(
                self.engine.spec.review_title,
                f"{in_.full_repo}: after {run.attempts} attempts the "
                f"{self.engine.spec.name} fix still fails CI. Please review.",
                link,
            )
            return

        self._update_for_retry(run.session_id, in_.output_text)
        try:
            res = await self.lr.resume(
                run.session_id,
                run.call_id,
                TOOL_AWAIT_CI,
                {"conclusion": in_.conclusion, "output": in_.output_text},
            )
        except Exception:
            self._clear(run.session_id)
            raise
        self._log(
            logging.INFO,
            "fix retrying",
            repo=in_.full_repo,
            pr=in_.pr_number,
            attempt=run.attempts + 1,
        )
        self._after_drive(run.session_id, in_.full_repo, res, run.attempts + 1)

    async def on_timeout(self, key: str) -> None:
        """Fires (from the registry timer) when a parked run's CI never reports. Claims
        the run, frees it, and asks for human review."""
        run = self.reg.resolve(key)
        if run is None:
            return  # already resolved by a webhook
        self._clear(run.session_id)
        full_repo, pr = _split_pr_key(key)
        link = _pull_url(full_repo, pr)
        self._log(
            logging.WARNING,
            "fix timed out waiting for CI",
            repo=full_repo,
            pr=pr,
            timeout=self.timeout,
        )
        self.engine.notify(
            self.engine.spec.review_title,
            f"{full_repo}: the {self.engine.spec.name} fix timed out after "
            f"{self.timeout} waiting for CI. Please review.",
            link,
        )

    # --- internals ---------------------------------------------------------

    def _after_drive(
        self, sid: str, full_repo: str, res: setup.DriveResult, attempt: int
    ) -> None:
        """Inspect a drive's outcome and either surface an apply error or park the run
        (and its timeout) under its PR key."""
        apply = res.tool_responses.get(TOOL_APPLY_FIX)
        if apply is not None and "error" in apply:
            self._clear(sid)
            raise RuntimeError(
                f"{full_repo} {self.engine.spec.name}: {apply['error']}"
            )
        if res.parked_call_id == "":
            self._clear(sid)
            raise RuntimeError(
                f"{full_repo} {self.engine.spec.name}: run did not park on CI wait"
            )
        pr = _pr_number_from(apply)
        if pr == 0:
            self._clear(sid)
            raise RuntimeError(
                f"{full_repo} {self.engine.spec.name}: parked without a PR number"
            )
        self.reg.park(
            _pr_key(full_repo, pr),
            ParkedRun(session_id=sid, call_id=res.parked_call_id, attempts=attempt),
            self.timeout,
            self.on_timeout,
        )
        self._log(
            logging.INFO,
            "fix applied; awaiting CI",
            repo=full_repo,
            pr=pr,
            attempt=attempt,
        )

    def _log(self, level: int, msg: str, **fields: Any) -> None:
        """Mirror the Go driver's slog calls: no-op when no logger is configured,
        otherwise emit with the workflow name and the given structured fields."""
        log = self.engine.d.log
        if log is not None:
            log.log(level, msg, extra={"workflow": self.engine.spec.name, **fields})

    def _new_session_id(self) -> str:
        self._seq += 1
        return f"run-{self._seq}"

    def _update_for_retry(self, sid: str, feedback: str) -> None:
        rp = self._runs.get(sid)
        if rp is not None:
            rp.feedback = "The previous attempt failed CI with:\n" + feedback
            rp.new_branch = False

    def _clear(self, sid: str) -> None:
        self._runs.pop(sid, None)


def _session_id(tool_context: Any) -> str:
    if tool_context is None:
        return ""
    # ADK ToolContext exposes the session directly.
    session = getattr(tool_context, "session", None)
    if session is not None and getattr(session, "id", None):
        return str(session.id)
    return ""


def _pr_key(full_repo: str, number: int) -> str:
    return f"{full_repo}#{number}"


def _split_pr_key(key: str) -> tuple[str, int]:
    repo, _, num = key.partition("#")
    try:
        n = int(num)
    except ValueError:
        n = 0
    return repo, n


def _pull_url(full_repo: str, number: int) -> str:
    return f"https://github.com/{full_repo}/pull/{number}"


def _pr_number_from(resp: dict[str, Any] | None) -> int:
    if not resp:
        return 0
    v = resp.get("pr_number")
    if isinstance(v, bool):
        return 0
    if isinstance(v, (int, float)):
        return int(v)
    return 0
