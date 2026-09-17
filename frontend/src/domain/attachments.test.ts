import { describe, expect, it } from 'vitest';
import {
  DEFAULT_ATTACHMENT_POLICY,
  attachmentAccept,
  attachmentIconName,
  formatAttachmentSize,
  validateAttachmentFile,
} from './attachments';

const file = (name: string, type: string, size: number) => ({ name, type, size }) as File;

describe('attachment selection policy', () => {
  it('rejects empty, oversized, and excess files at selection', () => {
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
    expect(attachmentAccept(DEFAULT_ATTACHMENT_POLICY)).toBe('');
  });
});

it.each([
  ['archive.zip', 'application/zip'],
  ['unknown.weird', 'application/x-unknown'],
  ['binary', ''],
  ['icon.svg', 'image/svg+xml'],
])('accepts arbitrary upload %s', (name, type) => {
  expect(validateAttachmentFile(file(name, type, 123), 0, DEFAULT_ATTACHMENT_POLICY)).toBeNull();
});

describe('attachment chip icon', () => {
  it.each([
    ['text/plain', 'notes.txt', 'file-text'],
    ['text/markdown', 'README.md', 'file-text'],
    ['application/pdf', 'report.pdf', 'file-text'],
    [
      'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
      'brief.docx',
      'file-text',
    ],
    ['application/zip', 'archive.zip', 'file-archive'],
    ['application/x-tar', 'src.tar', 'file-archive'],
    ['application/octet-stream', 'bundle.tar.gz', 'file-archive'],
    ['application/json', 'config.json', 'file-code'],
    ['application/x-sh', 'run.sh', 'file-code'],
    // A text/* source subtype must beat the generic text/ prefix rule.
    ['text/x-ruby', 'send_system_message_spec.rb', 'file-code'],
    ['text/x-python', 'train.py', 'file-code'],
    ['text/javascript', 'sw.js', 'file-code'],
    ['', 'server.go', 'file-code'],
    ['application/octet-stream', 'query.sql', 'file-code'],
    ['', 'Component.tsx', 'file-code'],
    ['text/csv', 'rows.csv', 'file-table'],
    ['', 'sheet.xlsx', 'file-table'],
    ['application/octet-stream', 'mystery.bin', 'file'],
    ['', 'Makefile', 'file'],
    // Images render as thumbnails rather than chips; the fallback stays honest.
    ['image/png', 'shot.png', 'file'],
  ])('maps %s / %s to %s', (mime, name, expected) => {
    expect(attachmentIconName(mime, name)).toBe(expected);
  });

  it('ignores MIME parameters that browsers and API clients attach', () => {
    expect(attachmentIconName('text/csv; charset=utf-8', 'rows.bin')).toBe('file-table');
    expect(attachmentIconName('application/json;charset=UTF-8', 'payload.bin')).toBe('file-code');
  });

  it('trusts the MIME over a misleading extension', () => {
    expect(attachmentIconName('text/csv', 'data.txt')).toBe('file-table');
    expect(attachmentIconName('application/zip', 'fake.go')).toBe('file-archive');
    expect(attachmentIconName('TEXT/Plain', 'notes.csv')).toBe('file-text');
  });
});

describe('attachment chip size label', () => {
  it('formats bytes, kilobytes, megabytes and gigabytes', () => {
    expect(formatAttachmentSize(640)).toBe('640 B');
    expect(formatAttachmentSize(1024)).toBe('1 KB');
    expect(formatAttachmentSize(1234)).toBe('1.2 KB');
    expect(formatAttachmentSize(3.5 * 1024 * 1024)).toBe('3.5 MB');
    expect(formatAttachmentSize(2 * 1024 ** 3)).toBe('2 GB');
  });

  it('promotes the unit before rounding can invent 1024 of the smaller one', () => {
    expect(formatAttachmentSize(1024 * 1024 - 1)).toBe('1 MB');
    expect(formatAttachmentSize(1024 ** 3 - 1)).toBe('1 GB');
  });

  it('stays empty when the size is unknown or empty', () => {
    expect(formatAttachmentSize(undefined)).toBe('');
    expect(formatAttachmentSize(0)).toBe('');
    expect(formatAttachmentSize(-5)).toBe('');
    expect(formatAttachmentSize(Number.NaN)).toBe('');
  });
});
