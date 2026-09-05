import { useEffect, useRef } from 'preact/hooks';
import { useStore } from '../app/context';
import { Overlay } from './Overlay';
import type { CommitChange } from '../stores/commit-store';

export function fileChangeStats(changes: Array<CommitChange | undefined>): string {
  const present = changes.filter((change): change is CommitChange => Boolean(change));
  if (present.some((change) => change.binary)) return 'binary';
  const additions = present.reduce((total, change) => total + (change.additions || 0), 0);
  const deletions = present.reduce((total, change) => total + (change.deletions || 0), 0);
  return additions || deletions ? `+${additions} −${deletions}` : 'no line changes';
}

function fileChangeStatsLabel(changes: Array<CommitChange | undefined>): string {
  const present = changes.filter((change): change is CommitChange => Boolean(change));
  if (present.some((change) => change.binary)) return 'binary file';
  const additions = present.reduce((total, change) => total + (change.additions || 0), 0);
  const deletions = present.reduce((total, change) => total + (change.deletions || 0), 0);
  if (!additions && !deletions) return 'no line changes';
  return `${additions} ${additions === 1 ? 'addition' : 'additions'}, ${deletions} ${
    deletions === 1 ? 'deletion' : 'deletions'
  }`;
}

export function CommitModal() {
  const store = useStore();
  const commit = store.commitStore;
  const state = commit.state.value;
  const status = state.status;
  const staged = status?.staged || [];
  const unstaged = status?.unstaged || [];
  const untracked = status?.untracked || [];
  const paths = commit.allPaths();
  const uniformlyStaged =
    paths.length > 0 &&
    staged.length === paths.length &&
    unstaged.length === 0 &&
    untracked.length === 0 &&
    staged.every((entry) => !entry.partially_staged);
  const willApplySelection = state.selectionNeedsApply || state.reviewRequired;
  const phaseContent = useRef<HTMLDivElement>(null);
  const publishing = state.publishBusy || state.publishLoading;
  const locked = state.phase === 'committing' || state.publishBusy;
  const form = state.publishForm;
  const hasPublishForm = Boolean(form);
  const prURL = String(state.publishResult?.pr_url || '');
  const safePRURL = /^https:\/\//i.test(prURL) ? prURL : '';
  const busy =
    publishing ||
    ['loading', 'planning_scope', 'staging', 'drafting_message', 'committing'].includes(
      state.phase,
    );
  useEffect(() => {
    if (state.sessionId && store.activeSession.value?.id !== state.sessionId) commit.reset();
  }, [commit, state.sessionId, store.activeSession.value?.id]);
  useEffect(() => {
    const frame = requestAnimationFrame(() => {
      const root = phaseContent.current;
      if (!root || root.contains(document.activeElement)) return;
      root
        .querySelector<HTMLElement>(
          'textarea:not([disabled]), input:not([disabled]), button:not([disabled])',
        )
        ?.focus();
    });
    return () => cancelAnimationFrame(frame);
  }, [state.phase, hasPublishForm]);
  const close = () => commit.close();
  const counts = status
    ? [
        [status.total_staged ?? staged.length, 'staged'],
        [status.total_unstaged ?? unstaged.length, 'unstaged'],
        [status.total_untracked ?? untracked.length, 'untracked'],
      ]
        .filter(([count]) => Number(count) > 0)
        .map(([count, label]) => `${count} ${label}`)
        .join(' · ') || '0 changes'
    : '';

  return (
    <Overlay
      title="Git commit"
      className="commit-modal"
      onEscape={locked ? () => undefined : close}
      onClose={close}
      dismissDisabled={locked}
      close={!locked}
    >
      <div class="commit-summary">
        {status && (
          <p>
            <strong>{status.detached ? 'Detached HEAD' : status.branch || 'Git checkout'}</strong>
            {state.phase !== 'success' && <> · {counts}</>}
          </p>
        )}
        {state.info && (
          <p class="commit-progress" role="status" aria-live="polite">
            {state.info}
          </p>
        )}
        {status?.truncated && (
          <p class="commit-warning">
            The visible file list is truncated. Exact selection is disabled.
          </p>
        )}
        {state.reviewRequired && (
          <p class="commit-warning">
            Commit is disabled until repository status and staged files are reviewed again.
          </p>
        )}
        {state.error && (
          <p class="commit-error" role="alert">
            {state.error}
          </p>
        )}
      </div>

      <div ref={phaseContent} class="commit-phase-content">
        {state.phase === 'choosing_scope' && (
          <section aria-label="Choose commit scope">
            <p>Changes are already staged. Choose deliberately before message generation.</p>
            <div class="commit-actions commit-scope-actions">
              <button
                class="btn primary"
                type="button"
                onClick={() => void commit.chooseEverything()}
              >
                Commit everything
              </button>
              <button class="btn" type="button" onClick={() => void commit.chooseStaged()}>
                Commit staged
              </button>
              {state.intent && (
                <button class="btn" type="button" onClick={() => void commit.followRequest()}>
                  Follow request
                </button>
              )}
            </div>
            <p class="commit-note">
              Partially staged files keep only their staged portion with “Commit staged”; “Commit
              everything” stages their remaining content.
            </p>
          </section>
        )}

        {state.phase === 'reviewing_scope' && (
          <section aria-label="Review selected files">
            <p>{state.scopeSummary || 'Choose whole files for this commit.'}</p>
            <div
              class="commit-file-list"
              role="group"
              aria-label={`Files in this commit (${paths.length})`}
            >
              {paths.map((path) => {
                const stagedChange = staged.find((entry) => entry.path === path);
                const working = unstaged.find((entry) => entry.path === path);
                const untrackedChange = untracked.find((entry) => entry.path === path);
                const changes = [stagedChange, working, untrackedChange];
                const splitStats =
                  stagedChange && working
                    ? `staged ${fileChangeStats([stagedChange])} · unstaged ${fileChangeStats([
                        working,
                      ])}`
                    : fileChangeStats(changes);
                const badges = [
                  stagedChange && !working && !uniformlyStaged && 'staged',
                  working && !stagedChange && 'unstaged',
                  untrackedChange && 'untracked',
                  (stagedChange?.partially_staged || working?.partially_staged) &&
                    'partially staged',
                ]
                  .filter(Boolean)
                  .join(', ');
                const details = [badges, splitStats].filter(Boolean).join(' · ');
                const spokenStats =
                  stagedChange && working
                    ? `staged ${fileChangeStatsLabel([
                        stagedChange,
                      ])}, unstaged ${fileChangeStatsLabel([working])}`
                    : fileChangeStatsLabel(changes);
                const spokenDetails = [badges, spokenStats].filter(Boolean).join(', ');
                return (
                  <label class="commit-file" key={path}>
                    <input
                      type="checkbox"
                      checked={state.selected.includes(path)}
                      onChange={(event) => commit.setSelected(path, event.currentTarget.checked)}
                    />
                    <span class="commit-file-path">{path}</span>
                    <span class="commit-file-badges" aria-label={spokenDetails}>
                      {details}
                    </span>
                  </label>
                );
              })}
            </div>
            <p id="commitScopeNote" class="commit-note">
              {willApplySelection
                ? 'Returning applies the checked files to the Git index. They remain staged if you later cancel.'
                : 'No selection changes. Return without changing the Git index.'}
            </p>
            <div class="commit-actions">
              <button
                class="btn primary"
                type="button"
                aria-describedby="commitScopeNote"
                onClick={() => void commit.backToMessage()}
              >
                Back to message
              </button>
            </div>
          </section>
        )}

        {['editing', 'error'].includes(state.phase) && status && (
          <section aria-label="Edit commit message">
            <label class="settings-label" for="commitMessage">
              Commit message
            </label>
            <textarea
              id="commitMessage"
              class="commit-message"
              rows={9}
              value={state.message}
              autoFocus
              onInput={(event) => commit.setMessage(event.currentTarget.value)}
            />
            <p class="commit-stats">
              {status.summary?.files ?? staged.length} files · +{status.summary?.additions ?? 0} −
              {status.summary?.deletions ?? 0}
              {unstaged.length || untracked.length ? ' · working changes remain' : ''}
            </p>
            {state.agent && (
              <p class="commit-agent">
                Message agent: {state.agent}
                {state.agentSource ? ` (${state.agentSource})` : ''}
              </p>
            )}
            <div class="commit-actions">
              <button
                class="btn primary"
                type="button"
                disabled={state.reviewRequired || !state.message.split('\n')[0]?.trim()}
                onClick={() => void commit.commit()}
              >
                Commit
              </button>
              <button class="btn" type="button" onClick={() => void commit.regenerate()}>
                Regenerate
              </button>
              <button class="btn" type="button" onClick={() => void commit.reviewFiles()}>
                Review files
              </button>
              <button class="btn" type="button" onClick={close}>
                Cancel
              </button>
            </div>
          </section>
        )}

        {state.phase === 'success' && (
          <section class="commit-success" aria-label="Commit complete">
            <p>
              Committed <strong>{String(state.result?.short_oid || '')}</strong>{' '}
              {String(state.result?.subject || '')}
            </p>
            {state.result?.tree_changed && (
              <p class="commit-warning">A hook changed the committed tree after review.</p>
            )}
            {safePRURL && (
              <p>
                <a href={safePRURL} target="_blank" rel="noopener noreferrer">
                  Open pull request ↗
                </a>
              </p>
            )}
            {form?.kind === 'pr' ? (
              <form
                class="commit-publish-form"
                onSubmit={(event) => {
                  event.preventDefault();
                  void commit.publish();
                }}
              >
                <p class="commit-note">
                  Publish <strong>{String(state.result?.short_oid || '')}</strong> to{' '}
                  <strong>
                    {String(form.plan.remote)}/{form.branch}
                  </strong>
                  <br />
                  {String(form.plan.url)}
                </p>
                <p class="commit-note">
                  This publishes the branch history through this commit, not uncommitted changes. No
                  force-push.
                </p>
                {form.kind === 'pr' && (
                  <>
                    <label class="settings-label" for="commitPRBranch">
                      PR branch
                    </label>
                    <input
                      id="commitPRBranch"
                      class="settings-input"
                      required
                      value={form.branch}
                      onInput={(event) => commit.editPublish({ branch: event.currentTarget.value })}
                    />
                    {form.branch !== form.plan.target && (
                      <p class="commit-note">
                        A new remote branch will be created. Your local branch is not switched or
                        reset. If this commit is already on the base branch, there may be nothing to
                        propose.
                      </p>
                    )}
                    <label class="settings-label" for="commitPRBase">
                      Base branch
                    </label>
                    <input
                      id="commitPRBase"
                      class="settings-input"
                      required
                      value={form.base}
                      onInput={(event) => commit.editPublish({ base: event.currentTarget.value })}
                    />
                    <label class="settings-label" for="commitPRTitle">
                      PR title
                    </label>
                    <input
                      id="commitPRTitle"
                      class="settings-input"
                      required
                      value={form.title}
                      onInput={(event) => commit.editPublish({ title: event.currentTarget.value })}
                    />
                    <label class="settings-label" for="commitPRBody">
                      PR description
                    </label>
                    <textarea
                      id="commitPRBody"
                      class="commit-message"
                      rows={5}
                      value={form.body}
                      onInput={(event) => commit.editPublish({ body: event.currentTarget.value })}
                    />
                    <label>
                      <input
                        type="checkbox"
                        checked={form.draft}
                        onChange={(event) =>
                          commit.editPublish({ draft: event.currentTarget.checked })
                        }
                      />{' '}
                      Create as draft
                    </label>
                    <p class="commit-note">
                      Pushes first, then creates a PR in {String(form.plan.repository)}. An existing
                      open PR for this head/base will be reused.
                    </p>
                  </>
                )}
                <div class="commit-actions">
                  <button
                    class="btn primary"
                    type="submit"
                    disabled={
                      publishing ||
                      !form.branch.trim() ||
                      (form.kind === 'pr' &&
                        (!form.title.trim() ||
                          !form.base.trim() ||
                          form.branch === form.base ||
                          form.branch === form.plan.default_branch))
                    }
                  >
                    Push & make PR
                  </button>
                  <button class="btn" type="button" onClick={() => commit.cancelPublish()}>
                    Back
                  </button>
                </div>
              </form>
            ) : (
              <div class="commit-actions">
                {state.publishPending ? (
                  <button
                    class="btn"
                    type="button"
                    disabled={publishing}
                    onClick={() => void commit.reconnectPublish()}
                  >
                    Reconnect
                  </button>
                ) : (
                  <>
                    <button
                      class="btn"
                      type="button"
                      disabled={publishing || Boolean(state.publishResult?.pushed)}
                      onClick={() => void commit.preparePublish('push')}
                    >
                      {state.publishResult?.pushed ? 'Pushed' : 'Push'}
                    </button>
                    <button
                      class="btn"
                      type="button"
                      disabled={publishing || Boolean(safePRURL)}
                      onClick={() => void commit.preparePublish('pr')}
                    >
                      Make PR
                    </button>
                  </>
                )}
                <button
                  class="btn primary"
                  type="button"
                  disabled={state.publishBusy}
                  onClick={() => commit.reset()}
                >
                  Done
                </button>
              </div>
            )}
          </section>
        )}
      </div>
      {busy && <div class="commit-busy" aria-hidden="true" />}
    </Overlay>
  );
}
