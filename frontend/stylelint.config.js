export default {
  extends: ['stylelint-config-standard'],
  ignoreFiles: ['dist/**', 'coverage/**'],
  rules: {
    // The migrated stylesheet intentionally keeps prefixed fallbacks and a
    // final integration layer that overrides earlier legacy selectors.
    'declaration-property-value-keyword-no-deprecated': null,
    'no-descending-specificity': null,
    'no-duplicate-selectors': null,
    'property-no-deprecated': null,
    'property-no-vendor-prefix': null,
    'selector-class-pattern': null,
    'value-no-vendor-prefix': null,
  },
};
