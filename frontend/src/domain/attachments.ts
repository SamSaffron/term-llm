import type { IconName } from '../components/Icon';

export interface AttachmentPolicy {
  maxCount: number;
  maxBytes: number;
  mimeTypes: string[];
  extensions: string[];
}

export const DEFAULT_ATTACHMENT_POLICY: AttachmentPolicy = Object.freeze({
  maxCount: 10,
  maxBytes: 20 * 1024 * 1024,
  mimeTypes: ['*/*'],
  extensions: [],
});

export interface AttachmentValidationError {
  code: 'too_many' | 'empty' | 'too_large' | 'unsupported';
  message: string;
}

export function attachmentExtension(name: string): string {
  const index = name.lastIndexOf('.');
  return index >= 0 ? name.slice(index).toLowerCase() : '';
}

export function validateAttachmentFile(
  file: Pick<File, 'name' | 'type' | 'size'>,
  existingCount: number,
  policy: AttachmentPolicy,
): AttachmentValidationError | null {
  if (existingCount >= policy.maxCount)
    return {
      code: 'too_many',
      message: `${file.name}: the attachment limit is ${policy.maxCount} files.`,
    };
  if (file.size <= 0)
    return { code: 'empty', message: `${file.name}: empty files are not supported.` };
  if (file.size > policy.maxBytes)
    return {
      code: 'too_large',
      message: `${file.name}: ${(file.size / 1024 / 1024).toFixed(1)} MB exceeds the ${(policy.maxBytes / 1024 / 1024).toFixed(0)} MB limit.`,
    };
  const mime = file.type.toLowerCase();
  const extension = attachmentExtension(file.name);
  if (
    !policy.mimeTypes.includes('*/*') &&
    !policy.mimeTypes.includes(mime) &&
    !policy.extensions.includes(extension)
  )
    return {
      code: 'unsupported',
      message: `${file.name}: this file type is not supported.`,
    };
  return null;
}

export function attachmentAccept(policy: AttachmentPolicy): string {
  if (policy.mimeTypes.includes('*/*')) return '';
  return [...new Set([...policy.mimeTypes, ...policy.extensions])].join(',');
}

// Only these formats are prepared and sent as native model images.
export function isNativeImageType(type: string): boolean {
  return ['image/jpeg', 'image/png', 'image/gif', 'image/webp'].includes(type.toLowerCase());
}

// MIME-first tables. Anything not listed here falls through to the extension
// table and finally to the generic page glyph, so unknown types stay visible.
const CODE_MIMES = new Set([
  'application/json',
  'application/ld+json',
  'application/xml',
  'application/javascript',
  'application/ecmascript',
  'application/x-javascript',
  'application/typescript',
  'application/x-sh',
  'application/x-shellscript',
  'application/x-yaml',
  'application/yaml',
  'application/toml',
  'application/sql',
  'application/x-sql',
  'application/x-python',
  'application/x-ruby',
  'application/x-httpd-php',
  'application/x-perl',
  'application/x-lua',
  // Source files usually arrive as a text/* subtype, and the generic text/
  // prefix below is checked before the extension table, so these have to be
  // named explicitly or a .rb/.py upload reads as a plain document.
  'text/javascript',
  'text/ecmascript',
  'text/typescript',
  'text/css',
  'text/html',
  'text/xml',
  'text/yaml',
  'text/x-yaml',
  'text/x-toml',
  'text/x-sql',
  'text/x-sh',
  'text/x-shellscript',
  'text/x-ruby',
  'text/x-python',
  'text/x-go',
  'text/x-rust',
  'text/x-java',
  'text/x-java-source',
  'text/x-c',
  'text/x-c++',
  'text/x-csrc',
  'text/x-chdr',
  'text/x-c++src',
  'text/x-csharp',
  'text/x-kotlin',
  'text/x-swift',
  'text/x-php',
  'text/x-perl',
  'text/x-lua',
]);
const TABLE_MIMES = new Set([
  'text/csv',
  'text/tab-separated-values',
  'application/csv',
  'application/vnd.ms-excel',
  'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet',
  'application/vnd.oasis.opendocument.spreadsheet',
  'application/vnd.apple.numbers',
]);
const ARCHIVE_MIMES = new Set([
  'application/zip',
  'application/x-zip-compressed',
  'application/x-tar',
  'application/gzip',
  'application/x-gzip',
  'application/x-7z-compressed',
  'application/x-rar-compressed',
  'application/vnd.rar',
  'application/x-bzip2',
  'application/x-compressed-tar',
]);
const TEXT_MIMES = new Set([
  'application/pdf',
  'application/rtf',
  'application/msword',
  'application/vnd.openxmlformats-officedocument.wordprocessingml.document',
  'application/vnd.oasis.opendocument.text',
]);

const CODE_EXTENSIONS = new Set([
  '.rb',
  '.go',
  '.ts',
  '.tsx',
  '.js',
  '.jsx',
  '.py',
  '.rs',
  '.java',
  '.c',
  '.h',
  '.cpp',
  '.sh',
  '.sql',
  '.json',
  '.yml',
  '.yaml',
  '.toml',
  '.css',
  '.html',
  '.xml',
  '.php',
  '.cs',
  '.kt',
  '.swift',
]);
const TABLE_EXTENSIONS = new Set(['.csv', '.tsv', '.xls', '.xlsx', '.ods']);
const ARCHIVE_EXTENSIONS = new Set(['.zip', '.tar', '.gz', '.tgz', '.7z', '.rar', '.bz2', '.xz']);
const TEXT_EXTENSIONS = new Set(['.txt', '.md', '.markdown', '.rtf', '.pdf', '.doc', '.docx']);

/**
 * MIME-appropriate glyph for an attachment chip. Matching is MIME-first
 * (exact, then prefix), then the extension, then the generic page glyph.
 */
export function attachmentIconName(mime: string, name: string): IconName {
  // Browsers and API clients both send parameters (`text/csv; charset=utf-8`),
  // so the essence has to be isolated before any table lookup.
  const type = (mime || '').toLowerCase().split(';')[0].trim();
  if (TABLE_MIMES.has(type)) return 'file-table';
  if (ARCHIVE_MIMES.has(type)) return 'file-archive';
  if (CODE_MIMES.has(type)) return 'file-code';
  if (TEXT_MIMES.has(type)) return 'file-text';
  if (type.startsWith('text/')) return 'file-text';
  const extension = attachmentExtension(name);
  if (TABLE_EXTENSIONS.has(extension)) return 'file-table';
  if (ARCHIVE_EXTENSIONS.has(extension)) return 'file-archive';
  if (CODE_EXTENSIONS.has(extension)) return 'file-code';
  if (TEXT_EXTENSIONS.has(extension)) return 'file-text';
  return 'file';
}

/** Compact byte label for a chip (`1.2 KB`, `3.4 MB`); empty when unknown. */
export function formatAttachmentSize(bytes?: number): string {
  const value = Number(bytes);
  if (!Number.isFinite(value) || value <= 0) return '';
  if (value < 1024) return `${Math.round(value)} B`;
  const units = ['KB', 'MB', 'GB'];
  let scaled = value / 1024;
  let unit = 0;
  // The 1023.95 bound promotes before rounding, so 1,048,575 B reads 1 MB
  // rather than the nonsensical 1024 KB that toFixed(1) would produce.
  while (scaled >= 1023.95 && unit < units.length - 1) {
    scaled /= 1024;
    unit += 1;
  }
  const rounded = scaled.toFixed(1).replace(/\.0$/, '');
  return `${rounded} ${units[unit]}`;
}
