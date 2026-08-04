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
    name: '/compress',
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

const hide = () => {
  matches = [];
  selected = 0;
  menu.hidden = true;
  menu.replaceChildren();
  input.setAttribute('aria-expanded', 'false');
  input.setAttribute('aria-activedescendant', '');
};

const accept = (command = matches[selected]) => {
  if (!command) return false;
  input.value = `${command.name} `;
  hide();
  app.autoGrowPrompt?.();
  input.focus();
  return true;
};

const render = () => {
  menu.replaceChildren();
  matches.forEach((command, index) => {
    const option = document.createElement('button');
    option.type = 'button';
    option.id = `slash-command-${index}`;
    option.className = 'slash-command-option';
    option.setAttribute('role', 'option');
    option.setAttribute('aria-selected', String(index === selected));
    option.classList.toggle('selected', index === selected);

    const name = document.createElement('span');
    name.className = 'slash-command-name';
    name.textContent = command.displayName || command.name;
    const description = document.createElement('span');
    description.className = 'slash-command-description';
    description.textContent = command.description;
    option.append(name, description);

    option.addEventListener('mousedown', (event) => event.preventDefault());
    option.addEventListener('click', () => accept(command));
    option.addEventListener('mousemove', () => {
      if (selected === index) return;
      selected = index;
      render();
    });
    menu.append(option);
  });
  menu.hidden = matches.length === 0;
  input.setAttribute('aria-expanded', String(matches.length > 0));
  input.setAttribute('aria-activedescendant', matches.length > 0 ? `slash-command-${selected}` : '');
};

const update = () => {
  const value = String(input.value || '');
  if (!/^\/[^\s]*$/.test(value)) {
    hide();
    return;
  }
  const query = value.toLowerCase();
  matches = commands.filter((command) => (
    command.name.startsWith(query)
    && (
      !app.state?.streaming
      || command.skill?.execution === 'isolated'
      || command.name === '/side'
    )
  ));
  selected = 0;
  render();
};

const consume = (event) => {
  event.preventDefault();
  event.stopImmediatePropagation();
};

input.addEventListener('input', update);
input.addEventListener('blur', hide);
input.addEventListener('keydown', (event) => {
  if (menu.hidden || matches.length === 0 || event.isComposing) return;
  if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
    consume(event);
    const offset = event.key === 'ArrowDown' ? 1 : -1;
    selected = (selected + offset + matches.length) % matches.length;
    render();
    return;
  }
  if (event.key === 'Tab' || (event.key === 'Enter' && !event.shiftKey)) {
    consume(event);
    accept();
    return;
  }
  if (event.key === 'Escape') {
    consume(event);
    hide();
  }
});

Object.assign(app, {
  hideSlashCommands: hide,
  updateSlashCommands: update,
  setSkillCommands,
  refreshSkillCommands,
  matchSkillInvocation,
});
})();
