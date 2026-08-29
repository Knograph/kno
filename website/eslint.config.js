// @ts-check
import eslintPluginAstro from 'eslint-plugin-astro';

export default [
  {
    ignores: [
      'dist/',
      'node_modules/',
      '.astro/',
      'public/admin/',
      'playwright-report/',
      'test-results/',
    ],
  },
  ...eslintPluginAstro.configs.recommended,
];
