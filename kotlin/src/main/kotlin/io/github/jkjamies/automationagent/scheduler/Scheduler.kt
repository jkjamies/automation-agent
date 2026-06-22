/*
 * Package scheduler turns cron schedules into ingest envelopes. Each fire emits a normalized
 * ingest.Envelope so the root agent treats time-based triggers exactly like any other
 * ingress. Deterministic tooling — no agent imports.
 */
package io.github.jkjamies.automationagent.scheduler

import com.cronutils.model.CronType
import com.cronutils.model.definition.CronDefinitionBuilder
import com.cronutils.model.time.ExecutionTime
import com.cronutils.parser.CronParser
import io.github.jkjamies.automationagent.ingest.Envelope
import io.github.jkjamies.automationagent.ingest.Kind
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.currentCoroutineContext
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch
import java.time.Instant
import java.time.ZoneOffset
import java.time.ZonedDateTime
import kotlin.time.Duration
import kotlin.time.Duration.Companion.hours
import kotlin.time.Duration.Companion.milliseconds
import kotlin.time.Duration.Companion.minutes
import kotlin.time.Duration.Companion.seconds

/** Receives an envelope when a schedule fires. */
fun interface EmitFunc {
    operator fun invoke(envelope: Envelope)
}

/**
 * Registers cron specs that emit ingest envelopes. Supports 5-field UNIX cron
 * (`minute hour dom month dow`) and the `@every <duration>` form (e.g. `@every 1s`).
 */
class Scheduler(
    private val emit: EmitFunc,
    private val now: () -> Instant = Instant::now,
) {
    private val parser = CronParser(CronDefinitionBuilder.instanceDefinitionFor(CronType.UNIX))
    private val schedules = mutableListOf<Entry>()
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Default)
    private val jobs = mutableListOf<Job>()

    /**
     * Registers a schedule that emits an envelope of the given kind. Throws
     * [IllegalArgumentException] for an invalid spec.
     */
    fun add(spec: String, kind: Kind) {
        schedules += if (spec.startsWith(EVERY_PREFIX)) {
            val interval = parseEvery(spec.removePrefix(EVERY_PREFIX).trim())
                ?: throw IllegalArgumentException("invalid @every spec: $spec")
            Entry.Every(interval, kind)
        } else {
            val cron = parser.parse(spec).validate() // throws IllegalArgumentException on invalid
            Entry.Cron(ExecutionTime.forCron(cron), kind, now)
        }
    }

    /** Reports the number of registered schedules. */
    fun entries(): Int = schedules.size

    /** Begins the schedule loops (non-blocking). */
    fun start() {
        schedules.forEach { entry -> jobs += scope.launch { run(entry) } }
    }

    /** Halts scheduling and cancels running loops. */
    fun stop() {
        scope.cancel()
    }

    /**
     * Emits one envelope; separated from the scheduling loop so it is directly unit-testable
     * without waiting for a real schedule.
     */
    internal fun trigger(kind: Kind) {
        emit(Envelope.new(kind, "scheduler", ByteArray(0), now()))
    }

    private suspend fun run(entry: Entry) {
        while (currentCoroutineContext().isActive) {
            delay(entry.nextDelayMillis())
            trigger(entry.kind)
        }
    }
}

private const val EVERY_PREFIX = "@every "

private sealed interface Entry {
    val kind: Kind
    fun nextDelayMillis(): Long

    class Every(private val interval: Duration, override val kind: Kind) : Entry {
        override fun nextDelayMillis(): Long = interval.inWholeMilliseconds.coerceAtLeast(1)
    }

    class Cron(
        private val exec: ExecutionTime,
        override val kind: Kind,
        private val now: () -> Instant,
    ) : Entry {
        override fun nextDelayMillis(): Long {
            // Cron fields are interpreted in UTC, not the (undocumented) host zone, so "0 9 * * *"
            // means 09:00 UTC on every deployment regardless of the container's local timezone.
            // The clock is injected (not ZonedDateTime.now) so the next-fire delay is testable.
            val nowZ = ZonedDateTime.ofInstant(now(), ZoneOffset.UTC)
            val next = exec.nextExecution(nowZ).orElse(null) ?: return 60_000
            return java.time.Duration.between(nowZ, next).toMillis().coerceAtLeast(1)
        }
    }
}

/** Parses a duration string (the `@every` argument): `1s`, `500ms`, `1h30m`. */
private fun parseEvery(s: String): Duration? {
    if (s.isEmpty()) return null
    var i = 0
    var total = Duration.ZERO
    var sawSegment = false
    while (i < s.length) {
        val start = i
        while (i < s.length && (s[i].isDigit() || s[i] == '.')) i++
        if (i == start) return null
        val value = s.substring(start, i).toDoubleOrNull() ?: return null
        val unitStart = i
        while (i < s.length && !s[i].isDigit() && s[i] != '.') i++
        val segment = when (s.substring(unitStart, i)) {
            "ms" -> value.milliseconds
            "s" -> value.seconds
            "m" -> value.minutes
            "h" -> value.hours
            else -> return null
        }
        total += segment
        sawSegment = true
    }
    return if (sawSegment) total else null
}
