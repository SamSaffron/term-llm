import highlight from 'highlight.js/lib/core';
import javascript from 'highlight.js/lib/languages/javascript';
import typescript from 'highlight.js/lib/languages/typescript';
import json from 'highlight.js/lib/languages/json';
import bash from 'highlight.js/lib/languages/bash';
import go from 'highlight.js/lib/languages/go';
import plaintext from 'highlight.js/lib/languages/plaintext';
import xml from 'highlight.js/lib/languages/xml';
import python from 'highlight.js/lib/languages/python';
import ruby from 'highlight.js/lib/languages/ruby';
import rust from 'highlight.js/lib/languages/rust';
import yaml from 'highlight.js/lib/languages/yaml';
import markdown from 'highlight.js/lib/languages/markdown';
import cpp from 'highlight.js/lib/languages/cpp';
import c from 'highlight.js/lib/languages/c';
import csharp from 'highlight.js/lib/languages/csharp';

highlight.registerLanguage('javascript', javascript);
highlight.registerLanguage('js', javascript);
highlight.registerLanguage('typescript', typescript);
highlight.registerLanguage('ts', typescript);
highlight.registerLanguage('json', json);
highlight.registerLanguage('bash', bash);
highlight.registerLanguage('sh', bash);
highlight.registerLanguage('go', go);
highlight.registerLanguage('plaintext', plaintext);
highlight.registerLanguage('text', plaintext);
highlight.registerLanguage('txt', plaintext);
highlight.registerLanguage('gitattributes', plaintext);
highlight.registerLanguage('xml', xml);
highlight.registerLanguage('python', python);
highlight.registerLanguage('py', python);
highlight.registerLanguage('ruby', ruby);
highlight.registerLanguage('rb', ruby);
highlight.registerLanguage('rust', rust);
highlight.registerLanguage('rs', rust);
highlight.registerLanguage('yaml', yaml);
highlight.registerLanguage('yml', yaml);
highlight.registerLanguage('markdown', markdown);
highlight.registerLanguage('md', markdown);
highlight.registerLanguage('cpp', cpp);
highlight.registerLanguage('c', c);
highlight.registerLanguage('csharp', csharp);
highlight.registerLanguage('cs', csharp);

const DIFF_LANGUAGE_ALIASES: Record<string, string> = {
  jsx: 'javascript',
  mjs: 'javascript',
  cjs: 'javascript',
  tsx: 'typescript',
  zsh: 'bash',
  cc: 'cpp',
  cxx: 'cpp',
  hpp: 'cpp',
  h: 'c',
};
export function highlightDiffLine(source: string, language: string): string {
  const name = DIFF_LANGUAGE_ALIASES[language] || language;
  if (!source || !highlight.getLanguage(name)) return '';
  try {
    return highlight.highlight(source, { language: name, ignoreIllegals: true }).value;
  } catch {
    return '';
  }
}
export { highlight };
