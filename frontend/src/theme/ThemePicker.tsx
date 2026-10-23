import { useId } from 'preact/hooks';
import { useStore } from '../app/context';
import { themeOptions } from './theme-store';

export function ThemePicker() {
  const { theme } = useStore();
  const id = useId();
  return (
    <div class="settings-field">
      <label class="settings-label" for={id}>
        Theme
      </label>
      <select
        id={id}
        class="settings-select"
        aria-describedby={`${id}-description`}
        value={theme.selected.value}
        onChange={(event) => theme.set(event.currentTarget.value)}
      >
        {themeOptions.map(({ value, label }) => (
          <option key={value} value={value}>
            {label}
          </option>
        ))}
      </select>
      <p class="extension-caption" id={`${id}-description`}>
        Applies immediately to this interface in this browser. Default follows your device’s light
        or dark appearance.
      </p>
      {theme.error.value && <p role="alert">{theme.error.value}</p>}
    </div>
  );
}
