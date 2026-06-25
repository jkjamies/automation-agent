// Tests for the shared streaming run config.
import { StreamingMode } from '@google/adk';
import { describe, expect, it } from 'vitest';

import { STREAMING_RUN_CONFIG } from './runconfig';

describe('STREAMING_RUN_CONFIG', () => {
  it('enables SSE streaming so long Ollama generations are not capped by a first-byte timeout', () => {
    expect(STREAMING_RUN_CONFIG.streamingMode).toBe(StreamingMode.SSE);
  });
});
