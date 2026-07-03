/*
 * Standards-aware review — steer off the reviewed repo's own conventions.
 *
 * The reviewer steers off the conventions of the repo *under review* — `.agents/standards`,
 * `.cursor/rules`, `CLAUDE.md`, whatever that repo has, not automation-agent's own. A base-tier
 * sub-agent distills the discovered docs (heterogeneous formats) into one uniform tagged rule list;
 * the compact list is injected into every lens and a lazy `get_rule` tool serves the full text on
 * demand. All API-only (no clone).
 */
package com.automation.agent.agent.reviewer

import com.automation.agent.agent.setup.driveText
import com.automation.agent.agent.setup.newRunner
import com.automation.agent.githubapi.PRFile
import com.automation.agent.githubapi.TreeEntry
import com.google.adk.kt.types.FunctionDeclaration
import com.google.adk.kt.types.Schema
import com.google.adk.kt.types.Type
import kotlinx.serialization.json.Json
import kotlinx.serialization.json.JsonArray
import kotlinx.serialization.json.JsonObject
import kotlinx.serialization.json.JsonPrimitive
import java.lang.System.Logger.Level
import java.security.MessageDigest

// The distiller drive kick; the real instruction (the distill prompt + the repo's standards docs)
// lives in the agent's system instruction.
const val DISTILL_TRIGGER = "Extract the repository's rules as the JSON array specified."

/**
 * One distilled, dimension-tagged convention rule extracted from the reviewed repo's own standards
 * docs.
 */
data class Rule(
    val id: String,
    val dimension: Dimension = Dimension.OTHER,
    val summary: String = "",
    val source: String = "", // the doc path the rule came from
)

/**
 * The distilled rule set for one repo at one docs revision: the compact rule menu injected into
 * every lens, plus the full source docs for lazy getRule drill-down.
 */
class Standards(
    val rules: List<Rule>,
    val byId: Map<String, Rule>,
    val docs: Map<String, String>, // source path -> full doc text
    val sources: List<String>, // distinct source paths, sorted
) {
    /** Reports whether there are no rules to inject, so callers can fall back to generic. */
    fun empty(): Boolean = rules.isEmpty()

    /**
     * Renders the compact rule list for an agent prompt: one line per rule (id, dimension, summary,
     * source). Small by construction — summaries, not full text.
     */
    fun menu(): String {
        if (empty()) return ""
        return rules.joinToString("") { r -> "- ${r.id} [${r.dimension.value}] ${r.summary} (source: ${r.source})\n" }
    }

    /** Reports whether [ruleId] is a rule in this set (the citation gate's check). */
    fun validId(ruleId: String): Boolean = byId.containsKey(ruleId)

    /**
     * Returns the full source-doc text for a rule id, for lazy drill-down. Empty if the id is
     * unknown or its source doc is absent.
     */
    fun ruleDoc(ruleId: String): String {
        val r = byId[ruleId] ?: return ""
        return docs[r.source] ?: ""
    }

    /** Returns the applied source paths (empty when no standards), for the summary report. */
    fun sourceList(): List<String> = if (empty()) emptyList() else sources
}

/** Reports whether a (possibly null) standards set has no rules to inject. */
fun isEmpty(std: Standards?): Boolean = std == null || std.empty()

/**
 * Fetches and distills the reviewed repo's convention docs into a tagged rule list, cached per repo
 * + docs revision. Returns null (review generic) when standards are disabled, none are found, or
 * distillation yields nothing. Best-effort: a discovery/fetch error logs and returns null rather
 * than failing the review.
 */
suspend fun discoverStandards(engine: Engine, owner: String, repo: String, ref: String, changed: List<PRFile>): Standards? {
    if (!engine.standardsEnabled) return null
    val tree =
        try {
            engine.requireGh().tree(owner, repo, ref)
        } catch (e: Exception) {
            engine.log.log(Level.WARNING, "standards: list tree failed; reviewing generic repo=$owner/$repo err=${e.message}")
            return null
        }
    if (tree.truncated) {
        // A truncated tree (very large repo) may have missed convention files. Steering off a
        // knowingly-incomplete rule set is worse than a generic review, so degrade to generic (no
        // cache, so a later event with a complete tree retries).
        engine.log.log(Level.WARNING, "standards: repo tree truncated; reviewing generic repo=$owner/$repo")
        return null
    }
    // Per-module scoping: a per-directory instruction file applies only when the PR touches its
    // module. Repo-global conventions always apply.
    val matched = scopeToTouched(matchStandards(tree.entries, engine.standardsGlobs), changed)
    if (matched.isEmpty()) return null
    // Cache on the matched docs' blob SHAs, so distillation runs once per standards change.
    val key = standardsCacheKey(owner, repo, matched)
    val cached = engine.standardsCache.get(key)
    if (cached.ok) return cached.std

    val docs = linkedMapOf<String, String>()
    val sources = mutableListOf<String>()
    var total = 0
    var fetchOk = true
    for (m in matched) {
        val content =
            try {
                engine.requireGh().getFileContent(owner, repo, m.path, ref)
            } catch (e: Exception) {
                // A transient fetch failure leaves the rule set incomplete; degrade to generic for
                // this round (and don't memoize, so a later event retries the full set).
                engine.log.log(Level.WARNING, "standards: fetch failed; reviewing generic path=${m.path} err=${e.message}")
                fetchOk = false
                break
            }
        if (total + content.length > engine.standardsMaxBytes) {
            engine.log.log(Level.WARNING, "standards: byte cap reached; remaining docs skipped cap=${engine.standardsMaxBytes} applied=${sources.size}")
            break
        }
        total += content.length
        docs[m.path] = content
        sources.add(m.path)
    }
    if (!fetchOk || docs.isEmpty()) {
        // Incomplete discovery (a fetch failed) or nothing fetched: review generic, uncached.
        return null
    }

    val rules =
        try {
            distill(engine, docs, sources)
        } catch (e: Exception) {
            engine.log.log(Level.WARNING, "standards: distillation failed; reviewing generic repo=$owner/$repo err=${e.message}")
            return null
        }
    val std = buildStandards(rules, docs, sources)
    // Discovery was complete (whole tree, every matched doc fetched), so memoize — incl. a legitimate
    // empty distill, so a rule-less repo isn't re-distilled until its docs change.
    engine.standardsCache.put(key, std)
    if (isEmpty(std)) {
        engine.log.log(Level.INFO, "standards: discovered docs but distilled no rules; reviewing generic repo=$owner/$repo docs=${sources.size}")
        return null
    }
    engine.log.log(Level.INFO, "standards: applied repo=$owner/$repo rules=${std?.rules?.size ?: 0} sources=${std?.sources?.joinToString(", ")}")
    return std
}

/**
 * Returns the tree's blob entries whose path matches any standards glob, sorted by path for
 * deterministic ordering and cache keys.
 */
fun matchStandards(entries: List<TreeEntry>, globs: List<String>): List<TreeEntry> {
    val pats = compileStandardsGlobs(globs)
    return entries.filter { it.type == "blob" && matchesGlob(pats, it.path) }.sortedBy { it.path }
}

/**
 * Builds path matchers from the configured globs. A glob with no '/' matches the basename; one with
 * a '/' matches the full path. Reuses the exclude-filter glob compiler.
 */
fun compileStandardsGlobs(globs: List<String>): List<GlobPattern> =
    globs.mapNotNull { raw ->
        val g = raw.trim()
        if (g.isEmpty()) null else GlobPattern(re = globToRegExp(g), basename = !g.contains('/'))
    }

/** Reports whether [p] matches any compiled standards glob. */
fun matchesGlob(pats: List<GlobPattern>, p: String): Boolean {
    val base = posixBasename(p)
    for (pat in pats) {
        val target = if (pat.basename) base else p
        if (pat.re.matches(target)) return true
    }
    return false
}

/**
 * Drops per-directory instruction files (AGENTS.md/CLAUDE.md/GEMINI.md nested below the repo root)
 * for modules the PR does not touch — so a finding in one module isn't judged against another
 * module's conventions. Repo-global conventions (root files, dotfolder rule dirs, linter configs)
 * always apply.
 */
fun scopeToTouched(matched: List<TreeEntry>, changed: List<PRFile>): List<TreeEntry> {
    val touched = touchedDirs(changed)
    return matched.filterNot { m -> moduleScoped(m.path) && posixDirname(m.path) !in touched }
}

/**
 * Reports whether a convention file is a per-directory instruction file below the repo root (applies
 * only to its own module). Root files and non-instruction conventions are repo-global.
 */
fun moduleScoped(p: String): Boolean {
    val d = posixDirname(p)
    if (d == "" || d == ".") return false
    return posixBasename(p) in setOf("AGENTS.md", "CLAUDE.md", "GEMINI.md")
}

/**
 * The set of every ancestor directory (up to the root ".") of the changed files, so a per-module
 * instruction file applies when any file in its subtree changed.
 */
fun touchedDirs(changed: List<PRFile>): Set<String> {
    val dirs = mutableSetOf<String>()
    for (f in changed) {
        var d = posixDirname(f.path)
        if (d == "") d = "."
        while (true) {
            dirs.add(d)
            if (d == ".") break
            val parent = posixDirname(d)
            d = if (parent != "") parent else "."
        }
    }
    return dirs
}

/**
 * Hashes the repo and the matched docs' (path, blob SHA) pairs, so the cache keys on the standards
 * revision: any change to a standards file changes its blob SHA and misses.
 */
fun standardsCacheKey(owner: String, repo: String, matched: List<TreeEntry>): String {
    val parts = matched.map { "${it.path}:${it.sha}" }.sorted()
    val input = "$owner/$repo\n" + parts.joinToString("\n")
    val digest = MessageDigest.getInstance("SHA-256").digest(input.toByteArray(Charsets.UTF_8))
    return digest.joinToString("") { "%02x".format(it) }
}

/**
 * Runs the base-tier distiller sub-agent over the discovered docs, returning the parsed rule list.
 * Best-effort: a runner/drive error propagates to the caller (which degrades to generic).
 */
suspend fun distill(engine: Engine, docs: Map<String, String>, sources: List<String>): List<Rule> {
    val agent = buildDistillerAgent(engine, docs, sources)
    val text = driveText(newRunner("reviewer-distill", agent), "system", "distill", DISTILL_TRIGGER)
    return parseRules(text)
}

/**
 * Composes the distiller's instruction: the distill prompt followed by each discovered standards
 * doc, fenced so the doc content (untrusted) can't break the prompt.
 */
fun buildDistillerInstruction(promptBody: String, docs: Map<String, String>, sources: List<String>): String {
    val parts = StringBuilder(promptBody)
    parts.append("\n\n## Repository standards documents\n\n")
    for (src in sources) {
        val doc = docs[src] ?: ""
        parts.append("### Document: $src\n\n")
        var fence = "`".repeat(maxBacktickRun(doc) + 1)
        if (fence.length < 3) fence = "```"
        parts.append(fence).append("\n")
        parts.append(doc)
        if (!doc.endsWith("\n")) parts.append("\n")
        parts.append(fence).append("\n\n")
    }
    return parts.toString()
}

/**
 * Assembles the standards from distilled rules + the fetched docs. null when there are no rules (so
 * [isEmpty] and a generic fallback hold).
 */
fun buildStandards(rules: List<Rule>, docs: Map<String, String>, sources: List<String>): Standards? {
    if (rules.isEmpty()) return null
    val byId = rules.associateBy { it.id }
    return Standards(rules = rules, byId = byId, docs = docs, sources = sources.sorted())
}

private val rulesJson = Json { ignoreUnknownKeys = true }

// The wire keys whose values must be strings when present (a strict typed decode).
private val RULE_STR_FIELDS = listOf("id", "dimension", "summary", "source")

/**
 * Extracts the distilled rule list from the base model's output. Defensive by design (mirrors
 * [parseFindings]): it scans for the first JSON array that decodes into the rule shape, tolerating
 * fences/prose, and never throws — a garbled distillation degrades to "no rules" (a generic review)
 * rather than failing.
 */
fun parseRules(raw: String): List<Rule> {
    for (i in raw.indices) {
        if (raw[i] != '[') continue
        val end = matchRuleArrayEnd(raw, i)
        if (end < 0) continue
        val value =
            try {
                rulesJson.parseToJsonElement(raw.substring(i, end + 1))
            } catch (_: Exception) {
                continue
            }
        if (value !is JsonArray || value.isEmpty() || !validRuleArray(value)) continue
        val out = mutableListOf<Rule>()
        val seen = mutableSetOf<String>()
        for (el in value) {
            val w = el as? JsonObject ?: continue
            val ruleId = ruleStr(w["id"]).trim()
            val summary = ruleStr(w["summary"]).trim()
            if (ruleId == "" || summary == "" || ruleId in seen) continue // a rule needs a unique id and a summary
            seen.add(ruleId)
            out.add(
                Rule(
                    id = ruleId,
                    dimension = normalizeDimension(ruleStr(w["dimension"])),
                    summary = summary,
                    source = ruleStr(w["source"]).trim(),
                ),
            )
        }
        if (out.isNotEmpty()) return out
    }
    return emptyList()
}

/**
 * Reports whether every element decodes cleanly into the rule shape: an object whose known string
 * fields are strings. A type mismatch fails the whole array so the scan moves on.
 */
private fun validRuleArray(value: JsonArray): Boolean {
    for (el in value) {
        val obj = el as? JsonObject ?: return false
        for (key in RULE_STR_FIELDS) {
            val v = obj[key] ?: continue
            if (v !is JsonPrimitive || !v.isString) return false
        }
    }
    return true
}

/** Returns the index of the `]` that closes the `[` at [start], respecting string literals. */
private fun matchRuleArrayEnd(raw: String, start: Int): Int {
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

private fun ruleStr(v: kotlinx.serialization.json.JsonElement?): String {
    val p = v as? JsonPrimitive ?: return ""
    return if (p.isString) p.content else ""
}

/**
 * A model-callable drill-down tool: its genai declaration plus a self-wrapping executor. The review
 * lenses are direct-model-call code nodes (no ADK tool runner), so the lens loop executes a called
 * tool through [execute] and feeds the result back. Tool errors self-wrap as `{"error": …}`.
 */
class RuleTool(val declaration: FunctionDeclaration, val execute: (Map<String, Any?>) -> Map<String, Any?>)

/**
 * Returns the lazy get_rule drill-down tool bound to this run's rule set, or an empty list when
 * there are no standards (the lenses then run without it). The compact rule menu lives in the
 * prompt; full text is fetched on demand.
 */
fun standardsTools(std: Standards?): List<RuleTool> {
    if (isEmpty(std)) return emptyList()
    val real = std ?: return emptyList()
    val declaration =
        FunctionDeclaration(
            name = "get_rule",
            description = "Return the full source text of a repo standard rule by its id (e.g. \"R3\") so you can read the exact wording before flagging a conformance issue.",
            parameters = Schema(type = Type.OBJECT, properties = mapOf("id" to Schema(type = Type.STRING, description = "the rule id, e.g. \"R3\"")), required = listOf("id")),
        )
    return listOf(
        RuleTool(declaration) { args ->
            try {
                mapOf("rule" to real.ruleDoc((args["id"] as? String ?: "").trim()))
            } catch (e: Exception) {
                mapOf("error" to (e.message ?: e.toString()))
            }
        },
    )
}

// The lenses whose findings assert "this violates the repo's documented standard" — they must cite
// a real injected rule_id. Other dimensions (e.g. security) stand on their own.
val CONFORMANCE_DIMENSIONS: Set<Dimension> = setOf(Dimension.PATTERN_VIOLATION, Dimension.ARCHITECTURE)

/**
 * Enforces that a conformance finding (pattern/architecture) is anchored to one of the repo's own
 * injected rules: an empty or unknown rule_id is dropped or demoted to nitpick per
 * REVIEW_UNCITED_MODE. When standards-awareness is off, findings pass through untouched.
 */
fun gateCitations(engine: Engine, findings: List<Finding>, std: Standards?): List<Finding> {
    if (!engine.standardsEnabled || isEmpty(std)) return findings
    val real = std ?: return findings
    val out = mutableListOf<Finding>()
    for (f in findings) {
        if (f.dimension in CONFORMANCE_DIMENSIONS && !real.validId(f.ruleId)) {
            if (engine.uncitedDrop) continue
            out.add(f.copy(severity = Severity.NITPICK)) // demote an unanchored "violation"
        } else {
            out.add(f)
        }
    }
    return out
}

/** The result of a cache lookup: the cached value (may be null) and whether the key was present. */
data class CacheHit(val std: Standards?, val ok: Boolean)

/**
 * Memoizes distilled rule sets per repo + docs revision (in-memory; a cold start re-distills). A
 * cached null means "discovered docs, distilled nothing" and is retained so a generic repo isn't
 * re-distilled until its docs change.
 */
class StandardsCache {
    private val lock = Any()
    private val m = HashMap<String, Standards?>()

    fun get(key: String): CacheHit =
        synchronized(lock) {
            if (m.containsKey(key)) CacheHit(m[key], true) else CacheHit(null, false)
        }

    fun put(key: String, std: Standards?) {
        synchronized(lock) { m[key] = std }
    }
}

/** The final path segment (basename), splitting on '/' as posix paths do. */
private fun posixBasename(p: String): String {
    val idx = p.lastIndexOf('/')
    return if (idx < 0) p else p.substring(idx + 1)
}

/** The parent directory of a posix path: "" for a bare name, else everything before the last '/'. */
private fun posixDirname(p: String): String {
    val idx = p.lastIndexOf('/')
    return if (idx < 0) "" else p.substring(0, idx)
}
