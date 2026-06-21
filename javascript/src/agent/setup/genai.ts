/**
 * Re-exports the small genai surface used by code outside `setup`, so the raw provider
 * SDK import stays confined to this layer (enforced by the arch tests).
 */
export { Type } from '@google/genai';
export type { Content, FunctionCall, FunctionResponse, Part, Schema, Tool } from '@google/genai';
