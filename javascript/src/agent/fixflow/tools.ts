/**
 * Read-only repository tools for a tool-using agent.
 *
 * {@link repoTools} returns `read_file` and `list_dir` rooted at the checkout, so an
 * agent can examine the real repository — its standards docs, existing tests, and
 * layout — and ground decisions in what the repo actually does. Both tools are path-safe
 * via {@link safeJoin}.
 */
import { readdirSync } from 'node:fs';
import { type BaseTool, FunctionTool } from '@google/adk';

import { Type } from '../setup/genai';
import { readFile, safeJoin } from './files';

/**
 * List a checkout directory (path-safe), suffixing subdirectories with `/` and hiding
 * the `.git` directory. Entries are sorted.
 */
export function listDirEntries(root: string, rel: string): string[] {
  const full = safeJoin(root, rel);
  const names: string[] = [];
  for (const entry of readdirSync(full, { withFileTypes: true })) {
    if (entry.name === '.git') {
      continue;
    }
    names.push(entry.isDirectory() ? entry.name + '/' : entry.name);
  }
  names.sort();
  return names;
}

/** Return read-only tools (`read_file`, `list_dir`) rooted at the checkout. */
export function repoTools(root: string): BaseTool[] {
  const readFileTool = new FunctionTool({
    name: 'read_file',
    description: 'Read a repository file by its repo-relative path (e.g. "src/main.ts" or "AGENTS.md").',
    parameters: {
      type: Type.OBJECT,
      properties: { path: { type: Type.STRING, description: 'repo-relative file path' } },
      required: ['path'],
    },
    // Self-wrap so a bad/missing path is a recoverable tool error (the model can retry),
    // not a thrown rejection that aborts the analyze/explore run.
    execute: (input) => {
      try {
        return { content: readFile(root, (input as { path: string }).path) };
      } catch (err) {
        return { error: (err as Error).message };
      }
    },
  });

  const listDirTool = new FunctionTool({
    name: 'list_dir',
    description:
      'List the files and subdirectories of a repository directory by its repo-relative path. Use "." for the repository root.',
    parameters: {
      type: Type.OBJECT,
      properties: { path: { type: Type.STRING, description: 'repo-relative directory path' } },
      required: ['path'],
    },
    execute: (input) => {
      try {
        return { entries: listDirEntries(root, (input as { path: string }).path) };
      } catch (err) {
        return { error: (err as Error).message };
      }
    },
  });

  return [readFileTool, listDirTool];
}
