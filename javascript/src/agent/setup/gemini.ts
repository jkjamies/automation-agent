/**
 * Gemini-backed model factory for the cloud deployment.
 *
 * Credentials/backend are read from the environment by the genai client (API key, or
 * Vertex via GOOGLE_GENAI_USE_VERTEXAI / GOOGLE_CLOUD_PROJECT).
 */
import { type BaseLlm, Gemini } from '@google/adk';

/**
 * Build the Gemini-backed model for the given model name.
 *
 * @throws Error if `geminiModel` is empty.
 */
export function newGeminiModel(geminiModel: string): BaseLlm {
  if (geminiModel === '') {
    throw new Error('GEMINI_MODEL must be set when LLM_PROVIDER=gemini');
  }
  return new Gemini({ model: geminiModel });
}
