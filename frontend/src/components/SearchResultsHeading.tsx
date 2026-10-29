import { useId, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import type { SessionSearchSort } from '../api/endpoints';
import { Icon } from './Icon';
import { useMenuKeyboard } from './Menu';
import { useMenuFlip } from './useMenuFlip';

const choices: { value: SessionSearchSort; label: string }[] = [
  { value: 'relevance', label: 'Relevance' },
  { value: 'newest', label: 'Newest first' },
  { value: 'oldest', label: 'Oldest first' },
];

export function SearchResultsHeading() {
  const store = useStore().sessionStore;
  const [open, setOpen] = useState(false);
  const trigger = useRef<HTMLButtonElement>(null);
  const menuID = useId();
  const { menu, up } = useMenuFlip(open);
  const keyboardMenu = useMenuKeyboard(open, () => setOpen(false), trigger);
  const selected = store.searchSort.value;
  const label = choices.find((choice) => choice.value === selected)!.label;
  return (
    <div class="search-results-heading">
      <h3>Search results</h3>
      <div class="search-sort">
        <button
          ref={trigger}
          type="button"
          class="icon-btn search-sort-trigger"
          aria-label={`Sort search results: ${label}`}
          title={`Sort search results: ${label}`}
          aria-haspopup="menu"
          aria-controls={open ? menuID : undefined}
          aria-expanded={open}
          onClick={() => setOpen(!open)}
          onKeyDown={(event) => {
            if (event.key === 'Escape' && open) {
              event.preventDefault();
              event.stopPropagation();
              setOpen(false);
            } else if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
              event.preventDefault();
              setOpen(true);
            }
          }}
        >
          <Icon name="sort" />
        </button>
        {open && (
          <div
            ref={(element) => {
              menu.current = element;
              keyboardMenu.current = element;
            }}
            id={menuID}
            class={`session-menu search-sort-menu ${up ? 'open-up' : ''}`}
            role="menu"
            aria-label="Sort search results"
            onKeyDown={(event) => {
              // Escape closes this menu, not the mobile sidebar containing it.
              if (event.key === 'Escape') {
                event.preventDefault();
                event.stopPropagation();
                setOpen(false);
              }
            }}
          >
            {choices.map((choice) => (
              <button
                key={choice.value}
                type="button"
                role="menuitemradio"
                aria-checked={selected === choice.value}
                onClick={() => {
                  setOpen(false);
                  void store.setSearchSort(choice.value);
                }}
              >
                <span class="session-menu-icon">
                  {selected === choice.value && <Icon name="check" />}
                </span>
                {choice.label}
              </button>
            ))}
          </div>
        )}
      </div>
    </div>
  );
}
