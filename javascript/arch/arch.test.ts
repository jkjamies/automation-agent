// Import-boundary conformance tests:
//  * deterministic tooling must never import agent modules;
//  * the genai provider SDK (and the Gemini model) live only in agent/setup;
//  * nothing imports the cmd entrypoints;
//  * only config reads the environment.
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

import { collectImports, rel, repoRoot, tsFiles, under } from './helpers';

const TOOLING = ['src/auth', 'src/githubapi', 'src/gitrepo', 'src/webhook', 'src/notify', 'src/tasks', 'src/obs'];

// process.env may be read only here; every other module receives config via injection.
const CONFIG_FILE = 'src/config/config.ts';
const ENV_READ_RE = /\bprocess\s*\.\s*env\b/;
// Only config may reference an OTEL_* env var literal; the rest of the package takes tracing
// settings as a typed struct. Match every JS quote style (single, double, backtick) so a stray
// read in any spelling is caught. Comments are stripped first, since JSDoc legitimately mentions
// `OTEL_*` in prose (markdown backticks) — only code references are a violation.
const OTEL_READ_RE = /['"`]OTEL_/;

/** Remove line and block comments so a scan sees only code, not prose that mentions env vars. */
function stripComments(src: string): string {
  return src.replace(/\/\*[\s\S]*?\*\//g, '').replace(/\/\/.*$/gm, '');
}

describe('import boundaries', () => {
  it('tooling does not import agents', () => {
    const errors: string[] = [];
    for (const file of tsFiles(join(repoRoot(), 'src'))) {
      if (!under(file, ...TOOLING)) {
        continue;
      }
      for (const imp of collectImports(file).imports) {
        if (imp.resolved && under(imp.resolved, 'src/agent')) {
          errors.push(`${rel(file)} imports agent module ${imp.specifier} — tooling must not depend on agents`);
        }
      }
    }
    expect(errors).toEqual([]);
  });

  it('confines the genai provider SDK to agent/setup', () => {
    const errors: string[] = [];
    for (const file of tsFiles(join(repoRoot(), 'src'))) {
      if (under(file, 'src/agent/setup', 'src/testutil')) {
        continue;
      }
      for (const imp of collectImports(file).imports) {
        if (imp.specifier === '@google/genai') {
          errors.push(`${rel(file)} imports the genai SDK outside agent/setup`);
        }
        if (imp.specifier === '@google/adk' && imp.names.includes('Gemini')) {
          errors.push(`${rel(file)} imports the Gemini model outside agent/setup`);
        }
      }
    }
    expect(errors).toEqual([]);
  });

  it('only config reads the environment', () => {
    const errors: string[] = [];
    for (const file of tsFiles(join(repoRoot(), 'src'))) {
      if (under(file, 'src/config')) {
        continue;
      }
      if (ENV_READ_RE.test(readFileSync(file, 'utf-8'))) {
        errors.push(`${rel(file)} reads process.env — only ${CONFIG_FILE} may`);
      }
    }
    expect(errors).toEqual([]);
  });

  it('only config reads the OTEL_* environment', () => {
    const errors: string[] = [];
    for (const file of tsFiles(join(repoRoot(), 'src'))) {
      if (under(file, 'src/config')) {
        continue;
      }
      if (OTEL_READ_RE.test(stripComments(readFileSync(file, 'utf-8')))) {
        errors.push(`${rel(file)} references an OTEL_ env var literal — only ${CONFIG_FILE} may read OTEL_*`);
      }
    }
    expect(errors).toEqual([]);
  });

  it('nothing outside cmd imports cmd', () => {
    const errors: string[] = [];
    for (const dir of ['src', 'arch']) {
      for (const file of tsFiles(join(repoRoot(), dir), true)) {
        for (const imp of collectImports(file).imports) {
          if (imp.resolved && under(imp.resolved, 'cmd')) {
            errors.push(`${rel(file)} imports cmd module ${imp.specifier}`);
          }
        }
      }
    }
    expect(errors).toEqual([]);
  });
});
