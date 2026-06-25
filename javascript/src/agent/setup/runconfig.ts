/**
 * Single source of truth for the ADK run config used at every runner call site.
 *
 * Streaming is enabled (`StreamingMode.SSE`): the Ollama transport is tuned for a
 * long-lived streaming body, so a non-streaming run would let the response-header
 * timeout bound the *entire* generation (a long code change on slow hardware then
 * fails). With SSE the headers + first chunk arrive after model-load + prefill and
 * the long token-by-token decode streams over the body with no overall cap.
 *
 * Streaming is transparent to every consumer because the drive loops collect text
 * only from non-partial events (see `runner.ts` / `longrun.ts`), so partial chunks
 * are ignored and tool calls still surface on the final event.
 */
import { type RunConfig, StreamingMode } from '@google/adk';

/** Shared run config — SSE streaming on — passed to every `runAsync` call. */
export const STREAMING_RUN_CONFIG: RunConfig = {
  streamingMode: StreamingMode.SSE,
};
