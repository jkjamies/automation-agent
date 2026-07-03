/*
 * The count-based scorecard: a per-dimension severity histogram + overall grade.
 *
 * Count-based, not a synthetic 0-100 score. The overall grade is the critical-cap (any critical in
 * an always-on critical dimension -> red) combined with the worst dimension level.
 */
package com.automation.agent.agent.reviewer

/** A per-dimension and overall grade. Ordered so "worst level wins". */
enum class Level(val rank: Int) {
    GREEN(0),
    YELLOW(1),
    RED(2),
}

/** Renders a level as its scorecard glyph. */
fun levelGlyph(l: Level): String =
    when (l) {
        Level.RED -> "🔴" // 🔴
        Level.YELLOW -> "🟡" // 🟡
        Level.GREEN -> "🟢" // 🟢
    }

/** The textual grade shown beside the glyph in headers and the check. */
fun levelWord(l: Level): String =
    when (l) {
        Level.RED -> "Red"
        Level.YELLOW -> "Yellow"
        Level.GREEN -> "Green"
    }

/** The severity histogram for one dimension plus its derived level. */
data class DimScore(
    val dimension: Dimension,
    var critical: Int = 0,
    var major: Int = 0,
    var medium: Int = 0,
    var nitpick: Int = 0,
    var level: Level = Level.GREEN,
)

/** The count-based review result: a per-dimension histogram and an overall grade. */
data class Scorecard(
    val dims: List<DimScore>, // sorted by dimension for stable rendering
    val overall: Level,
    val total: Int, // total findings counted (after the confidence gate)
)

/**
 * Derives a dimension's level from its severity counts (pilot-tunable thresholds): red on any
 * critical or >=2 major; yellow on any major or >=3 medium; else green.
 */
fun dimLevel(s: DimScore): Level {
    if (s.critical >= 1 || s.major >= 2) return Level.RED
    if (s.major >= 1 || s.medium >= 3) return Level.YELLOW
    return Level.GREEN
}

/**
 * Builds the scorecard from already-confidence-gated findings: a per-dimension histogram + level,
 * then overall = critical-cap (any critical in an always-on critical dimension -> red) combined with
 * the worst dimension level.
 */
fun scoreFindings(findings: List<Finding>): Scorecard {
    val byDim = LinkedHashMap<Dimension, DimScore>()
    var criticalCap = false
    for (f in findings) {
        val d = byDim.getOrPut(f.dimension) { DimScore(dimension = f.dimension) }
        when (f.severity) {
            Severity.CRITICAL -> {
                d.critical++
                if (CRITICAL_DIMENSIONS.contains(f.dimension)) criticalCap = true
            }
            Severity.MAJOR -> d.major++
            Severity.MEDIUM -> d.medium++
            Severity.NITPICK -> d.nitpick++
        }
    }

    var worst = Level.GREEN
    val dims = byDim.values.toMutableList()
    for (d in dims) {
        d.level = dimLevel(d)
        if (d.level.rank > worst.rank) worst = d.level
    }
    dims.sortBy { it.dimension.value }

    val overall = if (criticalCap) Level.RED else worst
    return Scorecard(dims = dims, overall = overall, total = findings.size)
}
