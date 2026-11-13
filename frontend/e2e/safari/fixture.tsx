import { render } from 'preact';
import { StoreContext } from '../../src/app/context';
import { Composer } from '../../src/components/Composer';
import { DiffSidebar } from '../../src/components/Panels';
import { ToggleSwitch } from '../../src/components/ToggleSwitch';
import { copyText } from '../../src/platform/clipboard';
import { AppStore } from '../../src/stores/app-store';
import '../../src/styles/app.css';

// This entry is never part of the shipped application. Bundle actual components
// and CSS, replacing only server/provider boundaries, not browser API behavior.
const store = new AppStore({
  prefix: '',
  version: 'safari-smoke',
  sidebarCategories: ['all'],
  agentName: '',
  agentNames: [],
  title: '',
  locationSharing: false,
  worktrees: false,
  hub: null,
  vapidKey: '',
});
store.sessions.value = [
  {
    id: 'smoke',
    title: 'Smoke',
    name: '',
    mode: 'chat',
    origin: 'web',
    archived: false,
    pinned: false,
    created: 1,
    lastMessageAt: 1,
    messages: [],
  },
];
store.activeSessionId.value = 'smoke';
store.draftActive.value = false;
store.diff.value = {
  ...store.diff.peek(),
  open: true,
  sessionId: 'smoke',
  files: [{ path: 'smoke.txt', status: 'modify', additions: 1, deletions: 1 }],
};
const audit = { sends: 0, errors: [] as string[], direct: '', requests: 0 };
Object.assign(window, { safariSmoke: audit });
store.toast = (error) => {
  audit.errors.push(String(error));
};
store.send = async () => {
  audit.sends++;
  store.prompt.value = '';
};
store.endpoints.diffComments = async () => ({ comments: [], transcript_rev: 0 });
store.endpoints.fileDiff = async () => {
  audit.requests++;
  const response = await fetch('/diff?ms=6000');
  if (!response.ok) throw new Error(`Diff HTTP ${response.status}`);
  return response.json();
};

render(
  <StoreContext.Provider value={store}>
    <main style="padding:24px;max-width:700px">
      <h1>Safari production component smoke</h1>
      <p>Trusted clicks overwrite the clipboard with known fixture text.</p>
      <button
        id="direct"
        onClick={() => {
          void copyText('safari-smoke-sentinel').then(
            () => {
              audit.direct = 'ok';
            },
            (error: unknown) => {
              audit.direct = String(error);
            },
          );
        }}
      >
        Direct copy control
      </button>
      <p>
        <ToggleSwitch aria-label="Smoke toggle" />
      </p>
      <section aria-label="Diff fixture" style="height:240px;display:flex;flex-direction:column">
        <DiffSidebar />
      </section>
      <section aria-label="Composer fixture">
        <Composer />
      </section>
    </main>
  </StoreContext.Provider>,
  document.getElementById('root')!,
);
