import { describe, expect, it, vi } from 'vitest';
import {
  convertServerMessages,
  indexTranscriptTurns,
  mergeDurableProjection,
  sanitizeSession,
  windowTranscript,
} from './transcript';
import type { Message } from './types';

describe('transcript domain', () => {
  it('converts text, measured media, tools and compaction rows', () => {
    vi.setSystemTime(new Date('2026-01-01T00:00:00Z'));
    const messages = convertServerMessages([
      {
        id: 1,
        sequence: 0,
        role: 'user',
        client_message_id: 'c1',
        interrupt_state: 'queue',
        parts: [
          { type: 'text', text: 'hello' },
          {
            type: 'image',
            image_url: '/image.png',
            mime_type: 'image/png',
            width: 800,
            height: 400,
          },
        ],
      },
      {
        id: 2,
        sequence: 1,
        role: 'assistant',
        response_id: 'r1',
        assistant_segment_ordinal: 0,
        parts: [{ type: 'text', text: 'answer' }],
      },
      {
        id: 3,
        sequence: 2,
        role: 'tool',
        parts: [{ type: 'function_call', call_id: 't1', name: 'read_file', arguments: '{}' }],
      },
      { id: 4, sequence: 3, role: 'compaction', parts: [{ type: 'text', text: 'summary' }] },
    ]);
    expect(messages[0]).toMatchObject({
      role: 'user',
      content: 'hello',
      clientMessageId: 'c1',
      interruptState: 'queue',
      attachments: [{ width: 800, height: 400 }],
    });
    expect(messages[1]).toMatchObject({ role: 'assistant', responseId: 'r1', content: 'answer' });
    expect(messages[2].tools?.[0]).toMatchObject({ id: 't1', name: 'read_file' });
    expect(messages[3]).toMatchObject({ role: 'compaction-boundary', rawContent: 'summary' });
  });

  it('sanitizes server session defaults and seconds timestamps', () => {
    const session = sanitizeSession({
      id: 's1',
      created_at: 1_700_000_000,
      title: '',
      pinned: 1,
      file_change_summary: { file_count: 2, adds: 7, dels: 3, git: true },
      messages: [],
    });
    expect(session).toMatchObject({
      id: 's1',
      title: 'New chat',
      mode: 'chat',
      origin: 'web',
      pinned: true,
      created: 1_700_000_000_000,
      fileChangeSummary: { fileCount: 2, additions: 7, deletions: 3, git: true },
    });
  });

  it('prefers server-resolved titles while retaining editable generated metadata', () => {
    const session = sanitizeSession({
      id: 's1',
      name: 'Manual name',
      short_title: 'Manual name',
      long_title: 'Preferred detail',
      generated_short_title: 'Older generated title',
      generated_long_title: 'Older generated detail',
      messages: [],
    });
    expect(session).toMatchObject({
      name: 'Manual name',
      title: 'Manual name',
      longTitle: 'Preferred detail',
      generatedShortTitle: 'Older generated title',
      generatedLongTitle: 'Older generated detail',
    });
  });

  it('windows old turns behind a stable gap and always keeps the tail', () => {
    const messages = Array.from({ length: 240 }, (_, index): Message => ({
      id: String(index),
      role: index % 3 === 0 ? 'user' : 'assistant',
      content: String(index),
      created: index,
    }));
    const runs = windowTranscript(messages, 3, true);
    expect(runs[0]).toMatchObject({ type: 'gap' });
    expect(runs[1].messages?.at(-1)?.id).toBe('239');
    expect(runs[1].messages?.filter((message) => message.role === 'user').length).toBe(3);
  });

  it('indexes copy-turn text in one transcript pass', () => {
    const messages = Array.from({ length: 1_000 }, (_, index): Message => ({
      id: String(index),
      role: index % 5 === 0 ? 'user' : 'assistant',
      content: String(index),
      created: index,
    }));
    const read = vi.fn((message: Message) => message.content);
    const contexts = indexTranscriptTurns(messages, read);
    expect(read).toHaveBeenCalledTimes(messages.length);
    expect(contexts.size).toBe(messages.length);
    expect(contexts.get(messages[1])?.copyTarget).toBe(false);
    expect(contexts.get(messages[4])).toMatchObject({
      turnText: ['0', '1', '2', '3', '4'].join('\n\n'),
      copyTarget: true,
    });
  });

  it('hands projected rows off atomically to matching durable identities', () => {
    const projected: Message[] = [
      { id: 'p1', role: 'user', content: 'question', created: 1, clientMessageId: 'c1' },
      {
        id: 'p2',
        role: 'assistant',
        content: 'answer',
        created: 2,
        responseId: 'r1',
        assistantSegmentOrdinal: 0,
      },
    ];
    const durable: Message[] = projected.map((message, index) => ({
      ...message,
      id: `d${index}`,
      durableRowId: index + 1,
    }));
    expect(mergeDurableProjection(durable, projected)).toEqual(durable);
  });
});
