(function () {
  'use strict';

  var active = new Map();
  var keyNames = { '\x1b[A': 'up', '\x1b[B': 'down', '\x1b[D': 'left', '\x1b[C': 'right', '\t': 'tab', ' ': 'space', '\r': 'enter', '\x1b': 'escape' };

  function setup(element) {
    if (active.has(element)) return;
    var status = element.querySelector('.console-status');
    var controls = element.querySelectorAll('[data-console-key]');
    var display = element.querySelector('.console-display');
    var terminal = null;
    var runId = '';
    var cursor = 0;
    var timer = null;
    var stopped = false;
    var ready = false;
    var sending = false;
    var polling = false;
    var controller = new AbortController();
    var endpoint = element.dataset.console;
    var columns = 80;

    function enable(enabled) {
      controls.forEach(function (button) { button.disabled = !enabled; });
    }

    function fail(message) {
      ready = false;
      stopped = true;
      enable(false);
      status.textContent = message;
    }

    async function request(body) {
      var response = await fetch(endpoint, {
        method: 'POST', headers: { 'X-Vibe-Remote': '1', 'Content-Type': 'application/json' },
        body: JSON.stringify(Object.assign({runId: runId}, body)), cache: 'no-store', signal: controller.signal,
      });
      if (!response.ok) throw new Error('The session changed or the Mac did not accept the key. Close and reopen this console.');
    }

    async function send(key) {
      if (!ready || sending || !element.open || stopped) return;
      sending = true;
      enable(false);
      try {
        await request({key: key});
      } catch (error) {
        if (error.name !== 'AbortError') fail(error.message);
      } finally {
        sending = false;
        enable(ready && !stopped);
      }
    }

    async function poll() {
      if (polling || stopped || !element.open || !element.isConnected || document.visibilityState !== 'visible') return;
      polling = true;
      window.clearTimeout(timer);
      try {
        var response = await fetch(endpoint + '?cursor=' + cursor, {headers: {'X-Vibe-Remote': '1'}, cache: 'no-store', signal: controller.signal});
        if (!response.ok) throw new Error('Console unavailable. Check that the session is running, then close and reopen this console.');
        var chunk = await response.json();
        if (typeof chunk.runId !== 'string' || !chunk.runId || !Number.isSafeInteger(chunk.start) || !Number.isSafeInteger(chunk.end) || !Number.isSafeInteger(chunk.total) || chunk.start < 0 || chunk.end < chunk.start || chunk.total < chunk.end || (chunk.data !== null && typeof chunk.data !== 'string')) {
          throw new Error('The Mac returned an invalid console response. Close and reopen this console.');
        }
        if (runId && runId !== chunk.runId) throw new Error('Session restarted. Close and reopen this console.');
        var first = !runId;
        runId = chunk.runId;
        if (chunk.start !== cursor) terminal.reset();
        if (chunk.data) {
          var bytes = Uint8Array.from(atob(chunk.data), function (character) {return character.charCodeAt(0);});
          if (bytes.length !== chunk.end - chunk.start) throw new Error('Console output was interrupted. Close and reopen this console.');
          await new Promise(function (resolve) {
            function finish() { controller.signal.removeEventListener('abort', finish); resolve(); }
            controller.signal.addEventListener('abort', finish, {once: true});
            terminal.write(bytes, finish);
          });
          if (stopped || !element.isConnected) return;
        }
        cursor = chunk.end;
        if (first) {
          await request({columns: columns, rows: 21});
          await request({columns: columns, rows: 20});
        }
        ready = chunk.end === chunk.total;
        enable(ready && !sending);
        status.textContent = ready ? 'Connected to this session.' : 'Loading the current prompt…';
        timer = window.setTimeout(poll, chunk.data ? 100 : 750);
      } catch (error) {
        if (error.name !== 'AbortError') fail(error.message);
      } finally {
        polling = false;
      }
    }

    function start() {
      if (terminal || !element.open) return;
      columns = Math.max(40, Math.min(100, Math.floor((display.clientWidth - 24) / 8)));
      terminal = new window.Terminal({cols: columns, rows: 20, fontSize: 13, scrollback: 300, screenReaderMode: true, theme: {background: '#111318', foreground: '#f1f3f6'}, allowProposedApi: false});
      terminal.open(display);
      terminal.onData(function (data) { if (keyNames[data]) send(keyNames[data]); });
      status.textContent = 'Connecting…';
      poll();
    }

    function dispose() {
      stopped = true;
      controller.abort();
      window.clearTimeout(timer);
      if (terminal) terminal.dispose();
      element.removeEventListener('toggle', toggle);
      element.removeEventListener('click', click);
      active.delete(element);
    }

    function toggle() {
      if (!element.open) {
        dispose();
        display.replaceChildren();
        status.textContent = 'Open to connect.';
        setup(element);
      } else start();
    }

    function click(event) {
      var button = event.target.closest('[data-console-key]');
      if (button) send(button.dataset.consoleKey);
    }

    enable(false);
    element.addEventListener('toggle', toggle);
    element.addEventListener('click', click);
    active.set(element, {dispose: dispose, poll: poll, hasTerminal: function () { return terminal !== null && !stopped; }});
    start();
  }

  function scan() {
    active.forEach(function (console, element) {if (!element.isConnected) console.dispose();});
    document.querySelectorAll('[data-console]').forEach(setup);
  }

  function disposeAll() { active.forEach(function (entry) { entry.dispose(); }); }
  document.addEventListener('htmx:beforeCleanupElement', function (event) {
    active.forEach(function (entry, element) { if (event.target === element || event.target.contains(element)) entry.dispose(); });
  });
  document.addEventListener('htmx:afterSwap', scan);
  document.addEventListener('htmx:historyRestore', function () { disposeAll(); scan(); });
  window.addEventListener('pagehide', disposeAll);
  window.addEventListener('pageshow', scan);
  window.vibeRemoteConsoles = function () {
    var terminals = 0;
    active.forEach(function (entry) { if (entry.hasTerminal()) terminals++; });
    return {records: active.size, terminals: terminals};
  };
  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'visible') active.forEach(function (console) {console.poll();});
  });
  scan();
})();
