// Tests for fixflow units and tools: pure helpers, path-safety, envelope, repo tools.
import { mkdirSync, mkdtempSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { parallelAnalyze } from './analyze';
import type { FileEdit } from './applyfix';
import type { FileWork } from './engine';
import { parseKickoff } from './envelope';
import { readFile, safeJoin } from './files';
import { listDirEntries, repoTools } from './tools';
import { extractJsonArray, extractJsonObject, stripFences } from './util';

let dir: string;
beforeEach(() => {
  dir = mkdtempSync(join(tmpdir(), 'ff-units-'));
});
afterEach(() => {
  rmSync(dir, { recursive: true, force: true });
});

describe('envelope', () => {
  it('parses a valid kickoff and rejects bad ones', () => {
    const k = parseKickoff(Buffer.from('{"repo":"acme/api","report":{"x":1}}'));
    expect(k.owner()).toBe('acme');
    expect(k.name()).toBe('api');
    expect(k.base).toBe('main');
    expect(k.reportText()).not.toBe('');

    for (const body of ['{', '{"report":{"x":1}}', '{"repo":"noslash","report":{"x":1}}', '{"repo":"a/b"}']) {
      expect(() => parseKickoff(body)).toThrow();
    }
  });

  it('renders report text, unquoting a JSON-string report', () => {
    expect(parseKickoff('{"repo":"a/b","report":{"x":1}}').reportText()).toBe('{"x":1}');
    expect(parseKickoff('{"repo":"a/b","report":"TN:\\nSF:calc.go\\nDA:7,0\\n"}').reportText()).toBe(
      'TN:\nSF:calc.go\nDA:7,0\n',
    );
  });
});

describe('util', () => {
  it('extracts JSON and strips fences', () => {
    expect(extractJsonArray('noise [1,2] x')).toBe('[1,2]');
    expect(extractJsonArray('none')).toBe('');
    expect(extractJsonObject('x {"a":1} y')).toBe('{"a":1}');
    expect(extractJsonObject('none')).toBe('');
    expect(stripFences('```ts\nconst x = 1;\n```')).toBe('const x = 1;\n');
    expect(stripFences('const y = 2;')).toBe('const y = 2;\n');
  });
});

describe('files / safeJoin', () => {
  it('reads files and rejects traversal', () => {
    writeFileSync(join(dir, 'a.txt'), 'hello');
    expect(readFile(dir, 'a.txt')).toBe('hello');
    expect(() => readFile(dir, '../../etc/passwd')).toThrow();
  });

  it('rejects escapes and accepts safe paths', () => {
    for (const bad of ['../escape', '../../etc/cron.d/x', '/etc/passwd', 'a/../../b']) {
      expect(() => safeJoin(dir, bad)).toThrow();
    }
    for (const ok of ['a.ts', 'sub/dir/b.test.ts', '.']) {
      expect(() => safeJoin(dir, ok)).not.toThrow();
    }
  });
});

describe('tools', () => {
  it('lists entries, hiding .git and suffixing dirs', () => {
    mkdirSync(join(dir, 'sub'));
    mkdirSync(join(dir, '.git'));
    writeFileSync(join(dir, 'f.ts'), 'x');
    const ents = listDirEntries(dir, '.');
    const joined = ents.join(',');
    expect(joined).toContain('f.ts');
    expect(joined).toContain('sub/');
    expect(joined).not.toContain('.git');
    expect(() => listDirEntries(dir, '../..')).toThrow();
  });

  it('exposes read_file and list_dir tools', () => {
    writeFileSync(join(dir, 'AGENTS.md'), 'docs');
    mkdirSync(join(dir, 'src'));
    const tools = repoTools(dir);
    expect(new Set(tools.map((t) => t.name))).toEqual(new Set(['read_file', 'list_dir']));
  });
});

describe('parallelAnalyze', () => {
  it('collects non-empty edits sorted by path', async () => {
    const work: FileWork[] = [
      { path: 'b.ts', items: [] },
      { path: 'a.ts', items: [] },
    ];
    const fn = async (w: FileWork): Promise<FileEdit> => ({
      path: w.path + '.test.ts',
      content: 'export {};\n',
    });
    const edits = await parallelAnalyze(work, fn);
    expect(edits).toHaveLength(2);
    expect(edits[0]!.path).toBe('a.ts.test.ts');
    expect(edits[1]!.path).toBe('b.ts.test.ts');
  });

  it('throws when nothing is produced or there is no work', async () => {
    const skip = async (): Promise<FileEdit> => ({ path: '', content: '' });
    await expect(parallelAnalyze([{ path: 'a.ts', items: [] }], skip)).rejects.toThrow(/no edits/);
    await expect(parallelAnalyze([], skip)).rejects.toThrow();
  });
});
