/*
 * Package setup holds shared utilities for building agents: the LLM provider switch and
 * adapters, the prompt loader, and ADK helpers. It is the only package permitted to import
 * provider SDKs (Ollama, Gemini) — enforced by the :konsist arch rules.
 */
package io.github.jkjamies.automationagent.agent.setup

/**
 * Loads markdown prompt files from the classpath. Each agent keeps its prompts under
 * `src/main/resources/prompts/<agent>/<name>.md`, so they stay reviewable markdown next to
 * the agent that uses them.
 */
class Prompts(
    private val basePath: String,
    private val loader: ClassLoader = Prompts::class.java.classLoader,
) {
    /**
     * Returns the trimmed contents of `<basePath>/<name>.md`. Throws
     * [IllegalArgumentException] if the resource is absent — call at agent construction time,
     * where a missing prompt is a programming error that should fail fast at startup.
     */
    fun get(name: String): String =
        getOrNull(name) ?: throw IllegalArgumentException("read prompt \"$name\": resource not found at $basePath/$name.md")

    /** Like [get] but returns null instead of throwing when the prompt is absent. */
    fun getOrNull(name: String): String? =
        loader.getResourceAsStream("$basePath/$name.md")
            ?.use { it.readBytes().toString(Charsets.UTF_8).trim() }

    companion object {
        /** Prompts rooted at `prompts/<agent>` (e.g. `forAgent("summary").get("summarize")`). */
        fun forAgent(agent: String): Prompts = Prompts("prompts/$agent")
    }
}
