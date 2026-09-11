// Applies the saved theme before first paint.
//
// This lives in its own file, loaded without `defer`, because the page is served
// under `script-src 'self'` with no `unsafe-inline`, nonce or hash: the inline
// bootstrap this replaces was silently blocked by the browser, so the stored
// preference never reached the first paint.
(function () {
  'use strict';
  var STORAGE_KEY = 'vibe-remote-theme';
  var preference = 'system';
  try {
    var saved = window.localStorage.getItem(STORAGE_KEY);
    if (saved === 'system' || saved === 'light' || saved === 'dark') preference = saved;
  } catch (error) {
    // Private browsing, or site data blocked. The system preference still works.
  }
  var dark = preference === 'dark' ||
    (preference === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches);
  document.documentElement.dataset.theme = dark ? 'dark' : 'light';
  document.documentElement.dataset.themePreference = preference;
})();
