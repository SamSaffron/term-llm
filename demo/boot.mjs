import { createDemoAPI } from './mock-api.mjs';

// Keep ordinary frontend drafts/preferences ephemeral too. No service worker.
const storage = () => {
  const values = new Map();
  return { get length() { return values.size; }, key: index => [...values.keys()][index] ?? null,
    getItem: key => values.get(String(key)) ?? null, setItem: (key, value) => values.set(String(key), String(value)),
    removeItem: key => values.delete(String(key)), clear: () => values.clear() };
};
Object.defineProperty(window, 'localStorage', { configurable: true, value: storage() });
Object.defineProperty(window, 'sessionStorage', { configurable: true, value: storage() });
Object.assign(window, { TERM_LLM_UI_PREFIX: '/demo-api', TERM_LLM_SHELL_AUTHORIZED: true,
  TERM_LLM_UI_TITLE: 'term-llm · UI demo', TERM_LLM_CACHE_SCOPE: '',
  TERM_LLM_WORKTREES_ENABLED: false, TERM_LLM_LOCATION_SHARING_ENABLED: false, TERM_LLM_PUSH_SUPPORTED: false });
const api = createDemoAPI();
const entry = new URL(location.href);
entry.searchParams.delete('draft');
entry.searchParams.set('session', api.initialSession);
history.replaceState(null, '', entry);
for (const method of ['pushState', 'replaceState']) {
  const original = history[method].bind(history);
  history[method] = (state, title, target) => {
    if (target != null) {
      const url = new URL(target, location.href);
      if (url.origin === entry.origin && (url.pathname === '/demo-api' || url.pathname.startsWith('/demo-api/'))) {
        url.pathname = entry.pathname;
        target = url.href;
      }
    }
    return original(state, title, target);
  };
}
const nativeFetch = window.fetch.bind(window);
window.fetch = (input, init) => {
  const url = new URL(input instanceof Request ? input.url : input, location.href);
  if (url.origin !== entry.origin || !(url.pathname === '/demo-api' || url.pathname.startsWith('/demo-api/'))) return nativeFetch(input, init);
  return api.fetch(new Request(input instanceof Request ? input : url, init));
};
await import(window.TERM_LLM_DEMO_ENTRY);
