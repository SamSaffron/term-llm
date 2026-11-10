import { Markdown } from './Markdown';

/** Use the ordinary markdown code renderer so highlighting and copying stay consistent. */
export function CodeModeScript({ raw }: { raw: string }) {
  let code: string;
  try {
    const args: unknown = JSON.parse(raw);
    if (!args || typeof args !== 'object' || !('code' in args) || typeof args.code !== 'string')
      return null;
    code = args.code;
  } catch {
    // Arguments can still be streaming; never show the JSON wrapper as JavaScript.
    return null;
  }
  // A script may contain markdown fences. Use a longer delimiter so none of its
  // contents can escape the code block and become rendered markup.
  const fence = '`'.repeat(
    Math.max(3, ...[...code.matchAll(/`+/g)].map(([run]) => run.length + 1)),
  );
  return (
    <>
      <div class="tool-details-label">JavaScript</div>
      <Markdown value={`${fence}javascript\n${code}\n${fence}`} />
    </>
  );
}
