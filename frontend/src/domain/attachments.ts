export interface AttachmentPolicy {
  maxCount: number;
  maxBytes: number;
  mimeTypes: string[];
  extensions: string[];
}

export const DEFAULT_ATTACHMENT_POLICY: AttachmentPolicy = Object.freeze({
  maxCount: 10,
  maxBytes: 20 * 1024 * 1024,
  mimeTypes: [
    'image/jpeg',
    'image/png',
    'image/gif',
    'image/webp',
    'application/pdf',
    'text/plain',
    'text/markdown',
    'application/json',
    'text/csv',
    'audio/mpeg',
    'audio/wav',
    'audio/ogg',
    'video/mp4',
    'video/webm',
  ],
  extensions: [
    '.jpg',
    '.jpeg',
    '.png',
    '.gif',
    '.webp',
    '.pdf',
    '.txt',
    '.md',
    '.markdown',
    '.json',
    '.csv',
    '.yaml',
    '.yml',
    '.xml',
    '.go',
    '.js',
    '.jsx',
    '.ts',
    '.tsx',
    '.py',
    '.rb',
    '.rs',
    '.java',
    '.c',
    '.h',
    '.cpp',
    '.hpp',
    '.mp3',
    '.wav',
    '.ogg',
    '.mp4',
    '.webm',
  ],
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
  if (!policy.mimeTypes.includes(mime) && !policy.extensions.includes(extension))
    return {
      code: 'unsupported',
      message: `${file.name}: this file type is not supported.`,
    };
  return null;
}

export function attachmentAccept(policy: AttachmentPolicy): string {
  return [...new Set([...policy.mimeTypes, ...policy.extensions])].join(',');
}
