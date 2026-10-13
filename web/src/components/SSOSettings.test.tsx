import { afterEach, expect, it, vi } from 'vitest';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { SSOSettings } from './SSOSettings';

afterEach(() => { cleanup(); vi.unstubAllGlobals(); });

it('shows installer ownership without provider writers and copies only the public callback', async () => {
  const callback = 'https://yard.example/api/sso/kyidentity/callback';
  const fetcher = vi.fn(async () => new Response(JSON.stringify([{ id: 'kyidentity', name: 'KyIdentity', kind: 'oidc', enabled: true, secret_set: true, callback_url: callback, installer_managed: true }])));
  const writeText = vi.fn(async () => {});
  vi.stubGlobal('fetch', fetcher);
  vi.stubGlobal('navigator', { clipboard: { writeText } });
  render(<SSOSettings />);
  await screen.findByText(/Managed by KyQuickStart/);
  expect(screen.queryByText('Add sign-in provider')).toBeNull();
  expect(screen.queryByRole('button', { name: 'Disable' })).toBeNull();
  expect(screen.queryByRole('button', { name: 'Remove provider' })).toBeNull();
  expect(screen.queryByLabelText('New client secret')).toBeNull();
  fireEvent.click(screen.getByRole('button', { name: 'Copy callback for KyIdentity' }));
  await screen.findByRole('status');
  expect(writeText).toHaveBeenCalledWith(callback);
  expect(fetcher).toHaveBeenCalledTimes(1);
});

it('retains ordinary provider creation and editing outside installer ownership', async () => {
  vi.stubGlobal('fetch', vi.fn(async () => new Response(JSON.stringify([{ id: 'idp_test', name: 'Existing provider', kind: 'oidc', enabled: true, secret_set: true, callback_url: 'https://yard.example/api/sso/idp_test/callback' }]))));
  render(<SSOSettings />);
  await screen.findByRole('button', { name: 'Disable' });
  expect(screen.getByText('Add sign-in provider')).toBeTruthy();
  expect(screen.getByRole('button', { name: 'Remove provider' })).toBeTruthy();
});
