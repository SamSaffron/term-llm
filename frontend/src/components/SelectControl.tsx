import { Icon } from './Icon';
import type { SelectHTMLAttributes } from 'preact';
import './select-control.css';

/** Keep native selection and keyboard behavior with a consistently inset arrow. */
export function SelectControl(
  props: Omit<SelectHTMLAttributes<HTMLSelectElement>, 'multiple' | 'size' | 'role'>,
) {
  return (
    <span class="control-select">
      <select {...props} />
      <Icon class="control-select-chevron" name="chevron-down" />
    </span>
  );
}
