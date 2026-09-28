import '../../styles/features/mcp.css';
import { useEffect, useRef, useState } from 'preact/hooks';
import { useStore } from '../../app/context';
import type { MCPAddResult } from '../../domain/types';
import { Icon } from '../Icon';
import { Overlay } from '../Overlay';
import { MCPAddServer } from './MCPAddServer';
import { MCPServerList } from './MCPServerList';

/**
 * MCP servers dialog. One overlay hosts two views so switching between the
 * list and the pushed "Add server" sheet never re-opens the dialog.
 */
export function MCP() {
  const store = useStore();
  const [view, setView] = useState<'list' | 'add'>('list');
  const [notice, setNotice] = useState('');
  const addButton = useRef<HTMLButtonElement>(null);
  const returning = useRef(false);

  useEffect(() => () => store.dismissRemovedMCPServer(), [store]);
  useEffect(() => {
    if (view !== 'list' || !returning.current) return;
    returning.current = false;
    addButton.current?.focus();
  }, [view]);

  const openAdd = () => {
    setNotice('');
    setView('add');
  };
  const back = () => {
    returning.current = true;
    setView('list');
  };
  const added = (result: MCPAddResult) => {
    setNotice(
      result.needs_input
        ? `Added ${result.name}. Fill in its placeholder values in mcp.json, then turn it on.`
        : '',
    );
    back();
  };

  if (view === 'add')
    return (
      <Overlay
        title="Add server"
        className="mcp-modal"
        onEscape={back}
        titleLeading={
          <button
            class="icon-btn mcp-back"
            type="button"
            aria-label="Back to MCP servers"
            onClick={back}
          >
            <Icon name="arrow-left" />
          </button>
        }
      >
        <MCPAddServer onAdded={added} onCancel={back} />
      </Overlay>
    );

  return (
    <Overlay
      title="MCP servers"
      className="mcp-modal"
      titleActions={
        <button ref={addButton} class="mcp-button" type="button" onClick={openAdd}>
          <Icon name="add" />
          Add
        </button>
      }
    >
      <MCPServerList notice={notice} onAdd={openAdd} />
    </Overlay>
  );
}
