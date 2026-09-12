// Everything the dashboard needs beyond htmx, which is not much: htmx handles
// navigation, forms, swaps and errors declaratively. What is left is the work
// that genuinely needs scripting.
//
// This file replaces roughly a thousand lines of hand-rolled DOM rendering. It
// must stay small. Anything that can be an hx-* attribute belongs in a template.
//
// Note there are no inline handlers and no eval: the page runs under
// `script-src 'self'` with no `unsafe-inline` and no `unsafe-eval`, which is
// also why htmx's `hx-on:` and `hx-vals="js:"` are never used in the templates.
(function () {
  'use strict';

  var THEME_KEY = 'vibe-remote-theme';
  var DEFAULT_REFRESH_MS = 20000;

  ['gesturestart', 'gesturechange'].forEach(function (type) {
    document.addEventListener(type, function (event) {
      if (window.matchMedia('(pointer: coarse)').matches) event.preventDefault();
    }, { passive: false });
  });

  // ---- periodic refresh -------------------------------------------------
  //
  // htmx's own `every 20s` trigger would do this, but pausing it on a hidden
  // tab needs an event filter, and those are compiled with the Function
  // constructor, which the CSP forbids. A timer here is smaller than the
  // workaround would be, and it matches what the old dashboard did.
  //
  // Only regions that carry data-refresh are polled, and only Home has any, so
  // a refresh can never land on a form or a confirmation the user is partway
  // through. That is what makes this safe, rather than any guard below.

  function refreshable() {
    return Array.prototype.slice.call(document.querySelectorAll('[data-refresh]'));
  }

  function due(element, now) {
    var every = parseInt(element.dataset.refreshEvery, 10) || DEFAULT_REFRESH_MS;
    var last = parseInt(element.dataset.refreshedAt, 10) || 0;
    return now - last >= every;
  }

  function refresh(force) {
    if (document.visibilityState !== 'visible') return;
    // Never pull the ground out from under someone who is typing.
    if (document.activeElement && document.activeElement.closest('form')) return;
    var now = Date.now();
    refreshable().forEach(function (element) {
      if (!force && !due(element, now)) return;
      element.dataset.refreshedAt = String(now);
      window.htmx.ajax('GET', element.dataset.refresh, {
        target: '#' + element.id,
        swap: 'outerHTML',
      });
    });
  }

  // The tick only decides whether anything is due; data-refresh-every decides
  // whether a request is actually made. Ticking every second costs one query
  // selector and keeps a region that asked for a fast cadence — a session card
  // waiting on Claude to register — close to the rate it asked for, instead of
  // rounding it up to the tick.
  window.setInterval(function () { refresh(false); }, 1000);
  document.addEventListener('visibilitychange', function () { refresh(false); });
  window.addEventListener('online', function () { refresh(true); });

  // ---- delegated clicks -------------------------------------------------
  // One listener on the document, so it keeps working across every htmx swap
  // without re-binding.

  document.addEventListener('click', function (event) {
    var refreshButton = event.target.closest('[data-refresh-now]');
    if (refreshButton) {
      refresh(true);
      return;
    }

    var copyButton = event.target.closest('[data-copy]');
    if (copyButton) {
      copy(copyButton);
    }
  });

  function copy(button) {
    var text = button.dataset.copy;
    var original = button.textContent;
    function done(message) {
      button.textContent = message;
      window.setTimeout(function () { button.textContent = original; }, 2000);
    }
    if (!navigator.clipboard) {
      done('Cannot copy here');
      return;
    }
    navigator.clipboard.writeText(text).then(
      function () { done('Copied'); },
      function () { done('Cannot copy here'); }
    );
  }

  // ---- theme ------------------------------------------------------------
  // theme.js applies the stored preference before first paint; this only
  // handles changing it. The <select> is re-created by every swap of the system
  // panel, so its value is set on change rather than bound once.

  document.addEventListener('change', function (event) {
    var select = event.target.closest('[data-theme-select]');
    if (!select) return;
    applyTheme(select.value);
  });

  document.addEventListener('htmx:afterSwap', function () {
    var select = document.querySelector('[data-theme-select]');
    if (select) select.value = document.documentElement.dataset.themePreference || 'system';
  });

  function applyTheme(preference) {
    var dark = preference === 'dark' ||
      (preference === 'system' && window.matchMedia('(prefers-color-scheme: dark)').matches);
    document.documentElement.dataset.theme = dark ? 'dark' : 'light';
    document.documentElement.dataset.themePreference = preference;
    try {
      window.localStorage.setItem(THEME_KEY, preference);
    } catch (error) {
      // Nothing to do: the choice applies for this page view only.
    }
  }

  window.matchMedia('(prefers-color-scheme: dark)').addEventListener('change', function () {
    if ((document.documentElement.dataset.themePreference || 'system') === 'system') {
      applyTheme('system');
    }
  });

  // ---- connection failure ------------------------------------------------
  // htmx swaps error *responses*; a request that never arrives has no body to
  // swap, so say so explicitly.

  document.addEventListener('htmx:sendError', function () {
    showNotice(navigator.onLine
      ? 'The Mac did not answer. Check the Tailscale connection.'
      : 'This device is offline.');
  });

  document.addEventListener('htmx:timeout', function () {
    showNotice('The Mac is taking too long to answer.');
  });

  function showNotice(message) {
    var host = document.getElementById('notice');
    if (!host) return;
    var paragraph = document.createElement('p');
    paragraph.className = 'notice';
    paragraph.dataset.tone = 'bad';
    paragraph.dataset.sticky = 'true';
    paragraph.textContent = message;
    host.replaceChildren(paragraph);
  }
})();
