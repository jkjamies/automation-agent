// OKF bundle conformance tests: the system's knowledge lives in the repo-root okf/
// bundle (Open Knowledge Format). Structural gate only — every concept opens with YAML
// frontmatter declaring a non-empty type, every directory carries an index.md, every
// bundle-absolute link resolves, and the repo-root AGENTS.md points at the bundle index.
import { existsSync, readdirSync, readFileSync } from 'node:fs';
import { basename, join } from 'node:path';
import { describe, expect, it } from 'vitest';

import { repoRoot } from './helpers';

const RESERVED = new Set(['index.md', 'log.md', 'AGENTS.md']);

function okfRoot(): string {
  const root = join(repoRoot(), '..', 'okf');
  expect(existsSync(root), `okf bundle missing at ${root}`).toBe(true);
  return root;
}

function walk(dir: string, out: { dirs: string[]; files: string[] }): void {
  out.dirs.push(dir);
  for (const entry of readdirSync(dir, { withFileTypes: true })) {
    const p = join(dir, entry.name);
    if (entry.isDirectory()) {
      walk(p, out);
    } else if (entry.name.endsWith('.md')) {
      out.files.push(p);
    }
  }
}

describe('okf bundle', () => {
  it('every concept has a frontmatter type', () => {
    const out = { dirs: [], files: [] } as { dirs: string[]; files: string[] };
    walk(okfRoot(), out);
    const bad: string[] = [];
    for (const p of out.files) {
      if (RESERVED.has(basename(p))) continue;
      const body = readFileSync(p, 'utf8');
      if (!body.startsWith('---\n')) {
        bad.push(`${p}: missing frontmatter block`);
        continue;
      }
      const end = body.indexOf('\n---', 4);
      if (end < 0) {
        bad.push(`${p}: frontmatter block not closed`);
        continue;
      }
      if (!/^type:\s*\S/m.test(body.slice(4, end))) {
        bad.push(`${p}: frontmatter missing required non-empty type field`);
      }
    }
    expect(bad).toEqual([]);
  });

  it('every directory has an index.md', () => {
    const out = { dirs: [], files: [] } as { dirs: string[]; files: string[] };
    walk(okfRoot(), out);
    expect(out.dirs.filter((d) => !existsSync(join(d, 'index.md')))).toEqual([]);
  });

  it('bundle-absolute links resolve', () => {
    const root = okfRoot();
    const out = { dirs: [], files: [] } as { dirs: string[]; files: string[] };
    walk(root, out);
    const dangling: string[] = [];
    for (const p of out.files) {
      const body = readFileSync(p, 'utf8');
      for (const m of body.matchAll(/\]\((\/[^)#]+\.md)(?:#[^)]*)?\)/g)) {
        const target = m[1] ?? '';
        if (!existsSync(join(root, target))) {
          dangling.push(`${p}: ${target}`);
        }
      }
    }
    expect(dangling).toEqual([]);
  });

  it('repo-root AGENTS.md points at the bundle index', () => {
    const p = join(repoRoot(), '..', 'AGENTS.md');
    expect(readFileSync(p, 'utf8')).toContain('okf/index.md');
  });
});
