import { Icon, type IconName } from './Icon';
import type { ComponentChildren, InputHTMLAttributes, SelectHTMLAttributes } from 'preact';
import { SelectControl } from './SelectControl';

export function SettingsSelect({
  id,
  label,
  children,
  ...select
}: {
  id: string;
  label: string;
  value: string;
  onChange: SelectHTMLAttributes<HTMLSelectElement>['onChange'];
  children: ComponentChildren;
}) {
  return (
    <div class="settings-field">
      <label class="settings-label" for={id}>
        {label}
      </label>
      <SelectControl class="settings-select" id={id} {...select}>
        {children}
      </SelectControl>
    </div>
  );
}

type SearchFieldProps = Pick<
  InputHTMLAttributes<HTMLInputElement>,
  'aria-label' | 'value' | 'placeholder' | 'autoFocus' | 'onInput' | 'onKeyDown'
> & { className: string; icon?: IconName };

export function SearchField({ className, icon = 'search', ...input }: SearchFieldProps) {
  return (
    <label class={className}>
      <span class={`${className}-icon`} aria-hidden="true">
        <Icon name={icon} />
      </span>
      <input type="search" {...input} />
    </label>
  );
}
