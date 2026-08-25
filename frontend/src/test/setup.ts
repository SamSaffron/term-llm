import '@testing-library/jest-dom/vitest';
import { cleanup } from '@testing-library/preact';
import { afterEach, vi } from 'vitest';

// Node 25+ exposes incomplete process-level Web Storage globals unless it was
// launched with storage-file flags, and test runners can copy those placeholders
// onto jsdom's window. Install a standards-shaped in-memory implementation so
// tests do not depend on Node flags or a writable filesystem path.
class MemoryStorage implements Storage {
  private readonly values = new Map<string, string>();
  get length(): number { return this.values.size; }
  clear(): void { this.values.clear(); }
  getItem(key: string): string | null { return this.values.get(String(key)) ?? null; }
  key(index: number): string | null { return [...this.values.keys()][index] ?? null; }
  removeItem(key: string): void { this.values.delete(String(key)); }
  setItem(key: string, value: string): void { this.values.set(String(key), String(value)); }
}
for (const name of ['localStorage', 'sessionStorage'] as const) {
  const storage = new MemoryStorage();
  Object.defineProperty(window, name, { configurable: true, enumerable: true, value: storage });
  if (globalThis !== window) Object.defineProperty(globalThis, name, { configurable: true, enumerable: true, value: storage });
}

afterEach(() => { cleanup(); vi.restoreAllMocks(); localStorage.clear(); sessionStorage.clear(); });
Object.defineProperty(window, 'matchMedia', { configurable: true, value: vi.fn(() => ({ matches: false, addEventListener: vi.fn(), removeEventListener: vi.fn() })) });
Object.defineProperty(Element.prototype, 'scrollTo', { configurable: true, value: vi.fn() });
