import type { ComponentChildren } from 'preact';
import { useLayoutEffect, useMemo, useRef, useState } from 'preact/hooks';
import { positionPopover } from '../platform/browser';

export interface ChipPickerOption {
  value: string;
  label: string;
  meta?: string;
}

interface ChipPickerProps {
  ariaLabel: string;
  value: string;
  options: ChipPickerOption[];
  onChange: (value: string) => void;
  triggerClass: string;
  popoverClass?: string;
  renderTrigger: (selected: ChipPickerOption, open: boolean) => ComponentChildren;
}

const FILTER_THRESHOLD = 10;

export function ChipPicker({
  ariaLabel,
  value,
  options,
  onChange,
  triggerClass,
  popoverClass = '',
  renderTrigger,
}: ChipPickerProps) {
  const [open, setOpen] = useState(false);
  const [filter, setFilter] = useState('');
  const trigger = useRef<HTMLButtonElement>(null);
  const popover = useRef<HTMLDialogElement>(null);
  const selected = options.find((option) => option.value === value) || options[0];
  const filterable = options.length > FILTER_THRESHOLD;
  const visibleOptions = useMemo(() => {
    const query = filter.trim().toLowerCase();
    if (!query) return options;
    return options.filter((option) =>
      `${option.label} ${option.meta || ''} ${option.value}`.toLowerCase().includes(query),
    );
  }, [filter, options]);

  const close = (restoreFocus = false) => {
    const panel = popover.current;
    if (panel && typeof panel.close === 'function') panel.close();
    setOpen(false);
    setFilter('');
    if (restoreFocus) trigger.current?.focus();
  };
  const choose = (next: string) => {
    close(true);
    if (next !== value) onChange(next);
  };
  const focusOption = (index: number) => {
    const items = popover.current?.querySelectorAll<HTMLButtonElement>('.chip-popover-item');
    if (!items?.length) return;
    items[(index + items.length) % items.length].focus();
  };
  const moveFrom = (current: HTMLButtonElement, direction: number) => {
    const items = [
      ...(popover.current?.querySelectorAll<HTMLButtonElement>('.chip-popover-item') || []),
    ];
    const index = items.indexOf(current);
    if (index >= 0 && items.length)
      items[(index + direction + items.length) % items.length].focus();
  };

  useLayoutEffect(() => {
    if (!open || !trigger.current || !popover.current) return;
    const panel = popover.current;
    positionPopover(trigger.current, panel, true);
    if (filterable) panel.querySelector<HTMLInputElement>('.chip-popover-filter')?.focus();
    else {
      const items = [...panel.querySelectorAll<HTMLButtonElement>('.chip-popover-item')];
      (items.find((item) => item.dataset.value === value) || items[0])?.focus();
    }
  }, [filterable, open, value]);

  if (!selected) return null;
  return (
    <>
      <button
        ref={trigger}
        type="button"
        class={triggerClass}
        aria-label={ariaLabel}
        aria-haspopup="listbox"
        aria-expanded={open}
        onClick={() => (open ? close() : setOpen(true))}
        onKeyDown={(event) => {
          if (event.key !== 'ArrowDown' && event.key !== 'ArrowUp') return;
          event.preventDefault();
          if (!open) setOpen(true);
        }}
      >
        {renderTrigger(selected, open)}
      </button>
      {open && (
        <dialog
          ref={popover}
          class={`chip-popover ${popoverClass}`.trim()}
          aria-label={ariaLabel}
          onCancel={(event) => {
            event.preventDefault();
            close(true);
          }}
          onClick={(event) => {
            if (event.target === event.currentTarget) close();
          }}
          onKeyDown={(event) => {
            if (event.key !== 'Escape') return;
            event.preventDefault();
            close(true);
          }}
        >
          {filterable && (
            <input
              type="search"
              class="chip-popover-filter"
              aria-label="Filter options"
              placeholder="Filter…"
              autocomplete="off"
              spellcheck={false}
              value={filter}
              onInput={(event) => setFilter(event.currentTarget.value)}
              onKeyDown={(event) => {
                if (event.key === 'ArrowDown') {
                  event.preventDefault();
                  focusOption(0);
                } else if (event.key === 'ArrowUp') {
                  event.preventDefault();
                  focusOption(-1);
                }
              }}
            />
          )}
          <div class="chip-popover-options" role="listbox" aria-label={ariaLabel}>
            {visibleOptions.map((option) => (
              <button
                type="button"
                key={option.value || 'default'}
                class="chip-popover-item"
                role="option"
                aria-selected={option.value === value}
                data-value={option.value}
                onClick={() => choose(option.value)}
                onKeyDown={(event) => {
                  if (event.key === 'ArrowDown') {
                    event.preventDefault();
                    moveFrom(event.currentTarget, 1);
                  } else if (event.key === 'ArrowUp') {
                    event.preventDefault();
                    moveFrom(event.currentTarget, -1);
                  } else if (event.key === 'Home') {
                    event.preventDefault();
                    focusOption(0);
                  } else if (event.key === 'End') {
                    event.preventDefault();
                    focusOption(-1);
                  }
                }}
              >
                <span class="chip-popover-item-label">{option.label}</span>
                {option.meta && <span class="chip-popover-item-meta">{option.meta}</span>}
              </button>
            ))}
            {!visibleOptions.length && <div class="chip-popover-empty">No matching options</div>}
          </div>
        </dialog>
      )}
    </>
  );
}
