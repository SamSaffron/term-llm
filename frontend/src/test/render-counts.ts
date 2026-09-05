import { options, type VNode } from 'preact';

/** Test-only instrumentation: count component executions, not just DOM mutations. */
export function observeRenders() {
  const hooks = options as typeof options & { __r?: (vnode: VNode) => void };
  const previous = hooks.__r;
  let observed = false;
  const counts = new Map<string, number>();
  hooks.__r = (vnode) => {
    previous?.(vnode);
    if (typeof vnode.type !== 'function') return;
    observed = true;
    const name = vnode.type.displayName || vnode.type.name;
    counts.set(name, (counts.get(name) || 0) + 1);
    const props = vnode.props as {
      message?: { id: string };
      node?: { id: string };
      line?: { newLine?: number };
      session?: { id: string };
    };
    const id = props.message?.id ?? props.node?.id ?? props.line?.newLine ?? props.session?.id;
    if (id !== undefined) {
      const key = `${name}:${id}`;
      counts.set(key, (counts.get(key) || 0) + 1);
    }
  };
  return {
    count: (name: string) => counts.get(name) || 0,
    clear: () => counts.clear(),
    dispose: () => {
      hooks.__r = previous;
      if (!observed) throw new Error('Preact render hook did not observe any components');
    },
  };
}
