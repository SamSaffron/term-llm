interface BlockedElement {
  element: HTMLElement;
  inert: boolean;
}

interface OverlayEntry {
  token: symbol;
  returnFocus: HTMLElement | null;
  surface: HTMLElement | null;
  surfaceWasInert: boolean;
  surfaceZIndex: string;
  blocked: BlockedElement[];
}

export class OverlayManager {
  private readonly stack: OverlayEntry[] = [];
  private previousOverflow = '';

  private blockBackground(surface: HTMLElement | null): BlockedElement[] {
    const shell = document.getElementById('appShell');
    const toastRegion = document.getElementById('toastRegion');
    const blocked: BlockedElement[] = [];
    const block = (element: HTMLElement | null) => {
      if (!element || element === surface || element.contains(surface)) return;
      blocked.push({ element, inert: Boolean(element.inert) });
      element.inert = true;
    };
    if (!shell) {
      block(toastRegion);
      return blocked;
    }
    if (!surface || !shell.contains(surface)) {
      block(shell);
      block(toastRegion);
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
        block(sibling);
      }
      branch = parent;
    }
    block(toastRegion);
    return blocked;
  }

  acquire(
    returnFocus = document.activeElement as HTMLElement | null,
    surface: HTMLElement | null = null,
  ): symbol {
    const token = Symbol('overlay');
    let blocked: BlockedElement[] = [];
    const surfaceZIndex = surface?.style.zIndex || '';
    if (surface) surface.style.zIndex = String(1000 + this.stack.length);
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
      surfaceZIndex,
      blocked,
    });
    return token;
  }

  release(token: symbol): void {
    const index = this.stack.findIndex((entry) => entry.token === token);
    if (index < 0) return;
    const wasTop = index === this.stack.length - 1;
    const [entry] = this.stack.splice(index, 1);
    if (entry.surface) entry.surface.style.zIndex = entry.surfaceZIndex;
    if (!wasTop && this.stack[index]) {
      if (!this.stack[index].returnFocus?.isConnected)
        this.stack[index].returnFocus = entry.returnFocus;
      if (entry.blocked.length) this.stack[index].blocked.push(...entry.blocked);
    }
    this.stack.forEach((remaining, remainingIndex) => {
      if (remaining.surface) remaining.surface.style.zIndex = String(1000 + remainingIndex);
    });
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
      if (entry.surface) {
        entry.surface.inert = entry.surfaceWasInert;
        entry.surface.style.zIndex = entry.surfaceZIndex;
      }
      entry.blocked.forEach(({ element, inert }) => {
        if (element.isConnected) element.inert = inert;
      });
    }
    this.stack.splice(0);
    document.documentElement.style.overflow = this.previousOverflow;
  }
}

export const overlayManager = new OverlayManager();
