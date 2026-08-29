import { useEffect, useLayoutEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { errorMessage } from '../domain/text';
import { skillExecutionDescription, skillExecutionLabel } from '../domain/completions';
import type { ApprovalPrompt, AskUserPrompt, Widget } from '../domain/types';
import { Icon } from './Icon';
import { Overlay } from './Overlay';
import { Markdown } from './Markdown';
import { ProjectAssignment } from './ProjectAssignment';
import { Worktrees } from './Worktrees';

function Settings() {
  const store = useStore();
  const [token, setToken] = useState(store.token.value);
  const [provider, setProvider] = useState(store.selectedProvider.value);
  const [model, setModel] = useState(store.selectedModel.value);
  const [effort, setEffort] = useState(store.selectedEffort.value);
  const [reasoning, setReasoning] = useState(store.selectedReasoningMode.value);
  const [agent, setAgent] = useState(store.selectedAgent.value);
  const save = () => {
    store.setPreference('provider', provider);
    store.setPreference('model', model);
    store.setPreference('effort', effort);
    store.setPreference('reasoning', reasoning);
    store.setPreference('agent', agent);
    store.saveSettings(token);
  };
  return (
    <Overlay title="Settings" close={!store.authRequired.value}>
      <div class="settings-field">
        <label class="settings-label" for="providerSelect">
          Provider
        </label>
        <select
          class="settings-select"
          id="providerSelect"
          value={provider}
          onChange={(event) => {
            setProvider(event.currentTarget.value);
            setModel('');
            void store.loadModels(event.currentTarget.value).catch(() => undefined);
          }}
        >
          <option value="">Auto (server default)</option>
          {store.providers.value.map((entry) => (
            <option value={entry.id} key={entry.id}>
              {entry.name}
            </option>
          ))}
        </select>
      </div>
      <div class="settings-field">
        <label class="settings-label" for="modelSelect">
          Model
        </label>
        <select
          class="settings-select"
          id="modelSelect"
          value={model}
          onChange={(event) => setModel(event.currentTarget.value)}
        >
          <option value="">Auto (server default)</option>
          {store.models.value.map((entry) => (
            <option value={entry.id} key={entry.id}>
              {entry.name || entry.id}
            </option>
          ))}
        </select>
      </div>
      <div class="settings-field">
        <label class="settings-label" for="effortSelect">
          Effort
        </label>
        <select
          class="settings-select"
          id="effortSelect"
          value={effort}
          onChange={(event) => setEffort(event.currentTarget.value)}
        >
          {['', 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'].map((value) => (
            <option value={value} key={value}>
              {value || 'Auto (server default)'}
            </option>
          ))}
        </select>
      </div>
      <div class="settings-field">
        <label class="settings-label" for="reasoningModeSelect">
          Reasoning mode
        </label>
        <select
          class="settings-select"
          id="reasoningModeSelect"
          value={reasoning}
          onChange={(event) => setReasoning(event.currentTarget.value)}
        >
          <option value="standard">Standard</option>
          <option value="pro">Pro</option>
        </select>
      </div>
      {store.config.agentNames.length > 1 && (
        <div class="settings-field">
          <label class="settings-label" for="agentSelect">
            Agent
          </label>
          <select
            class="settings-select"
            id="agentSelect"
            value={agent}
            onChange={(event) => setAgent(event.currentTarget.value)}
          >
            <option value="">Default</option>
            {store.config.agentNames.map((name) => (
              <option value={name} key={name}>
                {name}
              </option>
            ))}
          </select>
        </div>
      )}
      <div class="settings-field">
        <label class="settings-label" for="authTokenInput">
          Bearer token
        </label>
        <input
          id="authTokenInput"
          type="password"
          value={token}
          placeholder="paste your bearer token"
          autoComplete="off"
          onInput={(event) => setToken(event.currentTarget.value)}
        />
      </div>
      <div class="settings-field">
        <label class="settings-toggle">
          <span class="settings-label settings-label-inline">Show hidden sessions</span>
          <input
            type="checkbox"
            checked={store.showHidden.value}
            onChange={(event) => {
              store.showHidden.value = event.currentTarget.checked;
              store.storage.setItem(
                store.keys.showHiddenSessions,
                event.currentTarget.checked ? '1' : '0',
              );
              void store.refreshSidebar();
            }}
          />
        </label>
      </div>
      <div class="settings-field">
        <label class="settings-toggle">
          <span class="settings-label settings-label-inline">Show widgets in sidebar</span>
          <input
            type="checkbox"
            checked={store.showWidgets.value}
            onChange={(event) => {
              store.showWidgets.value = event.currentTarget.checked;
              store.storage.setItem(
                store.keys.showWidgetsSidebar,
                event.currentTarget.checked ? '1' : '0',
              );
            }}
          />
        </label>
      </div>
      <div class="settings-field notification-settings">
        <span class="settings-label">Notifications</span>
        <div
          class={`notification-state notification-state-${store.notifications.value.status}`}
          role="status"
          aria-live="polite"
        >
          <strong>
            {store.notifications.value.status === 'subscribed'
              ? store.notifications.value.verified
                ? 'Enabled'
                : 'Enabled · verification pending'
              : store.notifications.value.status === 'blocked'
                ? 'Blocked'
                : store.notifications.value.status === 'stale'
                  ? 'Needs repair'
                  : store.notifications.value.status === 'unsubscribed'
                    ? 'Not enabled'
                    : 'Unavailable'}
          </strong>
          <span>{store.notifications.value.detail}</span>
        </div>
        <div class="notification-actions">
          {store.notifications.value.status === 'unsubscribed' && (
            <button
              type="button"
              class="btn"
              disabled={store.notifications.value.busy}
              onClick={() => void store.enableNotifications()}
            >
              Enable notifications
            </button>
          )}
          {store.notifications.value.status === 'stale' && (
            <button
              type="button"
              class="btn"
              disabled={store.notifications.value.busy}
              onClick={() => void store.retryNotifications()}
            >
              Retry repair
            </button>
          )}
          {(store.notifications.value.status === 'subscribed' ||
            store.notifications.value.status === 'stale') && (
            <button
              type="button"
              class="btn"
              disabled={store.notifications.value.busy}
              onClick={() => void store.disableNotifications()}
            >
              Disable
            </button>
          )}
        </div>
      </div>
      <div class="modal-actions">
        {!store.authRequired.value && (
          <button
            class="btn"
            type="button"
            onClick={() => {
              store.modal.value = '';
            }}
          >
            Cancel
          </button>
        )}
        <button class="btn primary" type="button" onClick={save}>
          Save
        </button>
      </div>
    </Overlay>
  );
}

function Rename() {
  const store = useStore();
  const target = store.renameTarget.value;
  const [generatedMode, setGeneratedMode] = useState(false);
  const [name, setName] = useState(target?.name || target?.title || '');
  const [generatedTitle, setGeneratedTitle] = useState(
    target?.generatedShortTitle || target?.title || '',
  );
  const [generatedDetail, setGeneratedDetail] = useState(
    target?.generatedLongTitle || target?.longTitle || '',
  );
  const [improving, setImproving] = useState(false);
  const [saving, setSaving] = useState(false);
  const [error, setError] = useState('');
  if (!target) return null;
  const close = () => {
    store.renameTarget.value = null;
    store.modal.value = '';
  };
  const improve = async () => {
    if (improving || saving) return;
    setGeneratedMode(true);
    setImproving(true);
    setError('');
    try {
      const suggestion = await store.improveTitle();
      setGeneratedTitle(suggestion.title);
      setGeneratedDetail(suggestion.detail);
    } catch (value) {
      setError(value instanceof Error ? value.message : 'Failed to improve title.');
    } finally {
      setImproving(false);
    }
  };
  const save = async () => {
    if (saving || improving) return;
    setSaving(true);
    setError('');
    try {
      await store.renameSession(
        generatedMode
          ? { generatedShortTitle: generatedTitle, generatedLongTitle: generatedDetail }
          : { name },
      );
    } catch (value) {
      setError(value instanceof Error ? value.message : 'Failed to rename session.');
      setSaving(false);
    }
  };
  return (
    <Overlay title="Rename session" close={false} onEscape={close} className="rename-session-modal">
      <p>
        {generatedMode
          ? 'Review the AI suggestion before saving it as this session title.'
          : 'Choose the label shown in the sidebar, or let AI suggest a better title from this session.'}
      </p>
      {!generatedMode ? (
        <div class="settings-field">
          <label class="settings-label" for="renameSessionInput">
            Session name
          </label>
          <input
            id="renameSessionInput"
            autoFocus
            autoComplete="off"
            value={name}
            placeholder={target.title || 'Project kickoff notes'}
            onInput={(event) => setName(event.currentTarget.value)}
          />
        </div>
      ) : (
        <div class="rename-generated-fields">
          <div class="settings-field">
            <label class="settings-label" for="renameGeneratedTitleInput">
              Title
            </label>
            <input
              id="renameGeneratedTitleInput"
              autoFocus
              autoComplete="off"
              value={generatedTitle}
              placeholder="vLLM provider docs review"
              onInput={(event) => setGeneratedTitle(event.currentTarget.value)}
            />
          </div>
          <div class="settings-field">
            <label class="settings-label" for="renameGeneratedDetailInput">
              Detail
            </label>
            <textarea
              id="renameGeneratedDetailInput"
              rows={3}
              value={generatedDetail}
              placeholder="A longer description for the conversation"
              onInput={(event) => setGeneratedDetail(event.currentTarget.value)}
            />
          </div>
          <p class="rename-generated-note">Saving will use this generated title in the sidebar.</p>
        </div>
      )}
      <button
        class={`btn rename-improve-btn ${improving ? 'is-loading' : ''}`}
        type="button"
        disabled={improving || saving}
        onClick={() => void improve()}
      >
        {improving
          ? 'Improving title…'
          : generatedMode
            ? 'Try again with AI'
            : 'Improve title with AI'}
      </button>
      <div class="modal-error" role={error ? 'alert' : undefined}>
        {error}
      </div>
      <div class="modal-actions">
        <button class="btn" type="button" disabled={saving} onClick={close}>
          Cancel
        </button>
        <button
          class="btn primary"
          type="button"
          disabled={saving || improving}
          onClick={() => void save()}
        >
          {saving ? 'Saving…' : 'Save'}
        </button>
      </div>
    </Overlay>
  );
}

function AskUser({ interactionPrompt }: { interactionPrompt?: AskUserPrompt }) {
  const store = useStore();
  const prompt = interactionPrompt || store.askUser.value;
  const [answers, setAnswers] = useState<Record<number, string[]>>({});
  const [custom, setCustom] = useState<Record<number, string>>({});
  const [tab, setTab] = useState(0);
  const [error, setError] = useState('');
  const [sending, setSending] = useState(false);
  if (!prompt) return null;
  const question = prompt.questions[tab];
  const validate = (item: (typeof prompt.questions)[number], index: number) => {
    const selected = answers[index] || [];
    const own = custom[index]?.trim();
    if (item.multi_select && !selected.length)
      throw new Error(`${item.header || `Question ${index + 1}`}: choose at least one option.`);
    if (!item.multi_select && !selected.length && !own)
      throw new Error(`${item.header || `Question ${index + 1}`}: choose or enter an answer.`);
    return {
      question_index: index,
      header: item.header,
      selected: own || selected.join(', '),
      selected_list: item.multi_select ? selected : undefined,
      is_custom: Boolean(own),
      is_multi_select: Boolean(item.multi_select),
    };
  };
  const submit = async (cancelled = false) => {
    setError('');
    setSending(true);
    try {
      await store.answerAskUser(cancelled ? [] : prompt.questions.map(validate), cancelled, prompt);
    } catch (value) {
      setError(errorMessage(value));
    } finally {
      setSending(false);
    }
  };
  const dismiss = () => {
    if (tab > 0) setTab(tab - 1);
    else store.dismissInteraction('ask-user', prompt);
  };
  const next = () => {
    try {
      validate(question, tab);
      setError('');
      setTab(tab + 1);
    } catch (value) {
      setError(errorMessage(value));
    }
  };
  return (
    <Overlay
      title={
        prompt.questions.length > 1
          ? `Question ${tab + 1} of ${prompt.questions.length}`
          : 'Answer question'
      }
      close={false}
      onEscape={() => {
        if (!sending) dismiss();
      }}
    >
      <p>The agent needs your input to continue.</p>
      {prompt.questions.length > 1 && (
        <div class="ask-user-steps">
          {prompt.questions.map((_item, index) => (
            <button
              class={`ask-user-step ${index === tab ? 'active' : index < tab ? 'completed' : ''}`}
              disabled={sending}
              onClick={() => setTab(index)}
            >
              {index + 1}
            </button>
          ))}
        </div>
      )}
      <fieldset class="ask-user-question" disabled={sending} aria-busy={sending}>
        <legend class="ask-user-question-text">
          {question.header && <strong>{question.header}: </strong>}
          {question.question}
        </legend>
        {question.options?.map((option) => (
          <label class="ask-user-option" key={option.label}>
            <input
              type={question.multi_select ? 'checkbox' : 'radio'}
              name={`question-${tab}`}
              value={option.label}
              checked={(answers[tab] || []).includes(option.label)}
              onChange={(event) => {
                const current = answers[tab] || [];
                setAnswers({
                  ...answers,
                  [tab]: question.multi_select
                    ? event.currentTarget.checked
                      ? [...current, option.label]
                      : current.filter((value) => value !== option.label)
                    : [option.label],
                });
                setCustom({ ...custom, [tab]: '' });
              }}
            />
            <span>
              <strong>{option.label}</strong>
              {option.description && <small>{option.description}</small>}
            </span>
          </label>
        ))}
        {!question.multi_select && (
          <label class="ask-user-custom">
            <span>Other</span>
            <textarea
              placeholder="Type your answer…"
              value={custom[tab] || ''}
              onInput={(event) => {
                setCustom({ ...custom, [tab]: event.currentTarget.value });
                setAnswers({ ...answers, [tab]: [] });
              }}
            />
          </label>
        )}
      </fieldset>
      {error && <div class="modal-error">{error}</div>}
      <div class="modal-actions">
        <button class="btn" disabled={sending} onClick={dismiss}>
          {tab > 0 ? 'Back' : 'Dismiss'}
        </button>
        {tab === 0 && (
          <button class="btn danger" disabled={sending} onClick={() => void submit(true)}>
            {sending ? 'Cancelling…' : 'Cancel agent request'}
          </button>
        )}
        {tab < prompt.questions.length - 1 ? (
          <button class="btn primary" disabled={sending} onClick={next}>
            Next
          </button>
        ) : (
          <button class="btn primary" disabled={sending} onClick={() => void submit()}>
            {sending ? 'Sending…' : 'Continue'}
          </button>
        )}
      </div>
    </Overlay>
  );
}

function Approval({ interactionPrompt }: { interactionPrompt?: ApprovalPrompt }) {
  const store = useStore();
  const prompt = interactionPrompt || store.approval.value;
  const options = prompt?.options || [];
  const deny =
    options.find((option) => option.choice === 'deny')?.index ?? options.at(-1)?.index ?? 0;
  const [choice, setChoice] = useState(
    options.find((option) => option.choice !== 'deny')?.index ?? 0,
  );
  const [resume, setResume] = useState(false);
  const [error, setError] = useState('');
  const [sending, setSending] = useState(false);
  if (!prompt) return null;
  const decide = async (selected: number, cancelled = false) => {
    if (sending) return;
    setSending(true);
    setError('');
    try {
      await store.decideApproval(selected, resume, prompt, cancelled);
    } catch (value) {
      setError(errorMessage(value));
    } finally {
      setSending(false);
    }
  };
  return (
    <Overlay
      title={prompt.title || 'Access Request'}
      close={false}
      onEscape={() => store.dismissInteraction('approval', prompt)}
    >
      {prompt.intro && <div class="approval-intro">{prompt.intro}</div>}
      {prompt.path && <code class="approval-path">{prompt.path}</code>}
      <div class="approval-body">{prompt.body}</div>
      {options
        .filter((option) => option.choice !== 'deny')
        .map((option) => (
          <label class="approval-option">
            <input
              type="radio"
              name="approval"
              checked={choice === option.index}
              disabled={sending}
              onChange={() => setChoice(option.index)}
            />
            <span>
              {option.label || option.title || option.choice}
              {option.description && <small>{option.description}</small>}
            </span>
          </label>
        ))}
      {prompt.resumeAutoAvailable && (
        <label>
          <input
            type="checkbox"
            checked={resume}
            disabled={sending}
            onChange={(event) => setResume(event.currentTarget.checked)}
          />{' '}
          Resume Guardian auto-approval
        </label>
      )}
      {prompt.note && <div class="approval-note">{prompt.note}</div>}
      {error && <div class="modal-error">{error}</div>}
      <div class="modal-actions">
        <button
          class="btn"
          disabled={sending}
          onClick={() => store.dismissInteraction('approval', prompt)}
        >
          Dismiss
        </button>
        <button class="btn" disabled={sending} onClick={() => void decide(choice, true)}>
          Cancel request
        </button>
        <button class="btn" disabled={sending} onClick={() => void decide(deny)}>
          {sending ? 'Submitting…' : 'Deny'}
        </button>
        <button class="btn primary" disabled={sending} onClick={() => void decide(choice)}>
          {sending ? 'Submitting…' : 'Approve'}
        </button>
      </div>
    </Overlay>
  );
}

function MCP() {
  const store = useStore();
  const state = store.mcp.value;
  const [query, setQuery] = useState('');
  const normalizedQuery = query.trim().toLocaleLowerCase();
  const visibleServers = normalizedQuery
    ? state.servers.filter((server) => server.name.toLocaleLowerCase().includes(normalizedQuery))
    : state.servers;
  const enabledCount = state.enabled.length;
  const disabled = state.loading || Boolean(state.pending) || store.streaming.value;
  const serverErrors = state.servers.filter((server) => server.error);
  const hasErrors = !state.loading && (Boolean(state.error) || serverErrors.length > 0);
  const subtitle = (server: (typeof state.servers)[number]): string => {
    const status = server.status.toLocaleLowerCase();
    if (!server.configured) return 'Not found in your MCP configuration';
    if (status === 'ready') {
      if (server.active || server.deferred) {
        const parts = [];
        if (server.active) parts.push(`${server.active} active`);
        if (server.deferred) parts.push(`${server.deferred} deferred`);
        return `${server.tools} tool${server.tools === 1 ? '' : 's'} · ${parts.join(', ')}`;
      }
      return `${server.tools} tool${server.tools === 1 ? '' : 's'} available`;
    }
    if (status === 'starting') return 'Starting server…';
    if (server.error) return 'Failed to start';
    return '';
  };
  return (
    <Overlay title="MCP servers" className="mcp-modal">
      <div class="mcp-modal-intro">
        <p class="mcp-modal-subtitle">Turn on servers to add their tools.</p>
        {!state.loading && state.servers.length > 0 && (
          <span
            class="mcp-server-summary"
            aria-label={`${enabledCount} ${enabledCount === 1 ? 'server' : 'servers'} enabled`}
          >
            {enabledCount} of {state.servers.length} on
          </span>
        )}
      </div>
      {!state.loading && state.servers.length > 0 && (
        <label class="mcp-server-search">
          <span class="mcp-server-search-icon" aria-hidden="true">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8">
              <circle cx="11" cy="11" r="7" />
              <path d="m16.5 16.5 4 4" />
            </svg>
          </span>
          <input
            type="search"
            aria-label="Filter MCP servers"
            value={query}
            placeholder="Filter servers…"
            onInput={(event) => setQuery(event.currentTarget.value)}
          />
        </label>
      )}
      <div class="mcp-server-list" aria-busy={state.loading ? 'true' : undefined}>
        {state.loading ? (
          <div class="mcp-server-loading" role="status">
            Loading configured MCP servers…
          </div>
        ) : state.error && state.servers.length === 0 ? (
          <div class="mcp-server-empty" role="status">
            <strong>Unable to load MCP servers</strong>
            <span>The configured server list could not be read. See the error below.</span>
          </div>
        ) : state.servers.length === 0 ? (
          <div class="mcp-server-empty" role="status">
            <strong>No MCP servers configured</strong>
            <span>
              Add servers to <code>~/.config/term-llm/mcp.json</code>, then try again.
            </span>
          </div>
        ) : visibleServers.length === 0 ? (
          <div class="mcp-server-empty" role="status">
            <strong>No matching servers</strong>
            <span>Try a different name or clear the filter.</span>
          </div>
        ) : (
          visibleServers.map((server) => {
            const checked = state.enabled.includes(server.name);
            const status = server.configured ? server.status.toLocaleLowerCase() : 'failed';
            const statusClass = status.replace(/[^a-z0-9_-]/g, '') || 'stopped';
            return (
              <label
                class="mcp-server-row"
                data-enabled={checked ? 'true' : 'false'}
                key={server.name}
              >
                <span class="mcp-server-icon" aria-hidden="true">
                  <svg
                    viewBox="0 0 24 24"
                    fill="none"
                    stroke="currentColor"
                    stroke-width="1.8"
                    stroke-linecap="round"
                    stroke-linejoin="round"
                  >
                    <rect x="5" y="5" width="5" height="5" rx="1.2" />
                    <rect x="14" y="14" width="5" height="5" rx="1.2" />
                    <path d="M10 7.5h2.5a4 4 0 0 1 4 4V14" />
                    <path d="M14 16.5h-2.5a4 4 0 0 1-4-4V10" />
                  </svg>
                </span>
                <span class="mcp-server-copy">
                  <span class="mcp-server-title-row">
                    <span class="mcp-server-name">{server.name}</span>
                    <span class={`mcp-server-status ${statusClass}`}>
                      {server.configured ? server.status : 'missing'}
                    </span>
                  </span>
                  {subtitle(server) && <span class="mcp-server-subtitle">{subtitle(server)}</span>}
                  {server.refreshWarning && (
                    <span class="mcp-server-warning">{server.refreshWarning}</span>
                  )}
                </span>
                <span class="mcp-switch">
                  <input
                    class="mcp-switch-input"
                    type="checkbox"
                    aria-label={`${checked ? 'Disable' : 'Enable'} ${server.name}`}
                    checked={checked}
                    disabled={disabled}
                    onChange={() => void store.toggleMCP(server.name)}
                  />
                  <span class="mcp-switch-track" aria-hidden="true">
                    <span class="mcp-switch-thumb" />
                  </span>
                </span>
              </label>
            );
          })
        )}
      </div>
      {hasErrors && (
        <section class="mcp-error-panel" role="alert" aria-labelledby="mcp-error-title">
          <div class="mcp-error-header">
            <strong id="mcp-error-title">MCP server error</strong>
            <button class="mcp-retry" type="button" onClick={() => void store.loadMCP()}>
              Retry
            </button>
          </div>
          <div
            class="mcp-error-details"
            role="region"
            aria-label="MCP server error details"
            tabIndex={0}
          >
            {state.error
              ? state.error
              : serverErrors.map((server) => (
                  <div class="mcp-error-entry" key={server.name}>
                    <strong>{server.name}: </strong>
                    {server.error}
                  </div>
                ))}
          </div>
        </section>
      )}
      {(store.streaming.value || state.pending) && !state.error && (
        <div class="mcp-modal-feedback" aria-live="polite">
          {store.streaming.value ? (
            <span>Servers can’t be changed while a response is running.</span>
          ) : (
            <span>Saving {state.pending}…</span>
          )}
        </div>
      )}
    </Overlay>
  );
}
function GoalModal() {
  const store = useStore();
  const current = store.goal.value;
  const [objective, setObjective] = useState(current?.objective || '');
  const [budget, setBudget] = useState(current?.token_budget ? String(current.token_budget) : '');
  const save = () =>
    void store.saveGoal({
      objective: objective.trim(),
      token_budget: Number(budget) || undefined,
      status: 'active',
    });
  return (
    <Overlay title="Session goal">
      <p>Set a persistent objective the agent can keep pursuing across automatic continuations.</p>
      <label class="settings-label">Objective</label>
      <textarea
        class="goal-objective-input"
        rows={5}
        value={objective}
        onInput={(event) => setObjective(event.currentTarget.value)}
      />
      <label class="settings-label">Token budget (optional)</label>
      <input
        type="number"
        min={1}
        value={budget}
        onInput={(event) => setBudget(event.currentTarget.value)}
      />
      <div class="modal-actions goal-actions">
        {current && (
          <button class="btn" onClick={() => void store.saveGoal({ action: 'clear' })}>
            Clear
          </button>
        )}
        {current?.status === 'paused' ? (
          <button class="btn" onClick={() => void store.saveGoal({ action: 'resume' })}>
            Resume
          </button>
        ) : (
          current && (
            <button class="btn" onClick={() => void store.saveGoal({ action: 'pause' })}>
              Pause
            </button>
          )
        )}
        <button class="btn primary" disabled={!objective.trim()} onClick={save}>
          Set goal
        </button>
      </div>
    </Overlay>
  );
}
const WIDGET_STATUS: Record<string, { label: string; tone: string } | undefined> = {
  running: { label: 'Running', tone: 'running' },
  started: { label: 'Running', tone: 'running' },
  starting: { label: 'Starting', tone: 'starting' },
  error: { label: 'Unavailable', tone: 'error' },
};

function widgetStatus(widget: Widget): { label: string; tone: string } | null {
  const state = String(widget.state || 'stopped').toLowerCase();
  if (state === 'stopped') return null;
  return WIDGET_STATUS[state] || { label: state.replace(/[-_]/g, ' '), tone: 'other' };
}

function Widgets() {
  const store = useStore();
  const [query, setQuery] = useState('');
  const widgets = [...store.widgets.value].sort((left, right) =>
    left.name.localeCompare(right.name, undefined, { sensitivity: 'base' }),
  );
  const showSearch = widgets.length > 6;
  const normalizedQuery = query.trim().toLowerCase();
  const visibleWidgets = normalizedQuery
    ? widgets.filter((widget) =>
        [widget.name, widget.description, widget.mount].some((value) =>
          String(value || '')
            .toLowerCase()
            .includes(normalizedQuery),
        ),
      )
    : widgets;
  const countLabel = `${widgets.length} ${widgets.length === 1 ? 'widget' : 'widgets'}`;

  return (
    <Overlay title="Widgets" className="widgets-modal">
      <div class="widgets-modal-intro">
        <p class="widgets-modal-subtitle">Open a local tool without leaving your workspace.</p>
        {widgets.length > 0 && (
          <span class="widgets-modal-summary" aria-label={`${countLabel} available`}>
            {widgets.length} available
          </span>
        )}
      </div>
      {showSearch && (
        <label class="widgets-modal-search">
          <span class="widgets-modal-search-icon" aria-hidden="true">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.8">
              <circle cx="11" cy="11" r="7" />
              <path d="m16 16 4 4" />
            </svg>
          </span>
          <input
            type="search"
            aria-label="Filter widgets"
            value={query}
            placeholder="Find a widget…"
            autoFocus
            onInput={(event) => setQuery(event.currentTarget.value)}
          />
        </label>
      )}
      <div class="widget-grid">
        {widgets.length === 0 ? (
          <div class="widget-empty" role="status">
            <strong>No widgets available</strong>
            <span>Loaded local widgets will appear here.</span>
          </div>
        ) : visibleWidgets.length === 0 ? (
          <div class="widget-empty" role="status">
            <strong>No matching widgets</strong>
            <span>Try a different name or clear the filter.</span>
          </div>
        ) : (
          visibleWidgets.map((widget, index) => {
            const status = widgetStatus(widget);
            const detail =
              widget.description || (widget.mount ? `/${widget.mount}` : 'Local widget');
            return (
              <a
                class="widget-card"
                data-state={status?.tone || 'ready'}
                key={widget.id}
                href={widget.url}
                title={widget.description || widget.name}
                aria-label={`Open ${widget.name}${status ? `, ${status.label}` : ''}`}
                autoFocus={!showSearch && index === 0}
              >
                <span class="widget-card-icon" aria-hidden="true">
                  <Icon name="widgets" />
                </span>
                <span class="widget-card-copy">
                  <span class="widget-card-title-row">
                    <span class="widget-card-name">{widget.name}</span>
                    {status && (
                      <span class="widget-card-status">
                        <span class="widget-status-dot" aria-hidden="true" />
                        {status.label}
                      </span>
                    )}
                  </span>
                  <span class="widget-card-meta">{detail}</span>
                  {status?.tone === 'error' && widget.error && (
                    <span class="widget-card-error">{widget.error}</span>
                  )}
                </span>
                <Icon class="widget-card-chevron" name="chevron-right" />
              </a>
            );
          })
        )}
      </div>
      <div class="widgets-modal-footer">
        <Icon name="info" />
        <span>Local widgets open in this tab.</span>
      </div>
    </Overlay>
  );
}

function BranchContext() {
  const store = useStore();
  const [focus, setFocus] = useState('');
  const [mode, setMode] = useState<'choices' | 'focused'>('choices');
  const anchor = store.branchTarget.value;
  const prefill = store.branchPrefill.value;
  const busy = store.branchBusy.value;
  const error = store.branchError.value;
  const choose = (context: 'clean' | 'notes' | 'focused') => {
    if (busy) return;
    if (context === 'focused' && mode !== 'focused') {
      setMode('focused');
      return;
    }
    if (prefill) void store.branchFrom(anchor, context, focus.trim(), '', prefill);
    else void store.branchFrom(anchor, context, focus.trim());
  };
  return (
    <Overlay
      title="Start a conversation path"
      dismissDisabled={busy}
      onEscape={() => {
        if (!busy)
          if (mode === 'focused') setMode('choices');
          else store.modal.value = '';
      }}
    >
      <p>Choose how much context to carry after this turn.</p>
      <div aria-busy={busy ? 'true' : undefined}>
        {mode === 'choices' ? (
          <div class="branch-context-choices">
            <button type="button" disabled={busy} onClick={() => choose('clean')}>
              <strong>Clean branch</strong>
              <small>Continue only with context up to this turn.</small>
            </button>
            <button type="button" disabled={busy} onClick={() => choose('notes')}>
              <strong>Bring concise notes</strong>
              <small>Prepare a short summary of useful later discoveries.</small>
            </button>
            <button type="button" disabled={busy} onClick={() => choose('focused')}>
              <strong>Focused context</strong>
              <small>Tell the agent which later information matters.</small>
            </button>
          </div>
        ) : (
          <div class="branch-context-focus">
            <label for="branchContextFocus">What should this path carry forward?</label>
            <textarea
              id="branchContextFocus"
              autoFocus
              rows={5}
              value={focus}
              placeholder="For example: preserve the database findings, but not the abandoned UI approach."
              disabled={busy}
              onInput={(event) => setFocus(event.currentTarget.value)}
            />
            <div class="modal-actions">
              <button class="btn" disabled={busy} onClick={() => setMode('choices')}>
                Back
              </button>
              <button
                class="btn primary"
                disabled={busy || !focus.trim()}
                onClick={() => choose('focused')}
              >
                Create path
              </button>
            </div>
          </div>
        )}
      </div>
      {busy && (
        <div role="status" aria-live="polite">
          Creating path…
        </div>
      )}
      {error && (
        <div class="modal-error" role="alert">
          {error}
        </div>
      )}
      <p class="branch-tree-note">Filesystem and tool side effects are not undone.</p>
    </Overlay>
  );
}

function BranchTree() {
  const store = useStore();
  const tree = store.branchTree.value;
  const nodes =
    tree && Array.isArray(tree.nodes) ? (tree.nodes as Array<Record<string, unknown>>) : [];
  const points =
    tree && Array.isArray(tree.branch_points)
      ? (tree.branch_points as Array<Record<string, unknown>>).filter(
          (point) => String(point.role || '') === 'user',
        )
      : [];
  const active = String(tree?.active_session_id || store.activeSessionId.value);
  const root = String(tree?.root_session_id || '');
  return (
    <Overlay title="Conversation paths">
      <div class="branch-tree-list">
        {points.length > 0 && <div class="branch-tree-section-title">Existing paths</div>}
        {nodes.map((node, index) => {
          const id = String(node.session_id || '');
          const current = id === active;
          const session = store.sessions.value.find(
            (entry) =>
              entry.id === id ||
              (node.session_number && entry.number === Number(node.session_number)),
          );
          const content = (
            <div class="branch-tree-item-content">
              <div class="branch-tree-item-title">
                <strong>{String(node.title || session?.title || `Path ${index + 1}`)}</strong>
                {id === root && <span class="project-browser-badge">Origin</span>}
                {current && <span class="project-browser-badge is-added">Current</span>}
              </div>
              {node.anchor_preview && (
                <div class="branch-origin-preview">After “{String(node.anchor_preview)}”</div>
              )}
            </div>
          );
          if (current)
            return (
              <section class="branch-tree-item active" key={id || String(index)}>
                {content}
              </section>
            );
          return (
            <button
              class="branch-tree-item branch-tree-path"
              type="button"
              key={id || String(index)}
              disabled={!id}
              title="Open this path"
              onClick={() => {
                store.modal.value = '';
                if (session) void store.selectSession(session);
                else void store.resolveAndSelectSession(id);
              }}
            >
              {content}
            </button>
          );
        })}
        {points.length > 0 && <div class="branch-tree-section-title">Branch points</div>}
        {points.map((point, index) => {
          const sequence = Math.max(1, Number(point.sequence) + 1 || 1);
          const later = Math.max(0, Number(point.later_message_count) || 0);
          return (
            <button
              class="branch-tree-item branch-tree-point"
              type="button"
              key={String(point.message_id || index)}
              onClick={() =>
                store.openBranchContext(
                  String(Math.max(0, Number(point.anchor_message_id) || 0)),
                  String(point.prefill || ''),
                )
              }
            >
              <div class="branch-tree-item-content">
                <div class="branch-tree-item-title">
                  <strong>Edit: {String(point.preview || '(attachment content)')}</strong>
                </div>
                <small>
                  Message {sequence}
                  {later > 0 ? ` · ${later} later message${later === 1 ? '' : 's'}` : ''}
                </small>
              </div>
            </button>
          );
        })}
      </div>
    </Overlay>
  );
}

function Skills() {
  const store = useStore();
  const [selected, setSelected] = useState('');
  const [args, setArgs] = useState('');
  const selectedSkill = store.skills.value.find(
    (skill) => String(skill.name || skill.id || '') === selected,
  );
  const selectedBlocked = Boolean(
    selectedSkill && selectedSkill.execution !== 'isolated' && store.streaming.value,
  );
  return (
    <Overlay title="Skills">
      <div class="skills-list">
        {store.skills.value.map((skill) => {
          const name = String(skill.name || skill.id || '');
          return (
            <button
              class={`skill-row ${selected === name ? 'selected' : ''}`}
              key={name}
              onClick={() => setSelected(name)}
            >
              <strong>{name}</strong>
              <small>{String(skill.description || '')}</small>
              <small class="skill-provenance">Source: {String(skill.source || 'unknown')}</small>
              <small class="skill-execution">
                <strong>{skillExecutionLabel(skill)}</strong> — {skillExecutionDescription(skill)}
              </small>
            </button>
          );
        })}
      </div>
      {selected && (
        <form
          onSubmit={(event) => {
            event.preventDefault();
            if (!selectedBlocked) void store.invokeSkill(selected, args);
          }}
        >
          <label class="settings-label">Arguments for {selected}</label>
          <textarea value={args} onInput={(event) => setArgs(event.currentTarget.value)} />
          {selectedBlocked && (
            <p class="skill-run-blocked" role="status">
              This main-conversation skill cannot run until the active response finishes. Isolated
              skills can run now.
            </p>
          )}
          <button class="btn primary" type="submit" disabled={selectedBlocked}>
            Run skill
          </button>
        </form>
      )}
    </Overlay>
  );
}

function ProjectPicker() {
  const store = useStore();
  const [path, setPath] = useState('');
  const [name, setName] = useState('');
  const [browser, setBrowser] = useState(false);
  const [showHidden, setShowHidden] = useState(false);
  const [listing, setListing] = useState<Record<string, unknown> | null>(null);
  const [loading, setLoading] = useState(false);
  const [preview, setPreview] = useState<Record<string, unknown> | null>(null);
  const [error, setError] = useState('');
  const changePath = (value: string) => {
    setPath(value);
    setPreview(null);
    setError('');
  };
  const changeName = (value: string) => {
    setName(value);
    setPreview(null);
    setError('');
  };
  const loadDirectory = async (directory = '') => {
    setLoading(true);
    setError('');
    const controller = new AbortController();
    try {
      setListing(
        await store.endpoints.projectDirectories(directory, showHidden, controller.signal),
      );
      setBrowser(true);
    } catch (value) {
      setError(errorMessage(value));
    } finally {
      setLoading(false);
    }
  };
  const complete = async (id: string) => {
    await store.refreshSidebar();
    store.newChat(true, id);
    store.modal.value = '';
  };
  const submit = async () => {
    if (!path.trim()) return;
    setLoading(true);
    setError('');
    try {
      const data = await store.endpoints.createProject(
        { path: path.trim(), name: name.trim() },
        !preview,
      );
      const project =
        data.project && typeof data.project === 'object'
          ? (data.project as Record<string, unknown>)
          : null;
      const existing = String(data.existing_project_id || project?.id || '');
      if (!preview) {
        if (data.duplicate && existing && !project?.archived_at) {
          await complete(existing);
          return;
        }
        setPreview(data);
        return;
      }
      await complete(String(project?.id || data.id || existing));
    } catch (value) {
      setError(errorMessage(value));
    } finally {
      setLoading(false);
    }
  };
  const entries = Array.isArray(listing?.entries)
    ? (listing.entries as Array<Record<string, unknown>>)
    : [];
  const breadcrumbs = Array.isArray(listing?.breadcrumbs)
    ? (listing.breadcrumbs as Array<Record<string, unknown>>)
    : [];
  return (
    <Overlay title="Add project" wide>
      <section class="project-modal-fields">
        <div class="project-field">
          <div class="project-field-label-row">
            <label class="project-field-label" for="projectPathInput">
              Folder on server
            </label>
          </div>
          <div class="project-path-control">
            <input
              id="projectPathInput"
              class="project-path-input"
              aria-label="Project path"
              placeholder="/path/to/project"
              value={path}
              autoFocus
              autoCapitalize="none"
              autoCorrect="off"
              spellcheck={false}
              onInput={(event) => changePath(event.currentTarget.value)}
            />
            <button
              type="button"
              class="project-browse-button"
              aria-expanded={browser}
              onClick={() => (browser ? setBrowser(false) : void loadDirectory(path.trim()))}
            >
              {browser ? 'Hide browser' : 'Browse'}
            </button>
          </div>
        </div>
        {browser && (
          <section class="project-directory-browser">
            <div class="project-browser-toolbar">
              <button
                class="project-browser-icon-button"
                type="button"
                title="Parent folder"
                disabled={!listing?.parent}
                onClick={() => void loadDirectory(String(listing?.parent || ''))}
              >
                ↑
              </button>
              <button
                class="project-browser-icon-button"
                type="button"
                title="Home folder"
                onClick={() => void loadDirectory(String(listing?.home || ''))}
              >
                ⌂
              </button>
              <nav class="project-browser-breadcrumbs" aria-label="Folder path">
                {breadcrumbs.map((item, index) => (
                  <button
                    type="button"
                    aria-current={index === breadcrumbs.length - 1 ? 'page' : undefined}
                    onClick={() => void loadDirectory(String(item.path || ''))}
                  >
                    {String(item.label || item.path || '')}
                  </button>
                ))}
              </nav>
              <label class="project-browser-hidden">
                <input
                  type="checkbox"
                  checked={showHidden}
                  onChange={(event) => {
                    const checked = event.currentTarget.checked;
                    setShowHidden(checked);
                    void store.endpoints
                      .projectDirectories(String(listing?.path || ''), checked)
                      .then(setListing)
                      .catch((value) => setError(String(value)));
                  }}
                />
                Hidden
              </label>
            </div>
            <div class="project-browser-list" role="listbox" aria-busy={loading}>
              {loading ? (
                <div class="project-browser-skeleton">
                  <span />
                  <span />
                </div>
              ) : entries.length ? (
                entries.map((entry) => (
                  <button
                    type="button"
                    class="project-browser-row"
                    role="option"
                    onClick={() => void loadDirectory(String(entry.path || ''))}
                  >
                    <span class="project-browser-folder-icon">◇</span>
                    <span class="project-browser-row-name">
                      {String(entry.name || entry.path || '')}
                    </span>
                    <span class="project-browser-row-meta">
                      {entry.git && <span class="project-browser-badge">Git</span>}
                      {entry.existing_project_id && (
                        <span class="project-browser-badge is-added">Added</span>
                      )}
                      <span>›</span>
                    </span>
                  </button>
                ))
              ) : (
                <div class="project-browser-empty">
                  <strong>No subfolders here</strong>
                </div>
              )}
            </div>
            <div class="project-browser-footer">
              <div class="project-browser-status">
                {entries.length} folder{entries.length === 1 ? '' : 's'}
              </div>
              <button
                class="btn project-use-folder"
                type="button"
                disabled={!listing?.path}
                onClick={() => {
                  changePath(String(listing?.path || ''));
                  setBrowser(false);
                }}
              >
                Select folder
              </button>
            </div>
          </section>
        )}
        <div class="project-field">
          <div class="project-field-label-row">
            <label class="project-field-label" for="projectNameInput">
              Display name
            </label>
            <span class="project-field-optional">Optional</span>
          </div>
          <input
            id="projectNameInput"
            aria-label="Project name"
            placeholder="Defaults to the folder name"
            value={name}
            onInput={(event) => changeName(event.currentTarget.value)}
          />
          <div class="project-field-hint">
            Use a short name that is easy to spot in the sidebar.
          </div>
        </div>
      </section>
      {preview && (
        <div class="project-resolution-summary">
          <div class="project-resolution-top">
            <strong>{preview.git ? 'Git repository ready' : 'Folder ready'}</strong>
            {preview.git && <span class="project-browser-badge">Git root</span>}
          </div>
          <code>{String(preview.canonical_dir || '')}</code>
          <span class="project-resolution-note">
            {preview.duplicate &&
            (preview.project as Record<string, unknown> | undefined)?.archived_at
              ? 'This archived project will be restored.'
              : preview.git
                ? 'Conversations will use the repository root.'
                : 'Conversations will use this folder.'}
          </span>
        </div>
      )}
      {error && (
        <div class="modal-error" role="alert">
          {error}
        </div>
      )}
      <div class="modal-actions">
        <button
          class="btn"
          onClick={() => {
            store.modal.value = '';
          }}
        >
          Cancel
        </button>
        <button
          class="btn primary"
          disabled={!path.trim() || loading}
          onClick={() => void submit()}
        >
          {loading
            ? 'Checking…'
            : preview
              ? preview.duplicate
                ? 'Restore project'
                : 'Add project'
              : 'Preview'}
        </button>
      </div>
    </Overlay>
  );
}

export function SideQuestion() {
  const store = useStore();
  const state = store.sideQuestion.value;
  const session = store.activeSession.value;
  const transcript = useRef<HTMLDivElement>(null);
  const input = useRef<HTMLInputElement>(null);
  const stickToBottom = useRef(true);
  const hasCurrent = Boolean(state.question);
  const hasConversation = state.history.length > 0 || hasCurrent;

  useLayoutEffect(() => {
    const element = transcript.current;
    if (element && stickToBottom.current) element.scrollTop = element.scrollHeight;
  }, [state.history.length, state.response, state.running]);
  useEffect(() => {
    if (!state.loading && !state.running) input.current?.focus({ preventScroll: true });
  }, [state.loading, state.running]);

  if (!session || state.sessionId !== session.id) return null;

  const submit = () => {
    const question = state.draft.trim();
    if (!question || state.running) return;
    void store.askSideQuestion(question);
  };
  const escape = () => {
    if (state.running) store.cancelSideQuestion();
    else if (state.draft) store.setSideQuestionDraft('');
    else store.closeSideQuestion();
  };
  const starters = [
    'Summarise what we decided',
    "What's still unresolved?",
    'Explain the last change',
  ];

  return (
    <Overlay
      title="Side question"
      wide
      className="side-question-modal"
      onClose={() => store.closeSideQuestion()}
      onEscape={escape}
    >
      <div
        class={`side-question-transcript ${hasConversation ? '' : 'empty'}`}
        ref={transcript}
        onScroll={(event) => {
          const element = event.currentTarget;
          stickToBottom.current =
            element.scrollHeight - element.scrollTop - element.clientHeight < 96;
        }}
      >
        {state.loading && !hasConversation && (
          <div class="side-question-loading" role="status">
            <span class="side-question-loading-dot" />
            Loading side questions…
          </div>
        )}
        {!state.loading && !hasConversation && (
          <div class="side-question-empty">
            <div class="side-question-empty-mark" aria-hidden="true">
              ↗
            </div>
            <h3>Ask about this conversation</h3>
            <p>Answers use the transcript as context but are never added to it.</p>
            <div class="side-question-starters" aria-label="Suggested side questions">
              {starters.map((starter) => (
                <button
                  type="button"
                  key={starter}
                  onClick={() => {
                    store.setSideQuestionDraft(starter);
                    requestAnimationFrame(() => input.current?.focus());
                  }}
                >
                  {starter}
                </button>
              ))}
            </div>
          </div>
        )}
        {state.history.map((entry, index) => (
          <section class="side-question-exchange" key={`${index}-${entry.question}`}>
            <article class="message user">
              <div class="message-body">{entry.question}</div>
            </article>
            <article class="message assistant">
              <div class="message-body">
                <Markdown value={entry.response} className="markdown-body" />
              </div>
            </article>
          </section>
        ))}
        {hasCurrent && (
          <section class="side-question-exchange side-question-current">
            <article class="message user">
              <div class="message-body">{state.question}</div>
            </article>
            {(state.response || state.running) && (
              <article class="message assistant" aria-busy={state.running}>
                <div class="message-body">
                  {state.response ? (
                    <Markdown
                      value={state.response}
                      streaming={state.running}
                      className="markdown-body"
                    />
                  ) : (
                    <span class="side-question-thinking">
                      Thinking<span aria-hidden="true">…</span>
                    </span>
                  )}
                </div>
              </article>
            )}
          </section>
        )}
      </div>

      {state.error && (
        <div class="side-question-error" role="alert">
          <span>{state.error}</span>
          {state.question && !state.running && (
            <button type="button" onClick={() => void store.askSideQuestion(state.question)}>
              Try again
            </button>
          )}
        </div>
      )}

      <div class="side-question-status" role="status" aria-live="polite" aria-atomic="true">
        {state.loading
          ? 'Loading side questions…'
          : state.running
            ? 'Answering side question…'
            : ''}
      </div>

      {state.running ? (
        <button class="side-question-stop" type="button" onClick={() => store.cancelSideQuestion()}>
          <span aria-hidden="true" />
          Stop answering
        </button>
      ) : (
        <form
          class="side-question-composer"
          onSubmit={(event) => {
            event.preventDefault();
            submit();
          }}
        >
          <label class="visually-hidden" for="sideQuestionInput">
            Ask a side question
          </label>
          <input
            ref={input}
            id="sideQuestionInput"
            autoFocus
            autoComplete="off"
            value={state.draft}
            placeholder="Ask about this conversation…"
            disabled={state.loading}
            onInput={(event) => store.setSideQuestionDraft(event.currentTarget.value)}
          />
          <button
            class="side-question-send"
            type="submit"
            aria-label="Send side question"
            disabled={state.loading || !state.draft.trim()}
          >
            <Icon name="send" />
          </button>
        </form>
      )}
    </Overlay>
  );
}

export function Modals() {
  const store = useStore();
  const interaction = store.interactionOrder.value
    .map((key) => store.interactions.value[key])
    .find((entry) => entry && ['waiting', 'submitting', 'failed'].includes(entry.state));
  const modal = interaction
    ? interaction.kind === 'approval'
      ? 'approval'
      : 'ask-user'
    : store.approval.value
      ? 'approval'
      : store.askUser.value
        ? 'ask-user'
        : store.modal.value;
  switch (modal) {
    case 'settings':
      return <Settings />;
    case 'rename':
      return <Rename />;
    case 'project':
      return store.projectTarget.value ? <ProjectAssignment /> : <ProjectPicker />;
    case 'ask-user':
      return (
        <AskUser
          interactionPrompt={
            interaction?.kind === 'ask-user' ? (interaction.prompt as AskUserPrompt) : undefined
          }
        />
      );
    case 'approval':
      return (
        <Approval
          interactionPrompt={
            interaction?.kind === 'approval' ? (interaction.prompt as ApprovalPrompt) : undefined
          }
        />
      );
    case 'mcp':
      return <MCP />;
    case 'goal':
      return <GoalModal />;
    case 'widgets':
      return <Widgets />;
    case 'skills':
      return <Skills />;
    case 'side':
      return <SideQuestion />;
    case 'branch':
      return <BranchTree />;
    case 'branch-context':
      return <BranchContext />;
    case 'worktrees':
      return <Worktrees />;
    default:
      return null;
  }
}
