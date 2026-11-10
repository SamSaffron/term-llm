import {
  AGENT_AUTO_VALUE,
  AGENT_CHAT_VALUE,
  agentChoiceFromValue,
  agentChoiceValue,
} from '../domain/agent-choice';
import { useStore } from '../app/context';
import { ChipPicker } from './ChipPicker';
import { Icon } from './Icon';
import { agentPickerCommon, buildAgentMenu } from '../domain/agent-selection';

export function NewChatControls() {
  const store = useStore();
  if (!store.draftActive.value) return null;
  const projects = store.projects.value
    .filter((project) => !project.archived && project.available !== false)
    .sort((left, right) => left.name.localeCompare(right.name));
  const projectActive = Boolean(store.activeProjectId.value);
  const pinned = !store.config.agentNames.length && store.config.agentName;
  const effective = store.effectiveAgent.value;
  // A resolved project default always matches its agent option. When a server
  // or shell cannot resolve one, keep an honest automatic entry rather than
  // showing an unrelated agent as selected.
  const agentValue = effective
    ? agentChoiceValue(effective)
    : projectActive
      ? AGENT_AUTO_VALUE
      : AGENT_CHAT_VALUE;
  const menu = pinned
    ? null
    : buildAgentMenu({
        agentNames: store.config.agentNames,
        common: agentPickerCommon(store.config.agentPickerCommon),
        projectActive,
      });
  if (menu && !menu.options.some((option) => option.value === agentValue))
    menu.options.unshift({ value: agentValue, label: 'Automatic' });
  return (
    <div class="new-chat-pickers">
      {menu && (
        <div class="new-chat-project-picker new-chat-agent-picker">
          <ChipPicker
            ariaLabel="Choose agent for new chat"
            value={agentValue}
            options={menu.options}
            alwaysFilter
            notifyEqual
            matchValue={false}
            triggerClass="new-chat-project-trigger new-chat-agent-trigger"
            onChange={(value) => store.setAgentChoice(agentChoiceFromValue(value))}
            renderTrigger={(selected) => (
              <>
                <span class="new-chat-project-label">Agent</span>
                <span class="new-chat-project-value">{selected.label}</span>
                <span class="new-chat-project-chevron">
                  <Icon class="glyph-icon" name="chevron-down" />
                </span>
              </>
            )}
          />
        </div>
      )}
      {!menu && pinned && (
        <div class="new-chat-project-picker new-chat-agent-picker">
          <span class="new-chat-project-trigger new-chat-agent-trigger new-chat-agent-pinned">
            <span class="new-chat-project-label">Agent</span>
            <span class="new-chat-project-value">{pinned}</span>
          </span>
        </div>
      )}
      {store.projectsEnabled.value && (
        <div class="new-chat-project-picker">
          <ChipPicker
            ariaLabel="Choose chat or project"
            value={store.activeProjectId.value}
            options={[
              { value: '', label: 'No project' },
              ...projects.map((project) => ({ value: project.id, label: project.name })),
            ]}
            triggerClass="new-chat-project-trigger"
            actions={[
              {
                label: 'Add project…',
                icon: <Icon name="add" />,
                onSelect: () => store.openAddProject(),
              },
            ]}
            onChange={(projectId) => store.newChat(true, projectId)}
            renderTrigger={(selected) => (
              <>
                {selected.value && <span class="new-chat-project-label">Project</span>}
                <span class="new-chat-project-value">{selected.label}</span>
                <span class="new-chat-project-chevron">
                  <Icon class="glyph-icon" name="chevron-down" />
                </span>
              </>
            )}
          />
        </div>
      )}
    </div>
  );
}
