(function (factory) {
  'use strict';
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = factory();
  } else {
    window.TermLLMMarkdownStreaming = factory();
  }
})(function markdownStreamingFactory() {
  'use strict';

  const MAX_MUTABLE_MARKDOWN_CHARS = 64 * 1024;
  const MAX_STABLE_BOUNDARY_OPERATIONS = MAX_MUTABLE_MARKDOWN_CHARS * 8;

  function nextStreamingRenderDelay(contentLength) {
    const length = Math.max(0, Number(contentLength) || 0);
    if (length > 96000) return 250;
    if (length > 32000) return 150;
    if (length > 8000) return 75;
    return 33;
  }

  function createStreamingState() {
    return {
      messageId: '',
      body: null,
      stableContainer: null,
      tailContainer: null,
      stableLength: 0,
      stableHashLength: 0,
      stableHashA: 0,
      stableHashB: 0,
      latestContent: '',
      lastTailContent: '',
      lastTailSource: '',
      tailTextNode: null,
      dirty: false,
      rendering: false,
      rafId: 0,
      timerId: 0,
      lastRenderAt: 0,
      plainTextScanSource: '',
      plainTextEligible: true,
      plainFallback: false,
      lastBoundaryOperations: 0
    };
  }

  function fenceMarker(line) {
    const match = String(line || '').match(/^[ \t]{0,3}(`{3,}|~{3,})/);
    if (!match) return null;
    return { char: match[1][0], width: match[1].length };
  }

  function isFenceClose(line, active) {
    if (!active) return false;
    const trimmed = String(line || '').replace(/^[ \t]{0,3}/, '');
    let width = 0;
    while (width < trimmed.length && trimmed[width] === active.char) width += 1;
    return width >= active.width && /^[ \t]*$/.test(trimmed.slice(width));
  }

  function scanFenceState(text, pos = String(text || '').length) {
    const value = String(text || '');
    const safePos = Math.max(0, Math.min(value.length, Number(pos) || 0));
    let active = null;
    let count = 0;
    let lineStart = 0;

    for (let i = 0; i <= safePos; i += 1) {
      if (i !== safePos && value.charCodeAt(i) !== 10) continue;
      const marker = fenceMarker(value.slice(lineStart, i));
      if (marker) {
        if (!active) {
          active = marker;
          count += 1;
        } else if (marker.char === active.char && isFenceClose(value.slice(lineStart, i), active)) {
          active = null;
          count += 1;
        }
      }
      lineStart = i + 1;
    }

    return { active, count };
  }

  function countCodeFencesFast(text) {
    return scanFenceState(text).count;
  }

  function isInCodeBlockFast(text, pos) {
    return Boolean(scanFenceState(text, pos).active);
  }

  function withoutFencedCode(text) {
    const value = String(text || '');
    const out = [];
    let active = null;
    let lineStart = 0;

    for (let i = 0; i <= value.length; i += 1) {
      if (i !== value.length && value.charCodeAt(i) !== 10) continue;
      const line = value.slice(lineStart, i);
      const marker = fenceMarker(line);
      const wasActive = Boolean(active);
      if (marker) {
        if (!active) active = marker;
        else if (marker.char === active.char && isFenceClose(line, active)) active = null;
      }
      const masked = wasActive || marker ? ' '.repeat(line.length) : line;
      out.push(masked);
      if (i < value.length) out.push('\n');
      lineStart = i + 1;
    }

    return out.join('');
  }

  function isWhitespace(ch) {
    return ch == null || /\s/.test(ch);
  }

  function isWordChar(ch) {
    return ch != null && /[A-Za-z0-9]/.test(ch);
  }

  function isLineStart(text, index) {
    for (let i = index - 1; i >= 0; i -= 1) {
      if (text[i] === '\n') return true;
      if (text[i] !== ' ' && text[i] !== '\t') return false;
    }
    return true;
  }

  function isAsteriskListMarker(text, index, width) {
    return width === 1 && isLineStart(text, index) && isWhitespace(text[index + 1]);
  }

  function isSingleAsteriskDelimiter(text, index) {
    if (isAsteriskListMarker(text, index, 1)) return false;
    const prev = text[index - 1];
    const next = text[index + 1];
    if (isWhitespace(next)) return false;
    if (prev === '*' || next === '*') return false;
    return true;
  }

  function isDoubleAsteriskDelimiter(text, index) {
    if (isAsteriskListMarker(text, index, 1)) return false;
    const prev = text[index - 1];
    const next = text[index + 2];
    if (isWhitespace(next)) return false;
    if (prev === '*' || next === '*') return false;
    return true;
  }

  function isUnderscoreDelimiter(text, index) {
    const prev = text[index - 1];
    const next = text[index + 1];
    if (isWordChar(prev) && isWordChar(next)) return false;
    if (isWhitespace(next)) return false;
    return true;
  }

  function areInlineMarkersBalanced(text) {
    let inBold = false;
    let inItalicAsterisk = false;
    let inItalicUnderscore = false;
    let inStrikethrough = false;

    for (let i = 0; i < text.length; i += 1) {
      if (text[i] === '\\' && i + 1 < text.length) {
        i += 1;
        continue;
      }

      if (text[i] === '`') {
        let ticks = 1;
        while (i + ticks < text.length && text[i + ticks] === '`') {
          ticks += 1;
        }
        const closing = '`'.repeat(ticks);
        const closeIdx = text.indexOf(closing, i + ticks);
        if (closeIdx === -1) {
          return false;
        }
        i = closeIdx + ticks - 1;
        continue;
      }

      if (text[i] === '*' && i + 1 < text.length && text[i + 1] === '*' && isDoubleAsteriskDelimiter(text, i)) {
        inBold = !inBold;
        i += 1;
        continue;
      }

      if (text[i] === '*' && isSingleAsteriskDelimiter(text, i)) {
        inItalicAsterisk = !inItalicAsterisk;
        continue;
      }

      if (text[i] === '_' && isUnderscoreDelimiter(text, i)) {
        inItalicUnderscore = !inItalicUnderscore;
        continue;
      }

      if (text[i] === '~' && i + 1 < text.length && text[i + 1] === '~') {
        inStrikethrough = !inStrikethrough;
        i += 1;
      }
    }

    return !inBold && !inItalicAsterisk && !inItalicUnderscore && !inStrikethrough;
  }

  function areMathDelimitersBalanced(text) {
    let inlineParen = 0;
    let displayBracket = 0;
    let displayDollar = 0;

    for (let i = 0; i < text.length; i += 1) {
      if (text[i] === '`') {
        let ticks = 1;
        while (i + ticks < text.length && text[i + ticks] === '`') {
          ticks += 1;
        }
        const closing = '`'.repeat(ticks);
        const closeIdx = text.indexOf(closing, i + ticks);
        if (closeIdx === -1) {
          return false;
        }
        i = closeIdx + ticks - 1;
        continue;
      }

      if (text[i] === '\\' && i + 1 < text.length) {
        const next = text[i + 1];
        if (next === '(') {
          inlineParen += 1;
          i += 1;
          continue;
        }
        if (next === ')') {
          if (inlineParen === 0) return false;
          inlineParen -= 1;
          i += 1;
          continue;
        }
        if (next === '[') {
          displayBracket += 1;
          i += 1;
          continue;
        }
        if (next === ']') {
          if (displayBracket === 0) return false;
          displayBracket -= 1;
          i += 1;
          continue;
        }
        i += 1;
        continue;
      }

      if (text[i] === '$' && i + 1 < text.length && text[i + 1] === '$') {
        displayDollar = displayDollar === 0 ? 1 : 0;
        i += 1;
      }
    }

    return inlineParen === 0 && displayBracket === 0 && displayDollar === 0;
  }

  function containsMarkdownBlockSyntax(text) {
    return /^\s{0,3}(?:#{1,6}\s|>\s|[-+*]\s|\d+[.)]\s|```|~~~)/m.test(text)
      || /^\s*\|.*\|\s*$/m.test(text)
      || /^\s*[-:| ]+\|[-:| ]*$/m.test(text);
  }

  function containsMarkdownInlineSyntax(text) {
    if (/`/.test(text)) return true;
    if (/\[[^\]]*\]\([^\n)]+\)/.test(text)) return true;
    if (/(^|[^\\])!\[[^\]]*\]\([^\n)]+\)/.test(text)) return true;
    if (/(\*\*|~~)/.test(text)) return true;
    if (/<[A-Za-z!/][^>]*>/.test(text)) return true;
    if (/^\s*---+\s*$/m.test(text) || /^\s*===+\s*$/m.test(text)) return true;

    for (let i = 0; i < text.length; i += 1) {
      const ch = text[i];
      if (ch === '*' && isSingleAsteriskDelimiter(text, i)) return true;
      if (ch === '*' && text[i + 1] === '*' && isDoubleAsteriskDelimiter(text, i)) return true;
      if (ch === '_' && isUnderscoreDelimiter(text, i)) return true;
    }

    return false;
  }

  function containsMathDelimiterSyntax(text) {
    const value = String(text || '');
    return value.includes('\\(') || value.includes('\\[') || value.includes('$$');
  }

  function canStreamPlainTextTail(text) {
    const value = String(text || '');
    if (!value) return true;
    if (isInCodeBlockFast(value, value.length)) return false;
    if (containsMarkdownBlockSyntax(value)) return false;
    if (containsMarkdownInlineSyntax(value)) return false;
    if (containsMathDelimiterSyntax(value)) return false;
    if (!areInlineMarkersBalanced(value)) return false;
    if (!areMathDelimitersBalanced(value)) return false;
    return true;
  }

  function appendedTextIsPlainSafe(text) {
    // If a streamed delta contains only ordinary prose characters, it cannot
    // introduce markdown/math syntax by itself or complete syntax that began
    // in a previously plain prefix. Newlines and punctuation with markdown
    // meaning fall back to the full scanner.
    return !/[`\[\]()!*_~<\\$|#>\r\n]/.test(String(text || ''));
  }

  function canStreamPlainTextTailIncremental(streamState, text) {
    const value = String(text || '');
    if (!streamState) return canStreamPlainTextTail(value);

    const previous = String(streamState.plainTextScanSource || '');
    if (value.startsWith(previous)) {
      if (streamState.plainTextEligible === false) {
        streamState.plainTextScanSource = value;
        return false;
      }
      if (streamState.plainTextEligible === true && appendedTextIsPlainSafe(value.slice(previous.length))) {
        streamState.plainTextScanSource = value;
        return true;
      }
    }

    const eligible = canStreamPlainTextTail(value);
    streamState.plainTextScanSource = value;
    streamState.plainTextEligible = eligible;
    return eligible;
  }

  function containsListSyntax(text) {
    return /^\s{0,3}(?:[-+*]\s|\d+[.)]\s)/m.test(String(text || ''));
  }

  function boundarySplitsList(text, boundary, stableCandidate) {
    if (!containsListSyntax(stableCandidate)) return false;
    const remainder = String(text || '').slice(boundary);
    // Skip only complete blank lines. Keep the indentation on the first
    // nonblank line so loose-list continuations and list-item code remain
    // attached to the stable candidate.
    const nextLineMatch = remainder.match(/^(?:[ \t]*\r?\n)*([^\r\n]*)/);
    const nextLine = nextLineMatch ? nextLineMatch[1] : '';
    return /^\s{0,3}(?:[-+*]\s|\d+[.)]\s)/.test(nextLine) || /^(?: {2,}|\t)\S/.test(nextLine);
  }

  function lastBlankLineBoundaryBefore(text, maxIndex) {
    const value = String(text || '');
    const limit = Math.max(0, Math.min(value.length, Number(maxIndex) || 0));
    const blankLine = /\r?\n[ \t]*\r?\n/g;
    let best = 0;
    let match;

    while ((match = blankLine.exec(value)) !== null) {
      const boundary = match.index + match[0].length;
      if (boundary > limit) break;
      best = boundary;
    }

    return best;
  }

  function analyzeStableMarkdownBoundary(text, minTailLength, options = {}) {
    const value = String(text || '');
    const mutableLimit = Math.max(1, Number(options.maxMutableChars) || MAX_MUTABLE_MARKDOWN_CHARS);
    const operationLimit = Math.max(1, Number(options.maxOperations) || MAX_STABLE_BOUNDARY_OPERATIONS);
    const result = {
      boundary: 0,
      operations: 0,
      overBudget: false,
      mutableTailLength: value.length
    };

    // Do not start an unbounded scan. The caller can preserve the streamed
    // source with its incremental plain-text fallback until the final full
    // Markdown render.
    if (value.length > mutableLimit || value.length > operationLimit) {
      result.overBudget = true;
      return result;
    }

    const tailLength = Math.max(0, Number(minTailLength) || 0);
    const latestBoundary = value.length - tailLength;
    if (latestBoundary <= 0) return result;

    const boundary = lastBlankLineBoundaryBefore(value, latestBoundary);
    result.operations = value.length;
    if (boundary <= 0) return result;

    // One bounded fence scan, one fence masking pass, and one pass each for
    // inline and math state. The estimate is deliberately conservative and is
    // exposed for deterministic operation-count tests.
    const estimatedOperations = value.length + (boundary * 4);
    if (estimatedOperations > operationLimit) {
      result.overBudget = true;
      return result;
    }

    const stableCandidate = value.slice(0, boundary);
    if (!stableCandidate.trim()) return result;
    if (isInCodeBlockFast(value, boundary)) {
      result.operations = estimatedOperations;
      return result;
    }

    const balanceCandidate = withoutFencedCode(stableCandidate);
    if (!areInlineMarkersBalanced(balanceCandidate)) {
      result.operations = estimatedOperations;
      return result;
    }
    if (!areMathDelimitersBalanced(balanceCandidate)) {
      result.operations = estimatedOperations;
      return result;
    }
    if (boundarySplitsList(value, boundary, stableCandidate)) {
      result.operations = estimatedOperations;
      return result;
    }

    result.boundary = boundary;
    result.operations = estimatedOperations;
    return result;
  }

  function findStableMarkdownBoundary(text, minTailLength) {
    return analyzeStableMarkdownBoundary(text, minTailLength).boundary;
  }

  return {
    MAX_MUTABLE_MARKDOWN_CHARS,
    MAX_STABLE_BOUNDARY_OPERATIONS,
    createStreamingState,
    nextStreamingRenderDelay,
    countCodeFencesFast,
    isInCodeBlockFast,
    areInlineMarkersBalanced,
    areMathDelimitersBalanced,
    analyzeStableMarkdownBoundary,
    findStableMarkdownBoundary,
    canStreamPlainTextTail,
    canStreamPlainTextTailIncremental,
    appendedTextIsPlainSafe
  };
});
