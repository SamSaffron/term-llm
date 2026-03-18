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

const hardBoundaryInput = 'First paragraph.\n\nSecond paragraph starts here.';
expectEqual(
  'findStreamingBoundary uses paragraph break',
  streaming.findStreamingBoundary(hardBoundaryInput, 0),
  'First paragraph.\n\n'.length
);

const codeFenceInput = 'Before code.\n\n```\ncode block\n\nstill code\n';
expectEqual(
  'findStreamingBoundary avoids unclosed code fences',
  streaming.findStreamingBoundary(codeFenceInput, 0),
  'Before code.\n\n'.length
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

const longParagraph = 'alpha '.repeat(400);
const longBoundary = streaming.findStreamingBoundary(longParagraph, 0);
expectTrue(
  'findStreamingBoundary produces a soft boundary for long paragraphs',
  Number.isInteger(longBoundary) && longBoundary > 0 && longBoundary < longParagraph.length
);

const longSnakeCaseParagraph = 'term_llm foo_bar baz_qux '.repeat(160);
const snakeBoundary = streaming.findStreamingBoundary(longSnakeCaseParagraph, 0);
expectTrue(
  'findStreamingBoundary still finds boundaries in snake_case-heavy text',
  Number.isInteger(snakeBoundary) && snakeBoundary > 0 && snakeBoundary < longSnakeCaseParagraph.length
);

const longList = '* item one\n* item two\n* item three\n'.repeat(90);
const listBoundary = streaming.findStreamingBoundary(longList, 0);
expectTrue(
  'findStreamingBoundary still finds boundaries in list-heavy text',
  Number.isInteger(listBoundary) && listBoundary > 0 && listBoundary < longList.length
);

expectEqual('nextStreamingRenderDelay small', streaming.nextStreamingRenderDelay(4000), 33);
expectEqual('nextStreamingRenderDelay medium', streaming.nextStreamingRenderDelay(9000), 75);
expectEqual('nextStreamingRenderDelay large', streaming.nextStreamingRenderDelay(40000), 150);
expectEqual('nextStreamingRenderDelay huge', streaming.nextStreamingRenderDelay(200000), 250);

if (failures > 0) {
  console.error('\n' + failures + ' test(s) failed');
  process.exit(1);
} else {
  console.log('\nAll tests passed');
  process.exit(0);
}
