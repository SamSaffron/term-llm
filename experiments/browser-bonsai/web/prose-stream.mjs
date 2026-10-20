// Keep tool-call XML out of the assistant prose stream, even when a tag is
// split across GPU token callbacks. Completed calls are emitted structurally by
// the Go provider after the final response has been parsed and validated.
export function proseStream(emit) {
  const open = '<tool_call>';
  const close = '</tool_call>';
  let pending = '';
  let inTool = false;
  return {
    push(chunk) {
      pending += chunk;
      while (pending) {
        if (inTool) {
          const end = pending.indexOf(close);
          if (end < 0) { pending = pending.slice(-close.length); return; }
          pending = pending.slice(end + close.length);
          inTool = false;
        } else {
          const start = pending.indexOf(open);
          if (start >= 0) {
            if (start) emit(pending.slice(0, start));
            pending = pending.slice(start + open.length);
            inTool = true;
          } else {
            let held = 0;
            for (let n = 1; n < open.length && n <= pending.length; n++) if (open.startsWith(pending.slice(-n))) held = n;
            const visible = pending.slice(0, pending.length - held);
            if (visible) emit(visible);
            pending = held ? pending.slice(-held) : '';
            return;
          }
        }
      }
    },
    end() { if (!inTool && pending) emit(pending); pending = ''; },
  };
}
