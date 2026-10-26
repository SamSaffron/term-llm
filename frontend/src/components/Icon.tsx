// The icons' shapes live in a sprite: a cached file the page preloads, not part of
// the startup script. Each IconName is a <symbol> there (Icon.test.tsx checks).
import sprite from './icons.svg?url';

export type IconName =
  | 'add'
  | 'alert-circle'
  | 'arrow-left'
  | 'bot'
  | 'branch'
  | 'check'
  | 'chevron-down'
  | 'chevron-right'
  | 'chevron-up'
  | 'close'
  | 'compact'
  | 'copy'
  | 'diff'
  | 'dock-bottom'
  | 'dock-right'
  | 'edit'
  | 'expand'
  | 'file'
  | 'file-archive'
  | 'file-code'
  | 'file-link'
  | 'file-pen'
  | 'file-table'
  | 'file-text'
  | 'fork'
  | 'folder'
  | 'folder-search'
  | 'globe'
  | 'image'
  | 'info'
  | 'live'
  | 'logout'
  | 'menu'
  | 'more'
  | 'markdown'
  | 'message-circle-question'
  | 'microphone'
  | 'panel'
  | 'plug'
  | 'restore'
  | 'search'
  | 'send'
  | 'settings'
  | 'share'
  | 'steer'
  | 'stop'
  | 'pause'
  | 'terminal'
  | 'text-search'
  | 'trash'
  | 'widgets'
  | 'wrench';

export function Icon({ name, class: className }: { name: IconName; class?: string }) {
  return (
    <svg
      class={className}
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      stroke-width="1.9"
      stroke-linecap="round"
      stroke-linejoin="round"
      aria-hidden="true"
      focusable="false"
    >
      <use href={`${sprite}#${name}`} />
    </svg>
  );
}
