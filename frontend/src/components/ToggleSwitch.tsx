import type { JSX } from 'preact';

type ToggleSwitchProps = Pick<
  JSX.InputHTMLAttributes<HTMLInputElement>,
  'aria-label' | 'checked' | 'disabled' | 'onChange'
>;

/** Shared accessible switch for settings and MCP enablement. */
export function ToggleSwitch(props: ToggleSwitchProps) {
  return (
    <span class="toggle-switch">
      <input class="toggle-switch-input" type="checkbox" {...props} />
      <span class="toggle-switch-track" aria-hidden="true">
        <span class="toggle-switch-thumb" />
      </span>
    </span>
  );
}
