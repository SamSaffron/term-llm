import { fireEvent, render, screen } from '@testing-library/preact';
import { describe, expect, it, vi } from 'vitest';
import { SearchField, SettingsSelect } from './FormFields';

describe('form markup primitives', () => {
  it('retains select labels, options, DOM structure and change events', () => {
    const change = vi.fn();
    const { container } = render(
      <SettingsSelect id="modelSelect" label="Model" value="one" onChange={change}>
        <option value="one">One</option>
        <option value="two">Two</option>
      </SettingsSelect>,
    );
    const select = screen.getByLabelText('Model');
    expect(container.querySelector('.settings-field > label + select.settings-select')).toBe(
      select,
    );
    expect(select).toHaveValue('one');
    fireEvent.change(select, { target: { value: 'two' } });
    expect(change).toHaveBeenCalledOnce();
    expect(select).toHaveValue('two');
  });

  it('retains search icon variants, classes and input/keyboard handlers', () => {
    const input = vi.fn();
    const key = vi.fn((event: KeyboardEvent) => {
      if (event.key === 'Escape') event.preventDefault();
    });
    const { container } = render(
      <SearchField
        className="mcp-server-search"
        iconPath="m16.5 16.5 4 4"
        aria-label="Filter MCP servers"
        value=""
        placeholder="Filter servers…"
        onInput={input}
        onKeyDown={key}
      />,
    );
    const field = screen.getByRole('searchbox', { name: 'Filter MCP servers' });
    expect(
      container.querySelector('label.mcp-server-search > span.mcp-server-search-icon + input'),
    ).toBe(field);
    expect(container.querySelector('path')).toHaveAttribute('d', 'm16.5 16.5 4 4');
    fireEvent.input(field, { target: { value: 'git' } });
    expect(input).toHaveBeenCalledOnce();
    expect(fireEvent.keyDown(field, { key: 'Escape' })).toBe(false);
    expect(key).toHaveBeenCalledOnce();
  });
});
