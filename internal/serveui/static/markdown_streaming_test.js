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
  'createStreamingState starts with only tail streaming state',
  (() => {
    const state = streaming.createStreamingState();
    return state && state.stableContainer === undefined && state.committedLength === undefined && state.tailContainer === null;
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

if (failures > 0) {
  console.error('\n' + failures + ' test(s) failed');
  process.exit(1);
} else {
  console.log('\nAll tests passed');
  process.exit(0);
}
