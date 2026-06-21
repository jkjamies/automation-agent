// Flat ESLint config — the `go vet` / `ruff` analogue for the TypeScript port.
import js from '@eslint/js';
import tseslint from '@typescript-eslint/eslint-plugin';
import tsparser from '@typescript-eslint/parser';

export default [
  {
    ignores: ['dist/**', 'node_modules/**', 'coverage/**'],
  },
  js.configs.recommended,
  {
    files: ['**/*.ts'],
    languageOptions: {
      parser: tsparser,
      // Type-aware linting (projectService) so the high-value type-checked rules below can
      // run — chiefly no-floating-promises, which guards this promise-heavy codebase that
      // relies on explicit `void`-prefixing for fire-and-forget dispatch.
      parserOptions: { ecmaVersion: 2023, sourceType: 'module', projectService: true },
      globals: { process: 'readonly', console: 'readonly', fetch: 'readonly' },
    },
    plugins: { '@typescript-eslint': tseslint },
    rules: {
      ...tseslint.configs.recommended.rules,
      // TypeScript's own checker handles these; the core rules misfire on TS globals
      // (Buffer, AbortSignal, …) and the `const X = {...} as const; type X = ...` idiom.
      'no-undef': 'off',
      'no-redeclare': 'off',
      // `any` is occasionally forced by the ADK surface; `!` is used in a few well-guarded
      // spots (regex matches, indexed access). Both stay off, matching the documented idiom.
      '@typescript-eslint/no-explicit-any': 'off',
      '@typescript-eslint/no-unused-vars': ['error', { argsIgnorePattern: '^_', varsIgnorePattern: '^_' }],
      // Catch un-awaited promises that aren't explicitly discarded with `void`.
      '@typescript-eslint/no-floating-promises': 'error',
    },
  },
];
