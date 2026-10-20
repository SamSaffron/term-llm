// Qwen3.5 emits XML-style tool calls; earlier Bonsai releases use JSON.
// Parse only model-produced calls, validate against the tools actually offered,
// and reject incomplete markup instead of pretending it was ordinary prose.
export function parseToolResponse(text, tools = [], id = 'browser') {
  const calls = [];
  const available = new Map(tools.map(tool => [tool.function.name, tool.function]));
  const content = text.replace(/<tool_call>([\s\S]*?)<\/tool_call>/g, (_, raw) => {
    let call;
    const value = raw.trim();
    if (value.startsWith('{')) {
      call = JSON.parse(value);
      if (typeof call.arguments === 'string') call.arguments = JSON.parse(call.arguments);
    } else {
      const match = value.match(/^<function=([^>]+)>\s*([\s\S]*?)\s*<\/function>$/);
      if (!match) throw new Error('Malformed model tool call');
      const tool = available.get(match[1]);
      if (!tool) throw new Error(`Model requested undeclared tool: ${match[1]}`);
      const args = {};
      let remainder = match[2].replace(/<parameter=([^>]+)>([\s\S]*?)<\/parameter>/g, (block, name, input) => {
        const schema = tool.parameters?.properties?.[name];
        // Newlines immediately inside tags are template formatting, not payload.
        let parameter = input.replace(/^\r?\n/, '').replace(/\r?\n$/, '');
        if (schema?.type !== 'string') {
          try { parameter = JSON.parse(parameter); } catch {
            if (schema?.type && schema.type !== 'string') throw new Error(`Invalid ${schema.type} argument: ${name}`);
          }
        }
        args[name] = parameter;
        return '';
      });
      if (remainder.trim()) throw new Error('Unparsed tool parameters');
      call = { name: match[1], arguments: args };
    }
    if (!available.has(call.name)) throw new Error(`Model requested undeclared tool: ${call.name}`);
    if (!call.arguments || typeof call.arguments !== 'object' || Array.isArray(call.arguments)) throw new Error('Tool arguments must be an object');
    calls.push({ id: `call_${id}_${calls.length}`, type: 'function', function: { name: call.name, arguments: JSON.stringify(call.arguments) } });
    return '';
  });
  if (/<\/?(?:tool_call|function|parameter)\b/.test(content)) throw new Error('Incomplete or malformed model tool call');
  return { content: content.trim(), tool_calls: calls };
}
