import { useEffect, useMemo, useState } from 'preact/hooks';
import type { DiffLine } from '../domain/types';

type Highlighter = typeof import('../domain/rich-highlight').highlightDiffLine;
let loaded: Highlighter | null = null;
let loading: Promise<Highlighter> | null = null;

/** One readiness boundary and one prepared batch per visible file, not per line. */
export function useDiffHighlighting(
  lines: DiffLine[],
  language: string,
  limit: number,
  enabled: boolean,
) {
  const [highlight, setHighlight] = useState<Highlighter | null>(() => loaded);
  const [failed, setFailed] = useState(false);
  useEffect(() => {
    if (!enabled || !language || highlight) return;
    let live = true;
    // Publish plain, usable rows if the optional chunk stalls. The late result
    // is ignored for this mount to avoid a second visual upgrade after fallback.
    const deadline = setTimeout(() => {
      if (live) {
        live = false;
        setFailed(true);
      }
    }, 250);
    loading ||= import('../domain/rich-highlight').then(
      (module) => (loaded = module.highlightDiffLine),
    );
    void loading.then(
      (value) => {
        clearTimeout(deadline);
        if (live) setHighlight(() => value);
      },
      () => {
        clearTimeout(deadline);
        loading = null;
        if (live) setFailed(true);
      },
    );
    return () => {
      live = false;
      clearTimeout(deadline);
    };
  }, [enabled, language, highlight]);
  const html = useMemo(
    () =>
      enabled && language && highlight
        ? lines.slice(0, limit).map((line) => highlight(line.content, language))
        : [],
    [enabled, lines, language, limit, highlight],
  );
  return { html, pending: enabled && Boolean(language) && !highlight && !failed };
}
