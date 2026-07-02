/**
 * The consolidated category set + category selection (UI-only gating).
 *
 * Each category is one consolidated review agent bundling related dimensions; it emits
 * per-dimension-tagged findings over the whole filtered diff. The glue/synthesis pass
 * (architectural alignment, testability, test coverage) is built separately — it runs after these
 * and needs their findings.
 */

import type { PRFile } from '../../githubapi/client';

/**
 * Selects which model a category runs on: the code-reasoning model for the lenses that need it, the
 * base model for the lighter ones (model-size-split).
 */
export const Tier = {
  Base: 0, // OLLAMA_MODEL (base reasoning)
  Code: 1, // OLLAMA_CODE_MODEL (code reasoning)
} as const;
export type Tier = (typeof Tier)[keyof typeof Tier];

/** One consolidated review agent. */
export interface Category {
  name: string; // unique ADK sub-agent name + state-key suffix
  title: string; // human label
  promptName: string; // prompts/<promptName>.md
  tier: Tier;
  uiOnly: boolean; // accessibility runs only when the diff touches UI/markup files
  other: boolean; // the catch-all: its findings are forced to nitpick
}

function category(partial: Omit<Category, 'uiOnly' | 'other'> & Partial<Category>): Category {
  return { uiOnly: false, other: false, ...partial };
}

/** The consolidated agent set. The glue/synthesis pass is built separately. */
export const CATEGORIES: Category[] = [
  category({ name: 'safety', title: 'Safety', promptName: 'safety', tier: Tier.Code }),
  category({ name: 'security', title: 'Security', promptName: 'security', tier: Tier.Code }),
  category({ name: 'performance', title: 'Performance', promptName: 'performance', tier: Tier.Base }),
  category({ name: 'code_quality', title: 'Code quality', promptName: 'code_quality', tier: Tier.Code }),
  category({
    name: 'accessibility',
    title: 'Accessibility',
    promptName: 'accessibility',
    tier: Tier.Base,
    uiOnly: true,
  }),
  category({ name: 'other', title: 'Other', promptName: 'other', tier: Tier.Base, other: true }),
];

/**
 * Return the categories that apply to a changed-file set: all of them, minus the UI-only lens
 * (accessibility) when no UI/markup file changed.
 */
export function selectCategories(files: PRFile[]): Category[] {
  const ui = hasUiFiles(files);
  return CATEGORIES.filter((c) => !(c.uiOnly && !ui));
}

// The file types that warrant an accessibility lens (markup/templates/styles and component files).
const UI_EXTENSIONS: ReadonlySet<string> = new Set([
  '.html',
  '.htm',
  '.xhtml',
  '.css',
  '.scss',
  '.sass',
  '.less',
  '.jsx',
  '.tsx',
  '.vue',
  '.svelte',
  '.astro',
]);

/** Report whether any changed file is UI/markup, by extension. */
export function hasUiFiles(files: PRFile[]): boolean {
  return files.some((f) => UI_EXTENSIONS.has(extname(f.path).toLowerCase()));
}

/** The trailing extension of a path (including the dot), or "" when there is none. */
function extname(p: string): string {
  const base = p.slice(p.lastIndexOf('/') + 1);
  const dot = base.lastIndexOf('.');
  // A leading dot (dotfile) or no dot means no extension.
  return dot > 0 ? base.slice(dot) : '';
}
