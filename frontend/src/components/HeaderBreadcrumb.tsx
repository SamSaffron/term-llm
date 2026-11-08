import { useId, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { resolveProjectContext } from '../domain/project-context';
import { observePopoverPosition, positionPopover } from '../platform/browser';
import { copyText } from '../platform/clipboard';
import { containPopoverScroll } from '../platform/popover-scroll';
import { Icon } from './Icon';

type ProjectContext = NonNullable<ReturnType<typeof resolveProjectContext>>;

function ProjectCrumb({ context }: { context: ProjectContext }) {
  const store = useStore();
  const [open, setOpen] = useState(false);
  const id = useId();
  const trigger = useRef<HTMLButtonElement>(null);
  const popover = useRef<HTMLDialogElement>(null);
  useLayoutEffect(() => {
    if (!open || !trigger.current || !popover.current) return;
    positionPopover(trigger.current, popover.current, true);
    popover.current
      .querySelector<HTMLButtonElement>('.chip-popover-item')
      ?.focus({ preventScroll: true });
    const stopPositioning = observePopoverPosition(trigger.current, popover.current, true);
    const stopScroll = containPopoverScroll(popover.current);
    return () => {
      stopPositioning();
      stopScroll();
    };
  }, [open]);
  const close = () => {
    const panel = popover.current;
    if (panel && typeof panel.close === 'function') panel.close();
    setOpen(false);
    trigger.current?.focus({ preventScroll: true });
  };
  const canStart = Boolean(
    context.project && !context.project.archived && context.project.available !== false,
  );
  return (
    <>
      <button
        ref={trigger}
        type="button"
        class="header-crumb header-crumb-project"
        aria-label={`Project ${context.name}`}
        aria-haspopup="dialog"
        aria-expanded={open}
        aria-controls={id}
        onClick={() => (open ? close() : setOpen(true))}
      >
        <span>{context.name}</span>
        <Icon class="header-crumb-chevron" name="chevron-down" />
      </button>
      {open && (
        <dialog
          ref={popover}
          id={id}
          class="chip-popover chip-popover-project"
          aria-label={`Project ${context.name}`}
          onCancel={(event) => {
            event.preventDefault();
            close();
          }}
          onClick={(event) => {
            if (event.target === event.currentTarget) close();
          }}
        >
          <div class="runtime-popover-header">
            <div class="runtime-popover-heading">
              <div class="runtime-popover-title">{context.name}</div>
              {context.path && (
                <div class="project-popover-path" title={context.path}>
                  {context.path}
                </div>
              )}
            </div>
            <button
              type="button"
              class="close-button runtime-popover-close"
              aria-label="Close project menu"
              onClick={close}
            >
              <Icon name="close" />
            </button>
          </div>
          {canStart && (
            <button
              type="button"
              class="chip-popover-item"
              onClick={() => {
                close();
                void store.startProjectChat(context.id);
              }}
            >
              <Icon name="edit" />
              <span class="chip-popover-item-label">New chat in {context.name}</span>
            </button>
          )}
          {store.worktreesAvailable() && (
            <button
              type="button"
              class="chip-popover-item"
              onClick={() => {
                close();
                store.modal.value = 'worktrees';
                void store.loadWorktrees();
              }}
            >
              <Icon name="branch" />
              <span class="chip-popover-item-label">Worktrees…</span>
            </button>
          )}
          {context.path && (
            <button
              type="button"
              class="chip-popover-item"
              onClick={() => {
                close();
                void copyText(context.path!)
                  .then(() => store.toast('Copied project path', 'success'))
                  .catch((error: unknown) => store.toast(error, 'error'));
              }}
            >
              <Icon name="copy" />
              <span class="chip-popover-item-label">Copy path</span>
            </button>
          )}
        </dialog>
      )}
    </>
  );
}

export function HeaderBreadcrumb({ context }: { context: ProjectContext | null }) {
  const store = useStore();
  const session = store.activeSession.value;
  const dir = store.currentWorktreeDir.value;
  const worktree =
    store.worktreesAvailable() && (dir !== '' || store.draftActive.value || !context);
  const paths = Boolean(session && store.branchPathCount.value > 1);
  const label = dir.split('/').pop() || 'root';
  return (
    <div class={`header-title-context${context || worktree || paths ? ' has-context' : ''}`}>
      {context && <ProjectCrumb key={context.id} context={context} />}
      {worktree && (
        <button
          type="button"
          class="header-crumb header-crumb-worktree"
          id="chipWorktreeTrigger"
          aria-label={`Worktree ${label}`}
          onClick={() => {
            store.modal.value = 'worktrees';
            void store.loadWorktrees();
          }}
        >
          <Icon name="branch" />
          <span class="chip-label">{label}</span>
        </button>
      )}
      {(session?.title || !context) && (
        <h1 class="header-title" id="activeSessionTitle">
          {session?.title || 'Chat'}
        </h1>
      )}
      {paths && (
        <button
          class="header-title-badge branch-tree-trigger"
          id="branchTreeBtn"
          type="button"
          aria-haspopup="dialog"
          onClick={() => void store.loadBranchTree()}
        >
          {store.branchPathCount.value} paths
        </button>
      )}
    </div>
  );
}
