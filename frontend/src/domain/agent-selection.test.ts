import { AGENT_CHAT_VALUE, agentChoiceFromValue, agentChoiceValue } from './agent-choice';
import { describe, expect, it } from 'vitest';
import {
  agentPickerCommon,
  buildAgentMenu,
  DEFAULT_AGENT_PICKER_COMMON,
  uniqueAgentNames,
} from './agent-selection';

const stock = ['developer', 'sysadmin', 'extension-builder', 'widget-builder'];

describe('agent selection menu', () => {
  it('puts Chat first, then the configured block, one separator, and alphabetical rest', () => {
    const menu = buildAgentMenu({
      agentNames: [
        'widget-builder',
        'reviewer',
        'developer',
        'artist',
        'sysadmin',
        'extension-builder',
      ],
      common: stock,
      projectActive: false,
    });
    expect(menu.options.map((option) => option.label)).toEqual([
      'Chat',
      'developer',
      'sysadmin',
      'extension-builder',
      'widget-builder',
      'artist',
      'reviewer',
    ]);
    expect(menu.options.find((option) => option.label === 'artist')?.group).toBe(1);
  });

  it('does not duplicate a configured agent and preserves the first occurrence', () => {
    const menu = buildAgentMenu({
      agentNames: ['reviewer', 'developer', 'reviewer', 'sysadmin', 'reviewer'],
      common: ['developer', 'sysadmin'],
      projectActive: false,
    });
    expect(menu.options.map((option) => option.label)).toEqual([
      'Chat',
      'developer',
      'sysadmin',
      'reviewer',
    ]);
    expect(menu.options.find((option) => option.label === 'reviewer')?.group).toBe(1);
  });

  it('skips unavailable common entries and hides the separator when a block is empty', () => {
    const onlyCommon = buildAgentMenu({
      agentNames: ['developer', 'sysadmin'],
      common: stock,
      projectActive: false,
    });
    expect(onlyCommon.options.map((option) => option.label)).toEqual([
      'Chat',
      'developer',
      'sysadmin',
    ]);
    expect(onlyCommon.options.every((option) => option.group !== 1)).toBe(true);

    const onlyRemaining = buildAgentMenu({
      agentNames: ['reviewer', 'artist'],
      common: stock,
      projectActive: false,
    });
    expect(onlyRemaining.options.map((option) => option.label)).toEqual([
      'Chat',
      'artist',
      'reviewer',
    ]);
    expect(onlyRemaining.options.every((option) => option.group !== 1)).toBe(true);
  });

  it('treats an empty common list as one alphabetical block with Chat first', () => {
    const menu = buildAgentMenu({
      agentNames: ['reviewer', 'developer', 'artist'],
      common: [],
      projectActive: false,
    });
    expect(menu.options.map((option) => option.label)).toEqual([
      'Chat',
      'artist',
      'developer',
      'reviewer',
    ]);
    expect(menu.options.every((option) => option.group !== 1)).toBe(true);
  });

  it('treats custom agents like built-ins and keeps a shadowed builtin slot', () => {
    const menu = buildAgentMenu({
      agentNames: ['my-agent', 'developer', 'widget-builder'],
      common: ['my-agent', 'developer'],
      projectActive: false,
    });
    expect(menu.options.map((option) => option.label)).toEqual([
      'Chat',
      'my-agent',
      'developer',
      'widget-builder',
    ]);
    expect(menu.options.find((option) => option.label === 'widget-builder')?.group).toBe(1);
  });

  it('offers Chat only without a project', () => {
    const menu = buildAgentMenu({
      agentNames: ['developer', 'reviewer'],
      common: stock,
      projectActive: true,
    });
    expect(menu.options.map((option) => option.label)).toEqual(['developer', 'reviewer']);
    expect(menu.options.some((option) => option.value === AGENT_CHAT_VALUE)).toBe(false);
  });

  it('falls back to the stock block when a shell omits the injected list', () => {
    expect(agentPickerCommon(undefined)).toEqual(DEFAULT_AGENT_PICKER_COMMON);
    expect(agentPickerCommon([])).toEqual([]);
    expect(agentPickerCommon(['custom'])).toEqual(['custom']);
  });

  it('distinguishes Chat from a custom agent literally named chat', () => {
    expect(agentChoiceFromValue(AGENT_CHAT_VALUE)).toEqual({ kind: 'chat' });
    expect(agentChoiceFromValue(agentChoiceValue('chat'))).toEqual({ kind: 'agent', name: 'chat' });
    expect(agentChoiceFromValue('auto')).toEqual({ kind: 'auto' });
    expect(uniqueAgentNames(['chat', ' chat ', ''])).toEqual(['chat']);
  });
});
