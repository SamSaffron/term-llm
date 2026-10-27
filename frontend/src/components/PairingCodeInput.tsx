import { useLayoutEffect, useRef } from 'preact/hooks';

const onlyDigits = (text: string) => text.replace(/\D/g, '');

/**
 * A one-box-per-digit code entry. The value stays a contiguous digit string:
 * typing fills the next box, deleting walks back, and a pasted or autofilled
 * code spreads across the boxes. Only the box where the next digit goes is in
 * the tab order, so Tab moves past the code instead of through every box.
 */
export function PairingCodeInput({
  value,
  onChange,
  label,
  length = 6,
  disabled = false,
  autoFocus = false,
}: {
  value: string;
  onChange: (value: string) => void;
  label: string;
  length?: number;
  disabled?: boolean;
  autoFocus?: boolean;
}) {
  const boxes = useRef<(HTMLInputElement | null)[]>([]);
  // Focus moves before the parent re-renders with the new value, so handlers
  // read the value just written rather than the last rendered prop.
  const latest = useRef(value);
  latest.current = value;
  const insertion = Math.min(value.length, length - 1);
  const focusBox = (index: number) =>
    boxes.current[Math.max(0, Math.min(index, length - 1))]?.focus();
  const update = (next: string, focus: number) => {
    latest.current = next;
    onChange(next);
    focusBox(focus);
  };
  // Writes digits starting at a box, replacing what is there. A full-length
  // code always replaces the whole value, whichever box received it.
  const write = (index: number, digits: string) => {
    if (!digits) return;
    if (digits.length >= length) {
      update(digits.slice(0, length), length - 1);
      return;
    }
    const code = latest.current;
    const start = Math.min(index, code.length);
    const next = (code.slice(0, start) + digits + code.slice(start + digits.length)).slice(
      0,
      length,
    );
    update(next, start + digits.length);
  };
  const remove = (at: number) => {
    const code = latest.current;
    if (at < 0 || at >= code.length) return;
    update(code.slice(0, at) + code.slice(at + 1), at);
  };

  // Focus once when shown, before the user can type; afterwards focus
  // follows typing.
  const initialFocus = useRef(autoFocus ? value.length : -1);
  useLayoutEffect(() => {
    if (initialFocus.current >= 0)
      boxes.current[Math.min(initialFocus.current, length - 1)]?.focus();
    initialFocus.current = -1;
  }, [length]);

  // Reconciles the box with an edit that arrived as an input event: soft
  // keyboards, autofill, cut and composition all land here.
  // Deletes in the direction of an input type, from the box that has focus.
  const removeFrom = (index: number, kind: string) => {
    if (kind === 'deleteContentForward') remove(index);
    else remove(latest.current[index] ? index : index - 1);
  };
  const reconcile = (index: number, input: HTMLInputElement, event?: InputEvent) => {
    const code = latest.current;
    const kind = event?.inputType ?? '';
    if (kind.startsWith('delete') || (!kind && !input.value)) {
      removeFrom(index, kind);
      return;
    }
    // Typed text is the event's data when it has any; otherwise (older
    // engines, some keyboards) it is what the box now holds.
    const typed = kind === 'insertText' ? (event?.data ?? null) : null;
    let digits = onlyDigits(typed ?? input.value);
    if (typed === null && code[index] && digits.length === 2) {
      // The box held one digit and gained one: keep the new one.
      digits = digits[0] === code[index] ? digits[1] : digits[0];
    }
    if (!digits) {
      input.value = code[index] ?? '';
      return;
    }
    write(index, digits);
  };

  return (
    <div class="pairing-code" role="group" aria-label={label}>
      {Array.from({ length }, (_, index) => (
        <input
          key={index}
          ref={(element) => {
            boxes.current[index] = element;
          }}
          value={value[index] ?? ''}
          inputMode="numeric"
          autoComplete={index === 0 ? 'one-time-code' : 'off'}
          aria-label={`Digit ${index + 1} of ${length}`}
          tabIndex={index === insertion ? 0 : -1}
          disabled={disabled}
          onMouseDown={(event) => {
            // Keep the code contiguous: clicking past the end goes to the
            // next empty box.
            if (index > latest.current.length) {
              event.preventDefault();
              focusBox(latest.current.length);
            }
          }}
          onFocus={(event) => event.currentTarget.select()}
          onPaste={(event) => {
            event.preventDefault();
            write(index, onlyDigits(event.clipboardData?.getData('text') ?? ''));
          }}
          onBeforeInput={(event) => {
            // Soft keyboards often send no Backspace keydown, and deleting in
            // an already-empty box fires no input event, so delete here.
            const kind = (event as unknown as InputEvent).inputType ?? '';
            if (!kind.startsWith('delete')) return;
            event.preventDefault();
            removeFrom(index, kind);
          }}
          onInput={(event) => {
            const native = event as unknown as InputEvent;
            if (native.isComposing) return;
            reconcile(index, event.currentTarget, native);
          }}
          onCompositionEnd={(event) => reconcile(index, event.currentTarget)}
          onKeyDown={(event) => {
            if (event.isComposing) return;
            if (event.key === 'Backspace') {
              event.preventDefault();
              remove(latest.current[index] ? index : index - 1);
            } else if (event.key === 'Delete') {
              event.preventDefault();
              remove(index);
            } else if (event.key === 'ArrowLeft') {
              event.preventDefault();
              focusBox(index - 1);
            } else if (event.key === 'ArrowRight') {
              event.preventDefault();
              focusBox(Math.min(index + 1, latest.current.length));
            }
          }}
        />
      ))}
    </div>
  );
}
