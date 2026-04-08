(function (factory) {
  'use strict';
  if (typeof module !== 'undefined' && module.exports) {
    module.exports = factory();
  } else {
    window.TermLLMMarkdownStreaming = factory();
  }
})(function markdownStreamingFactory() {
  'use strict';

  const MIN_COMMIT_CHUNK = 512;
  const MAX_PENDING_TAIL = 2048;
  const HARD_BOUNDARY_ATTEMPTS = 12;
  const LINE_BOUNDARY_ATTEMPTS = 24;
  const SOFT_BOUNDARY_ATTEMPTS = 24;

  function nextStreamingRenderDelay(contentLength) {
    const length = Math.max(0, Number(contentLength) || 0);
    if (length > 96000) return 250;
    if (length > 32000) return 150;
    if (length > 8000) return 75;
    return 33;
  }

  function preferredTailLength(totalLength) {
    const length = Math.max(0, Number(totalLength) || 0);
    if (length > 96000) return 2048;
    if (length > 32000) return 1536;
    if (length > 8000) return 1024;
    return 768;
  }

  function createStreamingState() {
    return {
      messageId: '',
      body: null,
      stableContainer: null,
      tailContainer: null,
      committedLength: 0,
      latestContent: '',
      lastTailContent: '',
      lastTailSource: '',
      tailTextNode: null,
      dirty: false,
      rendering: false,
      rafId: 0,
      timerId: 0,
      lastRenderAt: 0
    };
  }

  function countCodeFencesFast(text) {
    let count = 0;
    let lineStart = 0;

    for (let i = 0; i <= text.length; i += 1) {
      if (i !== text.length && text.charCodeAt(i) !== 10) continue;
      if (i > lineStart) {
        const line = text.slice(lineStart, i);
        const trimmed = line.replace(/^[ \t]+/, '');
        if (trimmed.startsWith('```')) count += 1;
      }
      lineStart = i + 1;
    }

    return count;
  }

  function isInCodeBlockFast(text, pos) {
    const safePos = Math.max(0, Math.min(text.length, pos));
    return countCodeFencesFast(text.slice(0, safePos)) % 2 === 1;
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

  function isSafeBoundary(text, pos) {
    if (!Number.isInteger(pos) || pos <= 0 || pos > text.length) return false;
    if (isInCodeBlockFast(text, pos)) return false;
    const prefix = text.slice(0, pos);
    return areInlineMarkersBalanced(prefix) && areMathDelimitersBalanced(prefix);
  }

  function collectParagraphCandidates(text, minCandidate, maxCandidate, maxCandidates) {
    const positions = [];
    let searchFrom = Math.min(text.length, maxCandidate);

    while (positions.length < maxCandidates) {
      const idx = text.lastIndexOf('\n\n', searchFrom - 1);
      if (idx === -1) break;
      const candidate = idx + 2;
      if (candidate < minCandidate) break;
      if (candidate > maxCandidate) {
        searchFrom = idx;
        continue;
      }
      positions.push(candidate);
      searchFrom = idx;
    }

    return positions;
  }

  function collectLineCandidates(text, minCandidate, maxCandidate, maxCandidates) {
    const positions = [];
    let searchFrom = Math.min(text.length, maxCandidate);

    while (positions.length < maxCandidates) {
      const idx = text.lastIndexOf('\n', searchFrom - 1);
      if (idx === -1) break;
      const candidate = idx + 1;
      if (candidate < minCandidate) break;
      if (candidate > maxCandidate) {
        searchFrom = idx;
        continue;
      }
      positions.push(candidate);
      searchFrom = idx;
    }

    return positions;
  }

  function collectWhitespaceCandidates(text, minCandidate, maxCandidate, maxCandidates) {
    const positions = [];
    let i = Math.min(text.length - 1, maxCandidate - 1);

    while (i >= minCandidate && positions.length < maxCandidates) {
      if (!/\s/.test(text[i])) {
        i -= 1;
        continue;
      }
      const candidate = i + 1;
      if (candidate >= minCandidate) {
        positions.push(candidate);
      }
      while (i >= minCandidate && /\s/.test(text[i])) {
        i -= 1;
      }
    }

    return positions;
  }

  function dedupeDescending(values) {
    const seen = new Set();
    const result = [];
    values.forEach((value) => {
      if (!Number.isInteger(value) || seen.has(value)) return;
      seen.add(value);
      result.push(value);
    });
    return result.sort((a, b) => b - a);
  }

  function findFirstSafeBoundary(text, candidates, preferredMin) {
    let fallback = -1;
    for (const candidate of candidates) {
      if (!isSafeBoundary(text, candidate)) continue;
      if (candidate >= preferredMin) return candidate;
      if (fallback === -1) fallback = candidate;
    }
    return fallback;
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

  function canStreamPlainTextTail(text) {
    const value = String(text || '');
    if (!value) return true;
    if (isInCodeBlockFast(value, value.length)) return false;
    if (containsMarkdownBlockSyntax(value)) return false;
    if (containsMarkdownInlineSyntax(value)) return false;
    return true;
  }

  function findStreamingBoundary(content, lastCommittedLength) {
    const text = String(content || '');
    const lastCommitted = Math.max(0, Number(lastCommittedLength) || 0);
    const pending = text.length - lastCommitted;
    if (pending <= 0) return -1;

    const hardMinCandidate = lastCommitted + 1;
    const softMinCandidate = lastCommitted + MIN_COMMIT_CHUNK;
    const maxCandidate = text.length > 128
      ? Math.max(hardMinCandidate, text.length - 64)
      : Math.max(hardMinCandidate, text.length - 1);
    const preferredMin = Math.max(hardMinCandidate, text.length - preferredTailLength(text.length));

    const hardCandidates = dedupeDescending([
      ...collectParagraphCandidates(text, hardMinCandidate, maxCandidate, HARD_BOUNDARY_ATTEMPTS),
      ...collectLineCandidates(text, hardMinCandidate, maxCandidate, LINE_BOUNDARY_ATTEMPTS)
    ]);
    const hardBoundary = findFirstSafeBoundary(text, hardCandidates, preferredMin);
    if (hardBoundary !== -1) return hardBoundary;

    if (pending < MAX_PENDING_TAIL || softMinCandidate > maxCandidate) return -1;

    const softCandidates = collectWhitespaceCandidates(text, softMinCandidate, maxCandidate, SOFT_BOUNDARY_ATTEMPTS);
    return findFirstSafeBoundary(text, softCandidates, preferredMin);
  }

  return {
    MIN_COMMIT_CHUNK,
    MAX_PENDING_TAIL,
    createStreamingState,
    nextStreamingRenderDelay,
    countCodeFencesFast,
    isInCodeBlockFast,
    areInlineMarkersBalanced,
    areMathDelimitersBalanced,
    canStreamPlainTextTail,
    findStreamingBoundary
  };
});
