(() => {
'use strict';

const app = window.TermLLMApp;
const { elements } = app;
const input = elements.promptInput;
const menu = elements.slashCommandMenu;

const builtInCommands = [
  {
    name: '/compact',
    description: 'Compress the active conversation context',
  },
  {
    name: '/goal',
    description: 'Set or manage the session goal',
  },
  {
    name: '/mcp',
    description: 'Manage MCP servers for this conversation',
  },
  {
    name: '/model',
    description: 'Choose the provider and model',
  },
  {
    name: '/new',
    description: 'Start a new conversation',
  },
  {
    name: '/redo',
    description: 'Restore the turn removed by /undo',
  },
  {
    name: '/side',
    description: 'Ask without interrupting the main response',
    streamingSafe: true,
  },
  {
    name: '/tree',
    description: 'Browse conversation paths or branch from an earlier turn',
    streamingSafe: true,
  },
  {
    name: '/undo',
    description: 'Remove the latest user turn and everything after it',
  },
].sort((a, b) => a.name.localeCompare(b.name));

let skillCommands = [];
let commands = [...builtInCommands];
let skillRefreshGeneration = 0;
const builtInNames = new Set(builtInCommands.map((command) => command.name));

const rebuildCommands = () => {
  commands = [...builtInCommands, ...skillCommands]
    .sort((a, b) => a.name.localeCompare(b.name));
};

const setSkillCommands = (payload) => {
  const rows = Array.isArray(payload) ? payload : (Array.isArray(payload?.skills) ? payload.skills : []);
  const seen = new Set();
  skillCommands = [];
  rows.forEach((skill) => {
    const bareName = String(skill?.name || '').trim();
    if (!/^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$/.test(bareName) || skill?.collides_with_builtin) return;
    const name = `/${bareName}`;
    if (builtInNames.has(name) || seen.has(name)) return;
    seen.add(name);
    const hint = String(skill?.argument_hint || '').trim();
    const source = String(skill?.source || 'skill').trim();
    const isolated = skill?.execution === 'isolated';
    const markers = [`skill:${source}`];
    if (isolated) markers.push('isolated');
    skillCommands.push({
      type: 'slash',
      name,
      displayName: hint ? `${name} ${hint}` : name,
      description: `${String(skill?.description || '').trim()} · ${markers.join(' · ')}`,
      skill: {
        name: bareName,
        execution: isolated ? 'isolated' : 'main',
        source,
        argumentHint: hint,
      },
    });
  });
  rebuildCommands();
  update();
};

const matchSkillInvocation = (value) => {
  const match = String(value || '').match(/^\/([a-z0-9](?:[a-z0-9-]*[a-z0-9])?)(?:\s+([\s\S]*))?$/);
  if (!match) return null;
  const command = skillCommands.find((entry) => entry.skill?.name === match[1]);
  if (!command) return null;
  return {
    ...command.skill,
    arguments: match[2] || '',
    invocation: String(value || ''),
  };
};

const refreshSkillCommands = async (sessionId) => {
  const generation = ++skillRefreshGeneration;
  const id = String(sessionId || '').trim();
  if (!id || typeof fetch !== 'function') {
    if (generation === skillRefreshGeneration) setSkillCommands([]);
    return [];
  }
  const prefix = app.UI_PREFIX || '/ui';
  const headers = typeof app.requestHeaders === 'function'
    ? app.requestHeaders(id)
    : { 'Content-Type': 'application/json', session_id: id };
  try {
    const response = await app.apiFetch(`${prefix}/v1/sessions/${encodeURIComponent(id)}/skills`, { headers });
    if (!response.ok) {
      if (generation === skillRefreshGeneration) setSkillCommands([]);
      return [];
    }
    const payload = await response.json();
    if (generation !== skillRefreshGeneration) return [];
    setSkillCommands(payload);
    app.reconcileSkillRuns?.(id, payload?.runs);
    return skillCommands.map((entry) => ({ ...entry.skill }));
  } catch (error) {
    if (generation === skillRefreshGeneration) setSkillCommands([]);
    console.warn('Failed to refresh skills', error);
    return [];
  }
};

let matches = [];
let selected = 0;
let mode = '';
let mentionToken = null;
let mentionSourceValue = '';
let mentionSourceCursor = 0;
let mentionGeneration = 0;
let mentionTimer = null;
let mentionAbortController = null;
let mentionAcceptable = false;

const cancelMentionRequest = () => {
  mentionGeneration += 1;
  if (mentionTimer !== null) {
    clearTimeout(mentionTimer);
    mentionTimer = null;
  }
  mentionAbortController?.abort();
  mentionAbortController = null;
  mentionAcceptable = false;
  mentionToken = null;
};

const hide = (cancelRequest = true) => {
  if (cancelRequest) cancelMentionRequest();
  matches = [];
  selected = 0;
  mode = '';
  menu.hidden = true;
  menu.replaceChildren();
  input.setAttribute('aria-expanded', 'false');
  input.setAttribute('aria-activedescendant', '');
};

const inputCursor = (value) => {
  const cursor = Number.isInteger(input.selectionStart) ? input.selectionStart : value.length;
  return Math.max(0, Math.min(cursor, value.length));
};

const accept = (item = matches[selected]) => {
  if (!item) return false;
  if (item.type === 'mention') {
    if (!mentionAcceptable || !mentionToken) return false;
    const value = String(input.value || '');
    const cursor = inputCursor(value);
    if (value !== mentionSourceValue || cursor !== mentionSourceCursor) return false;
    const before = value.slice(0, mentionToken.start_utf16);
    const after = value.slice(mentionToken.end_utf16);
    const separator = after === '' || !/^\s/u.test(after) ? ' ' : '';
    input.value = before + item.insertText + separator + after;
    const nextCursor = before.length + item.insertText.length + separator.length;
    input.selectionStart = nextCursor;
    input.selectionEnd = nextCursor;
  } else {
    input.value = `${item.name} `;
    input.selectionStart = input.value.length;
    input.selectionEnd = input.value.length;
  }
  hide();
  app.autoGrowPrompt?.();
  input.focus();
  return true;
};

const render = () => {
  menu.replaceChildren();
  matches.forEach((item, index) => {
    const option = document.createElement('button');
    option.type = 'button';
    option.id = `composer-completion-${index}`;
    option.className = 'slash-command-option';
    option.setAttribute('role', 'option');
    option.setAttribute('aria-selected', String(index === selected));
    option.classList.toggle('selected', index === selected);

    const name = document.createElement('span');
    name.className = 'slash-command-name';
    if (item.type === 'mention' && Array.isArray(item.segments) && item.segments.length > 0) {
      item.segments.forEach((segment) => {
        const span = document.createElement('span');
        span.textContent = String(segment?.text || '');
        if (segment?.matched) span.className = 'mention-completion-match';
        name.append(span);
      });
      if (item.kind === 'directory') name.append(document.createTextNode('/'));
    } else {
      name.textContent = item.displayName || item.name;
    }
    const description = document.createElement('span');
    description.className = 'slash-command-description';
    description.textContent = item.description;
    option.append(name, description);

    option.addEventListener('mousedown', (event) => event.preventDefault());
    option.addEventListener('click', () => accept(item));
    option.addEventListener('mousemove', () => {
      if (selected === index) return;
      selected = index;
      render();
    });
    menu.append(option);
  });
  menu.hidden = matches.length === 0;
  input.setAttribute('aria-expanded', String(matches.length > 0));
  input.setAttribute('aria-activedescendant', matches.length > 0 ? `composer-completion-${selected}` : '');
};

const plausibleMentionAtCursor = (value, cursor) => {
  const before = value.slice(0, cursor);
  const line = before.slice(before.lastIndexOf('\n') + 1);
  return /(?:^|[\s。 、？！])@(?:"(?:\\.|[^"\\])*|[^\s"]*)$/u.test(line);
};

const mentionRequestContext = () => {
  const draft = Boolean(app.state?.draftSessionActive);
  const session = !draft && typeof app.getActiveSession === 'function' ? app.getActiveSession() : null;
  return {
    sessionId: String(session?.id || '').trim(),
    worktreeDir: String(session?.worktreeDir || (draft ? app.state?.selectedWorktreeDir : '') || '').trim(),
  };
};

const scheduleMentionUpdate = (value, cursor) => {
  const generation = ++mentionGeneration;
  if (mentionTimer !== null) clearTimeout(mentionTimer);
  mentionAbortController?.abort();
  mentionAbortController = null;
  mentionAcceptable = false;
  mentionToken = null;
  if (mode !== 'mention') {
    matches = [];
    selected = 0;
    mode = 'mention';
    render();
  }
  mentionTimer = setTimeout(async () => {
    mentionTimer = null;
    if (generation !== mentionGeneration || typeof app.apiFetch !== 'function') return;
    const context = mentionRequestContext();
    const headers = typeof app.requestHeaders === 'function'
      ? app.requestHeaders(context.sessionId)
      : { 'Content-Type': 'application/json', ...(context.sessionId ? { session_id: context.sessionId } : {}) };
    mentionAbortController = typeof AbortController === 'function' ? new AbortController() : null;
    const sourceValue = value;
    const sourceCursor = cursor;
    try {
      const response = await app.apiFetch(`${app.UI_PREFIX || '/ui'}/v1/mentions/search`, {
        method: 'POST',
        headers,
        body: JSON.stringify({
          text: sourceValue,
          cursor_utf16: sourceCursor,
          limit: 10,
          worktree_dir: context.worktreeDir,
        }),
        ...(mentionAbortController ? { signal: mentionAbortController.signal } : {}),
      }, app.API_FETCH_POLICY?.safeRead ? { policy: app.API_FETCH_POLICY.safeRead } : undefined);
      if (!response.ok) throw new Error(`mention search failed (${response.status})`);
      const payload = await response.json();
      const currentValue = String(input.value || '');
      const currentCursor = inputCursor(currentValue);
      if (generation !== mentionGeneration || currentValue !== sourceValue || currentCursor !== sourceCursor) return;
      if (!payload?.active || !payload?.token) {
        hide(false);
        return;
      }
      mode = 'mention';
      mentionToken = payload.token;
      mentionSourceValue = sourceValue;
      mentionSourceCursor = sourceCursor;
      mentionAcceptable = true;
      matches = (Array.isArray(payload.items) ? payload.items : []).map((item) => ({
        type: 'mention',
        name: String(item?.path || ''),
        description: item?.kind === 'directory' ? 'directory' : 'file',
        kind: item?.kind === 'directory' ? 'directory' : 'file',
        insertText: String(item?.insert_text || ''),
        segments: Array.isArray(item?.segments) ? item.segments : [],
      })).filter((item) => item.name && item.insertText);
      selected = 0;
      render();
    } catch (error) {
      if (generation !== mentionGeneration || error?.name === 'AbortError') return;
      hide(false);
      console.warn('Failed to search project mentions', error);
    } finally {
      if (generation === mentionGeneration) mentionAbortController = null;
    }
  }, 50);
};

const update = () => {
  const value = String(input.value || '');
  const cursor = inputCursor(value);
  if (/^\/[^\s]*$/.test(value) && cursor === value.length) {
    cancelMentionRequest();
    const query = value.toLowerCase();
    mode = 'slash';
    matches = commands.filter((command) => (
      command.name.startsWith(query)
      && (
        !app.state?.streaming
        || command.skill?.execution === 'isolated'
        || command.streamingSafe === true
      )
    )).map((command) => ({ type: 'slash', ...command }));
    selected = 0;
    render();
    return;
  }
  if (plausibleMentionAtCursor(value, cursor)) {
    scheduleMentionUpdate(value, cursor);
    return;
  }
  hide();
};

const invalidateMentionCompletions = () => {
  cancelMentionRequest();
  if (mode === 'mention') hide(false);
};

const consume = (event) => {
  event.preventDefault();
  event.stopImmediatePropagation();
};

input.addEventListener('input', update);
input.addEventListener('click', update);
input.addEventListener('keyup', (event) => {
  const key = typeof event.key === 'string' ? event.key : '';
  if (!menu.hidden && (key === 'ArrowUp' || key === 'ArrowDown')) return;
  if (key.startsWith('Arrow') || key === 'Home' || key === 'End') update();
});
input.addEventListener('blur', () => hide());
input.addEventListener('keydown', (event) => {
  if (menu.hidden || matches.length === 0 || event.isComposing) return;
  const key = typeof event.key === 'string' ? event.key : '';
  if (key === 'ArrowDown' || key === 'ArrowUp') {
    consume(event);
    const offset = key === 'ArrowDown' ? 1 : -1;
    selected = (selected + offset + matches.length) % matches.length;
    render();
    return;
  }
  if (mode === 'mention' && !mentionAcceptable) {
    if (key === 'Tab') consume(event);
    if (key === 'Escape') {
      consume(event);
      hide();
    }
    return;
  }
  if (key === 'Enter' && !event.shiftKey && mode === 'slash') {
    const command = matches[selected];
    if (command && String(input.value || '').trim().toLowerCase() === String(command.name || '').toLowerCase()) {
      hide();
      return;
    }
  }
  if (key === 'Enter' && !event.shiftKey && mode === 'mention' && !String(mentionToken?.query || '')) {
    return;
  }
  if (key === 'Tab' || (key === 'Enter' && !event.shiftKey)) {
    consume(event);
    accept();
    return;
  }
  if (key === 'Escape') {
    consume(event);
    hide();
  }
});

Object.assign(app, {
  hideSlashCommands: hide,
  updateSlashCommands: update,
  invalidateMentionCompletions,
  setSkillCommands,
  refreshSkillCommands,
  matchSkillInvocation,
});
})();
