#!/usr/bin/env node
'use strict';

const path = require('path');
const dir = __dirname;

const streaming = require(path.join(dir, 'markdown-streaming.js'));

let failures = 0;

function fail(name, message, details) {
  console.error('FAIL:', name, '-', message);
  if (details) {
    console.error('      ', details);
  }
  failures++;
}

function expectEqual(name, actual, expected) {
  if (actual !== expected) {
    fail(name, `expected ${expected}, got ${actual}`);
  } else {
    console.log('PASS:', name);
  }
}

function expectTrue(name, actual) {
  if (!actual) {
    fail(name, 'expected true, got false');
  } else {
    console.log('PASS:', name);
  }
}

function expectFalse(name, actual) {
  if (actual) {
    fail(name, 'expected false, got true');
  } else {
    console.log('PASS:', name);
  }
}

expectTrue(
  'createStreamingState starts with stable and tail streaming containers unset',
  (() => {
    const state = streaming.createStreamingState();
    return state && state.stableContainer === null && state.stableLength === 0 && state.tailContainer === null;
  })()
);

expectFalse(
  'areMathDelimitersBalanced detects open inline math',
  streaming.areMathDelimitersBalanced('Value: \\(x + y')
);
expectTrue(
  'areMathDelimitersBalanced accepts closed inline math',
  streaming.areMathDelimitersBalanced('Value: \\(x + y\\)')
);
expectTrue(
  'areInlineMarkersBalanced ignores snake_case words',
  streaming.areInlineMarkersBalanced('term_llm keeps foo_bar intact')
);
expectTrue(
  'areInlineMarkersBalanced ignores file paths with underscores',
  streaming.areInlineMarkersBalanced('/tmp/testing/term_llm_config/file_name.go')
);
expectTrue(
  'areInlineMarkersBalanced ignores list item markers',
  streaming.areInlineMarkersBalanced('* item one\n* item two\n')
);

expectEqual('nextStreamingRenderDelay small', streaming.nextStreamingRenderDelay(4000), 33);
expectEqual('nextStreamingRenderDelay medium', streaming.nextStreamingRenderDelay(9000), 75);
expectEqual('nextStreamingRenderDelay large', streaming.nextStreamingRenderDelay(40000), 150);
expectEqual('nextStreamingRenderDelay huge', streaming.nextStreamingRenderDelay(200000), 250);

expectTrue(
  'canStreamPlainTextTail accepts ordinary prose',
  streaming.canStreamPlainTextTail('This is just a steadily growing paragraph\nwith another plain line.')
);
expectTrue(
  'canStreamPlainTextTail accepts snake_case prose',
  streaming.canStreamPlainTextTail('term_llm keeps file_name.go untouched while streaming prose.')
);
expectFalse(
  'canStreamPlainTextTail rejects emphasis markers',
  streaming.canStreamPlainTextTail('This has *emphasis* in it.')
);
expectFalse(
  'canStreamPlainTextTail rejects markdown links',
  streaming.canStreamPlainTextTail('See [docs](https://example.com) for details.')
);
expectFalse(
  'canStreamPlainTextTail rejects list blocks',
  streaming.canStreamPlainTextTail('- first item\n- second item')
);
expectFalse(
  'canStreamPlainTextTail rejects fenced code blocks',
  streaming.canStreamPlainTextTail('```js\nconsole.log(1);\n```')
);
expectFalse(
  'canStreamPlainTextTail rejects math delimiters',
  streaming.canStreamPlainTextTail('Value: \\(x + y\\)')
);

expectTrue(
  'canStreamPlainTextTailIncremental reuses a safe growing plain prefix',
  (() => {
    const state = streaming.createStreamingState();
    if (!streaming.canStreamPlainTextTailIncremental(state, 'plain words')) return false;
    if (state.plainTextScanSource !== 'plain words') return false;
    if (!streaming.canStreamPlainTextTailIncremental(state, 'plain words keep growing')) return false;
    return state.plainTextScanSource === 'plain words keep growing' && state.plainTextEligible === true;
  })()
);
expectFalse(
  'canStreamPlainTextTailIncremental falls back when punctuation completes a link',
  (() => {
    const state = streaming.createStreamingState();
    if (!streaming.canStreamPlainTextTailIncremental(state, 'See [docs]')) return true;
    return streaming.canStreamPlainTextTailIncremental(state, 'See [docs](https://example.com)');
  })()
);
expectFalse(
  'canStreamPlainTextTailIncremental keeps a growing markdown tail in markdown mode',
  (() => {
    const state = streaming.createStreamingState();
    if (streaming.canStreamPlainTextTailIncremental(state, 'This has *emphasis*')) return true;
    return streaming.canStreamPlainTextTailIncremental(state, 'This has *emphasis* and more plain words');
  })()
);

expectEqual(
  'findStableMarkdownBoundary finds blank-line paragraph break',
  streaming.findStableMarkdownBoundary('First paragraph.\n\nSecond paragraph still streaming', 10),
  'First paragraph.\n\n'.length
);
expectEqual(
  'findStableMarkdownBoundary keeps configured tail length',
  streaming.findStableMarkdownBoundary('First paragraph.\n\nshort', 100),
  0
);
expectEqual(
  'findStableMarkdownBoundary avoids unclosed code fence',
  streaming.findStableMarkdownBoundary('```js\nconst value = 1;\n\nmore code still in fence', 5),
  0
);
expectEqual(
  'findStableMarkdownBoundary avoids unbalanced bold marker',
  streaming.findStableMarkdownBoundary('This starts **bold\n\nrest of response', 5),
  0
);
expectEqual(
  'findStableMarkdownBoundary avoids unbalanced italic marker',
  streaming.findStableMarkdownBoundary('This starts *italic\n\nrest of response', 5),
  0
);
expectEqual(
  'findStableMarkdownBoundary avoids unbalanced underscore marker',
  streaming.findStableMarkdownBoundary('This starts _italic\n\nrest of response', 5),
  0
);
expectEqual(
  'findStableMarkdownBoundary avoids unbalanced strikethrough marker',
  streaming.findStableMarkdownBoundary('This starts ~~strike\n\nrest of response', 5),
  0
);
expectEqual(
  'findStableMarkdownBoundary avoids unclosed inline code marker',
  streaming.findStableMarkdownBoundary('This starts `code\n\nrest of response', 5),
  0
);
expectEqual(
  'findStableMarkdownBoundary avoids unbalanced inline math',
  streaming.findStableMarkdownBoundary('Value: \\(x + y\n\nrest of response', 5),
  0
);
expectEqual(
  'findStableMarkdownBoundary avoids unbalanced display math',
  streaming.findStableMarkdownBoundary('Value:\n$$\nx + y\n\nrest of response', 5),
  0
);
expectEqual(
  'findStableMarkdownBoundary stabilizes completed lists',
  streaming.findStableMarkdownBoundary('- one\n- two\n\nrest of response', 5),
  '- one\n- two\n\n'.length
);
expectEqual(
  'findStableMarkdownBoundary does not split a loose list before another item',
  streaming.findStableMarkdownBoundary('- one\n\n- two continues the same list with enough text', 5),
  0
);
expectEqual(
  'findStableMarkdownBoundary preserves indentation when checking loose-list continuation',
  streaming.findStableMarkdownBoundary('- one\n\n  continued paragraph in the same loose list item with enough text', 5),
  0
);
expectEqual(
  'findStableMarkdownBoundary preserves indentation when checking list-item code',
  streaming.findStableMarkdownBoundary('- one\n\n    indented code in the same list item with enough text', 5),
  0
);
expectEqual(
  'findStableMarkdownBoundary stabilizes completed tables',
  streaming.findStableMarkdownBoundary('| a | b |\n| - | - |\n\nrest of response', 5),
  '| a | b |\n| - | - |\n\n'.length
);
expectEqual(
  'findStableMarkdownBoundary stabilizes GFM tables without outer pipes',
  streaming.findStableMarkdownBoundary('a | b\n--- | ---\n1 | 2\n\nrest of response', 5),
  'a | b\n--- | ---\n1 | 2\n\n'.length
);
expectEqual(
  'findStableMarkdownBoundary stabilizes closed tilde fences with markdown-like code',
  streaming.findStableMarkdownBoundary('~~~txt\n* literal marker\n~~~\n\nrest of response', 5),
  '~~~txt\n* literal marker\n~~~\n\n'.length
);

expectEqual(
  'findStableMarkdownBoundary rejects fence-like lines with trailing info as closers',
  streaming.findStableMarkdownBoundary('```txt\ncode\n```not-a-close\n\nrest of response', 5),
  0
);

const bounded = streaming.analyzeStableMarkdownBoundary(
  '```txt\n' + 'unfinished * fenced text\n'.repeat(5000),
  256
);
expectTrue('stable-boundary analysis reports oversized incomplete tails', bounded.overBudget);
expectEqual('oversized stable-boundary analysis does not scan past its budget', bounded.operations, 0);
expectTrue(
  'stable-boundary analysis exposes a fixed mutable markdown limit',
  Number(streaming.MAX_MUTABLE_MARKDOWN_CHARS) > 0
    && bounded.mutableTailLength > streaming.MAX_MUTABLE_MARKDOWN_CHARS
);

const operationLimited = streaming.analyzeStableMarkdownBoundary(
  'Paragraph one.\n\n' + 'tail '.repeat(100),
  10,
  { maxOperations: 32 }
);
expectTrue('stable-boundary analysis falls back on deterministic operation budget', operationLimited.overBudget);
expectTrue('operation-budget fallback stays within the declared budget', operationLimited.operations <= 32);

if (failures > 0) {
  console.error('\n' + failures + ' test(s) failed');
  process.exit(1);
} else {
  console.log('\nAll tests passed');
  process.exit(0);
}
