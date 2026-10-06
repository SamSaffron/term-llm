import { effect, type Signal } from '@preact/signals';
import { nodeFilterHref, parseNodeFilter, type NodeFilter } from '../domain/node-filter';

type FilterWindow = Pick<
  Window,
  'location' | 'history' | 'addEventListener' | 'removeEventListener'
>;

/**
 * Keeps `filter` and the page URL in step: the URL sets the filter at start
 * and on history navigation, and each filter change replaces the current
 * history entry so the address can be bookmarked or shared without adding a
 * Back step per click. Returns a function that stops syncing.
 */
export function bindNodeFilterURL(filter: Signal<NodeFilter>, view: FilterWindow): () => void {
  filter.value = parseNodeFilter(view.location.search);
  const stop = effect(() => {
    const href = nodeFilterHref(view.location.href, filter.value);
    if (href !== view.location.href) view.history.replaceState(view.history.state, '', href);
  });
  const onPopState = () => {
    filter.value = parseNodeFilter(view.location.search);
  };
  view.addEventListener('popstate', onPopState);
  return () => {
    stop();
    view.removeEventListener('popstate', onPopState);
  };
}
