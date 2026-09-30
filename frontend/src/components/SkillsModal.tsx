import { useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { skillExecutionDescription, skillExecutionLabel } from '../domain/completions';
import { Overlay } from './Overlay';

export function Skills() {
  const store = useStore();
  const [selected, setSelected] = useState('');
  const [args, setArgs] = useState('');
  const selectedSkill = store.skills.value.find(
    (skill) => String(skill.name || skill.id || '') === selected,
  );
  const selectedBlocked = Boolean(
    selectedSkill && selectedSkill.execution !== 'isolated' && store.streaming.value,
  );
  return (
    <Overlay title="Skills">
      <div class="skills-list">
        {store.skills.value.map((skill) => {
          const name = String(skill.name || skill.id || '');
          return (
            <button
              class={`skill-row ${selected === name ? 'selected' : ''}`}
              key={name}
              onClick={() => setSelected(name)}
            >
              <strong>{name}</strong>
              <small>{String(skill.description || '')}</small>
              <small class="skill-provenance">Source: {String(skill.source || 'unknown')}</small>
              <small class="skill-execution">
                <strong>{skillExecutionLabel(skill)}</strong> — {skillExecutionDescription(skill)}
              </small>
            </button>
          );
        })}
      </div>
      {selected && (
        <form
          onSubmit={(event) => {
            event.preventDefault();
            if (!selectedBlocked) void store.invokeSkill(selected, args);
          }}
        >
          <label class="settings-label">Arguments for {selected}</label>
          <textarea value={args} onInput={(event) => setArgs(event.currentTarget.value)} />
          {selectedBlocked && (
            <p class="skill-run-blocked" role="status">
              This main-conversation skill cannot run until the active response finishes. Isolated
              skills can run now.
            </p>
          )}
          <button class="btn primary" type="submit" disabled={selectedBlocked}>
            Run skill
          </button>
        </form>
      )}
    </Overlay>
  );
}
