// Import-boundary conformance tests:
//  * deterministic tooling must never import agent modules;
//  * the genai provider SDK (and the Gemini model) live only in agent/setup;
//  * nothing imports the cmd entrypoints.
import { join } from 'node:path';
import { describe, expect, it } from 'vitest';

import { collectImports, rel, repoRoot, tsFiles, under } from './helpers';

const TOOLING = ['src/githubapi', 'src/gitrepo', 'src/webhook', 'src/notify', 'src/scheduler'];

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
