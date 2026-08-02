const { test, expect } = require('@playwright/test');

const baseURL = process.env.TERM_LLM_SMOKE_URL || 'http://127.0.0.1:18080/ui/';

async function openUI(page) {
  const errors = [];
  page.on('pageerror', (error) => errors.push(String(error)));
  await page.goto(baseURL);
  await page.waitForFunction(() => window.TermLLMConversation?.ConversationController && window.TermLLMApp?.trackPendingIntent);
  return errors;
}

test('scroll gaps remain bounded and tail materializes', async ({ page }) => {
  const errors = await openUI(page);
  const state = await page.evaluate(async () => {
    const controller = new window.TermLLMConversation.ConversationController('pw-gap', { maxMaterializedTurns: 3, overscanTurns: 0 });
    const makeIndex = (ids, rev) => ({
      rev, compaction_seq: -1, compaction_count: 0,
      rows: {
        ids, seqs: ids.map((id) => id - 1), roles: 'u'.repeat(ids.length), flags: ids.map(() => 0),
        response_ids: ids.map(() => ''), assistant_segment_ordinals: ids.map(() => -1),
      },
    });
    const initialIDs = Array.from({ length: 101 }, (_, index) => index + 20);
    controller.applyIndex(makeIndex(initialIDs, 1));
    controller.setViewport(100, 100, { deferBudget: true });
    controller.materialize([{ id: 120, sequence: 119, role: 'user', client_message_id: 'pw-tail', parts: [{ type: 'text', text: 'tail' }] }]);
    controller.enforceBudget();

    const scroller = document.createElement('div');
    scroller.style.cssText = 'height:120px;overflow:auto;position:fixed;left:-9999px;top:0;width:200px';
    document.body.appendChild(scroller);
    const render = () => {
      scroller.replaceChildren();
      for (const run of controller.renderRuns()) {
        const node = document.createElement('div');
        if (run.type === 'gap') {
          node.dataset.gap = '1';
          node.style.height = `${run.height}px`;
        } else {
          node.dataset.durableId = String(controller.ids[run.startOrdinal]);
          node.style.height = '40px';
        }
        scroller.appendChild(node);
      }
    };
    render();
    scroller.scrollTop = scroller.scrollHeight - scroller.clientHeight;
    const tailTop = () => scroller.querySelector('[data-durable-id="120"]').getBoundingClientRect().top - scroller.getBoundingClientRect().top;
    const beforeTop = tailTop();
    const adapter = {
      capture: () => ({ id: 120, top: tailTop() }),
      render,
      topForID: () => tailTop(),
      adjustScroll: (delta) => { scroller.scrollTop += delta; },
    };
    const allIDs = Array.from({ length: 120 }, (_, index) => index + 1);
    await controller.withViewportAnchor(adapter, async () => controller.applyIndex(makeIndex(allIDs, 2)));
    const result = {
      gaps: controller.renderRuns().filter((run) => run.type === 'gap').length,
      materialized: controller.renderRuns().filter((run) => run.type === 'segment').length,
      beforeTop,
      afterTop: tailTop(),
    };
    scroller.remove();
    return result;
  });
  expect(state.gaps).toBeGreaterThan(0);
  expect(state.materialized).toBe(1);
  expect(Math.abs(state.afterTop - state.beforeTop)).toBeLessThan(1);
  expect(errors).toEqual([]);
});

test('terminal projection hands off atomically to durable rows', async ({ page }) => {
  await openUI(page);
  const result = await page.evaluate(() => {
    const api = window.TermLLMConversation;
    const controller = new api.ConversationController('pw-handoff');
    controller.applyIndex({ rev: 0, compaction_seq: -1, compaction_count: 0, rows: { ids: [], seqs: [], roles: '', flags: [], response_ids: [], assistant_segment_ordinals: [] } });
    controller.setActiveRun('pw-response', 0, 1, { clientMessageId: 'pw-user' });
    controller.addPendingIntent({ id: 'pw-user', clientMessageId: 'pw-user', role: 'user', content: 'question' });
    controller.applyResponseEvent('response.output_text.delta', { response_id: 'pw-response', run_epoch: 1, sequence_number: 1, delta: 'answer' });
    controller.applyResponseEvent('response.completed', {
      response_id: 'pw-response', run_epoch: 1, sequence_number: 2,
      final_rev: 2, durable_handoff: true, durable_output_count: 1,
      handoff_compaction_seq: -1, handoff_compaction_count: 0,
    });
    const before = api.sessionMessages({ transcript: controller }).map((message) => message.content);
    controller.applyIndex({
      rev: 2, compaction_seq: -1, compaction_count: 0,
      rows: { ids: [1, 2], seqs: [0, 1], roles: 'ua', flags: [0, 0], client_message_ids: { 0: 'pw-user' }, response_ids: ['', 'pw-response'], assistant_segment_ordinals: [-1, 0] },
    });
    controller.materialize([
      { id: 1, sequence: 0, role: 'user', client_message_id: 'pw-user', parts: [{ type: 'text', text: 'question' }] },
      { id: 2, sequence: 1, role: 'assistant', response_id: 'pw-response', assistant_segment_ordinal: 0, parts: [{ type: 'text', text: 'answer' }] },
    ]);
    controller.publishedMessages = [
      { id: 1, durableRowId: 1, role: 'user', clientMessageId: 'pw-user', content: 'question', durable: true },
      { id: 2, durableRowId: 2, role: 'assistant', responseId: 'pw-response', content: 'answer', durable: true },
    ];
    api.applyDurable(controller.conversation, controller);
    return { before, after: api.sessionMessages({ transcript: controller }).map((message) => message.content), active: controller.activeRun };
  });
  expect(result.before).toEqual(['question', 'answer']);
  expect(result.after).toEqual(['question', 'answer']);
  expect(result.active).toBeNull();
});

test('reload while streaming recovers complete active projection from server snapshot', async ({ page }) => {
  const responseId = 'pw-reload-run';
  await page.route(`**/ui/v1/responses/${responseId}`, async (route) => route.fulfill({
    status: 200, contentType: 'application/json',
    body: JSON.stringify({
      id: responseId, run_epoch: 1, status: 'failed', last_sequence_number: 2,
      final_rev: 0, durable_handoff: false, durable_output_count: 1,
      durable_handoff_error: 'synthetic persistence failure keeps output frozen',
      recovery: { sequence_number: 2, messages: [{ role: 'assistant', assistant_segment_ordinal: 0, content: 'partial survives reload' }] },
    }),
  }));
  await openUI(page);
  const before = await page.evaluate((id) => {
    const api = window.TermLLMConversation;
    const controller = new api.ConversationController('pw-streaming-reload');
    controller.setActiveRun(id, 0, 1, { anchorRowId: 1 });
    controller.applyResponseEvent('response.output_text.delta', { response_id: id, run_epoch: 1, sequence_number: 1, delta: 'partial survives reload' });
    return api.sessionMessages({ transcript: controller }).map((message) => message.content);
  }, responseId);
  expect(before).toEqual(['partial survives reload']);

  await page.reload();
  await page.waitForFunction(() => window.TermLLMApp?.resumeActiveResponse && window.TermLLMApp?.ensureSessionTranscript);
  const recovered = await page.evaluate(async (id) => {
    const app = window.TermLLMApp;
    const session = { id: 'pw-streaming-reload', activeResponseId: id, lastResponseId: null };
    app.state.sessions = [session];
    app.state.activeSessionId = session.id;
    app.state.draftSessionActive = false;
    app.ensureSessionTranscript(session);
    await app.resumeActiveResponse(session, { responseId: id, recoverFromSnapshot: true });
    return {
      messages: window.TermLLMConversation.sessionMessages(session).map((message) => message.content),
      terminal: session.transcript.activeRun?.terminal,
      protocolError: session.transcript.conversation.protocolError,
    };
  }, responseId);
  expect(recovered.messages).toEqual(['partial survives reload']);
  expect(recovered.terminal.status).toBe('failed');
  expect(recovered.protocolError).toContain('synthetic persistence failure');
});

test('hidden tab catches up durable revision before attaching newer run', async ({ context, page }) => {
  await openUI(page);
  await page.evaluate(() => {
    const app = window.TermLLMApp;
    const api = window.TermLLMConversation;
    const session = { id: 'pw-tabs', activeResponseId: 'old-run' };
    app.state.sessions = [session];
    app.state.activeSessionId = session.id;
    app.state.connected = true;
    const controller = app.ensureSessionTranscript(session);
    controller.applyIndex({ rev: 1, compaction_seq: -1, compaction_count: 0, rows: { ids: [1], seqs: [0], roles: 'u', flags: [0], client_message_ids: { 0: 'tab-a' }, response_ids: [''], assistant_segment_ordinals: [-1] } });
    controller.setActiveRun('old-run', 1, 1, { clientMessageId: 'tab-a' });
    window.__pwCatchupOrder = [];
    let visibility = 'visible';
    Object.defineProperty(document, 'visibilityState', { configurable: true, get: () => visibility });
    window.__pwSetVisibility = (next) => {
      visibility = next;
      document.dispatchEvent(new Event('visibilitychange'));
    };
    app.startSidebarStatusPoll = async () => controller.commands.enqueue(() => {
      controller.applyIndex({
        rev: 2, compaction_seq: -1, compaction_count: 0,
        rows: { ids: [1, 2], seqs: [0, 1], roles: 'uu', flags: [0, 0], client_message_ids: { 0: 'tab-a', 1: 'tab-b' }, response_ids: ['', ''], assistant_segment_ordinals: [-1, -1] },
      });
      session.activeResponseId = 'new-run';
      window.__pwCatchupOrder.push(`durable:${controller.rev}`);
    });
    app.resumeAndDrain = (activeSession, { responseId }) => {
      controller.commands.enqueue(() => {
        api.attachActiveRun(controller, { active_response_id: responseId, run_epoch: 2, started_rev: 0, client_message_id: 'tab-b' }, true);
        window.__pwCatchupOrder.push(`active:${controller.activeRun.id}`);
      });
      return Promise.resolve(activeSession);
    };
    window.__pwSetVisibility('hidden');
  });

  const second = await context.newPage();
  await openUI(second);
  await second.evaluate(() => window.TermLLMApp.trackPendingIntent({ id: 'pw-tabs' }, { id: 'tab-b', clientMessageId: 'tab-b', role: 'user', content: 'B' }));
  await page.evaluate(() => window.__pwSetVisibility('visible'));
  await page.waitForFunction(() => window.__pwCatchupOrder.length === 2);

  const result = await page.evaluate(() => {
    const controller = window.TermLLMApp.state.sessions[0].transcript;
    const stored = window.TermLLMApp.readPendingIntentRegistry()['pw-tabs'].map((intent) => intent.clientMessageId).sort();
    return { active: controller.activeRun, stored, order: window.__pwCatchupOrder };
  });
  expect(result.active.id).toBe('new-run');
  expect(result.active.epoch).toBe(2);
  expect(result.stored).toEqual(['tab-b']);
  expect(result.order).toEqual(['durable:2', 'active:new-run']);
  await second.close();
});
