/*
 * Package setup holds shared utilities for building agents. safeName lives here so the
 * workflow agents that derive an ADK sub-agent name from a repo or file path (fixflow
 * analyze, summary fetchers) share one implementation. Mirrors the Go reference's
 * setup.SafeName.
 */
package io.github.jkjamies.automationagent.agent.setup

/** Replaces every non-ASCII-alphanumeric character with `_`, for a safe ADK agent name. */
internal fun safeName(s: String): String =
    s.map { c -> if (c in 'a'..'z' || c in 'A'..'Z' || c in '0'..'9') c else '_' }.joinToString("")
