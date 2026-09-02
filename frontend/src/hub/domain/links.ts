import { hubPath } from '../config';
import type { HubDelegation, HubNode } from './types';

export interface DelegationArtifact {
  type: 'image' | 'link';
  url: string;
  label: string;
}

function mountedPathTargetsNode(path: string, prefix: string, targetNode: string): boolean {
  if (!targetNode || !path.startsWith(prefix)) return false;
  const encodedNode = path.slice(prefix.length).split(/[/?#]/, 1)[0];
  if (!encodedNode) return false;
  try {
    return decodeURIComponent(encodedNode) === targetNode;
  } catch {
    return false;
  }
}

export function safeArtifactURL(
  rawValue: string,
  delegation: Pick<HubDelegation, 'target_node'>,
  nodes: Pick<HubNode, 'id' | 'base_path'>[],
  basePath: string,
  pageURL: string,
): string {
  const raw = String(rawValue || '').trim();
  if (!raw || raw.startsWith('//') || raw.includes('\\')) return '';
  const mountedNodePrefix = hubPath(basePath, '/node/');
  if (raw.startsWith(mountedNodePrefix)) {
    return mountedPathTargetsNode(raw, mountedNodePrefix, delegation.target_node) ? raw : '';
  }
  if (basePath && raw.startsWith('/node/')) {
    return mountedPathTargetsNode(raw, '/node/', delegation.target_node)
      ? hubPath(basePath, raw)
      : '';
  }
  if (raw.startsWith('/') && delegation.target_node) {
    const nodeBase =
      nodes.find((node) => node.id === delegation.target_node)?.base_path?.replace(/\/$/, '') || '';
    let path = raw;
    if (nodeBase && path === nodeBase) path = '/';
    else if (nodeBase && path.startsWith(`${nodeBase}/`)) path = path.slice(nodeBase.length);
    return hubPath(basePath, `/node/${encodeURIComponent(delegation.target_node)}${path}`);
  }
  if (raw.startsWith('/')) return raw;
  try {
    const url = new URL(raw, pageURL);
    return url.protocol === 'http:' || url.protocol === 'https:' ? url.href : '';
  } catch {
    return '';
  }
}

export function firstDelegationArtifact(
  text: string,
  delegation: Pick<HubDelegation, 'target_node'>,
  nodes: Pick<HubNode, 'id' | 'base_path'>[],
  basePath: string,
  pageURL: string,
): DelegationArtifact | null {
  const image = text.match(/!\[([^\]]*)\]\(([^)]+)\)/);
  if (image) {
    const url = safeArtifactURL(image[2], delegation, nodes, basePath, pageURL);
    if (url) return { type: 'image', url, label: image[1] || 'Open artifact' };
  }
  const link = text.match(/(?<!!)\[([^\]]+)\]\(([^)]+)\)/);
  if (link) {
    const url = safeArtifactURL(link[2], delegation, nodes, basePath, pageURL);
    if (url) return { type: 'link', url, label: link[1] };
  }
  return null;
}
