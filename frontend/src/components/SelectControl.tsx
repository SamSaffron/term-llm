import type { SelectHTMLAttributes } from 'preact';
import './select-control.css';

/** Keep native selection and keyboard behavior with a consistently inset arrow. */
export function SelectControl(
  props: Omit<SelectHTMLAttributes<HTMLSelectElement>, 'multiple' | 'size' | 'role'>,
) {
  return (
    <span class="control-select">
      <select {...props} />
      <svg
        class="control-select-chevron"
        width="14"
        height="14"
        viewBox="0 0 16 16"
        aria-hidden="true"
      >
        <path
          d="m4 6 4 4 4-4"
          fill="none"
          stroke="currentColor"
          stroke-width="1.5"
          stroke-linecap="round"
          stroke-linejoin="round"
        />
      </svg>
    </span>
  );
}
