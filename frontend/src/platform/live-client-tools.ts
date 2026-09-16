// Trusted host callbacks only: provider frames never supply executable code.
export interface LiveClientToolDefinition {
  name: string;
  description: string;
  parameters: Record<string, unknown>;
}

export interface LiveClientTool extends LiveClientToolDefinition {
  // Must validate application-specific arguments/authorization before side effects.
  execute(
    args: Record<string, unknown>,
    context: { signal: AbortSignal; callId: string },
  ): unknown | Promise<unknown>;
}

const bytes = (value: string) => new TextEncoder().encode(value).length;
const object = (value: unknown): value is Record<string, unknown> =>
  value !== null && typeof value === 'object' && !Array.isArray(value);

export function snapshotClientTools(tools: readonly LiveClientTool[]): LiveClientTool[] {
  if (tools.length > 16) throw new Error('At most 16 live client tools are allowed.');
  const names = new Set<string>();
  return tools.map((tool) => {
    if (!/^ui_[A-Za-z0-9_]{1,60}$/.test(tool.name) || names.has(tool.name))
      throw new Error('Live client tool names must be unique and match ui_[A-Za-z0-9_]{1,60}.');
    names.add(tool.name);
    if (typeof tool.description !== 'string' || !tool.description || bytes(tool.description) > 1024)
      throw new Error('Live client tool descriptions must be 1–1024 bytes.');
    if (typeof tool.execute !== 'function')
      throw new Error('Live client tools require an execute callback.');
    const encoded = JSON.stringify(tool.parameters);
    if (!encoded || bytes(encoded) > 16384)
      throw new Error('Live client tool schemas must be at most 16 KiB.');
    const parameters: unknown = JSON.parse(encoded);
    if (!object(parameters) || parameters.type !== 'object' || !object(parameters.properties))
      throw new Error('Live client tools require an object schema with properties.');
    return { name: tool.name, description: tool.description, parameters, execute: tool.execute };
  });
}

interface FunctionCall {
  type: 'function_call';
  name: string;
  call_id: string;
  arguments: string;
}

// Opted-in Realtime calls have a single continuation owner: this browser.
// The sideband still executes built-ins, but does not send response.create for
// their results. Wait for every result in a response before continuing once.
export class LiveClientToolSession {
  private readonly tools: Map<string, LiveClientTool>;
  private readonly seen = new Set<string>();
  private readonly completed = new Set<string>();
  private readonly responses = new Map<string, string[]>();
  private readonly pending = new Set<AbortController>();
  private closed = false;

  constructor(
    tools: readonly LiveClientTool[],
    private readonly send: (event: unknown) => void,
    private readonly fail: (error: Error) => void,
  ) {
    this.tools = new Map(tools.map((tool) => [tool.name, tool]));
  }

  receive(data: unknown): void {
    if (this.closed || typeof data !== 'string') return;
    let event: Record<string, unknown>;
    try {
      const parsed: unknown = JSON.parse(data);
      if (!object(parsed)) return;
      event = parsed;
    } catch {
      return;
    }
    // GA uses added; accept created for compatible Realtime endpoints too.
    if (event.type === 'conversation.item.added' || event.type === 'conversation.item.created') {
      const item = event.item;
      if (
        object(item) &&
        item.type === 'function_call_output' &&
        typeof item.call_id === 'string'
      ) {
        if (this.completed.size >= 1024 && !this.completed.has(item.call_id)) {
          this.fail(new Error('Live client tool call limit reached; start a new call.'));
          return;
        }
        this.completed.add(item.call_id);
        this.flush();
      }
      return;
    }
    if (event.type !== 'response.done' || !object(event.response)) return;
    const response = event.response;
    if (
      response.status !== 'completed' ||
      typeof response.id !== 'string' ||
      !Array.isArray(response.output) ||
      this.responses.has(response.id)
    )
      return;
    const calls = response.output.filter(
      (item): item is FunctionCall =>
        object(item) &&
        item.type === 'function_call' &&
        typeof item.name === 'string' &&
        typeof item.call_id === 'string' &&
        !!item.call_id &&
        typeof item.arguments === 'string',
    );
    if (!calls.length) return;
    if (this.responses.size >= 1024 || this.seen.size + calls.length > 1024) {
      this.fail(new Error('Live client tool call limit reached; start a new call.'));
      return;
    }
    this.responses.set(
      response.id,
      calls.map((call) => call.call_id),
    );
    for (const call of calls) {
      if (this.seen.has(call.call_id)) continue;
      this.seen.add(call.call_id);
      if (call.name === 'delegate_to_controller') continue;
      void this.execute(call);
    }
    this.flush();
  }

  private async execute(call: FunctionCall): Promise<void> {
    const controller = new AbortController();
    this.pending.add(controller);
    let timer: ReturnType<typeof setTimeout> | undefined;
    let output: string;
    try {
      const tool = this.tools.get(call.name);
      if (!tool) throw new Error('Unregistered live client tool.');
      if (bytes(call.arguments) > 16384)
        throw new Error('Live client tool arguments exceed 16 KiB.');
      const args: unknown = JSON.parse(call.arguments);
      if (!object(args)) throw new Error('Live client tool arguments must be an object.');
      const canceled = new Promise<never>((_, reject) => {
        controller.signal.addEventListener(
          'abort',
          () => reject(new Error('Live client tool canceled or timed out.')),
          { once: true },
        );
        timer = setTimeout(() => controller.abort(), 10000);
      });
      const result = await Promise.race([
        Promise.resolve().then(() => {
          controller.signal.throwIfAborted();
          return tool.execute(args, { signal: controller.signal, callId: call.call_id });
        }),
        canceled,
      ]);
      output = JSON.stringify({ ok: true, result: result ?? null });
      if (bytes(output) > 16384) throw new Error('Live client tool result exceeds 16 KiB.');
    } catch (error) {
      output = JSON.stringify({
        ok: false,
        error: error instanceof Error ? error.message.slice(0, 1024) : 'Live client tool failed.',
      });
    } finally {
      clearTimeout(timer);
      this.pending.delete(controller);
    }
    if (this.closed) return;
    try {
      this.send({
        type: 'conversation.item.create',
        item: { type: 'function_call_output', call_id: call.call_id, output },
      });
      this.completed.add(call.call_id);
      this.flush();
    } catch (error) {
      this.fail(
        error instanceof Error ? error : new Error('Could not send live client tool result.'),
      );
    }
  }

  private flush(): void {
    if (this.closed) return;
    for (const [id, calls] of this.responses) {
      if (!calls.length || !calls.every((callId) => this.completed.has(callId))) continue;
      // Keep the response ID to suppress duplicates until teardown.
      this.responses.set(id, []);
      try {
        this.send({ type: 'response.create' });
      } catch (error) {
        this.fail(error instanceof Error ? error : new Error('Could not continue live response.'));
      }
    }
  }

  close(): void {
    this.closed = true;
    for (const controller of this.pending) controller.abort();
    this.pending.clear();
    this.responses.clear();
    this.seen.clear();
    this.completed.clear();
  }
}
