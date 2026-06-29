/*
 * Package ingest defines the normalized event envelope that every ingress source (Cloud
 * Scheduler, webhooks, and future hooks like GitHub/Jira/Confluence) is reduced to before
 * being handed to the root agent. See ../.agents/standards/architecture-design.md §2.
 */
package com.automation.agent.ingest

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable
import kotlinx.serialization.SerializationException
import kotlinx.serialization.json.Json
import java.time.Instant
import java.time.OffsetDateTime
import java.time.ZoneOffset
import java.time.format.DateTimeFormatter
import java.time.format.DateTimeParseException
import java.util.Base64
import java.util.Locale

/** Kind identifies what triggered an ingest, so the root agent can route it. */
enum class Kind(val value: String) {
    CRON_DAILY("cron.daily"), // daily Cloud Scheduler trigger -> summary digest
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
    val source: String, // human-readable origin, e.g. "internal:/cron/daily", "webhook:/lint"
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

/**
 * The JSON wire form of an [Envelope] crossing the task-queue boundary (tasks -> POST
 * /internal/dispatch). It is an external contract and must stay byte-identical across all four
 * language ports (spec §7). [payload] is an explicit standard-base64 string — never a raw byte
 * array — so an empty/absent payload is the empty string in every port, with no language-specific
 * null/[]/"" divergence. [receivedAt] is nullable so an absent or JSON-null timestamp decodes to
 * the zero value rather than failing.
 */
@Serializable
private data class WireEnvelope(
    val kind: String,
    val source: String = "",
    @SerialName("received_at") val receivedAt: String? = null, // RFC 3339
    val payload: String = "", // standard base64 of the raw bytes ("" when empty)
)

// encode emits every field (encodeDefaults) in declaration order with compact separators, so the
// bytes match the cross-port wire contract regardless of whether a field equals its default.
private val encodeJson = Json { encodeDefaults = true }

// decode tolerates unknown keys (an extra field is not poison, matching the reference ports'
// object decoders) and coerces a JSON null on a defaulted field to that default.
private val decodeJson = Json {
    ignoreUnknownKeys = true
    coerceInputValues = true
}

// The date-time portion of an RFC 3339 instant, fixed to UTC; the fractional second and trailing
// "Z" are appended by toRfc3339 so the trailing-zero trimming matches the reference exactly.
// Pinned to Locale.ROOT so the digits stay ASCII (a non-default JVM locale could otherwise emit
// localized numerals and break the byte-identical cross-port contract).
private val RFC3339_SECONDS = DateTimeFormatter.ofPattern("yyyy-MM-dd'T'HH:mm:ss", Locale.ROOT)

/**
 * Serializes an envelope to its JSON wire form for the Cloud Tasks transport (the in-process
 * transport passes the object directly and never calls this). The bytes follow the cross-port wire
 * contract exactly (spec §7): compact separators, field order kind/source/received_at/payload, an
 * RFC 3339 instant with a trailing "Z" and trimmed fractional-second zeros, and a standard-base64
 * payload.
 *
 * Unlike the string-typed ports, [Envelope.kind] is a [Kind] enum, so an unknown kind is
 * unrepresentable here — the "reject unknown kind on encode" guard the other ports need is enforced
 * by the type system.
 */
fun encode(e: Envelope): ByteArray {
    val wire = WireEnvelope(
        kind = e.kind.value,
        source = e.source,
        receivedAt = toRfc3339(e.receivedAt),
        payload = Base64.getEncoder().encodeToString(e.payload),
    )
    return encodeJson.encodeToString(WireEnvelope.serializer(), wire).toByteArray(Charsets.UTF_8)
}

/**
 * Parses an envelope from its JSON wire form (see [encode]) and rejects an unknown kind.
 *
 * A malformed body, bad base64, or unrecognized kind is a permanent (poison) error thrown as
 * [IllegalArgumentException]: the caller should ack the delivery rather than retry it — a redelivery
 * cannot fix a poison payload. [Envelope.source] and [Envelope.receivedAt] are informational (only
 * kind and payload drive dispatch), so an absent (or JSON-null) value decodes to the zero value —
 * but a present value of the wrong type (a non-string source, a non-string/unparseable received_at)
 * is a malformed body, i.e. poison.
 */
fun decode(b: ByteArray): Envelope = decode(b.toString(Charsets.UTF_8))

/** Parses an envelope from its JSON wire string. See [decode]. */
fun decode(s: String): Envelope {
    val wire = try {
        decodeJson.decodeFromString(WireEnvelope.serializer(), s)
    } catch (e: SerializationException) {
        throw IllegalArgumentException("ingest: decode envelope: ${e.message}", e)
    }
    val kind = Kind.from(wire.kind)
        ?: throw IllegalArgumentException("ingest: unknown kind \"${wire.kind}\"")
    val payload = strictBase64Decode(wire.payload)
    val receivedAt = parseReceivedAt(wire.receivedAt)
    return Envelope.new(kind, wire.source, payload, receivedAt)
}

/**
 * Formats an instant as RFC 3339 with nanosecond precision: a trailing "Z" whose fractional second
 * has trailing zeros trimmed (a whole second has no fractional part at all). This reproduces Go's
 * RFC3339Nano spelling — ".000" -> "", ".500" -> ".5" — which the cross-port wire contract requires
 * (spec §7); java.time's own ISO formatter groups fractional digits in threes and would diverge.
 */
private fun toRfc3339(t: Instant): String {
    val utc = t.atOffset(ZoneOffset.UTC)
    val base = utc.format(RFC3339_SECONDS)
    if (utc.nano == 0) return base + "Z"
    val frac = String.format(Locale.ROOT, "%09d", utc.nano).trimEnd('0')
    return "$base.${frac}Z"
}

/**
 * Decodes a standard-base64 string strictly. Java's basic decoder tolerates some non-canonical
 * input (e.g. missing padding), so re-encode and compare to reject anything that is not canonical
 * standard base64 — the wire contract is canonical standard base64 (matching Go's StdEncoding).
 */
private fun strictBase64Decode(s: String): ByteArray {
    val decoded = try {
        Base64.getDecoder().decode(s)
    } catch (e: IllegalArgumentException) {
        throw IllegalArgumentException("ingest: decode payload: \"$s\" is not valid standard base64", e)
    }
    if (Base64.getEncoder().encodeToString(decoded) != s) {
        throw IllegalArgumentException("ingest: decode payload: \"$s\" is not valid standard base64")
    }
    return decoded
}

/**
 * Parses received_at: an absent or JSON-null value is the epoch zero value (never poison); a present
 * but unparseable RFC 3339 string (including "") is poison. ISO_OFFSET_DATE_TIME requires the full
 * date-time-and-offset form, so a date-only or offset-less string is rejected rather than coerced.
 */
private fun parseReceivedAt(value: String?): Instant {
    if (value == null) return Instant.EPOCH
    return try {
        OffsetDateTime.parse(value, DateTimeFormatter.ISO_OFFSET_DATE_TIME).toInstant()
    } catch (e: DateTimeParseException) {
        throw IllegalArgumentException("ingest: decode received_at: \"$value\" is not a valid RFC 3339 timestamp", e)
    }
}
