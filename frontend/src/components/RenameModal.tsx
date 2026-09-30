import { useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { Overlay } from './Overlay';

export function Rename() {
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
  const [notice, setNotice] = useState('');
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
    setNotice('');
    try {
      const suggestion = await store.improveTitle();
      setGeneratedTitle(suggestion.title);
      setGeneratedDetail(suggestion.detail);
      if (suggestion.abstained) {
        setNotice(
          "AI couldn't produce a specific title from this session. Edit the current title or try again.",
        );
      }
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
      <div
        class={`modal-error ${notice && !error ? 'modal-notice' : ''}`}
        role={error ? 'alert' : notice ? 'status' : undefined}
      >
        {error || notice}
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
