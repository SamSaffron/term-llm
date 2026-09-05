import { Component, createElement, type FunctionComponent } from 'preact';

/** A shallow render boundary without preact/compat's global DOM-event overrides.
 * Props must be immutable. Fresh children VNodes still invalidate the boundary;
 * refs address this wrapper, rather than being forwarded to its child.
 */
export function memo<Props extends object>(render: FunctionComponent<Props>) {
  return class Memoized extends Component<Props> {
    static displayName = `Memo(${render.displayName || render.name})`;

    shouldComponentUpdate(next: Props): boolean {
      const previous = this.props;
      const keys = Object.keys(next) as (keyof Props)[];
      return (
        keys.length !== Object.keys(previous).length ||
        keys.some((key) => !Object.hasOwn(previous, key) || !Object.is(previous[key], next[key]))
      );
    }

    render() {
      return createElement(render, this.props);
    }
  };
}
