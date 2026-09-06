import type { ComponentChildren, JSX } from 'preact';

export function SettingsSelect({
  id,
  label,
  children,
  ...select
}: {
  id: string;
  label: string;
  value: string;
  onChange: JSX.SelectHTMLAttributes<HTMLSelectElement>['onChange'];
  children: ComponentChildren;
}) {
  return (
    <div class="settings-field">
      <label class="settings-label" for={id}>
        {label}
      </label>
      <select class="settings-select" id={id} {...select}>
        {children}
      </select>
    </div>
  );
}

type SearchFieldProps = Pick<
  JSX.InputHTMLAttributes<HTMLInputElement>,
  'aria-label' | 'value' | 'placeholder' | 'autoFocus' | 'onInput' | 'onKeyDown'
> & { className: string; iconPath?: string };

export function SearchField({ className, iconPath = 'm16 16 4 4', ...input }: SearchFieldProps) {
  return (
    <label class={className}>
      <span class={`${className}-icon`} aria-hidden="true">
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8">
          <circle cx="11" cy="11" r="7" />
          <path d={iconPath} />
        </svg>
      </span>
      <input type="search" {...input} />
    </label>
  );
}
