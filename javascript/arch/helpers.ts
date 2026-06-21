/**
 * Shared helpers for the architecture-conformance tests. They scan each module's
 * import statements and resolve relative specifiers, so the tests can assert the
 * project's import boundaries without a full type-checker.
 */
import { readFileSync, readdirSync } from 'node:fs';
import { dirname, join, relative, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';

const SKIP_DIRS = new Set(['node_modules', '.git', 'coverage', 'dist', '.idea']);

/** The project root (the parent of the arch/ directory). */
export function repoRoot(): string {
  return dirname(dirname(fileURLToPath(import.meta.url)));
}

/** A parsed import: the raw module specifier and the named bindings it imports. */
export interface Import {
  specifier: string;
  names: string[];
  /** Absolute resolved path (no extension) for a relative specifier, else undefined. */
  resolved?: string;
}

export interface FileImports {
  path: string; // absolute
  imports: Import[];
}

/** Recursively list .ts files under `dir`, optionally excluding *.test.ts. */
export function tsFiles(dir: string, includeTests = false): string[] {
  const out: string[] = [];
  const walk = (d: string): void => {
    for (const entry of readdirSync(d, { withFileTypes: true })) {
      if (entry.isDirectory()) {
        if (!SKIP_DIRS.has(entry.name)) {
          walk(join(d, entry.name));
        }
      } else if (entry.name.endsWith('.ts')) {
        if (includeTests || !entry.name.endsWith('.test.ts')) {
          out.push(join(d, entry.name));
        }
      }
    }
  };
  walk(dir);
  return out.sort();
}

const IMPORT_RE = /(?:import|export)\s+(?:type\s+)?([\s\S]*?)\s+from\s+['"]([^'"]+)['"]/g;
const BARE_IMPORT_RE = /import\s+['"]([^'"]+)['"]/g;

/** Parse the static imports of a file. */
export function collectImports(file: string): FileImports {
  const src = readFileSync(file, 'utf-8');
  const imports: Import[] = [];
  for (const m of src.matchAll(IMPORT_RE)) {
    const clause = m[1] ?? '';
    const specifier = m[2]!;
    imports.push(makeImport(file, specifier, parseNames(clause)));
  }
  for (const m of src.matchAll(BARE_IMPORT_RE)) {
    imports.push(makeImport(file, m[1]!, []));
  }
  return { path: file, imports };
}

function makeImport(file: string, specifier: string, names: string[]): Import {
  const imp: Import = { specifier, names };
  if (specifier.startsWith('.')) {
    imp.resolved = resolve(dirname(file), specifier);
  }
  return imp;
}

function parseNames(clause: string): string[] {
  const brace = clause.match(/\{([\s\S]*)\}/);
  if (!brace) {
    return [];
  }
  return brace[1]!
    .split(',')
    .map((s) => s.trim().replace(/^type\s+/, '').split(/\s+as\s+/)[0]!.trim())
    .filter((s) => s !== '');
}

/** Repo-relative path for nicer error messages. */
export function rel(p: string): string {
  return relative(repoRoot(), p);
}

/** True if `resolvedPath` is inside one of the given repo-relative directories. */
export function under(resolvedPath: string, ...dirs: string[]): boolean {
  return dirs.some((d) => {
    const base = join(repoRoot(), d);
    return resolvedPath === base || resolvedPath.startsWith(base + '/');
  });
}
