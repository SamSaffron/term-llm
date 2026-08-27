import { describe, expect, it } from 'vitest';
import { DEFAULT_ATTACHMENT_POLICY, attachmentAccept, validateAttachmentFile } from './attachments';

const file = (name: string, type: string, size: number) => ({ name, type, size }) as File;

describe('attachment selection policy', () => {
  it('rejects empty, oversized, unsupported, and excess files at selection', () => {
    expect(
      validateAttachmentFile(file('empty.txt', 'text/plain', 0), 0, DEFAULT_ATTACHMENT_POLICY)
        ?.code,
    ).toBe('empty');
    expect(
      validateAttachmentFile(
        file('huge.pdf', 'application/pdf', DEFAULT_ATTACHMENT_POLICY.maxBytes + 1),
        0,
        DEFAULT_ATTACHMENT_POLICY,
      )?.code,
    ).toBe('too_large');
    expect(validateAttachmentFile(file('bad.exe', '', 1), 0, DEFAULT_ATTACHMENT_POLICY)?.code).toBe(
      'unsupported',
    );
    expect(
      validateAttachmentFile(
        file('ok.txt', 'text/plain', 1),
        DEFAULT_ATTACHMENT_POLICY.maxCount,
        DEFAULT_ATTACHMENT_POLICY,
      )?.code,
    ).toBe('too_many');
  });

  it('accepts supported extensions when browsers omit MIME', () => {
    expect(
      validateAttachmentFile(file('source.go', '', 10), 0, DEFAULT_ATTACHMENT_POLICY),
    ).toBeNull();
    expect(attachmentAccept(DEFAULT_ATTACHMENT_POLICY)).toContain('.go');
  });
});
