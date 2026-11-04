import { describe, expect, it } from 'vitest';
import { resolveProjectContext } from './project-context';

const projects = [{ id: 'p', name: 'Live project', path: '/repo' }];
describe('resolveProjectContext', () => {
  it('prefers the live project over a cached session name', () => {
    expect(
      resolveProjectContext(true, projects, { projectId: 'p', projectName: 'Old' }, ''),
    ).toEqual({ id: 'p', name: 'Live project', path: '/repo', project: projects[0] });
  });
  it('resolves a project draft', () => {
    expect(resolveProjectContext(true, projects, null, 'p')?.name).toBe('Live project');
  });
  it('keeps a historical name when the project is missing', () => {
    expect(resolveProjectContext(true, [], { projectId: 'p', projectName: 'Old' }, '')?.name).toBe(
      'Old',
    );
  });
  it('returns no context for a loose chat', () => {
    expect(resolveProjectContext(true, projects, {}, '')).toBeNull();
  });
  it('never exposes historical projects in no-project mode', () => {
    expect(
      resolveProjectContext(false, projects, { projectId: 'p', projectName: 'Old' }, 'p'),
    ).toBeNull();
  });
});
