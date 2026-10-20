// Browser inference adapter for Prism ML's Talk WebGPU engine (vendored in
// ../vendor/kernels.ts). The engine owns the GGUF reader, tokenizer, chat
// template and WebGPU kernels; this file only maps the demo's OpenAI-style
// bridge onto Bonsai.load/chat and the tool-call parser.
//
// The engine is TypeScript and is imported dynamically so this module stays
// importable from Node tests; bundle.mjs compiles both into one browser bundle.
import { parseToolResponse } from './tool-parser.mjs';

export const MODEL_REPO = 'prism-ml/Ternary-Bonsai-2-2B-gguf';
export const MODEL_REVISION = '683f7be2629342a50987627b40881e54761a2430';
export const MODEL_FILE = 'Ternary-Bonsai-2-2B-PTQ1_0.gguf';
export const MODEL_BYTES = 429183456;
export const MODEL = MODEL_REPO;
export const CONTEXT = 8192;
export const MODEL_ORIGIN_URL = `https://huggingface.co/${MODEL_REPO}/resolve/${MODEL_REVISION}/${MODEL_FILE}`;
// Byte-identical pinned GGUF, mirrored behind Cloudflare. Preserve the original
// cache identity so phones can reuse chunks they already fetched from HF.
export const MODEL_URL = 'https://demo.term-llm.com/models/bonsai2-2b-ptq1-36061ad406df.bin';
export const MODEL_CACHE_KEY = MODEL_ORIGIN_URL;

const DEFAULT_OUTPUT_TOKENS = 1024;
const MAX_OUTPUT_TOKENS = 2048;

export async function loadBonsai({ progress = () => {} } = {}) {
  if (!globalThis.navigator?.gpu) {
    throw new Error('WebGPU is unavailable in this browser context. This demo has no remote inference fallback.');
  }
  const report = event => {
    try {
      progress(event);
    } catch {
      // The gate's rendering must never fail a model load.
    }
  };
  const { Bonsai, UrlByteSource } = await import('../vendor/kernels.ts');
  // The model is immutable and its exact size is pinned. Cached response
  // metadata can omit the advisory Accept-Ranges header despite valid 206
  // responses. Avoid that probe; UrlByteSource still requires and validates
  // actual partial-content responses on reads.
  const source = await openPinnedModelSource(UrlByteSource);
  const engine = await Bonsai.load({
    model: source,
    contextLength: CONTEXT,
    // Weight chunks only. No prompt, conversation or workspace data is stored.
    cache: true,
    cacheKey: () => MODEL_CACHE_KEY,
    gpu: { powerPreference: 'high-performance' },
    onProgress: event => report(adaptProgress(event)),
    onCache: event => report(adaptCache(event)),
    onDiagnostic: diagnostic => console.error(
      `[bonsai ${diagnostic.stage}${diagnostic.label ? `/${diagnostic.label}` : ''}] ${diagnostic.message}`,
    ),
  });
  return createBonsaiSession(engine, { contextLength: engine.info.contextLength });
}

// Use the immutable model's pinned metadata rather than an unreliable HEAD
// advertisement. Never disable requireRangeRequests: a 200 full-body response
// must still fail instead of downloading hundreds of MB for every chunk.
export function openPinnedModelSource(UrlByteSource) {
  return UrlByteSource.open(MODEL_URL, {
    knownSize: MODEL_BYTES,
    knownAcceptsRanges: true,
    requireRangeRequests: true,
  });
}

/**
 * Adapt a loaded Talk `Bonsai` instance to the bridge contract. Exported so the
 * request/response and streaming behavior can be tested without WebGPU.
 */
export function createBonsaiSession(engine, { contextLength = engine?.info?.contextLength ?? CONTEXT } = {}) {
  for (const method of ['renderPrompt', 'encode', 'chat', 'dispose']) {
    if (typeof engine?.[method] !== 'function') throw new Error(`Bonsai engine is missing ${method}()`);
  }
  const context = Number.isFinite(Number(contextLength)) && Number(contextLength) > 0
    ? Number(contextLength)
    : CONTEXT;
  let busy = false;
  let disposed = false;

  return {
    model: MODEL,
    context,

    async complete(request, { signal, onText = () => {} } = {}) {
      if (disposed) throw new Error('Bonsai was disposed');
      if (busy) throw new Error('Browser inference is already running');
      if (signal?.aborted) throw abortError(signal);
      const normalized = normalizeRequest(request);
      const messages = toChatMessages(normalized.messages);
      const tools = normalized.tools;
      busy = true;

      const controller = new AbortController();
      const forwardAbort = () => controller.abort(signal.reason);
      signal?.addEventListener('abort', forwardAbort, { once: true });

      let raw = '';
      let stats;
      let reason = 'stop';
      let aborted = false;
      let promptTokens = 0;
      try {
        // Render once for the prompt+output budget check. chat() renders the
        // same template again for generation (and reuses the KV prefix).
        const prompt = engine.renderPrompt(messages, { thinking: false, tools });
        promptTokens = engine.encode(prompt).length;
        if (promptTokens + normalized.maxTokens > context) {
          throw new Error(`Prompt (${promptTokens}) plus output (${normalized.maxTokens}) exceeds this demo's ${context}-token context. Start a new conversation; no history was silently truncated.`);
        }

        for await (const event of engine.chat(messages, {
          thinking: false,
          maxNewTokens: normalized.maxTokens,
          tools,
          signal: controller.signal,
          sampling: { temperature: 0 },
        })) {
          if (event?.type === 'token') {
            const text = event.text ?? '';
            raw += text;
            // Native token callbacks, one chunk per generated token. The worker
            // filters tool-call XML; never re-send the final text as a chunk.
            if (event.phase === 'answer' && text) {
              try {
                onText(text);
              } catch {
                // A page-side render failure must not break generation.
              }
            }
          } else if (event?.type === 'done') {
            stats = event.stats;
            reason = event.reason;
            if (event.reason === 'abort') aborted = true;
          }
        }

        if (aborted || signal?.aborted || controller.signal.aborted) {
          throw abortError(signal ?? controller.signal);
        }

        const parsed = parseToolResponse(raw, tools, newId());
        const completionTokens = stats?.newTokens ?? engine.encode(raw).length;
        const usedPromptTokens = stats?.promptTokens ?? promptTokens;
        const hasToolCalls = parsed.tool_calls.length > 0;
        return {
          id: `bonsai_${newId()}`,
          object: 'chat.completion',
          created: Math.floor(Date.now() / 1000),
          model: MODEL,
          choices: [{
            index: 0,
            finish_reason: hasToolCalls ? 'tool_calls' : finishReason(reason),
            message: {
              role: 'assistant',
              content: parsed.content,
              ...(hasToolCalls ? { tool_calls: parsed.tool_calls } : {}),
            },
          }],
          usage: {
            prompt_tokens: usedPromptTokens,
            completion_tokens: completionTokens,
            total_tokens: usedPromptTokens + completionTokens,
          },
        };
      } finally {
        signal?.removeEventListener('abort', forwardAbort);
        busy = false;
      }
    },

    async dispose() {
      if (disposed) return;
      disposed = true;
      engine.dispose();
    },
  };
}

/** OpenAI chat-completions request -> the engine's inputs. */
export function normalizeRequest(request) {
  if (!request || typeof request !== 'object') throw new Error('Inference requires an OpenAI chat request');
  if (!Array.isArray(request.messages) || request.messages.length === 0) throw new Error('Inference requires messages');

  const choice = request.tool_choice;
  if (choice !== undefined && choice !== null && choice !== 'auto' && choice !== 'none') {
    throw new Error('The browser demo supports automatic (auto) or disabled (none) tool choice, not forced calls');
  }
  if (request.tools != null && !Array.isArray(request.tools)) throw new Error('tools must be an array');
  const tools = choice === 'none' ? [] : request.tools ?? [];
  for (const tool of tools) {
    if (!tool || typeof tool !== 'object' || typeof tool.function?.name !== 'string' || !tool.function.name) {
      throw new Error('Each tool must be an OpenAI function definition');
    }
  }

  const requested = request.max_completion_tokens ?? request.max_tokens ?? DEFAULT_OUTPUT_TOKENS;
  const numeric = Number(requested);
  const maxTokens = Number.isFinite(numeric)
    ? Math.max(1, Math.min(Math.floor(numeric), MAX_OUTPUT_TOKENS))
    : DEFAULT_OUTPUT_TOKENS;
  return { messages: request.messages, tools, maxTokens };
}

/** OpenAI messages -> Talk ChatMessage records (text only, plus structured tool turns). */
export function toChatMessages(messages) {
  return messages.map((message, index) => {
    if (!message || typeof message !== 'object') throw new Error(`Message ${index} is not an object`);
    if (!['system', 'user', 'assistant', 'tool'].includes(message.role)) {
      throw new Error(`Unsupported message role: ${message.role}`);
    }
    const content = textContent(message.content, message.role);
    if (message.role !== 'assistant') return { role: message.role, content };

    const out = { role: 'assistant', content };
    if (typeof message.reasoning === 'string' && message.reasoning) out.reasoning = message.reasoning;
    const calls = message.tool_calls ?? message.toolCalls;
    if (calls != null) {
      if (!Array.isArray(calls)) throw new Error('assistant tool_calls must be an array');
      const converted = calls.map((call, callIndex) => {
        const fn = call?.function;
        if (!fn || typeof fn.name !== 'string' || !fn.name) {
          throw new Error(`Assistant tool call ${callIndex} has no function name`);
        }
        return { name: fn.name, arguments: toolArguments(fn.arguments, fn.name) };
      });
      if (converted.length) out.toolCalls = converted;
    }
    return out;
  });
}

function textContent(content, role) {
  if (content == null) return '';
  if (typeof content === 'string') return content;
  if (!Array.isArray(content)) throw new Error(`Message content for role ${role} must be a string or text parts`);
  return content.map(part => {
    if (typeof part === 'string') return part;
    if (part && (part.type === 'text' || part.type === 'input_text') && typeof part.text === 'string') return part.text;
    throw new Error('This demo supports text messages only; image and audio parts are not supported');
  }).join('');
}

function toolArguments(value, name) {
  if (value == null || value === '') return {};
  if (typeof value === 'string') {
    try {
      value = JSON.parse(value);
    } catch {
      throw new Error(`Invalid JSON arguments for tool ${name}`);
    }
  }
  if (typeof value !== 'object' || Array.isArray(value)) {
    throw new Error(`Tool ${name} arguments must be an object`);
  }
  return value;
}

function finishReason(reason) {
  return reason === 'length' || reason === 'context_full' ? 'length' : 'stop';
}

function abortError(signal) {
  const reason = signal?.reason;
  if (reason instanceof Error) return reason;
  return new DOMException('Browser inference aborted', 'AbortError');
}

function newId() {
  return globalThis.crypto?.randomUUID?.() ?? Math.random().toString(36).slice(2);
}

/** Talk LoadProgress -> the progress shape the demo gate renders. */
export function adaptProgress(event = {}) {
  switch (event.stage) {
    case 'gpu':
      return { status: 'WebGPU', file: event.message ?? '' };
    case 'header':
      return { status: 'Reading GGUF header', file: event.message ?? '' };
    case 'tokenizer':
      return { status: 'Building tokenizer', file: event.message ?? '' };
    case 'weights': {
      const total = Number(event.total) || 0;
      const loaded = Number(event.loaded) || 0;
      return {
        status: 'Downloading weights',
        file: `${formatBytes(loaded)} / ${formatBytes(total)}${event.tensor ? ' · ' + event.tensor : ''}`,
        progress: total > 0 ? Math.min(100, (loaded / total) * 100) : undefined,
      };
    }
    case 'compile': {
      const total = Number(event.total) || 0;
      const done = Number(event.done) || 0;
      return {
        status: 'Compiling WebGPU kernels',
        file: event.message ?? `${done}/${total}`,
        progress: total > 0 ? Math.min(100, (done / total) * 100) : undefined,
      };
    }
    case 'check':
      return { status: 'Checking model', file: event.message ?? '' };
    case 'draft':
      return { status: 'Loading draft', file: event.message ?? '' };
    case 'ready':
      return { status: 'Model ready', file: event.message ?? '', progress: 100 };
    default:
      return { status: event.stage ? String(event.stage) : 'Loading model', file: event.message ?? '' };
  }
}

/** Talk WeightCacheProgress -> a progress event for the gate. */
export function adaptCache(event = {}) {
  const total = Number(event.total) || 0;
  const cached = Number(event.fromCache) || 0;
  const network = Number(event.fromNetwork) || 0;
  return {
    status: 'Loading model bytes',
    file: `${formatBytes(network)} downloaded · ${formatBytes(cached)} from cache · ${formatBytes(total)} total`,
    progress: total > 0 ? Math.min(100, ((cached + network) / total) * 100) : undefined,
  };
}

function formatBytes(value) {
  const bytes = Number(value);
  if (!Number.isFinite(bytes) || bytes <= 0) return '0 B';
  const units = ['B', 'KB', 'MB', 'GB'];
  let size = bytes;
  let unit = 0;
  while (size >= 1000 && unit < units.length - 1) {
    size /= 1000;
    unit += 1;
  }
  return `${unit === 0 ? size : size.toFixed(1)} ${units[unit]}`;
}
