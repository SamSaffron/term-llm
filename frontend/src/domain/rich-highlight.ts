import highlight from 'highlight.js/lib/core';
import javascript from 'highlight.js/lib/languages/javascript';
import typescript from 'highlight.js/lib/languages/typescript';
import json from 'highlight.js/lib/languages/json';
import bash from 'highlight.js/lib/languages/bash';
import go from 'highlight.js/lib/languages/go';
import 'highlight.js/styles/github-dark.css';

highlight.registerLanguage('javascript', javascript);
highlight.registerLanguage('js', javascript);
highlight.registerLanguage('typescript', typescript);
highlight.registerLanguage('ts', typescript);
highlight.registerLanguage('json', json);
highlight.registerLanguage('bash', bash);
highlight.registerLanguage('sh', bash);
highlight.registerLanguage('go', go);
export { highlight };
