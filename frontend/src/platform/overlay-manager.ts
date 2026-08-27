interface BlockedElement {
  element: HTMLElement;
  inert: boolean;
}

interface OverlayEntry {
  token: symbol;
  returnFocus: HTMLElement | null;
  surface: HTMLElement | null;
  surfaceWasInert: boolean;
  blocked: BlockedElement[];
}

export class OverlayManager {
  private readonly stack: OverlayEntry[] = [];
  private previousOverflow = '';

  private blockBackground(surface: HTMLElement | null): BlockedElement[] {
    const shell = document.getElementById('appShell');
    if (!shell) return [];
    const blocked: BlockedElement[] = [];
    if (!surface || !shell.contains(surface)) {
      blocked.push({ element: shell, inert: Boolean(shell.inert) });
      shell.inert = true;
      return blocked;
    }
    // A drawer can be rendered inside the application shell. Inert siblings
    // along its ancestor path rather than inerting the drawer's own ancestor.
    let branch: HTMLElement | null = surface;
    while (branch && branch !== shell) {
      const parent: HTMLElement | null = branch.parentElement;
      if (!parent) break;
      for (const sibling of parent.children) {
        if (sibling === branch || !(sibling instanceof HTMLElement)) continue;
        blocked.push({ element: sibling, inert: Boolean(sibling.inert) });
        sibling.inert = true;
      }
      branch = parent;
    }
    return blocked;
  }

  acquire(
    returnFocus = document.activeElement as HTMLElement | null,
    surface: HTMLElement | null = null,
  ): symbol {
    const token = Symbol('overlay');
    let blocked: BlockedElement[] = [];
    if (!this.stack.length) {
      blocked = this.blockBackground(surface);
      this.previousOverflow = document.documentElement.style.overflow;
      document.documentElement.style.overflow = 'hidden';
    } else {
      const lower = this.stack.at(-1);
      if (lower?.surface) lower.surface.inert = true;
    }
    this.stack.push({
      token,
      returnFocus,
      surface,
      surfaceWasInert: surface?.inert || false,
      blocked,
    });
    return token;
  }

  release(token: symbol): void {
    const index = this.stack.findIndex((entry) => entry.token === token);
    if (index < 0) return;
    const wasTop = index === this.stack.length - 1;
    const [entry] = this.stack.splice(index, 1);
    if (!wasTop && this.stack[index]) {
      if (!this.stack[index].returnFocus?.isConnected)
        this.stack[index].returnFocus = entry.returnFocus;
      if (entry.blocked.length) this.stack[index].blocked.push(...entry.blocked);
    }
    if (!this.stack.length) {
      entry.blocked.forEach(({ element, inert }) => {
        if (element.isConnected) element.inert = inert;
      });
      if (entry.surface) entry.surface.inert = entry.surfaceWasInert;
      document.documentElement.style.overflow = this.previousOverflow;
    } else {
      this.stack.forEach((remaining, remainingIndex) => {
        if (remaining.surface)
          remaining.surface.inert =
            remainingIndex === this.stack.length - 1 ? remaining.surfaceWasInert : true;
      });
    }
    if (wasTop && entry.returnFocus?.isConnected) entry.returnFocus.focus({ preventScroll: true });
  }

  isTop(token: symbol): boolean {
    return this.stack.at(-1)?.token === token;
  }

  get size(): number {
    return this.stack.length;
  }

  reset(): void {
    for (const entry of this.stack) {
      if (entry.surface) entry.surface.inert = entry.surfaceWasInert;
      entry.blocked.forEach(({ element, inert }) => {
        if (element.isConnected) element.inert = inert;
      });
    }
    this.stack.splice(0);
    document.documentElement.style.overflow = this.previousOverflow;
  }
}

export const overlayManager = new OverlayManager();
