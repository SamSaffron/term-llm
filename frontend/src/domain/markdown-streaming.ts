export const MAX_MUTABLE_MARKDOWN_CHARS = 64 * 1024;
export const MAX_STABLE_BOUNDARY_OPERATIONS = MAX_MUTABLE_MARKDOWN_CHARS * 8;

export interface StreamingMarkdownState {
  plainTextScanSource: string;
  plainTextEligible: boolean;
  latestContent: string;
  stableLength: number;
  lastBoundaryOperations: number;
}
interface Fence {
  char: '`' | '~';
  width: number;
}
export interface ActiveFencedCodeBlock extends Fence {
  type: 'fenced-code';
  sourceStart: number;
  contentStart: number;
  indent: number;
  language: string;
}
export interface FencedCodeInspection {
  closeStart: number | null;
  closeEnd: number | null;
  contentEnd: number;
}
export interface StreamingMarkdownAnalysis extends StableBoundaryAnalysis {
  activeBlock: ActiveFencedCodeBlock | null;
}
export interface StableBoundaryAnalysis {
  boundary: number;
  operations: number;
  overBudget: boolean;
  mutableTailLength: number;
}

export function createStreamingState(): StreamingMarkdownState {
  return {
    plainTextScanSource: '',
    plainTextEligible: true,
    latestContent: '',
    stableLength: 0,
    lastBoundaryOperations: 0,
  };
}

export const ELASTIC_STREAM_FRAME_MS = 32;
const ELASTIC_STREAM_MIN_CHARS_PER_SECOND = 40;
const ELASTIC_STREAM_MAX_CHARS_PER_SECOND = 12_000;
const ELASTIC_STREAM_BACKLOG_GAIN = 4;

export function elasticStreamingFrameDelay(contentLength: unknown): number {
  const length = Math.max(0, Number(contentLength) || 0);
  if (length > 64_000) return 200;
  if (length > 32_000) return 128;
  if (length > 8_000) return 64;
  return ELASTIC_STREAM_FRAME_MS;
}

export interface ElasticStreamingStep {
  characters: number;
  remainder: number;
}

/**
 * Converts queued source pressure into a bounded presentation rate. Small
 * queues drain gently; bursts accelerate rather than appearing in one jump.
 * The fractional remainder keeps the cadence accurate at low rates.
 */
export function elasticStreamingStep(
  backlog: number,
  elapsedMs: number,
  remainder = 0,
): ElasticStreamingStep {
  const queued = Math.max(0, Math.floor(Number(backlog) || 0));
  if (!queued) return { characters: 0, remainder: 0 };
  const elapsed = Math.max(0, Math.min(250, Number(elapsedMs) || 0));
  const rate = Math.min(
    ELASTIC_STREAM_MAX_CHARS_PER_SECOND,
    ELASTIC_STREAM_MIN_CHARS_PER_SECOND + queued * ELASTIC_STREAM_BACKLOG_GAIN,
  );
  const budget = Math.max(0, Number(remainder) || 0) + (rate * elapsed) / 1000;
  const characters = Math.min(queued, Math.max(1, Math.floor(budget)));
  return {
    characters,
    remainder: characters === queued ? 0 : Math.max(0, budget - characters),
  };
}

function fenceMarker(line: unknown): Fence | null {
  const match = String(line || '').match(/^[ \t]{0,3}(`{3,}|~{3,})([^\r\n]*)$/);
  if (!match || (match[1][0] === '`' && match[2].includes('`'))) return null;
  return { char: match[1][0] as Fence['char'], width: match[1].length };
}
function isFenceClose(line: unknown, active: Fence | null): boolean {
  if (!active) return false;
  const trimmed = String(line || '').replace(/^[ \t]{0,3}/, '');
  let width = 0;
  while (width < trimmed.length && trimmed[width] === active.char) width += 1;
  return width >= active.width && /^[ \t]*$/.test(trimmed.slice(width));
}
function scanFenceState(
  text: unknown,
  position = String(text || '').length,
): { active: Fence | null; count: number } {
  const value = String(text || '');
  const limit = Math.max(0, Math.min(value.length, Number(position) || 0));
  let active: Fence | null = null;
  let count = 0;
  let lineStart = 0;
  for (let index = 0; index <= limit; index += 1) {
    if (index !== limit && value.charCodeAt(index) !== 10) continue;
    const line = value.slice(lineStart, index);
    const marker = fenceMarker(line);
    if (marker) {
      if (!active) {
        active = marker;
        count += 1;
      } else if (marker.char === active.char && isFenceClose(line, active)) {
        active = null;
        count += 1;
      }
    }
    lineStart = index + 1;
  }
  return { active, count };
}
export function isInCodeBlockFast(text: unknown, position: number): boolean {
  return Boolean(scanFenceState(text, position).active);
}

/** Finds an opening fence whose line is complete but whose closing line has not
 * arrived yet. This intentionally treats a possible closing marker at EOF as
 * mutable: a later character can still turn it back into code. */
export function findActiveFencedCodeBlock(text: unknown): ActiveFencedCodeBlock | null {
  const value = String(text || '');
  let active:
    | (Fence & { sourceStart: number; contentStart: number; indent: number; language: string })
    | null = null;
  let lineStart = 0;
  for (let index = 0; index < value.length; index += 1) {
    if (value.charCodeAt(index) !== 10) continue;
    const line = value.slice(lineStart, index).replace(/\r$/, '');
    const marker = fenceMarker(line);
    if (!active && marker) {
      const indent = line.match(/^[ \t]{0,3}/)?.[0].length || 0;
      const info = line.slice(indent + marker.width).trim();
      active = {
        ...marker,
        sourceStart: lineStart,
        contentStart: index + 1,
        indent,
        language: info.split(/\s+/, 1)[0] || '',
      };
    } else if (active && marker?.char === active.char && isFenceClose(line, active)) {
      active = null;
    }
    lineStart = index + 1;
  }
  return active ? { type: 'fenced-code', ...active } : null;
}

export function findNextFencedCodeBlock(text: unknown, from = 0): ActiveFencedCodeBlock | null {
  const value = String(text || '');
  let lineStart = Math.max(0, Math.min(value.length, from));
  if (lineStart > 0 && value[lineStart - 1] !== '\n') {
    const nextLine = value.indexOf('\n', lineStart);
    if (nextLine < 0) return null;
    lineStart = nextLine + 1;
  }
  for (let index = lineStart; index < value.length; index += 1) {
    if (value.charCodeAt(index) !== 10) continue;
    const line = value.slice(lineStart, index).replace(/\r$/, '');
    const marker = fenceMarker(line);
    if (marker) {
      const indent = line.match(/^[ \t]{0,3}/)?.[0].length || 0;
      const info = line.slice(indent + marker.width).trim();
      return {
        type: 'fenced-code',
        ...marker,
        sourceStart: lineStart,
        contentStart: index + 1,
        indent,
        language: info.split(/\s+/, 1)[0] || '',
      };
    }
    lineStart = index + 1;
  }
  return null;
}

export function inspectFencedCodeBlock(
  text: unknown,
  active: ActiveFencedCodeBlock,
  finalized = false,
): FencedCodeInspection {
  const value = String(text || '');
  let lineStart = active.contentStart;
  for (let index = lineStart; index <= value.length; index += 1) {
    const complete = index < value.length ? value.charCodeAt(index) === 10 : finalized;
    if (!complete) continue;
    const line = value.slice(lineStart, index).replace(/\r$/, '');
    const marker = fenceMarker(line);
    if (marker?.char === active.char && isFenceClose(line, active)) {
      return {
        closeStart: lineStart,
        closeEnd: index < value.length ? index + 1 : index,
        contentEnd: lineStart,
      };
    }
    lineStart = index + 1;
  }

  // Do not append an incomplete line that can still become the closing fence.
  const pending = value.slice(lineStart);
  const trimmed = pending.replace(/^[ \t]{0,3}/, '');
  const run = trimmed.match(new RegExp(`^\\${active.char}+[ \\t]*$`));
  const contentEnd = run ? lineStart : value.length;
  return { closeStart: null, closeEnd: null, contentEnd };
}

function withoutFencedCode(text: unknown): string {
  const value = String(text || '');
  const output: string[] = [];
  let active: Fence | null = null;
  let lineStart = 0;
  for (let index = 0; index <= value.length; index += 1) {
    if (index !== value.length && value.charCodeAt(index) !== 10) continue;
    const line = value.slice(lineStart, index);
    const marker = fenceMarker(line);
    const wasActive = Boolean(active);
    if (marker) {
      if (!active) active = marker;
      else if (marker.char === active.char && isFenceClose(line, active)) active = null;
    }
    output.push(wasActive || marker ? ' '.repeat(line.length) : line);
    if (index < value.length) output.push('\n');
    lineStart = index + 1;
  }
  return output.join('');
}
const whitespace = (character: string | undefined): boolean =>
  character == null || /\s/.test(character);
const word = (character: string | undefined): boolean =>
  character != null && /[A-Za-z0-9]/.test(character);
function lineStart(text: string, index: number): boolean {
  for (let cursor = index - 1; cursor >= 0; cursor -= 1) {
    if (text[cursor] === '\n') return true;
    if (![' ', '\t'].includes(text[cursor])) return false;
  }
  return true;
}
function asteriskDelimiter(text: string, index: number, width: number): boolean {
  if (width === 1 && lineStart(text, index) && whitespace(text[index + 1])) return false;
  const previous = text[index - 1];
  const next = text[index + width];
  return !whitespace(next) && previous !== '*' && next !== '*';
}
function underscoreDelimiter(text: string, index: number): boolean {
  return !(word(text[index - 1]) && word(text[index + 1])) && !whitespace(text[index + 1]);
}

export function areInlineMarkersBalanced(text: string): boolean {
  let bold = false;
  let italic = false;
  let underscore = false;
  let strike = false;
  for (let index = 0; index < text.length; index += 1) {
    if (text[index] === '\\' && index + 1 < text.length) {
      index += 1;
      continue;
    }
    if (text[index] === '`') {
      let width = 1;
      while (text[index + width] === '`') width += 1;
      const close = text.indexOf('`'.repeat(width), index + width);
      if (close < 0) return false;
      index = close + width - 1;
      continue;
    }
    if (text.startsWith('**', index) && asteriskDelimiter(text, index, 2)) {
      bold = !bold;
      index += 1;
      continue;
    }
    if (text[index] === '*' && asteriskDelimiter(text, index, 1)) {
      italic = !italic;
      continue;
    }
    if (text[index] === '_' && underscoreDelimiter(text, index)) {
      underscore = !underscore;
      continue;
    }
    if (text.startsWith('~~', index)) {
      strike = !strike;
      index += 1;
    }
  }
  return !bold && !italic && !underscore && !strike;
}

export function areMathDelimitersBalanced(text: string): boolean {
  let inline = 0;
  let display = 0;
  let dollars = 0;
  for (let index = 0; index < text.length; index += 1) {
    if (text[index] === '`') {
      let width = 1;
      while (text[index + width] === '`') width += 1;
      const close = text.indexOf('`'.repeat(width), index + width);
      if (close < 0) return false;
      index = close + width - 1;
      continue;
    }
    if (text[index] === '\\' && index + 1 < text.length) {
      const next = text[index + 1];
      if (next === '(') inline += 1;
      else if (next === ')') {
        if (!inline) return false;
        inline -= 1;
      } else if (next === '[') display += 1;
      else if (next === ']') {
        if (!display) return false;
        display -= 1;
      }
      index += 1;
      continue;
    }
    if (text.startsWith('$$', index)) {
      dollars = dollars ? 0 : 1;
      index += 1;
    }
  }
  return inline === 0 && display === 0 && dollars === 0;
}

function containsBlockSyntax(text: string): boolean {
  return (
    /^\s{0,3}(?:#{1,6}\s|>\s|[-+*]\s|\d+[.)]\s|```|~~~)/m.test(text) ||
    /^\s*\|.*\|\s*$/m.test(text) ||
    /^\s*[-:| ]+\|[-:| ]*$/m.test(text)
  );
}
function containsInlineSyntax(text: string): boolean {
  if (/`|\[[^\]]*\]\([^\n)]+\)|(\*\*|~~)|<[A-Za-z!/][^>]*>|^\s*(?:---+|===+)\s*$/m.test(text))
    return true;
  for (let index = 0; index < text.length; index += 1) {
    if (text[index] === '*' && asteriskDelimiter(text, index, text[index + 1] === '*' ? 2 : 1))
      return true;
    if (text[index] === '_' && underscoreDelimiter(text, index)) return true;
  }
  return false;
}
export function canStreamPlainTextTail(text: unknown): boolean {
  const value = String(text || '');
  return (
    !value ||
    (!isInCodeBlockFast(value, value.length) &&
      !containsBlockSyntax(value) &&
      !containsInlineSyntax(value) &&
      !/[\\][([]|\$\$/.test(value) &&
      areInlineMarkersBalanced(value) &&
      areMathDelimitersBalanced(value))
  );
}
export function appendedTextIsPlainSafe(text: unknown): boolean {
  return !/[`[\]()!*_~<\\$|#>\r\n]/.test(String(text || ''));
}

/** Constructs that can change the interpretation of source committed much
 * earlier are kept on the canonical-render fallback path. */
export function hasIncrementalGlobalMarkdownSyntax(text: unknown): boolean {
  const value = withoutFencedCode(text);
  return (
    /^ {0,3}\[[^\]\r\n]+\]:\s*\S/m.test(value) ||
    /^(?: {4}|\t)\S/m.test(value) ||
    /^ {0,3}<(?:address|article|aside|base|blockquote|body|caption|center|col|colgroup|dd|details|dialog|dir|div|dl|dt|fieldset|figcaption|figure|footer|form|frame|frameset|h[1-6]|head|header|hr|html|iframe|legend|li|link|main|menu|menuitem|nav|noframes|ol|optgroup|option|p|param|search|section|summary|table|tbody|td|tfoot|th|thead|title|tr|track|ul)(?:\s|>|\/)/im.test(
      value,
    )
  );
}
export function canStreamPlainTextTailIncremental(
  state: StreamingMarkdownState | null,
  text: unknown,
): boolean {
  const value = String(text || '');
  if (!state) return canStreamPlainTextTail(value);
  const previous = state.plainTextScanSource;
  if (value.startsWith(previous)) {
    if (!state.plainTextEligible) {
      state.plainTextScanSource = value;
      return false;
    }
    if (appendedTextIsPlainSafe(value.slice(previous.length))) {
      state.plainTextScanSource = value;
      return true;
    }
  }
  const eligible = canStreamPlainTextTail(value);
  state.plainTextScanSource = value;
  state.plainTextEligible = eligible;
  return eligible;
}

function lastBlankBoundary(value: string, limit: number): number {
  const expression = /\r?\n[ \t]*\r?\n/g;
  let boundary = 0;
  let match: RegExpExecArray | null;
  while ((match = expression.exec(value))) {
    const candidate = match.index + match[0].length;
    if (candidate > limit) break;
    boundary = candidate;
  }
  return boundary;
}
function boundarySplitsList(value: string, boundary: number, stable: string): boolean {
  if (!/^\s{0,3}(?:[-+*]\s|\d+[.)]\s)/m.test(stable)) return false;
  const line = value.slice(boundary).match(/^(?:[ \t]*\r?\n)*([^\r\n]*)/)?.[1] || '';
  return /^\s{0,3}(?:[-+*]\s|\d+[.)]\s)/.test(line) || /^(?: {2,}|\t)\S/.test(line);
}
export function analyzeStableMarkdownBoundary(
  text: unknown,
  minTailLength: number,
  options: { maxMutableChars?: number; maxOperations?: number } = {},
): StableBoundaryAnalysis {
  const value = String(text || '');
  const mutableLimit = Math.max(1, options.maxMutableChars || MAX_MUTABLE_MARKDOWN_CHARS);
  const operationLimit = Math.max(1, options.maxOperations || MAX_STABLE_BOUNDARY_OPERATIONS);
  const result: StableBoundaryAnalysis = {
    boundary: 0,
    operations: 0,
    overBudget: false,
    mutableTailLength: value.length,
  };
  if (value.length > mutableLimit || value.length > operationLimit) {
    result.overBudget = true;
    return result;
  }
  const latest = value.length - Math.max(0, Number(minTailLength) || 0);
  if (latest <= 0) return result;
  const boundary = lastBlankBoundary(value, latest);
  result.operations = value.length;
  if (!boundary) return result;
  const estimated = value.length + boundary * 4;
  result.operations = estimated;
  if (estimated > operationLimit) {
    result.overBudget = true;
    return result;
  }
  const stable = value.slice(0, boundary);
  const balanced = withoutFencedCode(stable);
  if (
    !stable.trim() ||
    isInCodeBlockFast(value, boundary) ||
    !areInlineMarkersBalanced(balanced) ||
    !areMathDelimitersBalanced(balanced) ||
    boundarySplitsList(value, boundary, stable)
  )
    return result;
  result.boundary = boundary;
  result.mutableTailLength = value.length - boundary;
  return result;
}
export function findStableMarkdownBoundary(text: unknown, minTailLength: number): number {
  return analyzeStableMarkdownBoundary(text, minTailLength).boundary;
}

export function analyzeStreamingMarkdown(
  text: unknown,
  minTailLength: number,
  options: { maxMutableChars?: number; maxOperations?: number } = {},
): StreamingMarkdownAnalysis {
  const stable = analyzeStableMarkdownBoundary(text, minTailLength, options);
  return { ...stable, activeBlock: findActiveFencedCodeBlock(text) };
}
