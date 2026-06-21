// Tests for the markdown prompt loader.
import { mkdtempSync, mkdirSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { afterAll, beforeAll, describe, expect, it } from 'vitest';

import { Prompts } from './prompts';

let dir: string;

beforeAll(() => {
  dir = mkdtempSync(join(tmpdir(), 'prompts-'));
  mkdirSync(join(dir, 'prompts'), { recursive: true });
  writeFileSync(join(dir, 'prompts', 'summarize.md'), '\n  the prompt body  \n');
});

afterAll(() => {
  rmSync(dir, { recursive: true, force: true });
});

describe('Prompts', () => {
  it('returns trimmed prompt contents via get and mustGet', () => {
    const p = new Prompts(dir);
    const body = p.get('summarize');
    expect(body).toBe('the prompt body');
    expect(p.mustGet('summarize')).toBe(body);
  });

  it('throws on a missing prompt', () => {
    const p = new Prompts(dir);
    expect(() => p.get('does-not-exist')).toThrow(/read prompt/);
  });
});
