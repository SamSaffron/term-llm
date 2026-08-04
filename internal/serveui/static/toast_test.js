#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const source = fs.readFileSync(path.join(__dirname, 'toast.js'), 'utf8');

const makeNode = () => {
  const node = {
    children: [], attributes: {}, className: '', textContent: '', parentNode: null,
    classList: { add(value) { node.className += ` ${value}`; } },
    setAttribute(key, value) { node.attributes[key] = String(value); },
    addEventListener(type, listener) { node[`on${type}`] = listener; },
    append(...children) { children.forEach((child) => { child.parentNode = node; node.children.push(child); }); },
    appendChild(child) { child.parentNode = node; node.children.push(child); return child; },
    remove() {
      if (!node.parentNode) return;
      const index = node.parentNode.children.indexOf(node);
      if (index >= 0) node.parentNode.children.splice(index, 1);
    },
  };
  return node;
};

const region = makeNode();
const document = {
  getElementById(id) { return id === 'toastRegion' ? region : null; },
  createElement() { return makeNode(); },
};
const window = {
  TermLLMApp: {},
  setTimeout() { return 1; },
  clearTimeout() {},
};
vm.runInNewContext(source, { window, document, console }, { filename: 'toast.js' });

const toast = window.TermLLMApp.showToast('Transcript changed', { tone: 'error', duration: 0 });
if (!toast || region.children.length !== 1) throw new Error('toast was not mounted');
if (toast.attributes.role !== 'alert' || toast.attributes['aria-atomic'] !== 'true') throw new Error('error toast is not an atomic alert');
if (toast.children[0]?.textContent !== 'Transcript changed') throw new Error('toast message missing');
if (toast.children[1]?.attributes?.['aria-label'] !== 'Dismiss notification') throw new Error('dismiss button lacks accessible label');
toast.children[1].onclick();
if (!toast._dismissed) throw new Error('dismiss button did not dismiss toast');

const firstMutation = window.TermLLMApp.showToast('Undo complete', { id: 'transcript-mutation', duration: 0 });
const secondMutation = window.TermLLMApp.showToast('Redo complete', { id: 'transcript-mutation', duration: 0 });
const mutationToasts = region.children.filter((item) => item.attributes?.['data-toast-id'] === 'transcript-mutation');
if (mutationToasts.length !== 1 || mutationToasts[0] !== secondMutation || region.children.includes(firstMutation)) {
  throw new Error('toast IDs do not replace earlier notifications');
}

for (let i = 0; i < 6; i += 1) window.TermLLMApp.showToast(`message ${i}`, { duration: 0 });
if (region.children.length !== 4) throw new Error(`toast stack is not bounded: ${region.children.length}`);

console.log('PASS: accessible reusable toast notifications');
