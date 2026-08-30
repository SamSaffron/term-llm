import { useEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { APIError } from '../api/client';
import type { WorktreeRecoveryOffer } from '../domain/types';
import { Icon } from './Icon';
import { Overlay } from './Overlay';

type WorktreeRow = Record<string, unknown>;
type BusyAction = '' | 'diff' | 'merge' | 'recover' | 'promote' | 'remove' | 'create' | 'switch';
type RemoveStage = 'idle' | 'armed' | 'force';

const rowDir = (row: WorktreeRow | null): string => String(row?.dir || row?.path || '');
const rowName = (row: WorktreeRow): string =>
  String(row.name || row.branch || (row.root ? 'root checkout' : 'worktree'));
const isRoot = (row: WorktreeRow): boolean => row.root === true;
const cleanupRemoved = (value: unknown): boolean =>
  Boolean(
    value &&
    typeof value === 'object' &&
    (value as Record<string, unknown>).cleanup &&
    typeof (value as Record<string, unknown>).cleanup === 'object' &&
    ((value as Record<string, unknown>).cleanup as Record<string, unknown>).removed === true,
  );
const usageNames = (entries: unknown[]): string[] =>
  entries
    .map((entry) => {
      if (!entry || typeof entry !== 'object') return '';
      const value = entry as Record<string, unknown>;
      return String(
        value.title || value.name || (value.number ? `#${value.number}` : value.id || ''),
      );
    })
    .filter(Boolean);

const timestamp = (value: unknown): number => {
  if (!value) return 0;
  const parsed = new Date(String(value)).getTime();
  return Number.isFinite(parsed) ? parsed : 0;
};

const relativeActivity = (value: number): string => {
  const difference = Math.max(0, Date.now() - value);
  if (difference < 45_000) return 'just now';
  if (difference < 3_600_000) return `${Math.max(1, Math.floor(difference / 60_000))}m ago`;
  if (difference < 86_400_000) return `${Math.max(1, Math.floor(difference / 3_600_000))}h ago`;
  if (difference < 604_800_000) return `${Math.max(1, Math.floor(difference / 86_400_000))}d ago`;
  return new Date(value).toLocaleDateString(undefined, { month: 'short', day: 'numeric' });
};

function recoveryOffer(error: unknown): WorktreeRecoveryOffer | null {
  if (!(error instanceof APIError) || error.status !== 409 || !error.body) return null;
  try {
    const value = (JSON.parse(error.body) as Record<string, unknown>).recovery;
    if (!value || typeof value !== 'object') return null;
    const offer = value as WorktreeRecoveryOffer;
    if (
      offer.kind !== 'conflict' ||
      !offer.title ||
      !offer.question ||
      !offer.yes_label ||
      !offer.no_label
    )
      return null;
    return offer;
  } catch {
    return null;
  }
}

function errorDetails(error: unknown): { message: string; inUse: string[] } {
  let message = error instanceof Error ? error.message : String(error || 'Worktree action failed.');
  let inUse: string[] = [];
  if (error instanceof APIError && error.body) {
    try {
      const body = JSON.parse(error.body) as Record<string, unknown>;
      if (typeof body.message === 'string' && body.message.trim()) message = body.message;
      const entries = Array.isArray(body.in_use) ? body.in_use : [];
      inUse = usageNames(entries);
    } catch {
      // The API error's message is the best fallback for a non-JSON response.
    }
  }
  return { message, inUse };
}

function WorktreeBadges({
  row,
  current,
  details = false,
}: {
  row: WorktreeRow;
  current: boolean;
  details?: boolean;
}) {
  const dirty = Number(row.dirty_files || 0);
  const inUse = Array.isArray(row.in_use) ? row.in_use : [];
  const inUseNames = usageNames(inUse);
  return (
    <span class="worktree-badges">
      {current && <span class="worktree-badge current">Current</span>}
      {details && dirty > 0 && (
        <span
          class="worktree-badge dirty"
          aria-label={`${dirty} changed ${dirty === 1 ? 'file' : 'files'}`}
        >
          {dirty} changed
        </span>
      )}
      {inUse.length > 0 && (
        <span
          class="worktree-badge in-use"
          title={inUseNames.length ? `Used by ${inUseNames.join(', ')}` : undefined}
          aria-label={`In use by ${inUse.length} ${inUse.length === 1 ? 'conversation' : 'conversations'}`}
        >
          In use{inUse.length > 1 ? ` · ${inUse.length}` : ''}
        </span>
      )}
    </span>
  );
}

function WorktreeOption({
  row,
  draft,
  selected,
  current,
  first,
  actionDisabled,
  onChoose,
  onDetails,
}: {
  row: WorktreeRow;
  draft: boolean;
  selected: boolean;
  current: boolean;
  first: boolean;
  actionDisabled?: boolean;
  onChoose: () => void;
  onDetails?: () => void;
}) {
  const root = isRoot(row);
  const dir = rowDir(row);
  const inUse = Array.isArray(row.in_use) ? row.in_use : [];
  const conversations = usageNames(inUse);
  const latestConversationActivity = inUse.reduce((latest, entry) => {
    if (!entry || typeof entry !== 'object') return latest;
    return Math.max(latest, timestamp((entry as Record<string, unknown>).updated_at));
  }, 0);
  const lastBound = timestamp(row.last_bound_at);
  const created = timestamp(row.created_at);
  const activity = latestConversationActivity || lastBound || created;
  const activityLabel = activity
    ? `${latestConversationActivity ? 'Active' : lastBound ? 'Last used' : 'Created'} ${relativeActivity(activity)}`
    : '';
  const dirty = Number(row.dirty_files || 0);
  const mainAhead = Number(row.main_ahead || 0);
  const mainBehind = Number(row.main_behind || 0);
  const mainLabel = String(row.main_branch || 'main checkout');
  const state: string[] = [dirty ? `${dirty} changed ${dirty === 1 ? 'file' : 'files'}` : 'Clean'];
  if (!root && row.main_divergence_available === true) {
    if (mainBehind)
      state.push(`${mainBehind} ${mainBehind === 1 ? 'commit' : 'commits'} behind ${mainLabel}`);
    if (mainAhead) state.push(`${mainAhead} ahead`);
    if (!mainAhead && !mainBehind) state.push(`Up to date with ${mainLabel}`);
  }
  const disabled = !draft && root && (current || Boolean(actionDisabled));
  const move = (event: preact.JSX.TargetedKeyboardEvent<HTMLButtonElement>) => {
    if (!draft || !['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
    const options = [
      ...(event.currentTarget
        .closest('.worktree-list')
        ?.querySelectorAll<HTMLButtonElement>('.worktree-option') || []),
    ];
    const index = options.indexOf(event.currentTarget);
    if (index < 0 || !options.length) return;
    event.preventDefault();
    const next =
      event.key === 'Home'
        ? 0
        : event.key === 'End'
          ? options.length - 1
          : (index + (event.key === 'ArrowDown' ? 1 : -1) + options.length) % options.length;
    options[next]?.focus();
  };
  return (
    <div class={`worktree-row ${selected ? 'is-selected' : ''} ${current ? 'is-current' : ''}`}>
      <button
        type="button"
        class="worktree-option"
        role={draft ? 'radio' : undefined}
        aria-checked={draft ? selected : undefined}
        tabIndex={draft ? (selected || (!selected && first) ? 0 : -1) : undefined}
        disabled={disabled}
        onKeyDown={move}
        onClick={onChoose}
        title={
          disabled
            ? current
              ? 'This conversation already uses this checkout.'
              : 'Finish the current response before switching worktrees.'
            : dir || undefined
        }
      >
        <span class="worktree-option-icon" aria-hidden="true">
          {draft ? <span class="worktree-radio" /> : <Icon name="branch" />}
        </span>
        <span class="worktree-option-content">
          <span class="worktree-option-name-row">
            <strong class="worktree-option-name">{root ? 'root checkout' : rowName(row)}</strong>
            <WorktreeBadges row={row} current={current} />
          </span>
          {conversations.length > 0 && (
            <span class="worktree-option-conversation" title={conversations.join(', ')}>
              {conversations.length === 1
                ? conversations[0]
                : `${conversations[0]} + ${conversations.length - 1} more`}
            </span>
          )}
          <span
            class="worktree-option-summary"
            aria-label={dirty ? `${dirty} changed ${dirty === 1 ? 'file' : 'files'}` : undefined}
          >
            {state.join(' · ')}
            {activityLabel ? ` · ${activityLabel}` : ''}
          </span>
        </span>
      </button>
      {onDetails && (
        <button
          class="worktree-option-details"
          type="button"
          aria-label={`Manage ${rowName(row)}`}
          onClick={onDetails}
        >
          <Icon name="chevron-right" />
        </button>
      )}
    </div>
  );
}

export function Worktrees() {
  const store = useStore();
  const [name, setName] = useState('');
  const [clean, setClean] = useState(true);
  const [selected, setSelected] = useState<WorktreeRow | null>(null);
  const [branch, setBranch] = useState('');
  const [status, setStatus] = useState('');
  const [error, setError] = useState('');
  const [busy, setBusy] = useState<BusyAction>('');
  const [recovery, setRecovery] = useState<WorktreeRecoveryOffer | null>(null);
  const [removeStage, setRemoveStage] = useState<RemoveStage>('idle');
  const autoOpened = useRef(false);
  const detailTitle = useRef<HTMLHeadingElement>(null);
  const recoveryConfirm = useRef<HTMLButtonElement>(null);
  const draft = store.draftActive.value;
  const activeSession = store.activeSession.value;
  const activeDir = store.currentWorktreeDir.value;
  const apiRows = store.worktrees.value;
  const serverRoot = apiRows.find(isRoot);
  const project = store.projects.value.find(
    (entry) => entry.id === (activeSession?.projectId || store.activeProjectId.value),
  );
  const root: WorktreeRow =
    serverRoot ||
    ({
      root: true,
      name: 'root',
      dir: String(apiRows[0]?.repo_root || project?.path || ''),
    } as WorktreeRow);
  const rows = [root, ...apiRows.filter((row) => !isRoot(row))];
  const managedCount = rows.length - 1;
  const rootDirtyFiles = Number(root.dirty_files || 0);
  const dir = rowDir(selected);
  const streaming = store.streaming.value;

  useEffect(() => {
    if (draft || autoOpened.current || selected || !activeDir) return;
    const current = apiRows.find((row) => !isRoot(row) && rowDir(row) === activeDir);
    if (current) {
      autoOpened.current = true;
      setSelected(current);
    }
  }, [activeDir, apiRows, draft, selected]);

  useEffect(() => {
    if (!selected) return;
    const focusFrame = requestAnimationFrame(() => detailTitle.current?.focus());
    return () => cancelAnimationFrame(focusFrame);
  }, [selected]);

  useEffect(() => {
    if (!recovery?.available || draft) return;
    const focusFrame = requestAnimationFrame(() => recoveryConfirm.current?.focus());
    return () => cancelAnimationFrame(focusFrame);
  }, [draft, recovery]);

  const openDetail = (row: WorktreeRow) => {
    setSelected(row);
    setBranch('');
    setStatus('');
    setError('');
    setRecovery(null);
    setRemoveStage('idle');
  };
  const backToList = (nextStatus = '') => {
    setSelected(null);
    setBranch('');
    setStatus(nextStatus);
    setError('');
    setRecovery(null);
    setRemoveStage('idle');
  };
  const run = async (action: BusyAction, task: () => Promise<void>) => {
    setBusy(action);
    setError('');
    try {
      await task();
    } catch (value) {
      setError(errorDetails(value).message);
    } finally {
      setBusy('');
    }
  };
  const close = () => (store.modal.value = '');
  const projectName = activeSession?.projectName || project?.name || 'Git project';

  return (
    <Overlay title="Worktrees" className="worktree-modal" onEscape={selected ? backToList : close}>
      <div class="worktree-intro">
        <p>
          {draft
            ? 'Choose an isolated checkout for this conversation.'
            : 'Inspect and manage the checkouts for this project.'}
        </p>
        <span class="worktree-scope-pill" title={project?.path}>
          {projectName}
        </span>
      </div>

      {!draft && (
        <div class="worktree-context">
          <Icon name="branch" />
          <span>
            {activeDir
              ? `This conversation runs in ${selected && dir === activeDir ? rowName(selected) : activeDir.split('/').pop()}.`
              : 'This conversation runs in the root checkout.'}
          </span>
          <span class="worktree-badge current">Current</span>
        </div>
      )}

      {store.worktreeError.value && !selected && (
        <div class="worktree-error-panel" role="alert">
          <div>
            <strong>Couldn’t load worktrees</strong>
            <span>{store.worktreeError.value}</span>
          </div>
          <button class="btn" type="button" onClick={() => void store.loadWorktrees()}>
            Retry
          </button>
        </div>
      )}
      {error && !selected && (
        <div class="worktree-error-panel" role="alert">
          <div>
            <strong>Worktree operation failed</strong>
            <span>{error}</span>
          </div>
        </div>
      )}

      <div class="worktree-body">
        {selected ? (
          <section class="worktree-detail" aria-labelledby="worktreeDetailTitle">
            <button class="worktree-back" type="button" onClick={() => backToList()}>
              <Icon name="arrow-left" />
              All worktrees
            </button>
            <div class="worktree-detail-heading">
              <div>
                <h3 id="worktreeDetailTitle" ref={detailTitle} tabIndex={-1}>
                  {rowName(selected)}
                </h3>
                <span class="worktree-detail-ref">
                  {String(
                    selected.branch || (selected.detached ? 'Detached HEAD' : 'Managed worktree'),
                  )}
                  {selected.head_sha ? ` · ${String(selected.head_sha).slice(0, 8)}` : ''}
                </span>
              </div>
              <WorktreeBadges row={selected} current={dir === activeDir} details />
            </div>
            <code class="worktree-detail-path" title={dir}>
              {dir}
            </code>

            <div class="worktree-primary-actions">
              {dir !== activeDir ? (
                <button
                  class="btn primary worktree-use"
                  type="button"
                  disabled={Boolean(busy) || streaming}
                  onClick={() => {
                    if (draft) {
                      store.chooseDraftWorktree(dir);
                      return;
                    }
                    void run('switch', async () => {
                      autoOpened.current = true;
                      await store.switchWorktree(dir);
                      backToList();
                    });
                  }}
                >
                  <Icon name="branch" />
                  {busy === 'switch'
                    ? 'Switching…'
                    : draft
                      ? 'Run this conversation here'
                      : 'Switch conversation here'}
                </button>
              ) : (
                <button class="btn primary worktree-use" type="button" disabled>
                  <Icon name="branch" />
                  {draft ? 'Selected for this conversation' : 'Current worktree'}
                </button>
              )}
              <button
                class="btn"
                type="button"
                disabled={Boolean(busy)}
                onClick={() =>
                  void run('diff', async () => {
                    await store.openWorktreeDiff(dir, rowName(selected));
                    close();
                  })
                }
              >
                <Icon name="diff" />
                {busy === 'diff' ? 'Loading…' : 'Show changes'}
              </button>
              <button
                class="btn primary"
                type="button"
                disabled={Boolean(busy) || streaming}
                onClick={() =>
                  void run('merge', async () => {
                    try {
                      // A merge is an explicit request to update the root checkout. Do not
                      // make active conversations an additional confirmation gate.
                      const result = await store.mergeWorktree(dir, true);
                      if (cleanupRemoved(result)) backToList();
                      else setStatus('Merged into root; the old checkout is still in use.');
                    } catch (value) {
                      const offer = recoveryOffer(value);
                      if (offer) {
                        setRecovery(offer);
                        setStatus('');
                        return;
                      }
                      throw value;
                    }
                  })
                }
              >
                {busy === 'merge' ? 'Merging…' : 'Merge into root'}
              </button>
            </div>

            {recovery && (
              <div class="worktree-recovery" role="group" aria-labelledby="worktreeRecoveryTitle">
                <h4 id="worktreeRecoveryTitle">{recovery.title}</h4>
                <span>{recovery.question}</span>
                {recovery.details && <pre>{recovery.details}</pre>}
                {(!recovery.available || draft) && (
                  <span class="worktree-recovery-unavailable">
                    {draft
                      ? 'Start the conversation before using assisted recovery.'
                      : recovery.unavailable_reason}
                  </span>
                )}
                <div class="worktree-recovery-actions">
                  <button
                    ref={recoveryConfirm}
                    class="btn primary"
                    type="button"
                    disabled={Boolean(busy) || streaming || !recovery.available || draft}
                    onClick={() =>
                      void run('recover', async () => {
                        await store.recoverWorktree(dir);
                      })
                    }
                  >
                    {busy === 'recover' ? 'Starting recovery…' : recovery.yes_label}
                  </button>
                  <button
                    class="btn"
                    type="button"
                    disabled={Boolean(busy)}
                    onClick={() => {
                      setRecovery(null);
                      setStatus(
                        recovery.decline_message || 'Recovery declined; nothing was changed.',
                      );
                    }}
                  >
                    {recovery.no_label}
                  </button>
                </div>
              </div>
            )}

            <form
              class="worktree-promote"
              onSubmit={(event) => {
                event.preventDefault();
                if (!branch.trim()) return;
                void run('promote', async () => {
                  const result = await store.promoteWorktree(dir, branch.trim());
                  setBranch('');
                  if (cleanupRemoved(result)) backToList();
                  else setStatus(`Promoted to ${branch.trim()}; the old checkout is still in use.`);
                });
              }}
            >
              <div class="worktree-action-copy">
                <strong>Promote to a branch</strong>
                <span>Move these changes to a permanent Git branch.</span>
              </div>
              <div class="worktree-promote-controls">
                <input
                  aria-label="Branch name"
                  placeholder="Branch name"
                  value={branch}
                  disabled={Boolean(busy) || streaming}
                  onInput={(event) => setBranch(event.currentTarget.value)}
                />
                <button
                  class="btn"
                  type="submit"
                  disabled={!branch.trim() || Boolean(busy) || streaming}
                >
                  {busy === 'promote' ? 'Promoting…' : 'Promote'}
                </button>
              </div>
            </form>

            <div class={`worktree-remove ${removeStage !== 'idle' ? 'is-armed' : ''}`}>
              <div class="worktree-action-copy">
                <strong>
                  {removeStage === 'force' ? 'Worktree is in use' : 'Remove worktree'}
                </strong>
                <span id="worktreeRemoveHelp">
                  {removeStage === 'idle'
                    ? 'Delete this isolated checkout. This cannot be undone.'
                    : removeStage === 'armed'
                      ? 'Click confirm to permanently remove this worktree.'
                      : 'Force removal will move affected conversations back to the project root.'}
                </span>
              </div>
              <button
                class={`btn ${removeStage === 'idle' ? 'danger-quiet' : 'danger'}`}
                type="button"
                aria-describedby="worktreeRemoveHelp"
                disabled={Boolean(busy) || streaming}
                onClick={() => {
                  if (removeStage === 'idle') {
                    setRemoveStage('armed');
                    setStatus('Removal needs confirmation.');
                    return;
                  }
                  void run('remove', async () => {
                    try {
                      await store.removeWorktree(dir, removeStage === 'force');
                    } catch (value) {
                      const details = errorDetails(value);
                      if (
                        value instanceof APIError &&
                        value.status === 409 &&
                        removeStage !== 'force'
                      ) {
                        setRemoveStage('force');
                        setStatus(
                          details.inUse.length
                            ? `In use by ${details.inUse.join(', ')}.`
                            : details.message,
                        );
                        return;
                      }
                      throw value;
                    }
                    backToList(`Removed ${rowName(selected)}.`);
                  });
                }}
              >
                {busy === 'remove'
                  ? 'Removing…'
                  : removeStage === 'idle'
                    ? 'Remove…'
                    : removeStage === 'armed'
                      ? 'Confirm remove'
                      : 'Force remove'}
              </button>
            </div>

            {streaming && (
              <p class="worktree-muted-note">
                Finish the current response to change this worktree.
              </p>
            )}
            {(status || error) && (
              <div
                class={`worktree-status ${error ? 'is-error' : ''}`}
                role={error ? 'alert' : 'status'}
              >
                {error || status}
              </div>
            )}
          </section>
        ) : (
          <>
            <div class="worktree-list-heading">
              <strong>{draft ? 'Run conversation in' : 'Project checkouts'}</strong>
              <span>{managedCount} managed</span>
            </div>
            <div
              class="worktree-list"
              role={draft ? 'radiogroup' : undefined}
              aria-label={draft ? 'Available worktrees' : undefined}
            >
              {rows.map((row, index) => {
                const rootRow = isRoot(row);
                const rowDirectory = rowDir(row);
                const checked = draft ? (rootRow ? !activeDir : activeDir === rowDirectory) : false;
                const current = rootRow ? !activeDir : activeDir === rowDirectory;
                return (
                  <WorktreeOption
                    key={rootRow ? 'root' : rowDirectory || String(index)}
                    row={row}
                    draft={draft}
                    selected={checked}
                    current={current}
                    first={index === 0}
                    actionDisabled={Boolean(busy) || streaming}
                    onChoose={() => {
                      if (draft) {
                        store.chooseDraftWorktree(rootRow ? '' : rowDirectory);
                        return;
                      }
                      void run('switch', async () => {
                        autoOpened.current = true;
                        await store.switchWorktree(rootRow ? '' : rowDirectory);
                      });
                    }}
                    onDetails={rootRow ? undefined : () => openDetail(row)}
                  />
                );
              })}
              {managedCount === 0 && !store.worktreeError.value && (
                <div class="worktree-empty">
                  <strong>No managed worktrees yet</strong>
                  <span>Create one to work in an isolated checkout.</span>
                </div>
              )}
            </div>
            {status && (
              <div class="worktree-status" role="status">
                {status}
              </div>
            )}
          </>
        )}
      </div>

      {!selected && (
        <form
          class="worktree-footer"
          onSubmit={(event) => {
            event.preventDefault();
            if (!name.trim()) return;
            void run('create', async () => {
              await store.createWorktree(name.trim(), clean);
              if (!store.worktreeError.value) setName('');
            });
          }}
        >
          <div class="worktree-create-copy">
            <strong>New worktree</strong>
            <span>Create an isolated checkout from HEAD.</span>
          </div>
          <div class="worktree-create-controls">
            <input
              aria-label="New worktree name"
              placeholder="Name"
              value={name}
              disabled={busy === 'create' || streaming}
              onInput={(event) => setName(event.currentTarget.value)}
            />
            <button
              class="btn primary"
              type="submit"
              disabled={!name.trim() || Boolean(busy) || streaming}
            >
              <Icon name="add" />
              {busy === 'create' ? 'Creating…' : 'Create'}
            </button>
            {rootDirtyFiles > 0 && (
              <label class="worktree-create-clean">
                <input
                  type="checkbox"
                  checked={clean}
                  disabled={busy === 'create' || streaming}
                  onChange={(event) => setClean(event.currentTarget.checked)}
                />
                <span>
                  <strong>Start clean</strong>
                  <small>
                    Leave {rootDirtyFiles} changed {rootDirtyFiles === 1 ? 'file' : 'files'} in the
                    root checkout. Uncheck to move {rootDirtyFiles === 1 ? 'it' : 'them'} here.
                  </small>
                </span>
              </label>
            )}
          </div>
        </form>
      )}
    </Overlay>
  );
}
