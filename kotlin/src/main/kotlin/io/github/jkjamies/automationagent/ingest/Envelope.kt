/*
 * Package ingest defines the normalized event envelope that every ingress source (cron,
 * webhooks, and future hooks like GitHub/Jira/Confluence) is reduced to before being handed
 * to the root agent. See ../.agents/standards/architecture-design.md §2.
 */
package io.github.jkjamies.automationagent.ingest

import java.time.Instant

/** Kind identifies what triggered an ingest, so the root agent can route it. */
enum class Kind(val value: String) {
    CRON_DAILY("cron.daily"), // 09:00 daily -> summary digest
    CRON_WEEKLY("cron.weekly"), // 09:00 Monday
    LINT("lint"), // agnostic lint payload -> lint-fixer
    COVERAGE("coverage"), // agnostic coverage payload -> coverage-fixer
    CI("ci"), // GitHub check_run -> resume lint/coverage fixer
    ;

    override fun toString(): String = value

    companion object {
        /** Returns the Kind for [value], or null if it is not a recognized ingest kind. */
        fun from(value: String): Kind? = entries.firstOrNull { it.value == value }

        /** Reports whether [value] is a recognized ingest kind. */
        fun valid(value: String): Boolean = from(value) != null
    }
}

/**
 * Envelope is the normalized unit of work. [payload] carries the raw source body (e.g. the
 * lint JSON or check_run event) for the chosen workflow to parse.
 */
data class Envelope(
    val kind: Kind,
    val source: String, // human-readable origin, e.g. "scheduler", "webhook:/lint"
    val receivedAt: Instant,
    val payload: ByteArray,
) {
    // ByteArray needs structural equals/hashCode for value semantics.
    override fun equals(other: Any?): Boolean {
        if (this === other) return true
        if (other !is Envelope) return false
        return kind == other.kind &&
            source == other.source &&
            receivedAt == other.receivedAt &&
            payload.contentEquals(other.payload)
    }

    override fun hashCode(): Int {
        var result = kind.hashCode()
        result = 31 * result + source.hashCode()
        result = 31 * result + receivedAt.hashCode()
        result = 31 * result + payload.contentHashCode()
        return result
    }

    companion object {
        /** Constructs an Envelope. */
        fun new(kind: Kind, source: String, payload: ByteArray, at: Instant): Envelope =
            Envelope(kind = kind, source = source, receivedAt = at, payload = payload)
    }
}
