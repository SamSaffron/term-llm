import { useEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../../app/context';
import { errorMessage } from '../../domain/text';
import type {
  MCPAddRequest,
  MCPAddResult,
  MCPCatalogueEntry,
  MCPCatalogueResponse,
} from '../../domain/types';
import { SearchField } from '../FormFields';
import { Icon } from '../Icon';
import { parseMCPPairs } from './mcp-format';

type Source = 'catalogue' | 'url' | 'command';

const SOURCES: { id: Source; label: string }[] = [
  { id: 'catalogue', label: 'Catalogue' },
  { id: 'url', label: 'Remote URL' },
  { id: 'command', label: 'Local command' },
];

const SEARCH_DEBOUNCE_MS = 250;
const PREVIEW_DEBOUNCE_MS = 350;

interface AddProps {
  onAdded: (result: MCPAddResult) => void;
  onCancel: () => void;
}

/** Focuses the element once on mount; `autofocus` is ignored inside dialogs. */
function useMountFocus<T extends HTMLElement>() {
  const ref = useRef<T>(null);
  useEffect(() => {
    const frame = requestAnimationFrame(() => ref.current?.focus());
    return () => cancelAnimationFrame(frame);
  }, []);
  return ref;
}

function SourceTabs({ value, onChange }: { value: Source; onChange: (next: Source) => void }) {
  const move = (event: KeyboardEvent) => {
    const step = event.key === 'ArrowRight' ? 1 : event.key === 'ArrowLeft' ? -1 : 0;
    if (!step) return;
    event.preventDefault();
    const index = SOURCES.findIndex((source) => source.id === value);
    const next = SOURCES[(index + step + SOURCES.length) % SOURCES.length];
    onChange(next.id);
    const tabs = (event.currentTarget as HTMLElement).querySelectorAll<HTMLElement>('[role="tab"]');
    tabs[SOURCES.indexOf(next)]?.focus();
  };
  return (
    <div class="mcp-tabs" role="tablist" aria-label="Server source" onKeyDown={move}>
      {SOURCES.map((source) => (
        <button
          key={source.id}
          type="button"
          role="tab"
          id={`mcp-tab-${source.id}`}
          aria-selected={value === source.id}
          aria-controls="mcp-add-panel"
          tabIndex={value === source.id ? 0 : -1}
          onClick={() => onChange(source.id)}
        >
          {source.label}
        </button>
      ))}
    </div>
  );
}

const TRANSPORT_LABELS: Record<string, string> = {
  remote: 'remote',
  npm: 'npm',
  pypi: 'pypi',
  oci: 'docker',
};

function CatalogueRow({
  entry,
  busy,
  onAdd,
}: {
  entry: MCPCatalogueEntry;
  busy: boolean;
  onAdd: () => void;
}) {
  const transport = TRANSPORT_LABELS[entry.transport] || '';
  return (
    <div class="mcp-catalogue-row">
      <span class="mcp-catalogue-text">
        <span class="mcp-row-name">{entry.name}</span>
        {entry.description && <span class="mcp-row-meta">{entry.description}</span>}
      </span>
      {transport && <span class="mcp-tag">{transport}</span>}
      {entry.installed ? (
        <span class="mcp-installed">
          <Icon name="check" />
          Added
        </span>
      ) : (
        <button
          class="mcp-button"
          type="button"
          disabled={busy}
          aria-label={`Add ${entry.name}`}
          onClick={onAdd}
        >
          {busy ? 'Adding…' : 'Add'}
        </button>
      )}
    </div>
  );
}

interface CatalogueState {
  loading: boolean;
  data: MCPCatalogueResponse | null;
  error: string;
}

function useCatalogue(query: string): CatalogueState {
  const store = useStore();
  const [state, setState] = useState<CatalogueState>({ loading: true, data: null, error: '' });
  useEffect(() => {
    const controller = new AbortController();
    setState((current) => ({ ...current, loading: true }));
    const timer = setTimeout(
      () => {
        store
          .searchMCPCatalogue(query, controller.signal)
          .then((data) => setState({ loading: false, data, error: '' }))
          .catch((error: unknown) => {
            if (!controller.signal.aborted)
              setState({ loading: false, data: null, error: errorMessage(error) });
          });
      },
      query.trim() ? SEARCH_DEBOUNCE_MS : 0,
    );
    return () => {
      clearTimeout(timer);
      controller.abort();
    };
  }, [query, store]);
  return state;
}

function CataloguePanel({ onAdded, enable }: AddProps & { enable: boolean }) {
  const store = useStore();
  const [query, setQuery] = useState('');
  const [adding, setAdding] = useState('');
  const [addError, setAddError] = useState('');
  const catalogue = useCatalogue(query);
  const search = useRef<HTMLDivElement>(null);
  useEffect(() => {
    const frame = requestAnimationFrame(() => search.current?.querySelector('input')?.focus());
    return () => cancelAnimationFrame(frame);
  }, []);
  const add = async (entry: MCPCatalogueEntry) => {
    setAdding(entry.id);
    setAddError('');
    try {
      onAdded(await store.addMCPServer({ kind: 'catalogue', catalogue_id: entry.id }, enable));
    } catch (error) {
      setAddError(errorMessage(error));
    } finally {
      setAdding('');
    }
  };
  const entries = catalogue.data?.servers || [];
  return (
    <>
      <div ref={search} class="mcp-add-search">
        <SearchField
          className="mcp-filter"
          aria-label="Search MCP catalogue"
          value={query}
          placeholder="Search servers"
          onInput={(event) => setQuery(event.currentTarget.value)}
        />
      </div>
      {addError && (
        <p class="mcp-form-error" role="alert">
          {addError}
        </p>
      )}
      <div class="mcp-list mcp-catalogue" aria-busy={catalogue.loading ? 'true' : undefined}>
        {catalogue.error ? (
          <div class="mcp-empty" role="status">
            <strong>Catalogue unavailable</strong>
            <span>{catalogue.error}</span>
          </div>
        ) : entries.length === 0 && !catalogue.loading ? (
          <div class="mcp-empty" role="status">
            <strong>No servers match “{query.trim()}”</strong>
            <span>Try another name, or add it by URL or command.</span>
          </div>
        ) : (
          entries.map((entry) => (
            <CatalogueRow
              key={entry.id}
              entry={entry}
              busy={adding === entry.id}
              onAdd={() => void add(entry)}
            />
          ))
        )}
      </div>
      {catalogue.data?.registry_error && (
        <p class="mcp-add-note">Registry unreachable — showing built-in servers only.</p>
      )}
    </>
  );
}

interface Preview {
  name: string;
  exists: boolean;
  path: string;
}

/** Asks the server which name and config path a request would produce. */
function usePreview(request: MCPAddRequest | null): Preview | null {
  const store = useStore();
  const [preview, setPreview] = useState<Preview | null>(null);
  const key = request ? JSON.stringify(request) : '';
  useEffect(() => {
    if (!key) {
      setPreview(null);
      return;
    }
    let live = true;
    const timer = setTimeout(() => {
      store
        .previewMCPServer(JSON.parse(key) as MCPAddRequest)
        .then((result) => {
          if (live)
            setPreview({
              name: result.name,
              exists: Boolean(result.exists),
              path: result.config_path,
            });
        })
        .catch(() => live && setPreview(null));
    }, PREVIEW_DEBOUNCE_MS);
    return () => {
      live = false;
      clearTimeout(timer);
    };
  }, [key, store]);
  return preview;
}

function buildRequest(
  kind: 'url' | 'command',
  source: string,
  name: string,
  pairs: Record<string, string>,
): MCPAddRequest {
  const request: MCPAddRequest =
    kind === 'url' ? { kind, url: source.trim() } : { kind, command: source.trim() };
  if (name.trim()) request.name = name.trim();
  if (Object.keys(pairs).length) {
    if (kind === 'url') request.headers = pairs;
    else request.env = pairs;
  }
  return request;
}

function CustomPanel({
  kind,
  enable,
  onAdded,
  onCancel,
}: AddProps & { kind: 'url' | 'command'; enable: boolean }) {
  const store = useStore();
  const [source, setSource] = useState('');
  const [name, setName] = useState('');
  const [pairsText, setPairsText] = useState('');
  const [error, setError] = useState('');
  const [saving, setSaving] = useState(false);
  const sourceInput = useMountFocus<HTMLInputElement>();
  const pairs = parseMCPPairs(pairsText, kind === 'url' ? ':' : '=');
  const previewRequest = source.trim()
    ? {
        kind,
        name: name.trim() || undefined,
        ...(kind === 'url' ? { url: source.trim() } : { command: source.trim() }),
      }
    : null;
  const preview = usePreview(previewRequest);
  const conflict = preview?.exists
    ? `A server named “${preview.name}” already exists. Choose another name.`
    : '';
  const isURL = kind === 'url';

  const submit = async (event: Event) => {
    event.preventDefault();
    if (pairs.error) return setError(pairs.error);
    setSaving(true);
    setError('');
    try {
      onAdded(await store.addMCPServer(buildRequest(kind, source, name, pairs.values), enable));
    } catch (failure) {
      setError(errorMessage(failure));
    } finally {
      setSaving(false);
    }
  };

  return (
    <form class="mcp-form" onSubmit={(event) => void submit(event)}>
      <div class="mcp-form-body">
        <label class="mcp-field">
          <span>{isURL ? 'URL' : 'Command'}</span>
          <input
            ref={sourceInput}
            class={isURL ? '' : 'mono'}
            type={isURL ? 'url' : 'text'}
            inputMode={isURL ? 'url' : undefined}
            spellcheck={false}
            autoComplete="off"
            required
            value={source}
            placeholder={isURL ? 'https://example.com/mcp' : 'npx -y @scope/mcp-server'}
            onInput={(event) => setSource(event.currentTarget.value)}
          />
        </label>
        <label class="mcp-field">
          <span>Name</span>
          <input
            type="text"
            spellcheck={false}
            autoComplete="off"
            value={name}
            placeholder={
              preview?.name || (isURL ? 'Derived from the URL' : 'Derived from the command')
            }
            onInput={(event) => setName(event.currentTarget.value)}
          />
        </label>
        <details class="mcp-disclosure" open={Boolean(pairsText)}>
          <summary>{isURL ? 'Headers' : 'Environment variables'}</summary>
          <textarea
            class="mono"
            rows={3}
            spellcheck={false}
            aria-label={isURL ? 'Headers' : 'Environment variables'}
            value={pairsText}
            placeholder={isURL ? 'Authorization: Bearer …' : 'API_KEY=op://vault/item/field'}
            onInput={(event) => setPairsText(event.currentTarget.value)}
          />
        </details>
        {!isURL && (
          <p class="mcp-callout">
            Runs on the machine hosting term-llm, with your user’s permissions.
          </p>
        )}
        {(error || conflict) && (
          <p class="mcp-form-error" role="alert">
            {error || conflict}
          </p>
        )}
      </div>
      <div class="mcp-form-footer">
        <span class="mcp-config-path" title={preview?.path}>
          {preview?.path ? preview.path.replace(/^\/(Users|home)\/[^/]+/, '~') : 'mcp.json'}
        </span>
        <button class="mcp-button ghost" type="button" onClick={onCancel}>
          Cancel
        </button>
        <button
          class="mcp-button primary"
          type="submit"
          disabled={saving || !source.trim() || Boolean(conflict)}
        >
          {saving ? 'Adding…' : enable ? 'Add & turn on' : 'Add'}
        </button>
      </div>
    </form>
  );
}

/** The pushed "Add server" view: catalogue, remote URL, or local command. */
export function MCPAddServer({ onAdded, onCancel }: AddProps) {
  const store = useStore();
  const [source, setSource] = useState<Source>('catalogue');
  // Enabling needs an idle session; while a response runs, only save.
  const enable = !store.streaming.value;
  return (
    <div class="mcp-add">
      <SourceTabs value={source} onChange={setSource} />
      <div
        id="mcp-add-panel"
        class="mcp-add-panel"
        role="tabpanel"
        aria-labelledby={`mcp-tab-${source}`}
      >
        {source === 'catalogue' ? (
          <CataloguePanel onAdded={onAdded} onCancel={onCancel} enable={enable} />
        ) : (
          <CustomPanel
            key={source}
            kind={source}
            enable={enable}
            onAdded={onAdded}
            onCancel={onCancel}
          />
        )}
      </div>
    </div>
  );
}
