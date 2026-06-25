/**
 * Ollama adapter for the ADK `BaseLlm` interface.
 *
 * adk-js ships no built-in Ollama model, so this subclasses {@link BaseLlm} and talks
 * to a local Ollama server's `/api/chat` endpoint over `fetch`. It honors the
 * request's generation config (temperature, num_ctx, JSON format) and tool
 * declarations, so tool-using agents work locally against Gemma.
 */
import { BaseLlm, type LlmRequest, type LlmResponse } from '@google/adk';
import type {
  Content,
  FunctionCall,
  GenerateContentConfig,
  Part,
  Schema,
  Tool,
} from '@google/genai';

// The context window requested from Ollama. gemma is served with a 32k window;
// setting it (and disabling truncation) avoids the server default (~4k) that would
// silently chop large file prompts.
const DEFAULT_NUM_CTX = 32768;
// Deterministic generation by default (temperature 0).
const DEFAULT_TEMPERATURE = 0.0;

interface OllamaToolCall {
  function: { name: string; arguments: Record<string, unknown> };
  id?: string;
}

interface OllamaMessage {
  role: 'system' | 'user' | 'assistant' | 'tool';
  content: string;
  tool_calls?: OllamaToolCall[];
  tool_name?: string;
}

/** One newline-delimited chunk of an Ollama `/api/chat` response. */
interface OllamaChatChunk {
  model?: string;
  message?: { content?: string; tool_calls?: OllamaToolCall[] };
  done?: boolean;
}

/**
 * Adapts a local Ollama server to the ADK `BaseLlm` interface so agents can run
 * against Gemma locally.
 */
export class OllamaLlm extends BaseLlm {
  private readonly host: string;

  /**
   * @param host - Ollama base URL, e.g. `http://localhost:11434`.
   * @param modelTag - model tag, e.g. `gemma4:12b`.
   * @throws Error if the model tag is empty.
   */
  constructor(host: string, modelTag: string) {
    if (modelTag === '') {
      throw new Error('ollama model tag must not be empty');
    }
    super({ model: modelTag });
    this.host = host.replace(/\/+$/, '');
  }

  /** List of models this adapter matches in the registry (all local tags). */
  static override readonly supportedModels: Array<string | RegExp> = [/^ollama\/.*/];

  /**
   * Implements `BaseLlm.generateContentAsync`. Forwards generation options and
   * tools, aggregates the response chunks, and yields a final response carrying
   * the full text plus any tool calls as genai function-call parts so the runner
   * can execute the tools.
   *
   * When `stream` is set (the runner uses `StreamingMode.SSE`), Ollama is asked to
   * stream: headers + the first chunk arrive after model-load + prefill and the
   * long token-by-token decode then streams over the body, so no first-byte timeout
   * can cap the whole generation. Non-empty text chunks are yielded as `partial`
   * responses (the drive loops ignore them and collect only the final text); the
   * full text and tool calls are aggregated and emitted on the terminal chunk.
   *
   * Timeouts: no overall request timeout is imposed — a long decode must be
   * unbounded — and the request inherits Node/undici's default 300s headers timeout,
   * the cold-start cushion (model-load + prefill) for the first streamed chunk. Only
   * `abortSignal` (cancellation from the runner) can interrupt the request.
   */
  override async *generateContentAsync(
    req: LlmRequest,
    stream = false,
    abortSignal?: AbortSignal,
  ): AsyncGenerator<LlmResponse, void> {
    const body: Record<string, unknown> = {
      model: this.modelName(req),
      messages: toOllamaMessages(req),
      stream,
      options: generationOptions(req.config),
      truncate: false, // fail loudly rather than silently truncate an oversized prompt
    };
    const tools = toOllamaTools(req.config);
    if (tools.length > 0) {
      body.tools = tools;
    }
    if (wantsJson(req.config)) {
      body.format = 'json';
    }

    const resp = await fetch(`${this.host}/api/chat`, {
      method: 'POST',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
      signal: abortSignal,
    });
    if (!resp.ok) {
      const text = await resp.text().catch(() => '');
      throw new Error(`ollama chat: ${resp.status} ${resp.statusText}: ${text.slice(0, 512)}`);
    }

    // Both modes are parsed as newline-delimited JSON: a non-streaming reply is a
    // single chunk, a streaming reply is many. Aggregate text + tool calls across
    // chunks so the final response is complete regardless of mode.
    let full = '';
    const toolCalls: OllamaToolCall[] = [];
    let model = '';
    let emittedFinal = false;
    for await (const chunk of iterChatChunks(resp)) {
      const content = chunk.message?.content ?? '';
      full += content;
      if (chunk.message?.tool_calls) {
        toolCalls.push(...chunk.message.tool_calls);
      }
      if (chunk.model) {
        model = chunk.model;
      }
      if (stream && !chunk.done && content.trim() !== '') {
        yield partialResponse(content, model);
      }
      if (chunk.done) {
        yield finalResponse(full, toolCalls, model);
        emittedFinal = true;
      }
    }
    // Non-streaming replies (and any stream that ends without an explicit `done`)
    // still need their terminal response emitted.
    if (!emittedFinal) {
      yield finalResponse(full, toolCalls, model);
    }
  }

  /** Live connections are not supported by the local adapter. */
  override async connect(): Promise<never> {
    throw new Error('ollama adapter does not support live connections');
  }

  /** Prefer req.model (a before-model callback may set it) over the default tag. */
  private modelName(req: LlmRequest): string {
    return req.model && req.model !== '' ? req.model : this.model;
  }
}

/** Map GenerateContentConfig onto Ollama options (deterministic defaults + large ctx). */
function generationOptions(config?: GenerateContentConfig): Record<string, unknown> {
  const opts: Record<string, unknown> = {
    num_ctx: DEFAULT_NUM_CTX,
    temperature: DEFAULT_TEMPERATURE,
  };
  if (!config) {
    return opts;
  }
  if (config.temperature != null) {
    opts.temperature = config.temperature;
  }
  if (config.topP != null) {
    opts.top_p = config.topP;
  }
  if (config.seed != null) {
    opts.seed = config.seed;
  }
  return opts;
}

function wantsJson(config?: GenerateContentConfig): boolean {
  return (config?.responseMimeType ?? '').toLowerCase().includes('json');
}

/**
 * Iterate the newline-delimited JSON chunks of an Ollama `/api/chat` response.
 *
 * A non-streaming reply is a single JSON object; a streaming reply is one object
 * per line. Only complete lines are parsed mid-stream — the trailing partial line is
 * buffered and parsed once the body ends. Blank lines are skipped; a malformed line
 * surfaces as a JSON parse error rather than being silently dropped (matching the Go
 * port, which propagates the official client's decode error).
 */
async function* iterChatChunks(resp: Response): AsyncGenerator<OllamaChatChunk, void> {
  if (!resp.body) {
    const text = await resp.text();
    yield* parseChunkLines(text);
    return;
  }
  const reader = resp.body.getReader();
  const decoder = new TextDecoder();
  let buf = '';
  for (;;) {
    const { value, done } = await reader.read();
    if (done) {
      break;
    }
    buf += decoder.decode(value, { stream: true });
    const nl = buf.lastIndexOf('\n');
    if (nl >= 0) {
      yield* parseChunkLines(buf.slice(0, nl));
      buf = buf.slice(nl + 1);
    }
  }
  buf += decoder.decode();
  yield* parseChunkLines(buf);
}

/** Parse each non-blank line of `text` as one Ollama chat chunk. */
function* parseChunkLines(text: string): Generator<OllamaChatChunk, void> {
  for (const line of text.split('\n')) {
    const trimmed = line.trim();
    if (trimmed === '') {
      continue;
    }
    yield JSON.parse(trimmed) as OllamaChatChunk;
  }
}

function partialResponse(text: string, model: string): LlmResponse {
  return {
    content: { role: 'model', parts: [{ text }] },
    partial: true,
    modelVersion: model,
  };
}

function finalResponse(text: string, toolCalls: OllamaToolCall[], model: string): LlmResponse {
  const parts: Part[] = [];
  if (text.trim() !== '') {
    parts.push({ text });
  }
  for (const tc of toolCalls) {
    parts.push({ functionCall: toGenaiFunctionCall(tc) });
  }
  return {
    content: { role: 'model', parts },
    turnComplete: true,
    modelVersion: model,
  };
}

function toGenaiFunctionCall(tc: OllamaToolCall): FunctionCall {
  // The ID must be preserved: the flow keys long-running tool tracking and
  // function-response correlation on it. Ollama omits ids, so fall back to the name.
  const id = tc.id && tc.id !== '' ? tc.id : tc.function.name;
  return { id, name: tc.function.name, args: tc.function.arguments ?? {} };
}

/** Convert genai function declarations into Ollama tool definitions. */
function toOllamaTools(config?: GenerateContentConfig): Array<Record<string, unknown>> {
  const tools: Array<Record<string, unknown>> = [];
  for (const t of (config?.tools ?? []) as Tool[]) {
    for (const fd of t.functionDeclarations ?? []) {
      tools.push({
        type: 'function',
        function: {
          name: fd.name,
          description: fd.description ?? '',
          parameters: toToolParams(fd.parameters),
        },
      });
    }
  }
  return tools;
}

function toToolParams(s?: Schema): Record<string, unknown> {
  if (!s) {
    return { type: 'object', properties: {} };
  }
  return lowerSchema(s);
}

/** Recursively lowercase genai Schema `type` (e.g. OBJECT -> object) for Ollama/JSON-schema. */
function lowerSchema(s: Schema): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  if (s.type) {
    out.type = String(s.type).toLowerCase();
  }
  if (s.description) {
    out.description = s.description;
  }
  if (s.required) {
    out.required = s.required;
  }
  if (s.items) {
    out.items = lowerSchema(s.items);
  }
  if (s.properties) {
    const props: Record<string, unknown> = {};
    for (const [name, ps] of Object.entries(s.properties)) {
      props[name] = lowerSchema(ps as Schema);
    }
    out.properties = props;
  }
  return out;
}

/**
 * Flatten the system instruction + contents into Ollama chat messages, including
 * assistant tool-calls and tool-result messages so the function-calling round-trip
 * works.
 */
function toOllamaMessages(req: LlmRequest): OllamaMessage[] {
  const msgs: OllamaMessage[] = [];
  const sys = systemText(req.config);
  if (sys !== '') {
    msgs.push({ role: 'system', content: sys });
  }
  for (const c of req.contents ?? []) {
    if (!c) {
      continue;
    }
    const role: OllamaMessage['role'] = c.role === 'model' ? 'assistant' : 'user';
    let text = '';
    const toolCalls: OllamaToolCall[] = [];
    for (const p of c.parts ?? []) {
      if (p.functionResponse) {
        msgs.push({
          role: 'tool',
          tool_name: p.functionResponse.name ?? '',
          content: jsonString(p.functionResponse.response),
        });
      } else if (p.functionCall) {
        toolCalls.push({
          function: {
            name: p.functionCall.name ?? '',
            arguments: (p.functionCall.args ?? {}) as Record<string, unknown>,
          },
        });
      } else if (p.text) {
        text += p.text;
      }
    }
    if (text !== '' || toolCalls.length > 0) {
      const m: OllamaMessage = { role, content: text };
      if (toolCalls.length > 0) {
        m.tool_calls = toolCalls;
      }
      msgs.push(m);
    }
  }
  return msgs;
}

function systemText(config?: GenerateContentConfig): string {
  const si = config?.systemInstruction;
  if (si == null) {
    return '';
  }
  if (typeof si === 'string') {
    return si;
  }
  // ContentUnion: a Content, an array of Parts, or a Part.
  if (Array.isArray(si)) {
    return si.map((p) => (typeof p === 'string' ? p : ((p as Part).text ?? ''))).join('');
  }
  return contentText(si as Content);
}

function contentText(c: Content | null | undefined): string {
  if (!c || !c.parts) {
    return '';
  }
  return c.parts.map((p) => p.text ?? '').join('');
}

function jsonString(v: unknown): string {
  if (v == null) {
    return '';
  }
  try {
    return JSON.stringify(v);
  } catch {
    return '';
  }
}
