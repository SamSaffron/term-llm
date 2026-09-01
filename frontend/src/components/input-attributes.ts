/** Opts non-credential fields out of browser and common password-manager autofill. */
export const passwordManagerIgnoreProps = {
  autoComplete: 'off',
  'data-1p-ignore': 'true',
  'data-bwignore': 'true',
  'data-form-type': 'other',
  'data-lpignore': 'true',
  'data-protonpass-ignore': 'true',
} as const;
