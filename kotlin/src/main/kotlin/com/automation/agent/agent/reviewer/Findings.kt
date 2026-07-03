/*
 * Package reviewer is the in-house PR code-review workflow. It reacts to GitHub pull_request events
 * and analyzes the diff: a deterministic model-free intake (skip / deny / review), a per-category
 * agent fan-out, a holistic glue pass, and a count-based scorecard. Publishing the scored review to
 * the PR is a follow-up; this stage stops at the scorecard.
 *
 * Findings.kt holds the finding schema, severity/dimension normalization, the fingerprint, and the
 * defensive parseFindings.
 */
package com.automation.agent.agent.reviewer

import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive

/**
 * Ranks a finding's importance. critical/major/medium are actionable (posted inline); nitpick is
 * collapsed/low-noise.
 */
enum class Severity(val value: String) {
    CRITICAL("critical"),
    MAJOR("major"),
    MEDIUM("medium"),
    NITPICK("nitpick"),
    ;

    override fun toString(): String = value
}

/** Orders severities (higher = worse) so dedup can keep the worst of a pair. */
fun severityRank(s: Severity): Int =
    when (s) {
        Severity.CRITICAL -> 4
        Severity.MAJOR -> 3
        Severity.MEDIUM -> 2
        Severity.NITPICK -> 1
    }

/**
 * Maps a model-emitted severity onto a known value, defaulting an unknown or blank value to
 * nitpick — the safe, low-noise bucket (a local model is biased toward fewer-but-real).
 */
fun normalizeSeverity(s: String): Severity =
    when (s.trim().lowercase()) {
        Severity.CRITICAL.value -> Severity.CRITICAL
        Severity.MAJOR.value -> Severity.MAJOR
        Severity.MEDIUM.value -> Severity.MEDIUM
        else -> Severity.NITPICK
    }

/**
 * One of the review lenses. A category agent tags each finding with the dimension it belongs to;
 * the scorecard is a per-dimension histogram.
 */
enum class Dimension(val value: String) {
    RUNTIME_SAFETY("runtime_safety"),
    ERROR_HANDLING("error_handling"),
    SECURITY("security"),
    PERFORMANCE("performance"),
    PATTERN_VIOLATION("pattern_violation"),
    MAINTAINABILITY("maintainability"),
    READABILITY("readability"),
    DOCUMENTATION("documentation"),
    ACCESSIBILITY("accessibility"),
    ARCHITECTURE("architectural_alignment"),
    TESTABILITY("testability"),
    TEST_COVERAGE("test_coverage"),
    OTHER("other"),
    ;

    override fun toString(): String = value
}

private val KNOWN_DIMENSIONS: Map<String, Dimension> = Dimension.entries.associateBy { it.value }

/**
 * Maps a model-emitted dimension onto a known value (lowercased, spaces and hyphens folded to
 * underscores), defaulting an unrecognized one to `other`.
 */
fun normalizeDimension(s: String): Dimension {
    val d = s.trim().lowercase().replace(' ', '_').replace('-', '_')
    return KNOWN_DIMENSIONS[d] ?: Dimension.OTHER
}

/**
 * The always-on dimensions where a critical finding caps the overall grade to red regardless of
 * the other lenses.
 */
val CRITICAL_DIMENSIONS: Set<Dimension> = setOf(Dimension.SECURITY, Dimension.RUNTIME_SAFETY)

/** One review observation from a category agent or the glue pass. */
data class Finding(
    val file: String = "",
    val line: Int = 0,
    val dimension: Dimension = Dimension.OTHER,
    val severity: Severity = Severity.NITPICK,
    val message: String = "",
    val suggestion: String = "", // optional ```suggestion body (a localized in-diff fix)
    val fixPrompt: String = "", // optional "Prompt for AI agents" body (feeds the future fix hand-off)
    val ruleId: String = "", // optional repo-standard rule id this finding cites
    val confidence: Double = 0.0, // 0..1; below REVIEW_MIN_CONFIDENCE is dropped before scoring
)

/**
 * Identifies a finding across re-reviews for reconciliation and for cross-lens dedup: file + line +
 * a normalized message. Dimension is deliberately omitted so the same line/message surfaced by two
 * different lenses collapses to one finding.
 */
fun fingerprint(f: Finding): String = "${f.file}:${f.line}:${normalizeMessage(f.message)}"

/**
 * Lowercases and collapses internal whitespace so trivially different renderings of the same
 * message fingerprint identically.
 */
fun normalizeMessage(s: String): String =
    s.lowercase().split(Regex("\\s+")).filter { it.isNotEmpty() }.joinToString(" ")

// The wire keys whose values must be strings when present (a strict typed decode).
private val STR_FIELDS = listOf("file", "dimension", "severity", "message", "suggestion", "fix_prompt", "rule_id")

private val json = Json { ignoreUnknownKeys = true }

/**
 * Extracts findings from a category agent's raw output. Best-effort by design: it pulls the first
 * JSON array out of the text and tolerates a malformed body by returning no findings (empty =
 * success). It never throws, so a garbled response degrades to "no findings for this lens" rather
 * than failing the whole review.
 */
fun parseFindings(raw: String): List<Finding> {
    val wires = decodeFirstFindingArray(raw) ?: return emptyList()
    val out = mutableListOf<Finding>()
    for (el in wires) {
        val w = el as? JsonObject ?: continue
        val message = str(w["message"]).trim()
        if (message.isEmpty()) continue // a finding with no message is unusable
        out.add(
            Finding(
                file = str(w["file"]).trim(),
                line = intOr(w["line"], 0),
                dimension = normalizeDimension(str(w["dimension"])),
                severity = normalizeSeverity(str(w["severity"])),
                message = message,
                suggestion = str(w["suggestion"]).trim(),
                fixPrompt = str(w["fix_prompt"]).trim(),
                ruleId = str(w["rule_id"]).trim(),
                confidence = clampConfidence(asFloat(w["confidence"])),
            ),
        )
    }
    return out
}

/**
 * Scans `raw` for the first `[` that begins a JSON array decoding cleanly into the findings shape,
 * returning its elements. Scanning for a *decodable* array (rather than slicing the first `[` to the
 * last `]`) tolerates ``` fences, prose, and stray brackets without over-grabbing. A valid but empty
 * array is skipped in case a populated one follows; if none decodes, it returns null (best-effort:
 * empty = success). Trailing prose after the array is ignored.
 */
private fun decodeFirstFindingArray(raw: String): JsonArray? {
    for (i in raw.indices) {
        if (raw[i] != '[') continue
        val end = matchArrayEnd(raw, i)
        if (end < 0) continue
        val value =
            try {
                json.parseToJsonElement(raw.substring(i, end + 1))
            } catch (_: Exception) {
                continue
            }
        if (value is JsonArray && value.isNotEmpty() && validFindingArray(value)) return value
    }
    return null
}

/**
 * Returns the index of the `]` that closes the `[` at `start`, respecting string literals and
 * escapes, or -1 if the array is unterminated. Mirrors a JSON decoder scanning one array value out
 * of a larger text without slicing to the last bracket.
 */
private fun matchArrayEnd(raw: String, start: Int): Int {
    var depth = 0
    var inString = false
    var escaped = false
    for (i in start until raw.length) {
        val ch = raw[i]
        if (inString) {
            when {
                escaped -> escaped = false
                ch == '\\' -> escaped = true
                ch == '"' -> inString = false
            }
            continue
        }
        when (ch) {
            '"' -> inString = true
            '[' -> depth++
            ']' -> {
                depth--
                if (depth == 0) return i
            }
        }
    }
    return -1
}

/**
 * Reports whether every element decodes cleanly into the findings shape: an object whose known
 * string fields are strings, whose `line` (if present) is an integer, and whose `confidence` (if
 * present) is a finite number. A type mismatch fails the whole array so the scan moves on to the
 * next bracket, mirroring a strict typed decode. A non-finite `confidence` (NaN/Infinity) is
 * rejected here in the validation layer.
 */
private fun validFindingArray(value: JsonArray): Boolean {
    for (el in value) {
        val obj = el as? JsonObject ?: return false
        for (key in STR_FIELDS) {
            val v = obj[key] ?: continue
            if (v !is JsonPrimitive || !v.isString) return false
        }
        obj["line"]?.let { line ->
            if (line !is JsonPrimitive || line.isString || line.content.toLongOrNull() == null) return false
        }
        obj["confidence"]?.let { c ->
            if (c !is JsonPrimitive || c.isString) return false
            val d = c.content.toDoubleOrNull() ?: return false
            if (!d.isFinite()) return false
        }
    }
    return true
}

/** Coerces a JSON number to a Double; anything else is treated as unspecified (0). */
private fun asFloat(v: kotlinx.serialization.json.JsonElement?): Double {
    val p = v as? JsonPrimitive ?: return 0.0
    if (p.isString) return 0.0
    if (p.content == "true" || p.content == "false") return 0.0
    return p.content.toDoubleOrNull() ?: 0.0
}

/**
 * Normalizes a confidence *threshold* into [0,1]. Unlike [clampConfidence] (which treats 0 as
 * "unspecified"), a 0 threshold is meaningful — it disables the gate (keep all) — so NaN and
 * negatives fold to 0 (keep all, the safe default) and values above 1 fold to 1.
 */
fun clampThreshold(f: Double): Double {
    if (!(f >= 0.0)) return 0.0 // also catches NaN
    if (f > 1.0) return 1.0
    return f
}

/**
 * Keeps confidence in [0,1]. A zero/absent value is treated as 0.5 (unspecified) so a model that
 * omits the field is not silently dropped by the gate. (No NaN guard, matching the intake: a
 * non-finite confidence is already rejected at the array-validation layer.)
 */
fun clampConfidence(c: Double): Double {
    if (c <= 0.0) return 0.5
    if (c > 1.0) return 1.0
    return c
}

/** Renders findings as a compact JSON array for embedding in the glue prompt. */
fun findingsJson(findings: List<Finding>): String =
    buildString {
        append('[')
        findings.forEachIndexed { i, f ->
            if (i > 0) append(',')
            append('{')
            append("\"file\":").append(jsonString(f.file)).append(',')
            append("\"line\":").append(f.line).append(',')
            append("\"dimension\":").append(jsonString(f.dimension.value)).append(',')
            append("\"severity\":").append(jsonString(f.severity.value)).append(',')
            append("\"message\":").append(jsonString(f.message)).append(',')
            append("\"suggestion\":").append(jsonString(f.suggestion)).append(',')
            append("\"fix_prompt\":").append(jsonString(f.fixPrompt)).append(',')
            append("\"rule_id\":").append(jsonString(f.ruleId)).append(',')
            append("\"confidence\":").append(f.confidence)
            append('}')
        }
        append(']')
    }

private fun jsonString(s: String): String = JsonPrimitive(s).toString()

/** Coerces a possibly-missing string field to "". */
private fun str(v: kotlinx.serialization.json.JsonElement?): String {
    val p = v as? JsonPrimitive ?: return ""
    return if (p.isString) p.content else ""
}

/** Coerces a possibly-missing integer field to [def]. */
private fun intOr(v: kotlinx.serialization.json.JsonElement?, def: Int): Int {
    val p = v as? JsonPrimitive ?: return def
    if (p.isString) return def
    return p.content.toLongOrNull()?.toInt() ?: def
}
