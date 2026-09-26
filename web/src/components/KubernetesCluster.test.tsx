import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { KubernetesCluster } from './KubernetesCluster';
import type { Endpoint, KubernetesInventory } from '../tenant';
afterEach(() => { cleanup(); vi.unstubAllGlobals(); });
const endpoint = { id: 'ep_k', environment_id: 'env-a', name: 'prod', runtime: 'kubernetes', state: 'active', facts: {}, fingerprint: '', capabilities: [], alerts: [], created_at: '', deploy_namespaces: [] } as Endpoint;
const empty: KubernetesInventory = { nodes: [], namespaces: [], workloads: [], pods: [], services: [], claims: [] };
const view = (inventory: KubernetesInventory, e: Endpoint = endpoint) => <KubernetesCluster org="a" base="/api/organizations/a/endpoints/ep_k" endpoint={e} inventory={inventory} instances={[]} admin={false} onChanged={vi.fn()} />;

it('shows a cluster that reports no nodes as unknown, not healthy', () => {
  render(view(empty));
  const health = screen.getByRole('region', { name: 'Cluster health' });
  expect(health.textContent).toContain('unknown');
  expect(health.textContent).toContain('0 of 0 nodes ready.');
  expect(screen.getByText('No nodes reported.')).toBeTruthy();
});

it('lists a pod with no containers reported yet, with nothing to open', () => {
  render(view({ ...empty, namespaces: ['shop'], pods: [{ namespace: 'shop', name: 'web-1', phase: 'Pending', node: '', owner_kind: '', owner_name: '', started_at: '', containers: [] }] }));
  expect(screen.getByText('shop/web-1')).toBeTruthy();
  expect(screen.getByText('Pending')).toBeTruthy();
  expect(screen.queryByRole('button', { name: /^Logs for/ })).toBeNull();
});

it('drops a namespace filter whose namespace left the cluster', () => {
  const pod = (namespace: string, name: string) => ({ namespace, name, phase: 'Running', node: 'n1', owner_kind: '', owner_name: '', started_at: '', containers: [] });
  const both = { ...empty, namespaces: ['billing', 'shop'], pods: [pod('billing', 'pay-1'), pod('shop', 'web-1')] };
  const { rerender } = render(view(both));
  fireEvent.change(screen.getByRole('combobox', { name: 'Namespace' }), { target: { value: 'shop' } });
  expect(screen.queryByText('billing/pay-1')).toBeNull();
  rerender(view({ ...both, namespaces: ['billing'], pods: [pod('billing', 'pay-1')] }));
  expect(screen.getByRole('combobox', { name: 'Namespace' })).toHaveProperty('value', '');
  expect(screen.getByText('billing/pay-1')).toBeTruthy();
});
