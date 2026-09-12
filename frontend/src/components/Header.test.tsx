import { render, screen } from '@testing-library/preact';
import userEvent from '@testing-library/user-event';
import { afterEach, describe, expect, it } from 'vitest';
import { StoreContext } from '../app/context';
import { initialProjection } from '../domain/response';
import { AppStore } from '../stores/app-store';
import { testConfig, testSession } from '../stores/store-test-fixtures';
import { Header } from './Header';

let store: AppStore | undefined;
afterEach(() => {
  store?.dispose();
  store = undefined;
});

describe('runtime header', () => {
  it('keeps the selected model visible while its session swap is in progress', async () => {
    store = new AppStore(testConfig);
    store.selectedProvider.value = 'chatgpt';
    store.selectedModel.value = 'gpt-5.6-sol';
    store.providers.value = [{ id: 'chatgpt', name: 'chatgpt' }];
    store.models.value = [
      { id: 'gpt-6-astra', name: 'gpt-6-astra', provider: 'chatgpt' },
      { id: 'gpt-5.6-sol', name: 'gpt-5.6-sol', provider: 'chatgpt' },
    ];
    store.sessions.value = [testSession({ activeProvider: 'chatgpt', activeModel: 'gpt-6-astra' })];
    store.activeSessionId.value = 's1';
    store.draftActive.value = false;
    store.runs.value = {
      s1: {
        ...initialProjection({
          responseId: 'response-swap',
          sessionId: 's1',
          epoch: 1,
          status: 'streaming',
          lastSequence: 1,
          startedRev: 0,
          reconnects: 0,
        }),
        modelSwap: {
          stage: 'naive_start',
          content: 'Switching model…',
          fromProvider: 'chatgpt',
          fromModel: 'gpt-6-astra',
          fromEffort: 'medium',
          toProvider: 'chatgpt',
          toModel: 'gpt-5.6-sol',
          toEffort: 'medium',
          swapStatus: 'started',
          swapStrategy: '',
        },
      },
    };

    render(
      <StoreContext.Provider value={store}>
        <Header />
      </StoreContext.Provider>,
    );

    const trigger = screen.getByRole('button', { name: /^Runtime settings:/ });
    expect(trigger).toHaveTextContent('gpt-5.6-sol');
    expect(trigger).not.toHaveTextContent('gpt-6-astra');
    await userEvent.click(trigger);
    expect(screen.getByRole('combobox', { name: 'Runtime model' })).toBeDisabled();
    expect(screen.getByRole('combobox', { name: 'Runtime model' })).toHaveValue('gpt-5.6-sol');
  });
});
