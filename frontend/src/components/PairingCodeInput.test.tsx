import { fireEvent, render, screen } from '@testing-library/preact';
import { useState } from 'preact/hooks';
import { describe, expect, it } from 'vitest';
import { PairingCodeInput } from './PairingCodeInput';

function Harness({ initial = '', disabled = false }: { initial?: string; disabled?: boolean }) {
  const [value, setValue] = useState(initial);
  return (
    <>
      <PairingCodeInput label="Code" value={value} onChange={setValue} disabled={disabled} />
      <output data-testid="value">{value}</output>
      <button type="button">After</button>
    </>
  );
}

const boxes = () => screen.getAllByRole('textbox') as HTMLInputElement[];
const value = () => screen.getByTestId('value').textContent;
const typeDigit = (box: HTMLInputElement, digit: string) =>
  fireEvent.input(box, { target: { value: digit }, inputType: 'insertText', data: digit });
const paste = (box: HTMLInputElement, text: string) =>
  fireEvent.paste(box, { clipboardData: { getData: () => text } });

describe('PairingCodeInput', () => {
  it('types one digit per box, ignores letters, and replaces a typed-over digit', () => {
    render(<Harness />);
    typeDigit(boxes()[0], '1');
    expect(boxes()[1]).toHaveFocus();
    typeDigit(boxes()[1], 'x');
    expect(value()).toBe('1');
    expect(boxes()[1].value).toBe('');
    typeDigit(boxes()[1], '2');
    // Typing into a filled box replaces it even when the caret sat before
    // the old digit (the box would read "92").
    fireEvent.input(boxes()[0], { target: { value: '91' }, inputType: 'insertText', data: '9' });
    expect(value()).toBe('92');
  });

  it('keeps only the next box in the tab order so Tab leaves the code', () => {
    render(<Harness initial="12" />);
    expect(boxes().map((box) => box.tabIndex)).toEqual([-1, -1, 0, -1, -1, -1]);
    const later = boxes()[5];
    const click = fireEvent.mouseDown(later);
    expect(click).toBe(false);
    expect(boxes()[2]).toHaveFocus();
  });

  it('spreads a full pasted code from any box and pastes partial codes in place', () => {
    render(<Harness initial="123456" />);
    paste(boxes()[2], '987654');
    expect(value()).toBe('987654');
    paste(boxes()[2], '39');
    expect(value()).toBe('983954');
    paste(boxes()[0], 'abc');
    expect(value()).toBe('983954');
  });

  it('accepts a whole autofilled code arriving in the first box', () => {
    render(<Harness />);
    fireEvent.input(boxes()[0], {
      target: { value: '393642' },
      inputType: 'insertReplacementText',
    });
    expect(value()).toBe('393642');
    expect(boxes()[5]).toHaveFocus();
  });

  it('deletes with Backspace, Delete, and input-only deletions from soft keyboards', () => {
    render(<Harness initial="1234" />);
    fireEvent.keyDown(boxes()[3], { key: 'Backspace' });
    expect(value()).toBe('123');
    fireEvent.keyDown(boxes()[3], { key: 'Backspace' });
    expect(value()).toBe('12');
    expect(boxes()[2]).toHaveFocus();
    fireEvent.keyDown(boxes()[0], { key: 'Delete' });
    expect(value()).toBe('2');
    fireEvent.input(boxes()[0], { target: { value: '' }, inputType: 'deleteContentBackward' });
    expect(value()).toBe('');
  });

  it('deletes on beforeinput when a soft keyboard sends no keydown or input event', () => {
    render(<Harness initial="12" />);
    const backward = fireEvent(
      boxes()[2],
      new InputEvent('beforeinput', {
        bubbles: true,
        cancelable: true,
        inputType: 'deleteContentBackward',
      }),
    );
    expect(backward).toBe(false);
    expect(value()).toBe('1');
    expect(boxes()[1]).toHaveFocus();
    fireEvent(
      boxes()[1],
      new InputEvent('beforeinput', {
        bubbles: true,
        cancelable: true,
        inputType: 'deleteContentForward',
      }),
    );
    expect(value()).toBe('1');
  });

  it('reads the digit from the box when insertText carries no data', () => {
    render(<Harness initial="1" />);
    fireEvent.input(boxes()[1], { target: { value: '7' }, inputType: 'insertText', data: null });
    expect(value()).toBe('17');
    fireEvent.input(boxes()[0], { target: { value: '15' }, inputType: 'insertText', data: null });
    expect(value()).toBe('57');
  });

  it('moves with the arrow keys without passing the end of the code', () => {
    render(<Harness initial="12" />);
    boxes()[1].focus();
    fireEvent.keyDown(boxes()[1], { key: 'ArrowLeft' });
    expect(boxes()[0]).toHaveFocus();
    fireEvent.keyDown(boxes()[0], { key: 'ArrowRight' });
    fireEvent.keyDown(boxes()[1], { key: 'ArrowRight' });
    fireEvent.keyDown(boxes()[2], { key: 'ArrowRight' });
    expect(boxes()[2]).toHaveFocus();
  });

  it('disables every box', () => {
    render(<Harness initial="12" disabled />);
    expect(boxes().every((box) => box.disabled)).toBe(true);
  });
});
