import { useEffect, useId, useRef } from 'preact/hooks';
import { useStore } from '../app/context';
import { Icon } from './Icon';

export function Overlay({
  title,
  children,
  wide = false,
  close = true,
  onEscape,
  className = '',
}: {
  title: string;
  children: preact.ComponentChildren;
  wide?: boolean;
  close?: boolean;
  onEscape?: () => void;
  className?: string;
}) {
  const store = useStore();
  const dialog = useRef<HTMLDivElement>(null);
  const label = useId();
  useEffect(() => {
    const previous = document.activeElement as HTMLElement | null;
    const shell = document.getElementById('appShell');
    if (shell) shell.inert = true;
    requestAnimationFrame(() =>
      dialog.current
        ?.querySelector<HTMLElement>(
          '[autofocus],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled])',
        )
        ?.focus(),
    );
    return () => {
      if (shell) shell.inert = false;
      previous?.focus({ preventScroll: true });
    };
  }, []);
  return (
    <div
      class="modal-overlay"
      role="presentation"
      onMouseDown={(event) => {
        if (close && event.target === event.currentTarget) store.modal.value = '';
      }}
    >
      <div
        ref={dialog}
        class={`modal ${wide ? 'wide-modal' : ''} ${className}`.trim()}
        role="dialog"
        aria-modal="true"
        aria-labelledby={label}
        tabIndex={-1}
        onKeyDown={(event) => {
          if (event.key === 'Escape' && (close || onEscape)) {
            event.preventDefault();
            if (onEscape) onEscape();
            else store.modal.value = '';
            return;
          }
          if (event.key !== 'Tab') return;
          const items = [
            ...event.currentTarget.querySelectorAll<HTMLElement>(
              'button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),a[href],[tabindex]:not([tabindex="-1"])',
            ),
          ];
          if (!items.length) {
            event.preventDefault();
            event.currentTarget.focus();
            return;
          }
          const first = items[0];
          const last = items.at(-1)!;
          if (event.shiftKey && document.activeElement === first) {
            event.preventDefault();
            last.focus();
          } else if (!event.shiftKey && document.activeElement === last) {
            event.preventDefault();
            first.focus();
          }
        }}
      >
        <div class="modal-title-row">
          <h2 id={label}>{title}</h2>
          {close && (
            <button
              class="icon-btn"
              type="button"
              aria-label={`Close ${title}`}
              onClick={() => {
                store.modal.value = '';
              }}
            >
              <Icon name="close" />
            </button>
          )}
        </div>
        {children}
      </div>
    </div>
  );
}
