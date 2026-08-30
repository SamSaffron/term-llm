import { useContext, useId, useLayoutEffect, useRef } from 'preact/hooks';
import { overlayManager } from '../platform/overlay-manager';
import { StoreContext } from '../app/context';
import { Icon } from './Icon';

export const OVERLAY_FOCUSABLE =
  'button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),a[href],audio[controls],video[controls],[contenteditable="true"],[tabindex]:not([tabindex="-1"])';

export function trapOverlayFocus(event: KeyboardEvent, selector = OVERLAY_FOCUSABLE): void {
  if (event.key !== 'Tab') return;
  const root = event.currentTarget as HTMLElement | null;
  if (!root) return;
  const items = [...root.querySelectorAll<HTMLElement>(selector)].filter((item) => item !== root);
  if (!items.length) {
    event.preventDefault();
    root.focus();
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
}

export function Overlay({
  title,
  children,
  wide = false,
  close = true,
  dismissDisabled = false,
  onClose,
  onEscape,
  className = '',
  id,
}: {
  title: string;
  children: preact.ComponentChildren;
  wide?: boolean;
  close?: boolean;
  dismissDisabled?: boolean;
  onClose?: () => void;
  onEscape?: () => void;
  className?: string;
  id?: string;
}) {
  const store = useContext(StoreContext);
  const overlay = useRef<HTMLDivElement>(null);
  const dialog = useRef<HTMLDivElement>(null);
  const token = useRef<symbol | null>(null);
  const label = useId();
  const dismiss =
    onClose ||
    (() => {
      if (store) store.modal.value = '';
    });
  useLayoutEffect(() => {
    token.current = overlayManager.acquire(undefined, overlay.current);
    const focusFrame = requestAnimationFrame(() => {
      const target =
        dialog.current?.querySelector<HTMLElement>('[autofocus]:not([disabled])') ||
        dialog.current?.querySelector<HTMLElement>(
          'button:not([disabled]):not([data-overlay-close]),input:not([disabled]),select:not([disabled]),textarea:not([disabled])',
        ) ||
        dialog.current?.querySelector<HTMLElement>('[data-overlay-close]:not([disabled])') ||
        dialog.current;
      target?.focus();
    });
    return () => {
      cancelAnimationFrame(focusFrame);
      if (token.current) overlayManager.release(token.current);
      token.current = null;
    };
  }, []);
  return (
    <div
      ref={overlay}
      class="modal-overlay"
      role="presentation"
      onPointerDown={(event) => {
        if (event.target === event.currentTarget)
          event.currentTarget.dataset.dismissPointer = String(event.pointerId);
      }}
      onPointerUp={(event) => {
        const startedOutside =
          event.currentTarget.dataset.dismissPointer === String(event.pointerId);
        delete event.currentTarget.dataset.dismissPointer;
        if (
          close &&
          !dismissDisabled &&
          startedOutside &&
          event.target === event.currentTarget &&
          token.current &&
          overlayManager.isTop(token.current)
        )
          dismiss();
      }}
      onPointerCancel={(event) => {
        delete event.currentTarget.dataset.dismissPointer;
      }}
    >
      <div
        ref={dialog}
        id={id}
        class={`modal ${wide ? 'wide-modal' : ''} ${className}`.trim()}
        role="dialog"
        aria-modal="true"
        aria-labelledby={label}
        tabIndex={-1}
        onKeyDown={(event) => {
          if (
            event.key === 'Escape' &&
            (close || onEscape) &&
            token.current &&
            overlayManager.isTop(token.current)
          ) {
            event.preventDefault();
            event.stopPropagation();
            if (!dismissDisabled) {
              if (onEscape) onEscape();
              else dismiss();
            }
            return;
          }
          trapOverlayFocus(event);
        }}
      >
        <div class="modal-title-row">
          <h2 id={label}>{title}</h2>
          {close && (
            <button
              class="icon-btn close-button"
              type="button"
              aria-label={`Close ${title}`}
              data-overlay-close
              disabled={dismissDisabled}
              onClick={dismiss}
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
