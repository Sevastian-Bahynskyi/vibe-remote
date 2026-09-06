'use strict';

const MUTATION_HEADER = Object.freeze({ 'X-Vibe-Remote': '1' });
const POLL_INTERVAL_MS = 20_000;

const elements = {
  accountEmail: document.querySelector('#account-email'),
  accountForm: document.querySelector('#account-form'),
  accountsList: document.querySelector('#accounts-list'),
  confirmButton: document.querySelector('#confirm-button'),
  confirmDialog: document.querySelector('#confirm-dialog'),
  confirmMessage: document.querySelector('#confirm-message'),
  confirmOption: document.querySelector('#confirm-option'),
  confirmOptionLabel: document.querySelector('#confirm-option-label'),
  confirmOptionRow: document.querySelector('#confirm-option-row'),
  confirmTitle: document.querySelector('#confirm-title'),
  connectionLabel: document.querySelector('#connection-label'),
  hookStatus: document.querySelector('#hook-status'),
  installHooksButton: document.querySelector('#install-hooks-button'),
  notice: document.querySelector('#notice'),
  powerWarningText: document.querySelector('#power-warning-text'),
  refreshButton: document.querySelector('#refresh-button'),
  sessionCount: document.querySelector('#session-count'),
  sessionsList: document.querySelector('#sessions-list'),
  showAccountForm: document.querySelector('#show-account-form'),
  showWorkspaceForm: document.querySelector('#show-workspace-form'),
  tailnetStatus: document.querySelector('#tailnet-status'),
  workerDetail: document.querySelector('#worker-detail'),
  workerStatus: document.querySelector('#worker-status'),
  workspaceForm: document.querySelector('#workspace-form'),
  workspaceLabel: document.querySelector('#workspace-label'),
  workspacePath: document.querySelector('#workspace-path'),
  workspaceSelect: document.querySelector('#workspace-select'),
  workspacesList: document.querySelector('#workspaces-list'),
};

let dashboardState = emptyState();
let confirmAction = null;
let noticeTimer = 0;
let loading = false;

function emptyState() {
  return { accounts: [], workspaces: [], sessions: [], worker: {}, system: {} };
}

function createElement(tagName, options = {}, children = []) {
  const node = document.createElement(tagName);
  if (options.className) node.className = options.className;
  if (options.text !== undefined) node.textContent = String(options.text);
  if (options.type) node.type = options.type;
  if (options.title) node.title = options.title;
  if (options.disabled) node.disabled = true;
  if (options.attrs) {
    Object.entries(options.attrs).forEach(([name, value]) => {
      if (value !== undefined && value !== null) node.setAttribute(name, String(value));
    });
  }
  children.forEach((child) => {
    if (child) node.append(child);
  });
  return node;
}

function replaceChildren(parent, children) {
  parent.replaceChildren(...children.filter(Boolean));
}

function asArray(value) {
  return Array.isArray(value) ? value : [];
}

function safeText(value, fallback = 'Unknown') {
  if (typeof value === 'string' && value.trim()) return value.trim();
  if (typeof value === 'number') return String(value);
  return fallback;
}

function sentenceCase(value) {
  return safeText(value).replaceAll('_', ' ').replace(/^./, (character) => character.toUpperCase());
}

function firstDefined(object, keys) {
  if (!object || typeof object !== 'object') return undefined;
  return keys.map((key) => object[key]).find((value) => value !== undefined && value !== null);
}

function humanTime(value) {
  const date = new Date(value);
  if (Number.isNaN(date.getTime())) return 'Time unavailable';

  const elapsedSeconds = Math.round((date.getTime() - Date.now()) / 1000);
  const divisions = [
    { amount: 60, unit: 'second' },
    { amount: 60, unit: 'minute' },
    { amount: 24, unit: 'hour' },
    { amount: 7, unit: 'day' },
  ];
  let duration = elapsedSeconds;
  let unit = 'second';
  for (const division of divisions) {
    unit = division.unit;
    if (Math.abs(duration) < division.amount) break;
    duration = Math.round(duration / division.amount);
  }
  if (Math.abs(duration) >= 7 && unit === 'day') {
    return new Intl.DateTimeFormat(undefined, { dateStyle: 'medium', timeStyle: 'short' }).format(date);
  }
  return new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' }).format(duration, unit);
}

async function apiRequest(path, options = {}) {
  const method = options.method || 'GET';
  const isMutation = method !== 'GET' && method !== 'HEAD';
  const headers = { Accept: 'application/json' };
  if (isMutation) Object.assign(headers, MUTATION_HEADER);
  if (options.body !== undefined) headers['Content-Type'] = 'application/json';

  let response;
  try {
    response = await fetch(path.replace(/^\//, ''), {
      method,
      headers,
      body: options.body === undefined ? undefined : JSON.stringify(options.body),
      cache: 'no-store',
    });
  } catch (error) {
    throw new Error(navigator.onLine ? 'This Mac did not answer. Check the Tailnet connection.' : 'This phone is offline.');
  }

  const responseText = await response.text();
  let payload = null;
  if (responseText) {
    try {
      payload = JSON.parse(responseText);
    } catch (error) {
      payload = { message: responseText };
    }
  }

  if (!response.ok) {
    const message = safeText(payload?.error || payload?.message, `Request failed (${response.status})`);
    const requestError = new Error(message);
    requestError.status = response.status;
    throw requestError;
  }
  return payload;
}

function setButtonBusy(button, busy, busyLabel = 'Working…') {
  if (busy) {
    button.dataset.originalLabel = button.textContent;
    button.textContent = busyLabel;
    button.disabled = true;
    button.setAttribute('aria-busy', 'true');
    return;
  }
  if (button.dataset.originalLabel) button.textContent = button.dataset.originalLabel;
  delete button.dataset.originalLabel;
  button.disabled = false;
  button.removeAttribute('aria-busy');
}

function showNotice(message, tone = 'success', persistent = false) {
  window.clearTimeout(noticeTimer);
  elements.notice.textContent = message;
  elements.notice.dataset.tone = tone;
  elements.notice.hidden = false;
  if (!persistent) {
    noticeTimer = window.setTimeout(() => {
      elements.notice.hidden = true;
    }, 6_000);
  }
}

function hideNotice() {
  window.clearTimeout(noticeTimer);
  elements.notice.hidden = true;
}

async function runMutation(button, path, options, successMessage) {
  setButtonBusy(button, true);
  try {
    const payload = await apiRequest(path, options);
    showNotice(safeText(payload?.message, successMessage));
    await loadState({ quiet: true });
    return payload;
  } catch (error) {
    showNotice(error.message, 'error', true);
    return null;
  } finally {
    setButtonBusy(button, false);
  }
}

function statusPill(text, tone = 'neutral') {
  return createElement('span', { className: `status-pill status-${tone}`, text });
}

function toneForStatus(status) {
  if (status === 'authenticated' || status === 'completed' || status === 'running') return 'good';
  if (status === 'error' || status === 'interrupted' || status === 'signed_out') return 'bad';
  if (status === 'pending' || status === 'prompted' || status === 'stopped') return 'warm';
  return 'neutral';
}

function renderSystem() {
  const worker = dashboardState.worker || {};
  const system = dashboardState.system || {};
  const activeAccount = dashboardState.accounts.find((account) => account.id === worker.accountId);
  const workerState = worker.running ? safeText(worker.state, 'Running') : safeText(worker.state, 'Stopped');

  elements.workerStatus.textContent = sentenceCase(workerState);
  elements.workerStatus.dataset.tone = worker.running ? 'good' : worker.lastError ? 'bad' : 'neutral';
  if (worker.running) {
    const owner = activeAccount ? activeAccount.email : worker.accountId;
    elements.workerDetail.textContent = `Active for ${safeText(owner, 'an account')}${worker.pid ? ` · PID ${worker.pid}` : ''}`;
  } else {
    elements.workerDetail.textContent = safeText(worker.lastError, 'No Claude worker is active.');
  }

  const tailnetOnly = firstDefined(system, ['tailnetOnly', 'tailnet_only', 'isTailnetOnly']);
  elements.tailnetStatus.textContent = typeof tailnetOnly === 'boolean' ? (tailnetOnly ? 'Enforced' : 'Not enforced') : safeText(firstDefined(system, ['tailnetStatus', 'tailnet']), 'Unknown');
  elements.tailnetStatus.dataset.tone = tailnetOnly === true ? 'good' : tailnetOnly === false ? 'bad' : 'neutral';

  const hooksHealthy = firstDefined(system, ['hooksHealthy', 'hookHealthy']);
  const hooksInstalled = system.hooksInstalled === true;
  elements.hookStatus.textContent = hooksHealthy === true ? 'Verified' : hooksInstalled ? 'Installed · verify /hooks' : 'Needs setup';
  elements.hookStatus.dataset.tone = hooksHealthy === true ? 'good' : hooksHealthy === false ? 'warm' : 'neutral';

  const powerMessage = firstDefined(system, ['powerWarning', 'powerMessage']);
  if (typeof powerMessage === 'string' && powerMessage.trim()) {
    elements.powerWarningText.textContent = powerMessage.trim();
  } else {
    elements.powerWarningText.textContent = 'Keep this Mac awake, connected to power, and online for remote control.';
  }
}

function renderWorkspaceSelect() {
  const previousSelection = elements.workspaceSelect.value;
  const options = [];
  if (dashboardState.workspaces.length === 0) {
    options.push(createElement('option', { text: 'Add a workspace first', attrs: { value: '' } }));
  } else {
    dashboardState.workspaces.forEach((workspace) => {
      const option = createElement('option', {
        text: workspace.label || workspace.path,
        attrs: { value: workspace.id },
      });
      if (workspace.id === previousSelection || (!previousSelection && workspace.selected)) option.selected = true;
      options.push(option);
    });
  }
  replaceChildren(elements.workspaceSelect, options);
}

function accountCard(account) {
  const authenticated = account.status === 'authenticated';
  const card = createElement('article', { className: `item-card${account.active ? ' active-item' : ''}` });
  const heading = createElement('div', { className: 'item-heading' }, [
    createElement('div', {}, [
      createElement('h3', { text: safeText(account.email, 'Unnamed account') }),
      createElement('p', { className: 'item-detail', text: account.active ? 'Active on this Mac' : authenticated ? 'Available to switch' : 'Sign-in required' }),
    ]),
    statusPill(account.active ? 'Active' : sentenceCase(account.status), account.active ? 'good' : toneForStatus(account.status)),
  ]);

  const actions = createElement('div', { className: 'item-actions' });
  const activate = createElement('button', {
    className: 'primary-button',
    text: account.active ? 'Restart / move' : 'Activate',
    type: 'button',
    disabled: !elements.workspaceSelect.value || !authenticated,
    title: !authenticated ? 'Refresh and verify this sign-in first' : elements.workspaceSelect.value ? 'Start this Claude account in the selected workspace' : 'Choose a workspace first',
  });
  activate.addEventListener('click', () => activateAccount(account, activate, false));
  actions.append(activate);

  const refresh = createElement('button', { className: 'secondary-button', text: 'Refresh sign-in', type: 'button' });
  refresh.addEventListener('click', async () => {
    const payload = await runMutation(refresh, `/api/auth/${encodeURIComponent(account.id)}/refresh`, { method: 'POST' }, 'Sign-in refresh started on this Mac.');
    const authURL = payload?.authUrl || payload?.url;
    if (typeof authURL === 'string' && /^https:\/\//.test(authURL)) window.open(authURL, '_blank', 'noopener,noreferrer');
  });
  actions.append(refresh);

  const remove = createElement('button', { className: 'danger-text-button', text: 'Remove', type: 'button' });
  remove.addEventListener('click', () => {
    openConfirmation({
      title: 'Remove Claude account?',
      message: `${safeText(account.email, 'This account')} will be removed from VIBE REMOTE.`,
      optionLabel: 'Also delete its saved checkpoints',
      confirmLabel: 'Remove account',
      action: async (deleteCheckpoints) => {
        await runMutation(remove, `/api/accounts/${encodeURIComponent(account.id)}?deleteCheckpoints=${deleteCheckpoints ? 'true' : 'false'}`, { method: 'DELETE' }, 'Account removed.');
      },
    });
  });
  actions.append(remove);

  card.append(heading);
  if (account.lastError) card.append(createElement('p', { className: 'error-detail', text: account.lastError }));
  card.append(actions);
  return card;
}

async function activateAccount(account, button, force) {
  const workspaceId = elements.workspaceSelect.value;
  if (!workspaceId) {
    showNotice('Choose a workspace before switching accounts.', 'error', true);
    elements.workspaceSelect.focus();
    return;
  }

  setButtonBusy(button, true, force ? 'Forcing switch…' : 'Switching…');
  try {
    await apiRequest(`/api/accounts/${encodeURIComponent(account.id)}/activate`, {
      method: 'POST',
      body: { workspaceId, force },
    });
    showNotice(`${safeText(account.email, 'Claude account')} is active.`);
    await loadState({ quiet: true });
  } catch (error) {
    if (error.status === 409 && !force) {
      openConfirmation({
        title: 'Force account switch?',
        message: `${error.message} Forcing the switch may stop the active Claude worker.`,
        confirmLabel: 'Force switch',
        danger: false,
        action: async () => activateAccount(account, button, true),
      });
    } else {
      showNotice(error.message, 'error', true);
    }
  } finally {
    setButtonBusy(button, false);
  }
}

function renderAccounts() {
  if (dashboardState.accounts.length === 0) {
    replaceChildren(elements.accountsList, [emptyMessage('No Claude accounts yet.', 'Add an account to enable switching and Claude handoffs.')]);
    return;
  }
  replaceChildren(elements.accountsList, dashboardState.accounts.map(accountCard));
}

function workspaceCard(workspace) {
  const remove = createElement('button', { className: 'danger-text-button', text: 'Remove', type: 'button' });
  remove.addEventListener('click', () => {
    openConfirmation({
      title: 'Remove workspace?',
      message: `${safeText(workspace.label, workspace.path)} will no longer be available for account switching. Existing files are not deleted.`,
      confirmLabel: 'Remove workspace',
      action: async () => {
        await runMutation(remove, `/api/workspaces/${encodeURIComponent(workspace.id)}`, { method: 'DELETE' }, 'Workspace removed.');
      },
    });
  });

  return createElement('article', { className: `item-card${workspace.selected ? ' active-item' : ''}` }, [
    createElement('div', { className: 'item-heading' }, [
      createElement('div', {}, [
        createElement('h3', { text: safeText(workspace.label, 'Workspace') }),
        createElement('p', { className: 'path-copy', text: safeText(workspace.path) }),
      ]),
      workspace.selected ? statusPill('Selected', 'good') : null,
    ]),
    createElement('div', { className: 'item-actions' }, [remove]),
  ]);
}

function renderWorkspaces() {
  if (dashboardState.workspaces.length === 0) {
    replaceChildren(elements.workspacesList, [emptyMessage('No workspaces yet.', 'Add a local project path before switching accounts.')]);
    return;
  }
  replaceChildren(elements.workspacesList, dashboardState.workspaces.map(workspaceCard));
}

function emptyMessage(title, detail) {
  return createElement('div', { className: 'empty-state' }, [
    createElement('p', { text: title }),
    createElement('span', { text: detail }),
  ]);
}

function checkpointCard(session) {
  const provider = safeText(session.provider, 'unknown').toLowerCase();
  const title = safeText(session.title, `${sentenceCase(provider)} session`);
  const workspace = safeText(session.workspacePath, 'Workspace unavailable');
  const branch = safeText(session.branch, 'No branch captured');
  const state = safeText(session.state, 'unknown');
  const details = createElement('div', { className: 'checkpoint-meta' }, [
    createElement('span', { text: workspace, title: workspace }),
    createElement('span', { text: branch, title: branch }),
    createElement('time', { text: humanTime(session.updatedAt), attrs: { datetime: session.updatedAt || '' } }),
  ]);

  const actionArea = createElement('div', { className: 'handoff-actions' });

  const editButton = createElement('button', { className: 'secondary-button', text: 'Rename', type: 'button' });
  editButton.addEventListener('click', async () => {
    const nextTitle = window.prompt('Checkpoint title', title);
    if (!nextTitle || !nextTitle.trim()) return;
    await updateCheckpoint(session, nextTitle.trim(), Boolean(session.pinned), editButton);
  });
  actionArea.append(editButton);

  const pinButton = createElement('button', {
    className: 'secondary-button',
    text: session.pinned ? 'Unpin' : 'Pin',
    type: 'button',
    title: session.pinned ? 'Allow cleanup after 30 days' : 'Keep this checkpoint until you unpin it',
  });
  pinButton.addEventListener('click', () => updateCheckpoint(session, title, !session.pinned, pinButton));
  actionArea.append(pinButton);

  if (session.resumeCommand) {
    const copyButton = createElement('button', { className: 'secondary-button', text: 'Copy resume command', type: 'button' });
    copyButton.addEventListener('click', async () => {
      try {
        await navigator.clipboard.writeText(String(session.resumeCommand));
        showNotice(session.desktopGuidance || 'Resume command copied.');
      } catch (error) {
        showNotice('Could not copy the command on this device.', 'error', true);
      }
    });
    actionArea.append(copyButton);
  }
  const codexButton = createElement('button', { className: 'primary-button', text: 'Continue with Codex', type: 'button' });
  codexButton.addEventListener('click', () => createHandoff(session, 'codex', '', codexButton));
  actionArea.append(codexButton);

  dashboardState.accounts.forEach((account) => {
    const button = createElement('button', {
      className: 'secondary-button',
      text: `Continue with ${safeText(account.email, 'Claude')}`,
      type: 'button',
      disabled: account.status !== 'authenticated',
      title: account.status === 'authenticated' ? `Continue using ${account.email}` : 'Authenticate this account first',
    });
    button.addEventListener('click', () => createHandoff(session, 'claude', account.id, button));
    actionArea.append(button);
  });

  if (dashboardState.accounts.length === 0) {
    actionArea.append(createElement('p', { className: 'muted-copy', text: 'Add a Claude account to hand this session to Claude.' }));
  }

  return createElement('article', { className: 'checkpoint-card' }, [
    createElement('div', { className: 'checkpoint-title-row' }, [
      createElement('div', {}, [
        createElement('h4', { text: title }),
        createElement('p', { className: 'session-id', text: safeText(session.id, 'Unidentified checkpoint') }),
      ]),
      createElement('div', { className: 'badge-row' }, [
        statusPill(sentenceCase(provider), provider === 'claude' ? 'violet' : 'blue'),
        statusPill(sentenceCase(state), toneForStatus(state)),
        session.pinned ? statusPill('Pinned', 'good') : null,
        session.sharedWorktree ? statusPill('Shared worktree', 'warm') : null,
      ]),
    ]),
    details,
    session.sharedWorktree ? createElement('p', { className: 'error-detail', text: 'Multiple active sessions share these files. Review live changes before continuing.' }) : null,
    session.desktopGuidance ? createElement('p', { className: 'muted-copy', text: session.desktopGuidance }) : null,
    actionArea,
  ]);
}

async function updateCheckpoint(session, title, pinned, button) {
  await runMutation(button, `/api/sessions/${encodeURIComponent(session.id)}`, {
    method: 'PATCH',
    body: { title, pinned },
  }, 'Checkpoint updated.');
}

function renderSessions() {
  elements.sessionCount.textContent = String(dashboardState.sessions.length);
  if (dashboardState.sessions.length === 0) {
    replaceChildren(elements.sessionsList, [emptyMessage('No checkpoints captured yet.', 'Use Codex or Claude in a configured workspace; checkpoints will appear here.')]);
    return;
  }

  const groups = new Map();
  dashboardState.sessions.forEach((session) => {
    const key = safeText(session.nativeSessionId, session.id);
    if (!groups.has(key)) groups.set(key, []);
    groups.get(key).push(session);
  });

  const sortedGroups = [...groups.entries()].sort((left, right) => {
    const leftTime = Math.max(...left[1].map((session) => new Date(session.updatedAt).getTime() || 0));
    const rightTime = Math.max(...right[1].map((session) => new Date(session.updatedAt).getTime() || 0));
    return rightTime - leftTime;
  });

  const nodes = sortedGroups.map(([nativeSessionId, sessions]) => {
    sessions.sort((left, right) => (new Date(right.updatedAt).getTime() || 0) - (new Date(left.updatedAt).getTime() || 0));
    return createElement('section', { className: 'checkpoint-group', attrs: { 'aria-label': `Native session ${nativeSessionId}` } }, [
      createElement('div', { className: 'group-heading' }, [
        createElement('h3', { text: 'Native session' }),
        createElement('code', { text: nativeSessionId, title: nativeSessionId }),
      ]),
      ...sessions.map(checkpointCard),
    ]);
  });
  replaceChildren(elements.sessionsList, nodes);
}

async function createHandoff(session, destinationProvider, destinationAccountId, button) {
  const payload = await runMutation(button, '/api/handoffs', {
    method: 'POST',
    body: {
      sourceSessionId: session.id,
      destinationProvider,
      destinationAccountId,
    },
  }, `Handoff ready. Open ${destinationProvider === 'codex' ? 'Codex' : 'Claude'} and continue.`);

  if (payload?.instructions) showNotice(String(payload.instructions), 'success', true);
}

function render() {
  renderSystem();
  renderWorkspaceSelect();
  renderAccounts();
  renderWorkspaces();
  renderSessions();
}

async function loadState({ quiet = false } = {}) {
  if (loading) return;
  loading = true;
  elements.refreshButton.disabled = true;
  elements.refreshButton.classList.add('is-spinning');
  if (!quiet) elements.connectionLabel.textContent = 'Connecting…';

  try {
    const payload = await apiRequest('/api/state');
    dashboardState = {
      accounts: asArray(payload?.accounts),
      workspaces: asArray(payload?.workspaces),
      sessions: asArray(payload?.sessions),
      worker: payload?.worker && typeof payload.worker === 'object' ? payload.worker : {},
      system: payload?.system && typeof payload.system === 'object' ? payload.system : {},
    };
    render();
    elements.connectionLabel.textContent = 'Connected';
    elements.connectionLabel.dataset.tone = 'good';
    if (!quiet) hideNotice();
  } catch (error) {
    elements.connectionLabel.textContent = 'Unavailable';
    elements.connectionLabel.dataset.tone = 'bad';
    showNotice(error.message, 'error', true);
    if (!quiet) {
      dashboardState = emptyState();
      render();
    }
  } finally {
    loading = false;
    elements.refreshButton.disabled = false;
    elements.refreshButton.classList.remove('is-spinning');
  }
}

function toggleForm(form, button, focusTarget) {
  const willShow = form.hidden;
  form.hidden = !willShow;
  button.setAttribute('aria-expanded', String(willShow));
  button.textContent = willShow ? 'Close' : 'Add';
  if (willShow) focusTarget.focus();
}

function openConfirmation({ title, message, optionLabel = '', confirmLabel, danger = true, action }) {
  elements.confirmTitle.textContent = title;
  elements.confirmMessage.textContent = message;
  elements.confirmOption.checked = false;
  elements.confirmOptionLabel.textContent = optionLabel;
  elements.confirmOptionRow.hidden = !optionLabel;
  elements.confirmButton.textContent = confirmLabel;
  elements.confirmButton.className = danger ? 'danger-button' : 'primary-button';
  confirmAction = action;
  elements.confirmDialog.showModal();
}

elements.confirmDialog.addEventListener('close', async () => {
  const action = confirmAction;
  confirmAction = null;
  if (elements.confirmDialog.returnValue === 'confirm' && action) {
    await action(elements.confirmOption.checked);
  }
});

elements.showAccountForm.addEventListener('click', () => toggleForm(elements.accountForm, elements.showAccountForm, elements.accountEmail));
elements.showWorkspaceForm.addEventListener('click', () => toggleForm(elements.workspaceForm, elements.showWorkspaceForm, elements.workspaceLabel));
elements.refreshButton.addEventListener('click', () => loadState());

elements.accountForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const email = elements.accountEmail.value.trim();
  if (!email) return;
  const button = elements.accountForm.querySelector('button[type="submit"]');
  const payload = await runMutation(button, '/api/accounts', { method: 'POST', body: { email } }, 'Claude account added. Refresh its sign-in to authenticate.');
  if (payload) {
    elements.accountForm.reset();
    toggleForm(elements.accountForm, elements.showAccountForm, elements.accountEmail);
  }
});

elements.workspaceForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const label = elements.workspaceLabel.value.trim();
  const path = elements.workspacePath.value.trim();
  if (!label || !path) return;
  const button = elements.workspaceForm.querySelector('button[type="submit"]');
  const payload = await runMutation(button, '/api/workspaces', { method: 'POST', body: { label, path } }, 'Workspace added.');
  if (payload) {
    elements.workspaceForm.reset();
    toggleForm(elements.workspaceForm, elements.showWorkspaceForm, elements.workspaceLabel);
  }
});

elements.installHooksButton.addEventListener('click', async () => {
  await runMutation(elements.installHooksButton, '/api/install-hooks', { method: 'POST' }, 'Checkpoint hooks installed.');
});

elements.workspaceSelect.addEventListener('change', renderAccounts);
window.addEventListener('online', () => loadState());
window.addEventListener('offline', () => {
  elements.connectionLabel.textContent = 'Phone offline';
  elements.connectionLabel.dataset.tone = 'bad';
});
document.addEventListener('visibilitychange', () => {
  if (document.visibilityState === 'visible') loadState({ quiet: true });
});

loadState();
window.setInterval(() => {
  if (document.visibilityState === 'visible') loadState({ quiet: true });
}, POLL_INTERVAL_MS);
