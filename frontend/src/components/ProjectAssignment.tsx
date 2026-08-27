import { useEffect, useState } from 'preact/hooks';
import { useStore } from '../app/context';
import { errorMessage } from '../domain/text';
import { Overlay } from './Overlay';

function assignmentCountCopy(count: number): string {
  return `${count} conversation${count === 1 ? '' : 's'} from this workspace will be grouped here.`;
}

export function ProjectAssignment() {
  const store = useStore();
  const target = store.projectTarget.value;
  const [candidate, setCandidate] = useState<Record<string, unknown> | null>(null);
  const [candidateLoading, setCandidateLoading] = useState(true);
  const [selectedProjectID, setSelectedProjectID] = useState('');
  const [candidateName, setCandidateName] = useState('');
  const [submitting, setSubmitting] = useState(false);
  const [error, setError] = useState('');
  const projects = store.projects.value
    .filter((project) => !project.archived && project.available !== false)
    .sort((a, b) => a.name.localeCompare(b.name));

  useEffect(() => {
    if (!target) return;
    const controller = new AbortController();
    setCandidateLoading(true);
    setError('');
    void store.endpoints
      .projectAssignment(target.id, controller.signal)
      .then((data) => {
        const next =
          data.candidate && typeof data.candidate === 'object'
            ? (data.candidate as Record<string, unknown>)
            : null;
        setCandidate(next);
        setCandidateName(String(next?.existing_name || next?.default_name || ''));
        const existingID = String(next?.existing_project_id || '');
        if (
          existingID &&
          !next?.existing_archived &&
          store.projects
            .peek()
            .some(
              (project) =>
                project.id === existingID && !project.archived && project.available !== false,
            )
        ) {
          setSelectedProjectID(existingID);
        }
      })
      .catch((value) => {
        if (controller.signal.aborted) return;
        setError(errorMessage(value));
      })
      .finally(() => {
        if (!controller.signal.aborted) setCandidateLoading(false);
      });
    return () => controller.abort();
  }, [store.endpoints, store.projects, target]);

  if (!target) return null;
  const candidateProjectID = String(candidate?.existing_project_id || '');
  const candidateProject = projects.find((project) => project.id === candidateProjectID);
  const candidateCount = Number(candidate?.matching_conversation_count) || 0;
  const submitExisting = async () => {
    if (!selectedProjectID) return;
    setSubmitting(true);
    setError('');
    try {
      await store.assignProject(selectedProjectID);
    } catch (value) {
      setError(errorMessage(value));
      setSubmitting(false);
    }
  };
  const submitCandidate = async () => {
    setSubmitting(true);
    setError('');
    try {
      await store.createProjectFromWorkspace(candidateName);
    } catch (value) {
      setError(errorMessage(value));
      setSubmitting(false);
    }
  };
  const moveProjectSelection = (event: preact.JSX.TargetedKeyboardEvent<HTMLButtonElement>) => {
    if (!['ArrowDown', 'ArrowUp', 'Home', 'End'].includes(event.key)) return;
    const rows = [
      ...(event.currentTarget.parentElement?.querySelectorAll<HTMLButtonElement>(
        '.project-choice',
      ) || []),
    ];
    const current = rows.indexOf(event.currentTarget);
    if (current < 0 || !rows.length) return;
    event.preventDefault();
    const next =
      event.key === 'Home'
        ? 0
        : event.key === 'End'
          ? rows.length - 1
          : event.key === 'ArrowDown'
            ? Math.min(rows.length - 1, current + 1)
            : Math.max(0, current - 1);
    rows[next].click();
    rows[next].focus();
  };

  return (
    <Overlay title="Assign project" className="project-assign-modal">
      <p class="project-assign-subtitle">
        Choose a sidebar group. Files and workspace stay unchanged.
      </p>
      <section class="project-modal-fields">
        {candidateLoading ? (
          <section class="project-assign-upgrade" aria-busy="true">
            <div class="project-browser-skeleton">
              <span />
              <span />
            </div>
          </section>
        ) : (
          candidate &&
          !candidateProject && (
            <section class="project-assign-upgrade">
              <div class="project-assign-upgrade-top">
                <strong>
                  {candidate.existing_archived
                    ? 'Restore project from current folder'
                    : 'Create project from current folder'}
                </strong>
                <span class="project-choice-recommended">Recommended</span>
              </div>
              <code class="project-manage-path">{String(candidate.canonical_dir || '')}</code>
              <span class="project-field-hint">{assignmentCountCopy(candidateCount)}</span>
              <div class="project-assign-upgrade-controls">
                <input
                  class="project-input"
                  aria-label="New project display name"
                  value={candidateName}
                  onInput={(event) => setCandidateName(event.currentTarget.value)}
                />
                <button
                  class="btn primary"
                  type="button"
                  disabled={submitting}
                  onClick={() => void submitCandidate()}
                >
                  {submitting
                    ? candidate.existing_archived
                      ? 'Restoring…'
                      : 'Creating…'
                    : candidate.existing_archived
                      ? 'Restore & assign'
                      : 'Create & assign'}
                </button>
              </div>
            </section>
          )
        )}

        <div class="project-field">
          <div class="project-field-label-row">
            <span class="project-field-label">Existing projects</span>
          </div>
          <div class="project-choice-list" role="radiogroup" aria-label="Available projects">
            {projects.length ? (
              projects.map((project) => {
                const selected = selectedProjectID === project.id;
                const recommended = candidateProjectID === project.id;
                return (
                  <button
                    type="button"
                    class={`project-choice ${selected ? 'is-selected' : ''} ${recommended ? 'is-recommended' : ''}`}
                    key={project.id}
                    role="radio"
                    aria-checked={selected}
                    tabIndex={selected || (!selectedProjectID && project === projects[0]) ? 0 : -1}
                    disabled={submitting}
                    onKeyDown={moveProjectSelection}
                    onClick={() => {
                      setSelectedProjectID(project.id);
                      setError('');
                    }}
                  >
                    <span class="project-choice-check" aria-hidden="true" />
                    <span class="project-choice-content">
                      <span class="project-choice-name-row">
                        <strong class="project-choice-name">{project.name}</strong>
                        <span class="project-choice-badges">
                          {recommended && (
                            <span class="project-choice-recommended">Current folder</span>
                          )}
                          {project.git && <span class="project-browser-badge">Git</span>}
                        </span>
                      </span>
                      <code class="project-choice-path">{project.path}</code>
                      {recommended && (
                        <span class="project-choice-impact">
                          {assignmentCountCopy(candidateCount)}
                        </span>
                      )}
                    </span>
                  </button>
                );
              })
            ) : (
              <div class="project-choice-empty">
                <strong>No available projects</strong>
                <span>Create a project from the current folder to continue.</span>
              </div>
            )}
          </div>
        </div>
      </section>
      {error && (
        <div class="project-modal-status is-error" role="alert">
          {error}
        </div>
      )}
      <div class="modal-actions project-assign-actions">
        <button class="btn" type="button" onClick={() => (store.modal.value = '')}>
          Cancel
        </button>
        <button
          class="btn primary"
          type="button"
          disabled={!selectedProjectID || submitting}
          onClick={() => void submitExisting()}
        >
          {submitting ? 'Assigning…' : 'Assign project'}
        </button>
      </div>
    </Overlay>
  );
}
