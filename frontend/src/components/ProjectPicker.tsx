import { useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { errorMessage } from '../domain/text';
import { Icon } from './Icon';
import { Overlay } from './Overlay';

export function ProjectPicker() {
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
                    <span class="project-browser-folder-icon">
                      <Icon name="folder" />
                    </span>
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
