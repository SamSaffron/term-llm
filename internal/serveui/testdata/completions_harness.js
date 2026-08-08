'use strict';

const fs = require('fs');
const path = require('path');
const vm = require('vm');
const loadSource = (name) => fs.readFileSync(path.join(__dirname, '..', 'static', name), 'utf8');

class ClassList {
  constructor(element) { this.element = element; }
  values() { return new Set(String(this.element.className || '').split(/\s+/).filter(Boolean)); }
  toggle(token, force) {
    const values = this.values();
    if (force) values.add(token); else values.delete(token);
    this.element.className = [...values].join(' ');
  }
}

class Element {
  constructor({ hidden = false, selection = true } = {}) {
    this.children = [];
    this.listeners = {};
    this.attributes = {};
    this.className = '';
    this.classList = new ClassList(this);
    this.hidden = hidden;
    this.value = '';
    if (selection) {
      this.selectionStart = 0;
      this.selectionEnd = 0;
    }
    this.textContent = '';
  }
  addEventListener(type, listener) { (this.listeners[type] ||= []).push(listener); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = [...children]; }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  focus() { this.focused = true; }
  dispatch(type, init = {}) {
    const event = {
      type,
      key: '',
      isComposing: false,
      defaultPrevented: false,
      immediatePropagationStopped: false,
      preventDefault() { this.defaultPrevented = true; },
      stopImmediatePropagation() { this.immediatePropagationStopped = true; },
      ...init,
    };
    for (const listener of this.listeners[type] || []) {
      listener(event);
      if (event.immediatePropagationStopped) break;
    }
    return event;
  }
}

const assert = (condition, message) => { if (!condition) throw new Error(message); };
const response = (payload) => ({ ok: true, status: 200, async json() { return payload; } });

module.exports = { Element, assert, response, loadSource, vm };
