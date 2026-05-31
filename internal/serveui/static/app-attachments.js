(() => {
'use strict';

// File attachment helpers. This is a dependency leaf: it reads state/elements
// and creates DOM, but never calls back into the streaming or interjection
// logic except updateSendButtonState (resolved lazily off the app bag so this
// file can load before app-stream.js without a load-order cycle).
const app = window.TermLLMApp;
const { state, elements, generateId } = app;

const refreshSendButtonState = () => {
  if (typeof app.updateSendButtonState === 'function') app.updateSendButtonState();
};

const getAttachmentPreviewURL = (att) => String(att?.previewURL || att?.dataURL || '');

const createAttachmentPreviewURL = (file) => {
  const urlAPI = window.URL || globalThis.URL;
  if (!urlAPI || typeof urlAPI.createObjectURL !== 'function') return '';
  try {
    return urlAPI.createObjectURL(file);
  } catch {
    return '';
  }
};

const revokeObjectURLString = (previewURL) => {
  const url = String(previewURL || '');
  if (!url) return;
  const urlAPI = window.URL || globalThis.URL;
  if (!urlAPI || typeof urlAPI.revokeObjectURL !== 'function') return;
  try {
    urlAPI.revokeObjectURL(url);
  } catch {
    // Ignore preview cleanup failures.
  }
};

const revokeAttachmentPreviewURL = (att) => {
  const previewURL = String(att?.previewURL || '');
  if (!previewURL || previewURL === att?.dataURL) return;
  revokeObjectURLString(previewURL);
};

const discardPendingAttachments = () => {
  state.attachments.forEach(revokeAttachmentPreviewURL);
  state.attachments = [];
  renderAttachments();
};

const readFileAsDataURL = (file, signal) => new Promise((resolve, reject) => {
  if (!file) {
    resolve('');
    return;
  }
  if (signal?.aborted) {
    reject(new DOMException('The operation was aborted.', 'AbortError'));
    return;
  }
  if (typeof FileReader !== 'function') {
    reject(new Error('File uploads are not supported in this browser.'));
    return;
  }

  const reader = new FileReader();
  if (typeof reader.readAsDataURL !== 'function') {
    reject(new Error('File uploads are not supported in this browser.'));
    return;
  }
  let settled = false;

  const cleanupAbort = () => {
    if (signal) signal.removeEventListener('abort', handleAbort);
  };

  const fail = (err) => {
    if (settled) return;
    settled = true;
    cleanupAbort();
    reject(err);
  };

  const succeed = (value) => {
    if (settled) return;
    settled = true;
    cleanupAbort();
    resolve(value);
  };

  const handleAbort = () => {
    try {
      reader.abort();
    } catch {
      fail(new DOMException('The operation was aborted.', 'AbortError'));
    }
  };

  if (signal) signal.addEventListener('abort', handleAbort, { once: true });

  reader.onload = () => succeed(typeof reader.result === 'string' ? reader.result : '');
  reader.onerror = () => fail(reader.error || new Error(`Failed to read ${file.name || 'attachment'}.`));
  reader.onabort = () => fail(new DOMException('The operation was aborted.', 'AbortError'));
  reader.readAsDataURL(file);
});

const materializeAttachmentDataURL = async (att, signal, options = {}) => {
  const name = String(att?.name || 'attachment');
  const type = String(att?.type || '');
  if (att?.dataURL) return { name, type, dataURL: att.dataURL };
  if (!att?.file) {
    if (options.skipUnavailable) return null;
    throw new Error(`Failed to read ${name}.`);
  }
  const dataURL = await readFileAsDataURL(att.file, signal);
  if (!dataURL) {
    throw new Error(`Failed to read ${name}.`);
  }
  return { name, type, dataURL };
};

const buildAttachmentInputParts = async (attachments, signal, options = {}) => {
  const materialized = await Promise.all((attachments || []).map(att => materializeAttachmentDataURL(att, signal, options)));
  return materialized.filter(Boolean).map(att => (
    att.type.startsWith('image/')
      ? { type: 'input_image', image_url: att.dataURL, filename: att.name }
      : { type: 'input_file', file_data: att.dataURL, filename: att.name }
  ));
};

const cloneAttachmentForMessage = (att) => {
  const cloned = {
    id: String(att?.id || generateId('att')),
    name: String(att?.name || 'file'),
    type: String(att?.type || 'application/octet-stream')
  };
  if (Number.isFinite(Number(att?.size))) {
    cloned.size = Number(att.size);
  }
  const previewURL = String(att?.previewURL || '');
  if (previewURL) {
    cloned.previewURL = previewURL;
  }
  if (att?.file) {
    cloned.file = att.file;
  }
  if (att?.dataURL) {
    cloned.dataURL = att.dataURL;
  }
  return cloned;
};

const renderAttachments = () => {
  const strip = elements.attachmentsStrip;
  if (!strip) return;
  strip.innerHTML = '';
  if (state.attachments.length === 0) {
    strip.style.display = 'none';
    refreshSendButtonState();
    return;
  }
  strip.style.display = 'flex';
  state.attachments.forEach((att) => {
    const chip = document.createElement('div');
    chip.className = 'attachment-chip';

    if (att.type.startsWith('image/')) {
      const previewURL = getAttachmentPreviewURL(att);
      if (previewURL) {
        const img = document.createElement('img');
        img.src = previewURL;
        img.alt = att.name;
        chip.appendChild(img);
      }
    }

    const name = document.createElement('span');
    name.className = 'att-name';
    name.textContent = att.name;
    name.title = `${att.name} (${(att.size / 1024).toFixed(1)} KB)`;
    chip.appendChild(name);

    const remove = document.createElement('button');
    remove.className = 'att-remove';
    remove.textContent = '×';
    remove.title = 'Remove';
    remove.addEventListener('click', () => {
      revokeAttachmentPreviewURL(att);
      state.attachments = state.attachments.filter(a => a.id !== att.id);
      renderAttachments();
    });
    chip.appendChild(remove);

    strip.appendChild(chip);
  });
  refreshSendButtonState();
};

const MAX_ATTACHMENTS = 10;
const MAX_FILE_BYTES = 20 * 1024 * 1024; // 20 MB

const handleFiles = (fileList) => {
  const files = Array.from(fileList);
  for (const file of files) {
    if (state.attachments.length >= MAX_ATTACHMENTS) {
      alert(`Maximum ${MAX_ATTACHMENTS} attachments allowed.`);
      return;
    }
    if (file.size > MAX_FILE_BYTES) {
      alert(`${file.name} exceeds the 20 MB file size limit.`);
      continue;
    }
    state.attachments.push({
      id: generateId('att'),
      name: file.name,
      type: file.type || 'application/octet-stream',
      size: file.size,
      file,
      previewURL: (file.type || '').startsWith('image/') ? createAttachmentPreviewURL(file) : ''
    });
    renderAttachments();
  }
};

Object.assign(app, {
  getAttachmentPreviewURL,
  createAttachmentPreviewURL,
  revokeAttachmentPreviewURL,
  discardPendingAttachments,
  materializeAttachmentDataURL,
  buildAttachmentInputParts,
  cloneAttachmentForMessage,
  renderAttachments,
  MAX_ATTACHMENTS,
  MAX_FILE_BYTES,
  handleFiles
});
})();
