import type { Project, Session } from './types';

/** Resolve live project context without leaking historical projects into single-workspace mode. */
export function resolveProjectContext(
  projectsEnabled: boolean,
  projects: Project[],
  session: Pick<Session, 'projectId' | 'projectName'> | null,
  activeProjectId: string,
) {
  if (!projectsEnabled) return null;
  const id = session?.projectId || activeProjectId;
  const project = projects.find((entry) => entry.id === id);
  const name = project?.name || session?.projectName;
  return name ? { id, name, path: project?.path, project } : null;
}
