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
   * tools, then yields one final response carrying the text plus any tool calls
   * as genai function-call parts so the runner can execute the tools.
   */
  override async *generateContentAsync(
    req: LlmRequest,
    _stream = false,
    abortSignal?: AbortSignal,
  ): AsyncGenerator<LlmResponse, void> {
    const body: Record<string, unknown> = {
      model: this.modelName(req),
      messages: toOllamaMessages(req),
      stream: false,
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
    const data = (await resp.json()) as {
      model?: string;
      message?: { content?: string; tool_calls?: OllamaToolCall[] };
    };
    yield finalResponse(
      data.message?.content ?? '',
      data.message?.tool_calls ?? [],
    );
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

function finalResponse(text: string, toolCalls: OllamaToolCall[]): LlmResponse {
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
