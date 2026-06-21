/**
 * Markdown prompt loader.
 *
 * Each agent ships its own `prompts/` directory of markdown files and reads them
 * through {@link Prompts}, so prompts stay reviewable next to the agent that uses
 * them. Files are read from disk relative to the agent's directory, resolved from
 * `import.meta.url`.
 */
import { readFileSync } from 'node:fs';
import { join } from 'node:path';

/** Loads `prompts/<name>.md` from a given anchor directory. */
export class Prompts {
  /**
   * @param anchorDir - the directory whose `prompts/` subdir holds the files.
   *   Agents pass `dirname(fileURLToPath(import.meta.url))`.
   */
  constructor(private readonly anchorDir: string) {}

  /**
   * Return the trimmed contents of `prompts/<name>.md`.
   *
   * @throws Error if the prompt file is missing or unreadable.
   */
  get(name: string): string {
    const path = join(this.anchorDir, 'prompts', `${name}.md`);
    try {
      return readFileSync(path, 'utf-8').trim();
    } catch (err) {
      throw new Error(`read prompt ${JSON.stringify(name)}: ${(err as Error).message}`);
    }
  }

  /**
   * Like {@link get}, but intended for agent-construction time where a missing
   * prompt is a programming error that should fail fast at startup.
   */
  mustGet(name: string): string {
    return this.get(name);
  }
}
