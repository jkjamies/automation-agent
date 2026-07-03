/*
 * The consolidated category set + category selection (UI-only gating).
 *
 * Each category is one consolidated review agent bundling related dimensions; it emits
 * per-dimension-tagged findings over the whole filtered diff. The glue/synthesis pass
 * (architectural alignment, testability, test coverage) is built separately — it runs after these
 * and needs their findings.
 */
package com.automation.agent.agent.reviewer

import com.automation.agent.githubapi.PRFile

/**
 * Selects which model a category runs on: the code-reasoning model for the lenses that need it, the
 * base model for the lighter ones.
 */
enum class Tier {
    BASE, // the base reasoning model
    CODE, // the code reasoning model
}

/** One consolidated review agent. */
data class Category(
    val name: String, // unique sub-agent name + state-key suffix
    val title: String, // human label
    val promptName: String, // prompts/reviewer/<promptName>.md
    val tier: Tier,
    val uiOnly: Boolean = false, // accessibility runs only when the diff touches UI/markup files
    val other: Boolean = false, // the catch-all: its findings are forced to nitpick
)

/** The consolidated agent set. The glue/synthesis pass is built separately. */
val CATEGORIES: List<Category> =
    listOf(
        Category(name = "safety", title = "Safety", promptName = "safety", tier = Tier.CODE),
        Category(name = "security", title = "Security", promptName = "security", tier = Tier.CODE),
        Category(name = "performance", title = "Performance", promptName = "performance", tier = Tier.BASE),
        Category(name = "code_quality", title = "Code quality", promptName = "code_quality", tier = Tier.CODE),
        Category(name = "accessibility", title = "Accessibility", promptName = "accessibility", tier = Tier.BASE, uiOnly = true),
        Category(name = "other", title = "Other", promptName = "other", tier = Tier.BASE, other = true),
    )

/**
 * Returns the categories that apply to a changed-file set: all of them, minus the UI-only lens
 * (accessibility) when no UI/markup file changed.
 */
fun selectCategories(files: List<PRFile>): List<Category> {
    val ui = hasUiFiles(files)
    return CATEGORIES.filter { !(it.uiOnly && !ui) }
}

// The file types that warrant an accessibility lens (markup/templates/styles and component files).
private val UI_EXTENSIONS: Set<String> =
    setOf(".html", ".htm", ".xhtml", ".css", ".scss", ".sass", ".less", ".jsx", ".tsx", ".vue", ".svelte", ".astro")

/** Reports whether any changed file is UI/markup, by extension. */
fun hasUiFiles(files: List<PRFile>): Boolean = files.any { UI_EXTENSIONS.contains(extname(it.path).lowercase()) }

/** The trailing extension of a path (including the dot), or "" when there is none. */
private fun extname(p: String): String {
    val base = p.substring(p.lastIndexOf('/') + 1)
    val dot = base.lastIndexOf('.')
    // A leading dot (dotfile) or no dot means no extension.
    return if (dot > 0) base.substring(dot) else ""
}
