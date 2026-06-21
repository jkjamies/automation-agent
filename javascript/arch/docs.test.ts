// AGENTS.md-presence conformance test: every meaningful directory must carry an
// AGENTS.md. Build/output and content (prompts) subdirs are exempt, as are hidden dirs
// other than .agents.
import { existsSync, readdirSync } from 'node:fs';
import { join, relative } from 'node:path';
import { describe, expect, it } from 'vitest';

import { repoRoot } from './helpers';

function skipDir(base: string): boolean {
  if (['node_modules', '.git', 'coverage', 'dist', '.idea', 'prompts'].includes(base)) {
    return true;
  }
  return base.startsWith('.') && base !== '.agents';
}

describe('docs', () => {
  it('every directory has an AGENTS.md', () => {
    const root = repoRoot();
    const missing: string[] = [];
    const walk = (dir: string): void => {
      if (!existsSync(join(dir, 'AGENTS.md'))) {
        const r = relative(root, dir);
        missing.push(r === '' ? '(root)' : r);
      }
      for (const entry of readdirSync(dir, { withFileTypes: true })) {
        if (entry.isDirectory() && !skipDir(entry.name)) {
          walk(join(dir, entry.name));
        }
      }
    };
    walk(root);
    expect(missing).toEqual([]);
  });
});
