import { afterEach, expect, it } from 'vitest';
import { cleanup, render, screen } from '@testing-library/react';
import { KUBERNETES_UNVERIFIED, ROLLBACK_REASONS, VALIDATION_VERDICTS, ValidationLine, pauseText, reasonText } from './ApplicationValidation';
import type { Validation } from '../tenant';
afterEach(cleanup);

const validation = (over: Partial<Validation> = {}): Validation => ({ deployment_id: 'd', policy_run_id: 'r', automated: true, is_rollback: false, phase: 'done', started_at: '2026-09-25T10:00:00Z', observe_until: '2026-09-25T10:02:30Z', verdict: 'healthy', detail: '', rollback: null, correlation_id: 'c', finished_at: '2026-09-25T10:02:31Z', ...over });
const line = (over: Partial<Validation>) => {
  cleanup();
  return render(<ValidationLine v={validation(over)} />).container.textContent ?? '';
};

it('renders every verdict from the fixed table', () => {
  for (const [verdict, text] of Object.entries(VALIDATION_VERDICTS)) expect(line({ verdict })).toContain(text);
  expect(line({ verdict: 'exploded' })).toContain('Unrecognised verdict.');
});

it('names the deciding service and the loop sentences, and nothing else the server sent', () => {
  expect(line({ verdict: 'unhealthy', detail: 'web' })).toContain('Service web.');
  expect(line({ verdict: 'unverifiable', detail: 'the host could not be observed' })).toContain('The host could not be observed.');
  expect(line({ verdict: 'changed', detail: 'the application was released' })).toContain('The application was released.');
  expect(line({ verdict: 'unverifiable', detail: 'secret-canary <b>' })).not.toContain('secret-canary');
});

it('shows the rollback revision and deployment prefix, or why there was none', () => {
  const id = '3f2b1c9e-8d4a-4e6f-9a0b-1c2d3e4f5a6b';
  render(<ValidationLine v={validation({ verdict: 'unhealthy', detail: 'web', rollback: { deployment_id: id, revision: 4, outcome: 'applied', detail: '' } })} />);
  expect(screen.getByText(/Rollback sent to revision 4\./)).toBeTruthy();
  expect(screen.getByText('3f2b1c9e').getAttribute('title')).toBe(id);
  for (const [code, text] of Object.entries(ROLLBACK_REASONS)) expect(line({ verdict: 'exited', rollback: { deployment_id: '', revision: 0, outcome: 'ineligible', detail: code } })).toContain(text);
  expect(line({ verdict: 'exited', rollback: { deployment_id: '', revision: 0, outcome: 'failed', detail: 'image_not_reported,made_up' } })).not.toContain('made_up');
  expect(reasonText('made_up')).toBe('The reason was not recognised.');
});

it('explains each validation pause and nothing else', () => {
  expect(pauseText('rolled back after a failed update')).toContain('was rolled back');
  expect(pauseText('update failed and could not be rolled back: prior_images_missing')).toContain(ROLLBACK_REASONS.prior_images_missing);
  expect(pauseText('update could not be validated: the host could not be observed')).toContain('The host could not be observed.');
  expect(pauseText('update could not be validated: secret-canary')).not.toContain('secret-canary');
  expect(pauseText('three consecutive windows failed')).toBe('');
});

// Mirrors internal/api/policies.go policyErrorCodes + internal/store/validations.go's Rollback*
// constants (plus policy_changed from validations.go and the "error" fallback): every code the
// server can emit in a rollback's detail must have a fixed text here.
it('has a fixed text for every rollback code the server can emit', () => {
  const emitted = [
    'no_prior_identity', 'prior_definition_invalid', 'service_set_changed', 'prior_images_missing',
    'rollback_in_flight', 'already_rolled_back', 'creator_lost', 'not_sent', 'interrupted',
    'policy_changed', 'endpoint_offline', 'not_adopted', 'mapping_required', 'adoption_changed',
    'deployment_in_progress', 'invalid', 'error',
  ];
  for (const code of emitted) expect(Object.hasOwn(ROLLBACK_REASONS, code)).toBe(true);
});

it('gives a Kubernetes apply the one unverifiable sentence instead of the upgrade advice', () => {
  cleanup();
  const text = render(<ValidationLine v={validation({ verdict: 'unverifiable', detail: 'the agent cannot report container health' })} kubernetes />).container.textContent ?? '';
  expect(text).toBe(KUBERNETES_UNVERIFIED);
  expect(text).not.toContain('upgrade');
  expect(line({ verdict: 'unverifiable', detail: 'the agent cannot report container health' })).toContain('upgrade it');
});
