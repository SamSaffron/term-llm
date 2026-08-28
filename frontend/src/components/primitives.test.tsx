import { fireEvent, render, screen, waitFor } from '@testing-library/preact';
import userEvent from '@testing-library/user-event';
import { useRef, useState } from 'preact/hooks';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { overlayManager } from '../platform/overlay-manager';
import { Drawer } from './Drawer';
import { Menu } from './Menu';
import { Overlay } from './Overlay';
import { useEdgeSwipeOpen } from './useSwipeDismiss';

afterEach(() => {
  overlayManager.reset();
  document.body.innerHTML = '';
});

describe('shared interaction primitives', () => {
  it('traps Drawer focus, dismisses with Escape, restores focus, and releases inert state', async () => {
    function Fixture() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <div id="appShell">
            <button onClick={() => setOpen(true)}>Open navigation</button>
          </div>
          <Drawer open={open} title="Navigation" side="left" onClose={() => setOpen(false)}>
            <button>First action</button>
            <button>Last action</button>
          </Drawer>
        </>
      );
    }
    render(<Fixture />);
    const trigger = screen.getByRole('button', { name: 'Open navigation' });
    await userEvent.click(trigger);
    const dialog = screen.getByRole('dialog', { name: 'Navigation' });
    await waitFor(() => expect(screen.getByRole('button', { name: 'First action' })).toHaveFocus());
    expect(document.getElementById('appShell')?.inert).toBe(true);

    screen.getByRole('button', { name: 'Last action' }).focus();
    fireEvent.keyDown(dialog, { key: 'Tab' });
    expect(screen.getByRole('button', { name: 'First action' })).toHaveFocus();
    fireEvent.keyDown(dialog, { key: 'Escape' });

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Navigation' })).toBeNull());
    expect(document.getElementById('appShell')?.inert).toBe(false);
    expect(trigger).toHaveFocus();
  });

  it('keeps nested Overlay ownership until the final surface closes', async () => {
    function Fixture() {
      const [outer, setOuter] = useState(false);
      const [inner, setInner] = useState(false);
      return (
        <>
          <div id="appShell">
            <button onClick={() => setOuter(true)}>Open outer</button>
          </div>
          {outer && (
            <Overlay title="Outer" onClose={() => setOuter(false)}>
              <button onClick={() => setInner(true)}>Open inner</button>
              {inner && (
                <Overlay title="Inner" onClose={() => setInner(false)}>
                  <button>Inner action</button>
                </Overlay>
              )}
            </Overlay>
          )}
        </>
      );
    }
    render(<Fixture />);
    await userEvent.click(screen.getByRole('button', { name: 'Open outer' }));
    await userEvent.click(screen.getByRole('button', { name: 'Open inner' }));
    fireEvent.keyDown(screen.getByRole('dialog', { name: 'Inner' }), { key: 'Escape' });
    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Inner' })).toBeNull());
    await waitFor(() => expect(screen.getByRole('dialog', { name: 'Outer' })).toBeInTheDocument());
    expect(document.getElementById('appShell')?.inert).toBe(true);
    fireEvent.keyDown(screen.getByRole('dialog', { name: 'Outer' }), { key: 'Escape' });
    await waitFor(() => expect(document.getElementById('appShell')?.inert).toBe(false));
  });

  it('implements the declared menu Arrow/Home/End/Escape contract', async () => {
    function Fixture() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen(!open)}>
            Actions
          </button>
          <Menu open={open} label="Actions" onClose={() => setOpen(false)}>
            <button role="menuitem">Alpha</button>
            <button role="menuitem">Beta</button>
            <button role="menuitem">Gamma</button>
          </Menu>
        </>
      );
    }
    render(<Fixture />);
    await userEvent.click(screen.getByRole('button', { name: 'Actions' }));
    const menu = screen.getByRole('menu', { name: 'Actions' });
    await waitFor(() => expect(screen.getByRole('menuitem', { name: 'Alpha' })).toHaveFocus());
    fireEvent.keyDown(menu, { key: 'End' });
    expect(screen.getByRole('menuitem', { name: 'Gamma' })).toHaveFocus();
    fireEvent.keyDown(menu, { key: 'Home' });
    expect(screen.getByRole('menuitem', { name: 'Alpha' })).toHaveFocus();
    fireEvent.keyDown(menu, { key: 'ArrowUp' });
    expect(screen.getByRole('menuitem', { name: 'Gamma' })).toHaveFocus();
    fireEvent.keyDown(menu, { key: 'Escape' });
    await waitFor(() => expect(screen.queryByRole('menu')).toBeNull());
  });

  it('dismisses a menu when focus moves outside without trapping Tab', async () => {
    function Fixture() {
      const [open, setOpen] = useState(false);
      return (
        <>
          <button aria-haspopup="menu" aria-expanded={open} onClick={() => setOpen(!open)}>
            Actions
          </button>
          <Menu open={open} label="Actions" onClose={() => setOpen(false)}>
            <button role="menuitem">Alpha</button>
            <button role="menuitem">Beta</button>
          </Menu>
          <button>Outside</button>
        </>
      );
    }
    render(<Fixture />);
    await userEvent.click(screen.getByRole('button', { name: 'Actions' }));
    await waitFor(() => expect(screen.getByRole('menuitem', { name: 'Alpha' })).toHaveFocus());

    screen.getByRole('button', { name: 'Outside' }).focus();

    await waitFor(() => expect(screen.queryByRole('menu')).toBeNull());
    expect(screen.getByRole('button', { name: 'Outside' })).toHaveFocus();
  });

  it('dismisses a right drawer with a deliberate touch swipe', async () => {
    function Fixture() {
      const [open, setOpen] = useState(true);
      return (
        <div id="appShell">
          <Drawer open={open} title="Changes" onClose={() => setOpen(false)}>
            <button>Action</button>
          </Drawer>
        </div>
      );
    }
    render(<Fixture />);
    const dialog = screen.getByRole('dialog', { name: 'Changes' });
    vi.spyOn(dialog, 'getBoundingClientRect').mockReturnValue({
      width: 300,
      height: 500,
      top: 0,
      right: 300,
      bottom: 500,
      left: 0,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    });

    fireEvent.pointerDown(dialog, {
      pointerId: 7,
      pointerType: 'touch',
      isPrimary: true,
      clientX: 100,
      clientY: 100,
    });
    fireEvent.pointerMove(dialog, {
      pointerId: 7,
      pointerType: 'touch',
      isPrimary: true,
      clientX: 210,
      clientY: 102,
    });
    fireEvent.pointerUp(dialog, {
      pointerId: 7,
      pointerType: 'touch',
      isPrimary: true,
      clientX: 220,
      clientY: 102,
    });

    await waitFor(() => expect(screen.queryByRole('dialog', { name: 'Changes' })).toBeNull());
  });

  it('opens an off-canvas surface from a deliberate edge swipe', async () => {
    function Fixture() {
      const [open, setOpen] = useState(false);
      const surface = useRef<HTMLElement>(null);
      useEdgeSwipeOpen(surface, {
        enabled: !open,
        property: '--panel-swipe-offset-x',
        onOpen: () => setOpen(true),
      });
      return (
        <aside ref={surface} aria-label="Navigation" data-open={open}>
          Navigation
        </aside>
      );
    }
    render(<Fixture />);
    const navigation = screen.getByLabelText('Navigation');
    vi.spyOn(navigation, 'getBoundingClientRect').mockReturnValue({
      width: 300,
      height: 500,
      top: 0,
      right: 300,
      bottom: 500,
      left: 0,
      x: 0,
      y: 0,
      toJSON: () => ({}),
    });

    fireEvent.pointerDown(window, {
      pointerId: 9,
      pointerType: 'touch',
      isPrimary: true,
      clientX: 4,
      clientY: 100,
    });
    fireEvent.pointerMove(window, {
      pointerId: 9,
      pointerType: 'touch',
      isPrimary: true,
      clientX: 120,
      clientY: 102,
    });
    fireEvent.pointerUp(window, {
      pointerId: 9,
      pointerType: 'touch',
      isPrimary: true,
      clientX: 130,
      clientY: 102,
    });

    await waitFor(() => expect(navigation).toHaveAttribute('data-open', 'true'));
  });
});
