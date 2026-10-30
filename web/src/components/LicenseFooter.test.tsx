import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { cleanup, fireEvent, render, screen } from '@testing-library/react';
import { afterEach, beforeEach, expect, test } from 'vitest';
import { LicenseFooter } from './LicenseFooter';
import mitLicenseText from '../mit.txt?raw';

beforeEach(() => {
  Object.defineProperty(HTMLDialogElement.prototype, 'showModal', { configurable: true, value: function (this: HTMLDialogElement) { this.open = true; } });
});
afterEach(() => { cleanup(); Reflect.deleteProperty(HTMLDialogElement.prototype, 'showModal'); });

test('the bundled licence is the repository LICENSE.txt, byte for byte', () => {
  expect(mitLicenseText).toBe(readFileSync(resolve(__dirname, '../../../LICENSE.txt'), 'utf8'));
});

test('the copyright line opens the About dialog with the version and licence', () => {
  render(<LicenseFooter appName="KyYard" version="0.5.1" />);
  fireEvent.click(screen.getByRole('button', { name: /MIT License/ }));
  const dialog = screen.getByRole('dialog', { name: 'About KyYard' }) as HTMLDialogElement;
  expect(dialog.open).toBe(true);
  expect(dialog.textContent).toContain('v0.5.1');
  expect(dialog.textContent).toContain('Developed by Busnes.app');
  expect((screen.getByRole('textbox', { name: 'MIT License' }) as HTMLTextAreaElement).value).toContain('Copyright (c) 2026 Busnes.app');
});

test('no version, no badge', () => {
  render(<LicenseFooter appName="KyYard" />);
  fireEvent.click(screen.getByRole('button', { name: /MIT License/ }));
  expect(screen.getByRole('dialog', { name: 'About KyYard' }).textContent).not.toMatch(/\bv\d/);
});
