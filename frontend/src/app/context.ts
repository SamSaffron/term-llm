import { createContext } from 'preact';
import { useContext } from 'preact/hooks';
import type { AppStore } from '../stores/app-store';

export const StoreContext = createContext<AppStore | null>(null);
export function useStore(): AppStore {
  const store = useContext(StoreContext);
  if (!store) throw new Error('AppStore is not mounted');
  return store;
}
