import { defineConfig } from 'vitest/config';

// Coverage gate: >= 80% over src/ (cmd is composition-only and excluded).
export default defineConfig({
  test: {
    globals: true,
    environment: 'node',
    include: ['src/**/*.test.ts', 'arch/**/*.test.ts'],
    coverage: {
      provider: 'v8',
      include: ['src/**/*.ts'],
      exclude: [
        'src/**/*.test.ts',
        'src/testutil/**',
        '**/*.d.ts',
        // The firestore backends are exercised only under the emulator (gated tests), so
        // they are out of the default in-process coverage gate — mirroring the Go/Python ports.
        'src/agent/setup/parkstore_firestore.ts',
        'src/agent/setup/session_firestore.ts',
      ],
      reporter: ['text', 'text-summary'],
      thresholds: {
        lines: 80,
        functions: 80,
        statements: 80,
        branches: 80,
      },
    },
  },
});
