"""The CI-wait suspend/resume loop on ADK long-running tools.

The Driver owns the long-run agent and a durable :class:`~automation_agent.agent.setup.ParkStore`
of suspended runs. All policy — retry vs give up, attempt counting, the per-run timeout —
lives here; the agent's Sequencer model only emits a fixed apply_fix -> await_ci sequence.

Lifecycle: kickoff applies a fix and parks on await_ci (recorded in the store, keyed by a
UUID session id and indexed by ``owner/repo#pr``). A check_run webhook drives resume, which
atomically claims the parked run (single winner) and either notifies success, resumes for
another attempt, or gives up at max_iter. If CI never reports, a soft per-run asyncio timer
fires on_timeout; the durable catch-all is :meth:`Driver.sweep_timeouts` (driven by Cloud
Scheduler via ``/internal/sweep``), so a restart can't strand a run on a durable backend.
With the default in-memory backend a restart still strands parked runs.

Terminal resolution (``_clear``) deletes both the park record and the ADK session so a
durable backend does not accumulate finished runs.

Tool error convention: Python ADK propagates a raised tool exception, so the apply_fix tool
callable wraps its work in try/except and returns ``{"error": str(e)}`` on failure — the
Sequencer's apply-error branch checks ``"error" in response`` and concludes.
"""

from __future__ import annotations

import asyncio
import json
import logging
import uuid
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import TYPE_CHECKING, Any, NoReturn

from google.adk.agents import LlmAgent
from google.adk.tools import FunctionTool, LongRunningFunctionTool

from automation_agent.agent import setup
from automation_agent.agent.fixflow.summary import (
    SummaryInput,
    TerminalOutcome,
    build_summary_text,
)
from automation_agent.agent.setup import MemoryParkStore, ParkRecord
from automation_agent.githubapi import Comparison

if TYPE_CHECKING:
    from automation_agent.agent.fixflow.engine import Engine, ResumeInput
    from automation_agent.agent.fixflow.envelope import Kickoff

TOOL_APPLY_FIX = "apply_fix"
TOOL_AWAIT_CI = "await_ci"


@dataclass
class RunParams:
    """Per-run inputs the apply_fix tool needs. Looked up by session id (never
    model-controlled, so a misbehaving model cannot redirect which repo or branch is
    edited) and persisted (serialized) in the ParkStore so a retry — or, with a durable
    backend, a restart — can reconstruct them."""

    owner: str
    repo: str
    full_repo: str
    base: str
    report: str
    feedback: str = ""  # previous attempt's CI failure, on retry
    new_branch: bool = True  # True on kickoff (create from base); False on retry

    def to_json(self) -> str:
        """Serialize the durable inputs. The key names match the Go reference's blob for
        cross-port parity."""
        return json.dumps(
            {
                "owner": self.owner,
                "repo": self.repo,
                "full_repo": self.full_repo,
                "base": self.base,
                "report": self.report,
                "feedback": self.feedback,
                "new_branch": self.new_branch,
            }
        )

    @staticmethod
    def from_json(s: str) -> RunParams:
        j = json.loads(s)
        return RunParams(
            owner=j["owner"],
            repo=j["repo"],
            full_repo=j["full_repo"],
            base=j["base"],
            report=j["report"],
            feedback=j.get("feedback", ""),
            new_branch=j.get("new_branch", True),
        )


class Driver:
    """Runs a Spec's CI-wait loop on ADK long-running suspend/resume over a ParkStore."""

    def __init__(self, engine: Engine) -> None:
        self.engine = engine
        # Injected durable store (falls back to in-memory: today's behavior, used by tests).
        self.store = engine.d.park_store or MemoryParkStore()
        self.timeout = engine.d.ci_timeout
        # Soft per-run timeout timers (lost on restart; the durable record in the store is
        # the source of truth — a restart's sweep re-arms them).
        self._timers: dict[str, asyncio.TimerHandle] = {}
        # Strong refs to in-flight timeout tasks: CPython only weakly references a bare
        # ensure_future task, so without this a fired timeout handler (which frees the run
        # and notifies for review) could be garbage-collected before it completes.
        self._timeout_tasks: set[asyncio.Future[None]] = set()

        seq_model = setup.Sequencer(
            action=TOOL_APPLY_FIX,
            wait=TOOL_AWAIT_CI,
            # The Driver only resumes a run when it has already decided to retry, so a
            # resumed failure always means "apply again". (success/timeout never resume.)
            retry_when=lambda resp: str(resp.get("conclusion")) == "failure",
            # A clean apply (triage found nothing) is already terminal: conclude without
            # parking on CI so the result is never forwarded to await_ci.
            stop_when=lambda resp: bool(resp.get("clean")),
        )
        # The Sequencer emits tool calls by name ("apply_fix"/"await_ci"), and ADK derives a
        # FunctionTool's name from the callable's __name__ — so the tool callables must carry
        # exactly those names. Bound methods can't be renamed, so wrap them in correctly-named
        # closures (tool_context is injected by name).
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
        # A durable session_service makes a parked run survive a restart; None falls back to
        # in-memory (today's behavior).
        self.lr = setup.LongRunDriver(
            "fixflow-" + engine.spec.name,
            "fixer",
            fixer,
            engine.d.session_service,
        )

    # --- tools -------------------------------------------------------------

    async def _apply_fix_tool(self, tool_context: Any = None) -> dict[str, Any]:
        """Run one fix attempt for the calling session. The run params are loaded from the
        store by session id (never model-supplied), so the model's args cannot influence the
        target.

        Wraps the work in try/except returning ``{"error": ...}`` so the Sequencer's
        apply-error branch can conclude (ADK propagates raised exceptions otherwise)."""
        # Local import avoids the engine<->driver import cycle (engine imports RunParams from
        # this module at its bottom).
        from automation_agent.agent.fixflow.engine import NoWorkError

        try:
            sid = _session_id(tool_context)
            rec = await self.store.get(sid)
            if rec is None:
                raise ValueError(f"apply_fix: no run params for session {sid!r}")
            rp = RunParams.from_json(rec.params)
            res = await self.engine.attempt_once(rp)
            return {"pr_number": res.pr.number, "head_sha": res.head_sha}
        except NoWorkError:
            # Triage found nothing actionable: not a failure. Return a clean-flagged result
            # (never {"error": ...}) so the sequencer concludes (stop_when) and _after_drive
            # sends a positive notice instead of the review alarm.
            return {"clean": True}
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
        rp = RunParams(
            owner=k.owner(),
            repo=k.name(),
            full_repo=k.repo,
            base=k.base,
            report=k.report_text(),
            new_branch=True,
        )
        await self._put_params(sid, rp)
        try:
            res = await self.lr.start(sid, "Apply the fix and wait for CI.")
        except Exception:
            await self._clear(sid)
            raise
        await self._after_drive(sid, k.repo, res, 1)

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
        run = await self.store.resolve_by_pr_key(key)
        if run is None:
            # Late, duplicate, raced with the timeout, or after a restart — nothing to do.
            self._log(logging.INFO, "resume: no parked run", pr=key, conclusion=in_.conclusion)
            return
        self._stop_timer(key)

        # Notify before clear so the message is sent while the record is intact, then clear
        # unconditionally. A duplicate webhook cannot double-notify: resolve_by_pr_key above
        # already claimed the run.
        if in_.conclusion == "success":
            self._log(logging.INFO, "fix succeeded", repo=in_.full_repo, pr=in_.pr_number)
            self._terminal_notify(
                TerminalOutcome.SUCCESS,
                self.engine.spec.success_title,
                run,
                in_.full_repo,
                in_.pr_number,
                "",
            )
            await self._clear(run.session_id)
            return

        # failure
        if run.attempts >= self.engine.d.max_iter:
            self._log(
                logging.WARNING,
                "fix exhausted attempts",
                repo=in_.full_repo,
                pr=in_.pr_number,
                attempts=run.attempts,
            )
            self._terminal_notify(
                TerminalOutcome.EXHAUSTED,
                self.engine.spec.review_title,
                run,
                in_.full_repo,
                in_.pr_number,
                in_.output_text,
            )
            await self._clear(run.session_id)
            return

        await self._update_for_retry(run.session_id, in_.output_text)
        try:
            res = await self.lr.resume(
                run.session_id,
                run.call_id,
                TOOL_AWAIT_CI,
                {"conclusion": in_.conclusion, "output": in_.output_text},
            )
        except Exception:
            await self._clear(run.session_id)
            raise
        self._log(
            logging.INFO,
            "fix retrying",
            repo=in_.full_repo,
            pr=in_.pr_number,
            attempt=run.attempts + 1,
        )
        await self._after_drive(run.session_id, in_.full_repo, res, run.attempts + 1)

    async def on_timeout(self, key: str) -> None:
        """Fires (from the soft per-run timer) when a parked run's CI never reports. Claims
        the run, frees it, and asks for human review."""
        run = await self.store.resolve_by_pr_key(key)
        if run is None:
            return  # already resolved by a webhook
        self._stop_timer(key)
        full_repo, pr = _split_pr_key(key)
        self._log(
            logging.WARNING,
            "fix timed out waiting for CI",
            repo=full_repo,
            pr=pr,
            timeout=self.timeout,
        )
        self._terminal_notify(
            TerminalOutcome.TIMEOUT, self.engine.spec.review_title, run, full_repo, pr, ""
        )
        await self._clear(run.session_id)

    async def sweep_timeouts(self) -> None:
        """Resolve every parked run whose CI never reported within ci_timeout — the durable
        catch-all behind the soft in-memory timer (which a restart loses). Driven by Cloud
        Scheduler via ``/internal/sweep``. The store's sweep claims each run atomically, so a
        webhook racing the sweep still resolves it at most once."""
        cutoff = _now() - self.timeout
        for run in await self.store.sweep(cutoff):
            self._stop_timer(run.pr_key)
            full_repo, pr = _split_pr_key(run.pr_key)
            self._log(
                logging.WARNING,
                "fix swept after timeout",
                repo=full_repo,
                pr=pr,
                timeout=self.timeout,
            )
            self._terminal_notify(
                TerminalOutcome.TIMEOUT,
                self.engine.spec.review_title,
                run,
                full_repo,
                pr,
                "",
            )
            await self._clear(run.session_id)

    # --- terminal summary --------------------------------------------------

    def _terminal_notify(
        self,
        outcome: TerminalOutcome,
        title: str,
        run: ParkRecord,
        full_repo: str,
        pr_number: int,
        last_output: str,
    ) -> None:
        """Build and send the status-aware summary for a finished run: the outcome framing,
        the original targeted findings, and what actually changed on the PR."""
        in_ = SummaryInput(
            outcome=outcome,
            workflow=self.engine.spec.name,
            full_repo=full_repo,
            pr_number=pr_number,
            attempts=run.attempts,
            last_output=last_output,
            timeout=str(self.timeout),
            check_name=self.engine.spec.check_name,
        )
        try:
            rp = RunParams.from_json(run.params)
            in_.report = rp.report
            in_.changed = self._gather_changes(rp)
        except Exception as exc:  # noqa: BLE001
            self._log(
                logging.WARNING,
                "decode run params for summary failed; sending without findings/diff",
                session=run.session_id,
                err=str(exc),
            )
        self.engine.notify(title, build_summary_text(in_), _pull_url(full_repo, pr_number))

    def _gather_changes(self, rp: RunParams) -> Comparison:
        """Best-effort fetch the PR branch's base...head diff for a terminal summary. On error
        returns an empty comparison so the summary still reports the attempt count and
        findings."""
        gh = self.engine.d.gh
        if gh is None:
            return Comparison()
        try:
            return gh.compare(rp.owner, rp.repo, rp.base, self.engine.spec.branch)
        except Exception as exc:  # noqa: BLE001
            self._log(
                logging.WARNING,
                "compare for summary failed",
                repo=rp.full_repo,
                err=str(exc),
            )
            return Comparison()

    # --- internals ---------------------------------------------------------

    async def _after_drive(
        self, sid: str, full_repo: str, res: setup.DriveResult, attempt: int
    ) -> None:
        """Inspect a drive's outcome and either surface an apply error or park the run (and
        its timeout) under its PR key."""
        # apply may be None if the apply tool never ran; _pr_number_from tolerates None
        # (returns 0), so the checks below funnel that case into _fail rather than crashing.
        apply = res.tool_responses.get(TOOL_APPLY_FIX)
        if apply is not None and "error" in apply:
            await self._fail(sid, full_repo, _pr_number_from(apply), str(apply["error"]))
        if apply is not None and apply.get("clean"):
            await self._finish_clean(sid, full_repo)
            return
        if res.parked_call_id == "":
            await self._fail(sid, full_repo, _pr_number_from(apply), "run did not park on CI wait")
        pr = _pr_number_from(apply)
        if pr == 0:
            await self._fail(sid, full_repo, pr, "parked without a PR number")
        await self._park(sid, _pr_key(full_repo, pr), res.parked_call_id, attempt)
        self._log(
            logging.INFO,
            "fix applied; awaiting CI",
            repo=full_repo,
            pr=pr,
            attempt=attempt,
        )

    async def _put_params(self, sid: str, rp: RunParams) -> None:
        """Store a fresh run's inputs (not yet parked: no PR key, no timer)."""
        await self.store.put(ParkRecord(session_id=sid, params=rp.to_json()))

    async def _park(self, sid: str, key: str, call_id: str, attempt: int) -> None:
        """Record that ``sid`` is now suspended awaiting CI under ``key``, and arm the soft
        timeout. Preserves the run's stored params (read-modify-write)."""
        rec = await self.store.get(sid)
        if rec is None:
            rec = ParkRecord(session_id=sid)
        rec.pr_key = key
        rec.call_id = call_id
        rec.attempts = attempt
        rec.parked_at = _now()
        await self.store.put(rec)
        self._arm_timer(key)

    async def _update_for_retry(self, sid: str, feedback: str) -> None:
        """Record the previous attempt's CI failure as feedback and switch the run off
        branch-creation, persisting the change for the retry's apply_fix."""
        rec = await self.store.get(sid)
        if rec is None:
            return
        rp = RunParams.from_json(rec.params)
        rp.feedback = "The previous attempt failed CI with:\n" + feedback
        rp.new_branch = False
        rec.params = rp.to_json()
        await self.store.put(rec)

    async def _fail(self, sid: str, full_repo: str, pr: int, reason: str) -> NoReturn:
        """Terminal apply failure: free the run, ask a human to review (so a fix that can
        never even open its PR doesn't vanish silently), and raise."""
        await self._clear(sid)
        link = _pull_url(full_repo, pr) if pr else ""
        self.engine.notify(
            self.engine.spec.review_title,
            f"{full_repo}: the {self.engine.spec.name} fix could not be applied "
            f"({reason}). Please review.",
            link,
        )
        raise RuntimeError(f"{full_repo} {self.engine.spec.name}: {reason}")

    async def _finish_clean(self, sid: str, full_repo: str) -> None:
        """Resolve a run whose triage found nothing to address. No PR was opened and the run
        never parked, so just free the run and send a positive "already clean" notice — never
        the human-review alarm. Returns (does not raise) so the dispatcher does not log a no-op
        as a failure."""
        self._log(logging.INFO, "nothing to address; already clean", repo=full_repo)
        await self._clear(sid)
        text = build_summary_text(
            SummaryInput(
                outcome=TerminalOutcome.CLEAN,
                workflow=self.engine.spec.name,
                full_repo=full_repo,
                pr_number=0,
                attempts=0,
            )
        )
        self.engine.notify(self.engine.spec.clean_title, text, "")

    async def _clear(self, sid: str) -> None:
        """Terminal cleanup: remove the park record and delete the ADK session so a durable
        backend does not leak completed runs."""
        try:
            await self.store.delete(sid)
        except Exception as exc:  # noqa: BLE001
            self._log(logging.ERROR, "clear run failed", session=sid, err=str(exc))
        try:
            await self.lr.delete_session(sid)
        except Exception as exc:  # noqa: BLE001
            self._log(logging.ERROR, "delete session failed", session=sid, err=str(exc))

    async def parked_count(self) -> int:
        """Number of currently parked runs (used by tests)."""
        return await self.store.parked_count()

    # --- timers ------------------------------------------------------------

    def _arm_timer(self, key: str) -> None:
        old = self._timers.pop(key, None)
        if old is not None:
            old.cancel()  # replace any prior parking for this PR (e.g. a retry re-park)
        loop = asyncio.get_running_loop()

        def _fire() -> None:
            task = asyncio.ensure_future(self.on_timeout(key))
            self._timeout_tasks.add(task)
            task.add_done_callback(self._timeout_tasks.discard)

        self._timers[key] = loop.call_later(self.timeout.total_seconds(), _fire)

    def _stop_timer(self, key: str) -> None:
        t = self._timers.pop(key, None)
        if t is not None:
            t.cancel()

    def _log(self, level: int, msg: str, **fields: Any) -> None:
        """Structured logging: no-op when no logger is configured, otherwise emit with the
        workflow name and the given structured fields."""
        log = self.engine.d.log
        if log is not None:
            log.log(level, msg, extra={"workflow": self.engine.spec.name, **fields})

    def _new_session_id(self) -> str:
        """A globally unique session id. A UUID (not a process-local counter) is required
        because the ParkStore is shared across Drivers and, with a durable backend, across
        restarts and instances — a counter would collide or overwrite persisted runs."""
        return str(uuid.uuid4())


def _now() -> datetime:
    return datetime.now(UTC)


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
