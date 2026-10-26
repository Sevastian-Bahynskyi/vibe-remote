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

  var regions = new Map();
  var generation = 0;
  var dirtyForms = new WeakSet();
  var formSequence = 0;
  var requests = 0;
  var obsolete = 0;
  var manual = null;

  try {
    if (!window.localStorage.getItem('vibe-remote-history-cleared-v1')) {
      window.localStorage.removeItem('htmx-history-cache');
      window.localStorage.setItem('vibe-remote-history-cleared-v1', '1');
    }
  } catch (error) {}

  function refreshable() {
    return Array.prototype.slice.call(document.querySelectorAll('[data-refresh]'));
  }

  function synchronize() {
    var elements = refreshable();
    regions.forEach(function (state, id) {
      var element = document.getElementById(id);
      if (!element || !element.dataset.refresh || element.dataset.refresh !== state.url) {
        if (state.controller) state.controller.abort();
        regions.delete(id);
      }
    });
    elements.forEach(function (element) {
      if (!regions.has(element.id)) regions.set(element.id, {url: element.dataset.refresh, last: Date.now(), controller: null});
    });
    return elements;
  }

  function invalidate() {
    generation++;
    regions.forEach(function (state) { if (state.controller) state.controller.abort(); });
    regions.clear();
    if (manual) manual.abort();
    manual = null;
  }

  function preserveControls(element, html, preserveForms) {
    var parsed = new DOMParser().parseFromString(html, 'text/html');
    var controls = element.querySelector('[data-testid="confirm"]');
    if (controls) {
      var old = controls.closest('[data-session-controls]');
      var next = old && parsed.getElementById(old.id);
      if (next) next.setAttribute('hx-preserve', 'true');
    }
    if (preserveForms) {
      var candidates = Array.prototype.slice.call(parsed.querySelectorAll('form'));
      element.querySelectorAll('form').forEach(function (form) {
        if (!dirtyForms.has(form)) return;
        var action = form.getAttribute('hx-post') || form.getAttribute('action') || '';
        var next = candidates.find(function (candidate) { return (candidate.getAttribute('hx-post') || candidate.getAttribute('action') || '') === action; });
        if (!next) return;
        if (!form.id) form.id = 'refresh-form-' + (++formSequence);
        next.id = form.id;
        next.setAttribute('hx-preserve', 'true');
        candidates.splice(candidates.indexOf(next), 1);
      });
    }
    return parsed.body.innerHTML;
  }

  async function poll(element, state) {
    var version = generation;
    var controller = new AbortController();
    state.controller = controller;
    requests++;
    try {
      var response = await fetch(state.url, {headers: {'X-Vibe-Remote': '1', 'HX-Request': 'true'}, cache: 'no-store', signal: controller.signal});
      if (!response.ok) throw new Error('Refresh failed. Try Refresh again.');
      var html = await response.text();
      if (version !== generation || regions.get(element.id) !== state || document.getElementById(element.id) !== element || document.visibilityState !== 'visible') { obsolete++; return; }
      if (Array.prototype.some.call(element.querySelectorAll('form'), function (form) { return dirtyForms.has(form); })) return;
      if (element.contains(document.activeElement) && document.activeElement.matches('a,button,input,select,textarea,[tabindex]') && !document.activeElement.closest('[data-console]')) return;
      var prepared = preserveControls(element, html, false);
      window.htmx.swap(element, prepared, {swapStyle: 'outerHTML', settleDelay: 0});
    } catch (error) {
      if (error.name !== 'AbortError' && version === generation && !controller.signal.aborted) showNotice(error.message);
    } finally {
      if (state.controller === controller) { state.controller = null; state.last = Date.now(); }
      synchronize();
    }
  }

  function refresh(force) {
    if (manual || document.visibilityState !== 'visible') return;
    synchronize().forEach(function (element) {
      var state = regions.get(element.id);
      var every = parseInt(element.dataset.refreshEvery, 10) || DEFAULT_REFRESH_MS;
      if (state.controller || (!force && Date.now() - state.last < every)) return;
      if (element.contains(document.activeElement) && document.activeElement.matches('a,button,input,select,textarea,[tabindex]') && !document.activeElement.closest('[data-console]')) return;
      if (Array.prototype.some.call(element.querySelectorAll('form'), function (form) { return dirtyForms.has(form); })) return;
      poll(element, state);
    });
  }

  async function refreshScreen() {
    if (manual || document.visibilityState !== 'visible') return;
    invalidate();
    var controller = new AbortController();
    manual = controller;
    var version = generation;
    var app = document.getElementById('app');
    var focused = document.activeElement;
    var selection = focused && typeof focused.selectionStart === 'number' ? [focused.selectionStart, focused.selectionEnd] : null;
    requests++;
    try {
      var response = await fetch(window.location.href, {headers: {'X-Vibe-Remote': '1', 'HX-Request': 'true'}, cache: 'no-store', signal: controller.signal});
      if (!response.ok) throw new Error('Refresh failed. Try again.');
      var html = await response.text();
      if (version !== generation || manual !== controller || !app.isConnected) return;
      window.htmx.swap(app, preserveControls(app, html, true), {swapStyle: 'innerHTML', settleDelay: 0});
      if (focused && focused.isConnected) {
        focused.focus({preventScroll: true});
        if (selection) focused.setSelectionRange(selection[0], selection[1]);
      }
    } catch (error) {
      if (error.name !== 'AbortError' && version === generation && !controller.signal.aborted) showNotice(error.message);
    } finally {
      if (manual === controller) manual = null;
      synchronize();
    }
  }

  document.addEventListener('input', function (event) { var form = event.target.closest('form'); if (form) dirtyForms.add(form); });
  document.addEventListener('change', function (event) { var form = event.target.closest('form'); if (form) dirtyForms.add(form); });
  document.addEventListener('htmx:beforeRequest', function () { invalidate(); });
  document.addEventListener('htmx:afterSwap', synchronize);
  document.addEventListener('htmx:historyRestore', function () { invalidate(); synchronize(); refresh(true); });
  window.addEventListener('pagehide', invalidate);
  window.addEventListener('pageshow', function () { synchronize(); refresh(true); });
  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState !== 'visible') invalidate();
    else { synchronize(); refresh(true); }
  });
  window.addEventListener('online', function () { refresh(true); });
  window.setInterval(function () { refresh(false); }, 500);
  window.vibeRemotePolling = function () {
    var inFlight = manual ? 1 : 0;
    regions.forEach(function (state) { if (state.controller) inFlight++; });
    return {regions: regions.size, inFlight: inFlight, requests: requests, obsoleteResponses: obsolete};
  };
  synchronize();

  // ---- delegated clicks -------------------------------------------------
  // One listener on the document, so it keeps working across every htmx swap
  // without re-binding.

  document.addEventListener('click', function (event) {
    var refreshButton = event.target.closest('[data-refresh-now]');
    if (refreshButton) {
      refreshScreen();
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
    if (event.target.matches('input[name="conversation"]')) {
      var choices = event.target.closest('#conversation-choices');
      var picker = choices && choices.querySelector('.conversation-picker');
      if (picker) {
        var past = event.target.closest('.conversation-picker');
        var label = event.target.closest('.choice').querySelector('.choice-label');
        picker.querySelector('summary').textContent = past ? label.textContent : 'Choose a previous conversation';
        if (!past) picker.open = false;
      }
    }
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
