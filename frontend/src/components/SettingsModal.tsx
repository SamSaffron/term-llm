import '../styles/features/settings.css';
import { useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { errorMessage } from '../domain/text';
import { SettingsSelect } from './FormFields';
import { lazyComponent } from './lazyComponent';
import { Overlay } from './Overlay';
import { ToggleSwitch } from './ToggleSwitch';
import { useMediaQuery } from './useMediaQuery';

const LazyExtensionSettings = lazyComponent(
  () =>
    import('./ExtensionSettings')
      .then(({ ExtensionSettings }) => ExtensionSettings)
      .catch(() => () => (
        <div class="settings-panel-body">
          <p>Could not load extension settings. Reload the page to retry.</p>
        </div>
      )),
  <div class="settings-panel-body">
    <p>Loading extension settings…</p>
  </div>,
);

const LazyLinkedDevicesSettings = lazyComponent(
  () =>
    import('./LinkedDevicesSettings')
      .then(({ LinkedDevicesSettings }) => LinkedDevicesSettings)
      .catch(() => () => (
        <div class="settings-panel-body">
          <p>Could not load linked devices settings. Reload the page to retry.</p>
        </div>
      )),
  <div class="settings-panel-body">
    <p>Loading linked devices settings…</p>
  </div>,
);

const settingsTabs = [
  {
    label: 'Model',
    slug: 'model',
    description: 'Provider, model, effort and agent used for chats from this browser.',
  },
  {
    label: 'Interface',
    slug: 'interface',
    description: 'Sidebar and notification preferences for this browser.',
  },
  {
    label: 'Extensions',
    slug: 'extensions',
    description: 'Choose which installed web UI extensions load, and in what order.',
  },
  {
    label: 'Linked devices',
    slug: 'linked-devices',
    description: 'Use this server from your phone or another computer.',
  },
  {
    label: 'Connection',
    slug: 'connection',
    description: 'How this browser signs in, and the data it keeps on this device.',
  },
] as const;

const previousTabKeys = new Set(['ArrowLeft', 'ArrowUp']);
const nextTabKeys = new Set(['ArrowRight', 'ArrowDown']);

export function Settings() {
  const store = useStore();
  const [tab, setTab] = useState('model');
  const [extensionsVisited, setExtensionsVisited] = useState(false);
  // Linked devices stays mounted once opened so a pairing in progress (its
  // one-time QR secret and any typed code) survives a trip to another tab.
  const [linkedDevicesVisited, setLinkedDevicesVisited] = useState(false);
  // Matches the sidebar breakpoint in settings.css.
  const verticalTabs = useMediaQuery('(width >= 720px)');
  const activeTab = tab;
  const visibleTabs = store.linkedDevicesMode.value
    ? settingsTabs.filter(({ slug }) => slug === 'model' || slug === 'interface')
    : settingsTabs;
  const selectTab = (name: string) => {
    setTab(name);
    if (name === 'extensions') setExtensionsVisited(true);
    if (name === 'linked-devices') setLinkedDevicesVisited(true);
  };
  const [token, setToken] = useState(store.token.value);
  const [provider, setProvider] = useState(store.selectedProvider.value);
  const [model, setModel] = useState(store.selectedModel.value);
  const [effort, setEffort] = useState(store.selectedEffort.value);
  const [reasoning, setReasoning] = useState(store.selectedReasoningMode.value);
  const [agent, setAgent] = useState(store.selectedAgent.value);
  const [saving, setSaving] = useState(false);
  const [saveError, setSaveError] = useState('');
  const activeTabInfo = visibleTabs.find(({ slug }) => slug === activeTab) ?? visibleTabs[0];
  const save = async () => {
    if (saving) return;
    setSaving(true);
    setSaveError('');
    try {
      store.setPreference('provider', provider);
      store.setPreference('model', model);
      store.setPreference('effort', effort);
      store.setPreference('reasoning', reasoning);
      store.setPreference('agent', agent);
      await store.saveSettings(token);
    } catch (error) {
      setSaveError(errorMessage(error));
    } finally {
      setSaving(false);
    }
  };
  // Rendered only in the active panel so a save error is announced once.
  const saveFooter = () => (
    <div class="modal-actions settings-footer">
      {saveError && (
        <p class="settings-save-error" role="alert">
          {saveError}
        </p>
      )}
      <button
        class="btn"
        type="button"
        disabled={saving}
        onClick={() => {
          store.modal.value = '';
        }}
      >
        Cancel
      </button>
      <button class="btn primary" type="button" onClick={save} disabled={saving}>
        {saving ? 'Saving…' : 'Save'}
      </button>
    </div>
  );
  return (
    <Overlay
      title="Settings"
      className="settings-modal"
      dismissDisabled={saving}
      titleActions={
        <div class="settings-panel-header">
          <h3>{activeTabInfo.label}</h3>
          <p>{activeTabInfo.description}</p>
        </div>
      }
    >
      <div class="settings-layout">
        <div
          class="settings-tabs"
          role="tablist"
          aria-label="Settings sections"
          aria-orientation={verticalTabs ? 'vertical' : 'horizontal'}
        >
          {visibleTabs.map(({ label, slug }, index) => {
            return (
              <button
                type="button"
                role="tab"
                id={`settings-${slug}-tab`}
                aria-controls={`settings-${slug}-panel`}
                aria-selected={activeTab === slug}
                tabIndex={activeTab === slug ? 0 : -1}
                onClick={() => selectTab(slug)}
                onKeyDown={(event) => {
                  let next: number;
                  if (nextTabKeys.has(event.key)) next = (index + 1) % visibleTabs.length;
                  else if (previousTabKeys.has(event.key))
                    next = (index + visibleTabs.length - 1) % visibleTabs.length;
                  else if (event.key === 'Home') next = 0;
                  else if (event.key === 'End') next = visibleTabs.length - 1;
                  else return;
                  event.preventDefault();
                  const nextTab = visibleTabs[next];
                  selectTab(nextTab.slug);
                  document.getElementById(`settings-${nextTab.slug}-tab`)?.focus();
                }}
              >
                {label}
              </button>
            );
          })}
        </div>
        <div class="settings-panels">
          <div
            id="settings-model-panel"
            role="tabpanel"
            aria-labelledby="settings-model-tab"
            hidden={activeTab !== 'model'}
            class="settings-panel settings-panel-model"
          >
            <div class="settings-panel-body">
              <div class="settings-model-fields">
                <SettingsSelect
                  id="providerSelect"
                  label="Provider"
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
                </SettingsSelect>
                <SettingsSelect
                  id="modelSelect"
                  label="Model"
                  value={model}
                  onChange={(event) => setModel(event.currentTarget.value)}
                >
                  <option value="">Auto (server default)</option>
                  {store.models.value.map((entry) => (
                    <option value={entry.id} key={entry.id}>
                      {entry.name || entry.id}
                    </option>
                  ))}
                </SettingsSelect>
                <SettingsSelect
                  id="effortSelect"
                  label="Effort"
                  value={effort}
                  onChange={(event) => setEffort(event.currentTarget.value)}
                >
                  {['', 'none', 'minimal', 'low', 'medium', 'high', 'xhigh', 'max'].map((value) => (
                    <option value={value} key={value}>
                      {value || 'Auto (server default)'}
                    </option>
                  ))}
                </SettingsSelect>
                <SettingsSelect
                  id="reasoningModeSelect"
                  label="Reasoning mode"
                  value={reasoning}
                  onChange={(event) => setReasoning(event.currentTarget.value)}
                >
                  <option value="standard">Standard</option>
                  <option value="pro">Pro</option>
                </SettingsSelect>
                {store.config.agentNames.length > 1 && (
                  <SettingsSelect
                    id="agentSelect"
                    label="Agent"
                    value={agent}
                    onChange={(event) => setAgent(event.currentTarget.value)}
                  >
                    <option value="">Default</option>
                    {store.config.agentNames.map((name) => (
                      <option value={name} key={name}>
                        {name}
                      </option>
                    ))}
                  </SettingsSelect>
                )}
              </div>
            </div>
            {activeTab === 'model' && saveFooter()}
          </div>
          <div
            id="settings-interface-panel"
            role="tabpanel"
            aria-labelledby="settings-interface-tab"
            hidden={activeTab !== 'interface'}
            class="settings-panel settings-panel-interface"
          >
            <div class="settings-panel-body">
              <div class="settings-field">
                <label class="settings-toggle">
                  <span class="settings-label settings-label-inline">Show archived sessions</span>
                  <ToggleSwitch
                    checked={store.showArchived.value}
                    onChange={(event) => {
                      store.showArchived.value = event.currentTarget.checked;
                      store.storage.setItem(
                        store.keys.showArchivedSessions,
                        event.currentTarget.checked ? '1' : '0',
                      );
                      void store.refreshSidebar();
                    }}
                  />
                </label>
              </div>
              {!store.linkedDevicesMode.value && (
                <div class="settings-field">
                  <label class="settings-toggle">
                    <span class="settings-label settings-label-inline">
                      Show widgets in sidebar
                    </span>
                    <ToggleSwitch
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
              )}
              <div class="settings-field notification-settings">
                <span class="settings-label">Notifications</span>
                {store.linkedDevicesMode.value ? (
                  <div class="notification-state notification-state-unsupported" role="status">
                    <strong>Unavailable</strong>
                    <span>Push notifications are not available through Linked devices.</span>
                  </div>
                ) : (
                  <>
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
                  </>
                )}
              </div>
            </div>
          </div>
          <div
            id="settings-connection-panel"
            role="tabpanel"
            aria-labelledby="settings-connection-tab"
            hidden={activeTab !== 'connection'}
            class="settings-panel settings-panel-connection"
          >
            <div class="settings-panel-body">
              {store.config.passkeyAuth ? (
                <div class="settings-field">
                  <p>Signed in with a passkey. No bearer token is stored in this browser.</p>
                  <a
                    class="btn"
                    onClick={() => store.composer.persist()}
                    href={`${store.config.prefix}/auth/security?return=${encodeURIComponent(location.pathname)}`}
                  >
                    Manage passkeys and sessions
                  </a>
                </div>
              ) : (
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
              )}
              <div class="settings-field">
                <p class="settings-help">
                  Recent conversations and model lists are kept on this device so the chat opens
                  quickly. They are always refreshed from the server.
                </p>
                <button class="btn" type="button" onClick={() => void store.clearLocalCache()}>
                  Clear cached data on this device
                </button>
              </div>
            </div>
            {activeTab === 'connection' && !store.config.passkeyAuth && saveFooter()}
          </div>
          <div
            id="settings-extensions-panel"
            role="tabpanel"
            aria-labelledby="settings-extensions-tab"
            hidden={activeTab !== 'extensions'}
            class="settings-panel"
          >
            {extensionsVisited && <LazyExtensionSettings />}
          </div>
          <div
            id="settings-linked-devices-panel"
            role="tabpanel"
            aria-labelledby="settings-linked-devices-tab"
            hidden={activeTab !== 'linked-devices'}
            class="settings-panel"
          >
            {linkedDevicesVisited && (
              <LazyLinkedDevicesSettings active={activeTab === 'linked-devices'} />
            )}
          </div>
        </div>
      </div>
    </Overlay>
  );
}
