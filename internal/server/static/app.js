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
  remoteSessionAccount: document.querySelector('#remote-session-account'),
  remoteSessionConversation: document.querySelector('#remote-session-conversation'),
  remoteSessionForm: document.querySelector('#remote-session-form'),
  remoteSessionName: document.querySelector('#remote-session-name'),
  remoteSessionWorkspace: document.querySelector('#remote-session-workspace'),
  remoteSessionsList: document.querySelector('#remote-sessions-list'),
  sessionCount: document.querySelector('#session-count'),
  sessionsList: document.querySelector('#sessions-list'),
  showAccountForm: document.querySelector('#show-account-form'),
  showRemoteSessionForm: document.querySelector('#show-remote-session-form'),
  showWorkspaceForm: document.querySelector('#show-workspace-form'),
  tailnetStatus: document.querySelector('#tailnet-status'),
  workerDetail: document.querySelector('#worker-detail'),
  workerStatus: document.querySelector('#worker-status'),
  workspaceForm: document.querySelector('#workspace-form'),
  workspaceLabel: document.querySelector('#workspace-label'),
  workspacePath: document.querySelector('#workspace-path'),
  workspacesList: document.querySelector('#workspaces-list'),
};

let dashboardState = emptyState();
let confirmAction = null;
let noticeTimer = 0;
let loading = false;

function emptyState() {
  return { accounts: [], workspaces: [], sessions: [], remoteSessions: [], runningCount: 0, system: {} };
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

function isClaudeRemoteURL(value) {
  if (typeof value !== 'string') return false;
  try {
    const parsed = new URL(value);
    return parsed.protocol === 'https:'
      && (parsed.hostname === 'claude.ai' || parsed.hostname === 'claude.com')
      && (parsed.pathname === '/code' || parsed.pathname === '/code/' || parsed.pathname.startsWith('/code/'));
  } catch (error) {
    return false;
  }
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
  const system = dashboardState.system || {};
  const sessions = dashboardState.remoteSessions;
  const running = sessions.filter((session) => session.worker && session.worker.running);
  const failed = sessions.filter((session) => session.worker && session.worker.lastError && !session.worker.running);

  elements.workerStatus.textContent = sessions.length === 0 ? 'None' : `${running.length} of ${sessions.length} running`;
  elements.workerStatus.dataset.tone = running.length > 0 ? 'good' : failed.length > 0 ? 'bad' : 'neutral';
  if (running.length > 0) {
    const names = running.map((session) => safeText(session.name, 'Unnamed session'));
    elements.workerDetail.textContent = `Live: ${names.join(', ')}`;
  } else if (sessions.length > 0) {
    elements.workerDetail.textContent = safeText(failed[0]?.worker?.lastError, 'No session is connected right now.');
  } else {
    elements.workerDetail.textContent = 'No sessions yet. Start one to control Claude from your phone.';
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

function authenticatedAccounts() {
  return dashboardState.accounts.filter((account) => account.status === 'authenticated');
}

function fillSelect(select, options, emptyLabel) {
  const previous = select.value;
  const nodes = [];
  if (options.length === 0) {
    nodes.push(createElement('option', { text: emptyLabel, attrs: { value: '' } }));
  } else {
    options.forEach((option) => {
      const node = createElement('option', { text: option.label, attrs: { value: option.value } });
      if (option.value === previous || (!previous && option.preferred)) node.selected = true;
      nodes.push(node);
    });
  }
  replaceChildren(select, nodes);
}

function accountOptions() {
  return authenticatedAccounts().map((account) => ({ value: account.id, label: safeText(account.email, 'Claude account') }));
}

function workspaceOptions() {
  return dashboardState.workspaces.map((workspace) => ({
    value: workspace.id,
    label: safeText(workspace.label, workspace.path),
    preferred: Boolean(workspace.selected),
  }));
}

// Only Claude checkpoints from the same account and workspace can be resumed,
// so the conversation list follows the other two selects.
function conversationOptions(accountId, workspacePath) {
  return dashboardState.sessions
    .filter((session) => session.provider === 'claude'
      && session.accountId === accountId
      && session.workspacePath === workspacePath)
    .slice(0, 40)
    .map((session) => ({
      value: session.id,
      label: `${safeText(session.title, 'Untitled conversation')} · ${humanTime(session.updatedAt)}`,
    }));
}

function workspacePathFor(workspaceId) {
  const workspace = dashboardState.workspaces.find((candidate) => candidate.id === workspaceId);
  return workspace ? workspace.path : '';
}

function renderNewSessionForm() {
  fillSelect(elements.remoteSessionAccount, accountOptions(), 'Add an authenticated account first');
  fillSelect(elements.remoteSessionWorkspace, workspaceOptions(), 'Add a workspace first');
  renderConversationSelect();
}

function renderConversationSelect() {
  const previous = elements.remoteSessionConversation.value;
  const options = conversationOptions(
    elements.remoteSessionAccount.value,
    workspacePathFor(elements.remoteSessionWorkspace.value),
  );
  const nodes = [createElement('option', { text: 'New conversation', attrs: { value: '' } })];
  options.forEach((option) => {
    const node = createElement('option', { text: option.label, attrs: { value: option.value } });
    if (option.value === previous) node.selected = true;
    nodes.push(node);
  });
  replaceChildren(elements.remoteSessionConversation, nodes);
}

function remoteSessionCard(session) {
  const worker = session.worker || {};
  const account = dashboardState.accounts.find((candidate) => candidate.id === session.accountId);
  const workspace = dashboardState.workspaces.find((candidate) => candidate.id === session.workspaceId);
  const running = Boolean(worker.running);
  const workspaceLabel = safeText(workspace?.label, session.workspacePath);
  const conversation = session.resumeSessionId ? `Resumes ${session.resumeSessionId}` : 'New conversation';

  const card = createElement('article', { className: `item-card${running ? ' active-item' : ''}` });
  card.append(createElement('div', { className: 'item-heading' }, [
    createElement('div', {}, [
      createElement('h3', { text: safeText(session.name, 'Unnamed session') }),
      createElement('p', { className: 'item-detail', text: `${safeText(account?.email, 'Unknown account')} · ${workspaceLabel}` }),
      createElement('p', { className: 'path-copy', text: safeText(session.workspacePath), title: safeText(session.workspacePath) }),
      createElement('p', { className: 'item-detail', text: conversation, title: conversation }),
    ]),
    statusPill(running ? 'Running' : sentenceCase(safeText(worker.state, session.desired === 'stopped' ? 'stopped' : 'idle')), running ? 'good' : worker.lastError ? 'bad' : 'neutral'),
  ]));
  if (worker.lastError) card.append(createElement('p', { className: 'error-detail', text: worker.lastError }));

  const actions = createElement('div', { className: 'item-actions' });
  if (running && isClaudeRemoteURL(worker.remoteUrl)) {
    actions.append(createElement('a', {
      className: 'primary-button connect-button',
      text: worker.remoteUrl === 'https://claude.ai/code' ? 'Open Claude' : 'Open live session',
      attrs: { href: worker.remoteUrl, target: '_blank', rel: 'noopener noreferrer' },
    }));
  }

  if (running) {
    const restart = createElement('button', { className: 'secondary-button', text: 'Restart', type: 'button', title: 'Restart this session to apply changes' });
    restart.addEventListener('click', () => remoteSessionLifecycle(session, 'restart', restart, false));
    actions.append(restart);

    const stop = createElement('button', { className: 'secondary-button', text: 'Stop', type: 'button' });
    stop.addEventListener('click', () => remoteSessionLifecycle(session, 'stop', stop, false));
    actions.append(stop);
  } else {
    const start = createElement('button', { className: 'primary-button', text: 'Start', type: 'button' });
    start.addEventListener('click', () => remoteSessionLifecycle(session, 'start', start, false));
    actions.append(start);
  }

  const editForm = buildRemoteSessionEditor(session);
  const edit = createElement('button', {
    className: 'secondary-button', text: 'Move / edit', type: 'button',
    attrs: { 'aria-expanded': 'false' },
  });
  edit.addEventListener('click', () => {
    const willShow = editForm.hidden;
    editForm.hidden = !willShow;
    edit.setAttribute('aria-expanded', String(willShow));
    edit.textContent = willShow ? 'Cancel' : 'Move / edit';
  });
  actions.append(edit);

  const remove = createElement('button', { className: 'danger-text-button', text: 'Remove', type: 'button' });
  remove.addEventListener('click', () => {
    openConfirmation({
      title: 'Remove this session?',
      message: `${safeText(session.name, 'This session')} will be stopped and removed. Its captured checkpoints are kept.`,
      confirmLabel: 'Remove session',
      action: async () => {
        await runMutation(remove, `/api/remote-sessions/${encodeURIComponent(session.id)}`, { method: 'DELETE' }, 'Session removed.');
      },
    });
  });
  actions.append(remove);

  card.append(actions, editForm);
  return card;
}

// The editor moves a live session to another account, workspace, or
// conversation; saving restarts the session so the change takes effect.
function buildRemoteSessionEditor(session) {
  const form = createElement('form', { className: 'inline-form' });
  form.hidden = true;

  const accountSelect = createElement('select', { attrs: { id: `edit-account-${session.id}` } });
  const workspaceSelect = createElement('select', { attrs: { id: `edit-workspace-${session.id}` } });
  const conversationSelect = createElement('select', { attrs: { id: `edit-conversation-${session.id}` } });
  const nameInput = createElement('input', { attrs: { id: `edit-name-${session.id}`, type: 'text', autocomplete: 'off' } });
  nameInput.value = safeText(session.name, '');

  const accounts = accountOptions();
  replaceChildren(accountSelect, accounts.map((option) => {
    const node = createElement('option', { text: option.label, attrs: { value: option.value } });
    if (option.value === session.accountId) node.selected = true;
    return node;
  }));
  replaceChildren(workspaceSelect, workspaceOptions().map((option) => {
    const node = createElement('option', { text: option.label, attrs: { value: option.value } });
    if (option.value === session.workspaceId) node.selected = true;
    return node;
  }));

  const fillConversations = () => {
    const options = conversationOptions(accountSelect.value, workspacePathFor(workspaceSelect.value));
    const nodes = [createElement('option', { text: 'New conversation', attrs: { value: '' } })];
    options.forEach((option) => {
      const node = createElement('option', { text: option.label, attrs: { value: option.value } });
      nodes.push(node);
    });
    replaceChildren(conversationSelect, nodes);
  };
  fillConversations();
  accountSelect.addEventListener('change', fillConversations);
  workspaceSelect.addEventListener('change', fillConversations);

  const save = createElement('button', { className: 'primary-button', text: 'Save & restart', type: 'submit' });
  form.append(
    createElement('div', { className: 'field-group' }, [
      createElement('label', { text: 'Claude account', attrs: { for: accountSelect.id } }), accountSelect,
    ]),
    createElement('div', { className: 'field-group' }, [
      createElement('label', { text: 'Workspace', attrs: { for: workspaceSelect.id } }), workspaceSelect,
    ]),
    createElement('div', { className: 'field-group' }, [
      createElement('label', { text: 'Conversation', attrs: { for: conversationSelect.id } }), conversationSelect,
    ]),
    createElement('label', { text: 'Name shown in the Claude app', attrs: { for: nameInput.id } }),
    createElement('div', { className: 'form-row' }, [nameInput, save]),
  );

  form.addEventListener('submit', async (event) => {
    event.preventDefault();
    await moveRemoteSession(session, {
      accountId: accountSelect.value,
      workspaceId: workspaceSelect.value,
      resumeSessionId: conversationSelect.value,
      name: nameInput.value.trim(),
    }, save, false);
  });
  return form;
}

async function moveRemoteSession(session, body, button, force) {
  setButtonBusy(button, true, 'Restarting…');
  try {
    const payload = await apiRequest(`/api/remote-sessions/${encodeURIComponent(session.id)}`, {
      method: 'PATCH',
      body: { ...body, force },
    });
    showNotice(safeText(payload?.message, 'Session updated.'));
    await loadState({ quiet: true });
  } catch (error) {
    if (error.status === 409 && !force) {
      openConfirmation({
        title: 'Force restart?',
        message: `${error.message} Forcing it stops the running Claude turn.`,
        confirmLabel: 'Force restart',
        danger: false,
        action: async () => moveRemoteSession(session, body, button, true),
      });
    } else {
      showNotice(error.message, 'error', true);
    }
  } finally {
    setButtonBusy(button, false);
  }
}

async function remoteSessionLifecycle(session, action, button, force) {
  const labels = { start: 'Starting…', restart: 'Restarting…', stop: 'Stopping…' };
  setButtonBusy(button, true, labels[action]);
  try {
    const payload = await apiRequest(`/api/remote-sessions/${encodeURIComponent(session.id)}/${action}`, {
      method: 'POST',
      body: { force },
    });
    showNotice(safeText(payload?.message, 'Session updated.'));
    await loadState({ quiet: true });
  } catch (error) {
    if (error.status === 409 && !force) {
      openConfirmation({
        title: 'Force this change?',
        message: `${error.message} Forcing it stops the running Claude turn.`,
        confirmLabel: 'Force',
        danger: false,
        action: async () => remoteSessionLifecycle(session, action, button, true),
      });
    } else {
      showNotice(error.message, 'error', true);
    }
  } finally {
    setButtonBusy(button, false);
  }
}

function renderRemoteSessions() {
  if (dashboardState.remoteSessions.length === 0) {
    replaceChildren(elements.remoteSessionsList, [emptyMessage(
      'No remote sessions yet.',
      'Tap New to start one. You can run as many as you need, side by side.',
    )]);
    return;
  }
  replaceChildren(elements.remoteSessionsList, dashboardState.remoteSessions.map(remoteSessionCard));
}

function accountCard(account) {
  const authenticated = account.status === 'authenticated';
  const sessionCount = dashboardState.remoteSessions.filter((session) => session.accountId === account.id).length;
  const card = createElement('article', { className: `item-card${account.active ? ' active-item' : ''}` });
  const detail = account.active
    ? `Live in ${sessionCount === 1 ? '1 session' : `${sessionCount} sessions`}`
    : authenticated
      ? sessionCount > 0 ? `${sessionCount === 1 ? '1 session' : `${sessionCount} sessions`} configured` : 'Available for new sessions'
      : 'Sign-in required';
  const heading = createElement('div', { className: 'item-heading' }, [
    createElement('div', {}, [
      createElement('h3', { text: safeText(account.email, 'Unnamed account') }),
      createElement('p', { className: 'item-detail', text: detail }),
    ]),
    statusPill(account.active ? 'Live' : sentenceCase(account.status), account.active ? 'good' : toneForStatus(account.status)),
  ]);

  const actions = createElement('div', { className: 'item-actions' });
  const newSession = createElement('button', {
    className: 'primary-button',
    text: 'New session',
    type: 'button',
    disabled: !authenticated,
    title: authenticated ? 'Start another Remote Control session with this account' : 'Refresh and verify this sign-in first',
  });
  newSession.addEventListener('click', () => openNewSessionForm(account.id));
  actions.append(newSession);

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
      message: `${safeText(account.email, 'This account')} and its ${sessionCount === 1 ? 'session' : 'sessions'} will be removed from Vibe Remote.`,
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

function openNewSessionForm(accountId) {
  if (elements.remoteSessionForm.hidden) {
    toggleForm(elements.remoteSessionForm, elements.showRemoteSessionForm, elements.remoteSessionAccount);
  }
  if (accountId) {
    elements.remoteSessionAccount.value = accountId;
    renderConversationSelect();
  }
  elements.remoteSessionForm.scrollIntoView({ behavior: 'smooth', block: 'center' });
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
    const copyButton = createElement('button', { className: 'secondary-button', text: 'Copy command', type: 'button' });
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
  const codexButton = createElement('button', { className: 'primary-button', text: 'Continue in Codex', type: 'button' });
  codexButton.addEventListener('click', () => createHandoff(session, 'codex', '', codexButton));
  actionArea.append(codexButton);

  dashboardState.accounts.forEach((account) => {
    const button = createElement('button', {
      className: 'secondary-button',
      text: `Continue in ${safeText(account.email, 'Claude')}`,
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

  const actionDisclosure = createElement('details', { className: 'checkpoint-actions' }, [
    createElement('summary', { text: 'Actions' }),
    actionArea,
  ]);

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
    actionDisclosure,
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
  renderNewSessionForm();
  renderRemoteSessions();
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
      remoteSessions: asArray(payload?.remoteSessions),
      runningCount: Number(payload?.runningCount) || 0,
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

function toggleForm(form, button, focusTarget, closedLabel = 'Add') {
  const willShow = form.hidden;
  form.hidden = !willShow;
  button.setAttribute('aria-expanded', String(willShow));
  button.textContent = willShow ? 'Close' : closedLabel;
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

elements.showRemoteSessionForm.addEventListener('click', () => toggleForm(elements.remoteSessionForm, elements.showRemoteSessionForm, elements.remoteSessionAccount, 'New'));
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

elements.remoteSessionAccount.addEventListener('change', renderConversationSelect);
elements.remoteSessionWorkspace.addEventListener('change', renderConversationSelect);

elements.remoteSessionForm.addEventListener('submit', async (event) => {
  event.preventDefault();
  const accountId = elements.remoteSessionAccount.value;
  const workspaceId = elements.remoteSessionWorkspace.value;
  if (!accountId) {
    showNotice('Add and authenticate a Claude account first.', 'error', true);
    return;
  }
  if (!workspaceId) {
    showNotice('Add a workspace under Settings first.', 'error', true);
    return;
  }
  const button = elements.remoteSessionForm.querySelector('button[type="submit"]');
  const payload = await runMutation(button, '/api/remote-sessions', {
    method: 'POST',
    body: {
      accountId,
      workspaceId,
      resumeSessionId: elements.remoteSessionConversation.value,
      name: elements.remoteSessionName.value.trim(),
    },
  }, 'Session started.');
  if (payload) {
    elements.remoteSessionName.value = '';
    elements.remoteSessionConversation.value = '';
    toggleForm(elements.remoteSessionForm, elements.showRemoteSessionForm, elements.remoteSessionAccount, 'New');
  }
});
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
