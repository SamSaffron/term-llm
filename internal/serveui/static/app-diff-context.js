(() => {
'use strict';

// Pure row-model helpers for the diff controller. Keeping omitted-region
// accounting separate from DOM rendering makes expansion behavior testable
// without coupling it to the sidebar lifecycle.
const app = window.TermLLMApp || (window.TermLLMApp = {});

// Flatten server hunks into renderable rows with old/new line numbers. Omitted
// unchanged regions become separator rows carrying their hidden line count.
const buildDiffRowModel = (hunks, lineCounts = null) => {
  const rows = [];
  const hunkList = Array.isArray(hunks) ? hunks : [];
  let previousOldEnd = 1;
  let previousNewEnd = 1;
  const addSeparator = (oldGap, newGap) => {
    const hiddenCount = Math.min(Math.max(0, oldGap), Math.max(0, newGap));
    if (hiddenCount > 0) rows.push({ type: 'hunk', oldNo: 0, newNo: 0, text: '', hiddenCount });
  };
  hunkList.forEach((hunk) => {
    let oldNo = Number(hunk.old_start);
    let newNo = Number(hunk.new_start);
    if (!Number.isFinite(oldNo)) oldNo = 1;
    if (!Number.isFinite(newNo)) newNo = 1;
    addSeparator(oldNo - previousOldEnd, newNo - previousNewEnd);
    (Array.isArray(hunk.lines) ? hunk.lines : []).forEach((line) => {
      const text = String(line.s ?? '');
      if (line.t === 'add') {
        rows.push({ type: 'add', oldNo: 0, newNo, text });
        newNo += 1;
      } else if (line.t === 'del') {
        rows.push({ type: 'del', oldNo, newNo: 0, text });
        oldNo += 1;
      } else {
        rows.push({ type: 'ctx', oldNo, newNo, text });
        oldNo += 1;
        newNo += 1;
      }
    });
    previousOldEnd = oldNo;
    previousNewEnd = newNo;
  });
  if (lineCounts && rows.length > 0) {
    addSeparator(Number(lineCounts.old) + 1 - previousOldEnd, Number(lineCounts.new) + 1 - previousNewEnd);
  }
  return rows;
};

const diffRowAnchorKey = (row) => `${row.type}:${row.oldNo || 0}:${row.newNo || 0}`;

const captureDiffContextAnchor = (button, list) => {
  const separator = button?.closest?.('.diff-row');
  const siblings = Array.from(separator?.parentNode?.children || []);
  const index = siblings.indexOf(separator);
  const element = (index >= 0 && (siblings[index + 1] || siblings[index - 1])) || null;
  if (!list || !element?.dataset?.diffAnchor) return null;
  return {
    key: element.dataset.diffAnchor,
    top: Number(element.getBoundingClientRect?.().top) || 0,
    scrollTop: Number(list.scrollTop) || 0
  };
};

const restoreDiffContextAnchor = (anchor, list, rows) => {
  const currentScrollTop = Number(list?.scrollTop);
  if (!anchor || !list || !Number.isFinite(currentScrollTop) || Math.abs(currentScrollTop - anchor.scrollTop) > 1) return;
  const element = Array.from(rows || []).find((row) => row.dataset?.diffAnchor === anchor.key);
  if (!element) return;
  const delta = (Number(element.getBoundingClientRect?.().top) || 0) - anchor.top;
  if (delta) list.scrollTop += delta;
};

const createDiffContextButton = (createEl, row, onExpand) => {
  const button = createEl('button', 'diff-hunk-expand');
  const label = createEl('span', 'diff-hunk-expand-label', `Show ${row.hiddenCount} hidden ${row.hiddenCount === 1 ? 'line' : 'lines'}`);
  button.setAttribute('type', 'button');
  button.addEventListener('click', (event) => {
    event.stopPropagation?.();
    onExpand(button);
    button.disabled = true;
    button.setAttribute('aria-busy', 'true');
    label.textContent = 'Expanding…';
  });
  button.appendChild(label);
  return button;
};

Object.assign(app, {
  buildDiffRowModel,
  diffRowAnchorKey,
  captureDiffContextAnchor,
  restoreDiffContextAnchor,
  createDiffContextButton
});
})();
