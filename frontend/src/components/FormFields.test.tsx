import { fireEvent, render, screen } from '@testing-library/preact';
import { describe, expect, it, vi } from 'vitest';
import { SearchField, SettingsSelect } from './FormFields';
import { ToggleSwitch } from './ToggleSwitch';

describe('form markup primitives', () => {
  it('shares the accessible switch markup between settings and MCP controls', () => {
    const change = vi.fn();
    const { container } = render(
      <label>
        Show archived sessions
        <ToggleSwitch checked={false} onChange={change} />
      </label>,
    );
    const checkbox = screen.getByRole('checkbox', { name: 'Show archived sessions' });
    expect(container.querySelector('.toggle-switch-input + .toggle-switch-track')).toBeVisible();
    expect(checkbox).not.toBeChecked();
    fireEvent.click(checkbox);
    expect(change).toHaveBeenCalledOnce();
  });

  it('retains accessible select labels, options and change events', () => {
    const change = vi.fn();
    render(
      <SettingsSelect id="modelSelect" label="Model" value="one" onChange={change}>
        <option value="one">One</option>
        <option value="two">Two</option>
      </SettingsSelect>,
    );
    const select = screen.getByRole('combobox', { name: 'Model', exact: true });
    expect(screen.getByLabelText('Model')).toBe(select);
    expect(screen.getAllByRole('option').map((option) => option.textContent)).toEqual([
      'One',
      'Two',
    ]);
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
