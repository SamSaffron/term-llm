import { useState } from 'preact/hooks';
import { useStore } from '../app/context';
import type { Widget } from '../domain/types';
import { SearchField } from './FormFields';
import { Icon } from './Icon';
import { Overlay } from './Overlay';

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

export function Widgets() {
  const store = useStore();
  const [query, setQuery] = useState('');
  const [stopping, setStopping] = useState<Set<string>>(() => new Set());
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

  const stopWidget = async (widget: Widget) => {
    if (!widget.mount || stopping.has(widget.id)) return;
    setStopping((current) => new Set(current).add(widget.id));
    try {
      await store.widgetStore.stop(widget.mount);
    } catch (error) {
      store.toast(error, 'error');
    } finally {
      setStopping((current) => {
        const next = new Set(current);
        next.delete(widget.id);
        return next;
      });
    }
  };

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
        <SearchField
          className="widgets-modal-search"
          aria-label="Filter widgets"
          value={query}
          placeholder="Find a widget…"
          autoFocus
          onInput={(event) => setQuery(event.currentTarget.value)}
        />
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
            const isStopping = stopping.has(widget.id);
            const state = String(widget.state || 'stopped').toLowerCase();
            const canStop = Boolean(
              widget.mount && ['running', 'started', 'starting'].includes(state),
            );
            const status = isStopping
              ? { label: 'Stopping', tone: 'starting' }
              : widgetStatus(widget);
            const detail =
              widget.description || (widget.mount ? `/${widget.mount}` : 'Local widget');
            return (
              <div
                class="widget-card"
                data-state={status?.tone || 'ready'}
                data-stoppable={canStop ? 'true' : undefined}
                key={widget.id}
              >
                <a
                  class="widget-card-open"
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
                {canStop && (
                  <button
                    class="widget-card-stop"
                    type="button"
                    aria-label={`Stop ${widget.name}`}
                    title={`Stop ${widget.name}`}
                    disabled={isStopping}
                    onClick={() => void stopWidget(widget)}
                  >
                    Stop
                  </button>
                )}
              </div>
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
