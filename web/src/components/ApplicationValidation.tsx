import type { Validation } from '../tenant';
import { planBlockers } from './ApplicationUpdates';

// Verdicts (docs/application-schema.md, Health validation); '' is a validation still running.
export const VALIDATION_VERDICTS: Record<string, string> = {
  '': 'Validating: watching the containers after the deployment.',
  healthy: 'Healthy: every service kept running and passed its healthcheck.',
  unhealthy: 'Unhealthy: a healthcheck failed or never passed.',
  exited: 'Exited: a container stopped or is gone.',
  restarting: 'Restarting: a container restarted.',
  unverifiable: 'Not validated.',
  changed: 'Changed: the containers were replaced after this deployment.',
};
// The loop's own sentences, as an unverifiable or changed detail or a pause reason carries them.
const VALIDATION_DETAILS: Record<string, string> = {
  'the host could not be observed': 'The host could not be observed.',
  'the server was not running during the window': 'The server was not running during the window.',
  'the agent cannot report container health': "The host's agent cannot report container health; upgrade it.",
  'an inspection failed validation': "An inspection answer from the host's agent failed validation.",
  'the application was released': 'The application was released.',
};
// Why an automated update was not rolled back: eligibility first, then what stopped an attempt.
export const ROLLBACK_REASONS: Record<string, string> = {
  no_prior_identity: 'No record of what ran before this update.',
  prior_definition_invalid: 'The earlier revision no longer validates.',
  service_set_changed: "The application's services changed since the earlier revision.",
  prior_images_missing: 'The earlier images are no longer on the host.',
  rollback_in_flight: 'Another rollback was still in progress.',
  already_rolled_back: 'This deployment was already rolled back.',
  creator_lost: 'The administrator who last saved the policy can no longer deploy.',
  not_sent: 'The host disconnected before the rollback was sent.',
  interrupted: 'The server restarted during the rollback.',
  policy_changed: 'The policy changed while the rollback was prepared.',
  endpoint_offline: 'The host was not connected.',
  not_adopted: 'The application is no longer adopted.',
  mapping_required: 'The services are not mapped.',
  adoption_changed: 'Adoption or inventory changed during the rollback.',
  deployment_in_progress: 'Another deployment was being applied.',
  invalid: 'The rollback was refused as invalid.',
  error: 'The rollback failed on the server; check the server log.',
};
const SERVICE = /^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$/;
const DEPLOYMENT = /^[0-9a-f-]{36}$/;
const fixed = (table: Record<string, string>, key: string) => Object.hasOwn(table, key) ? table[key] : '';

// verdictText is the verdict and why, through the fixed tables and the service-name shape only.
export function verdictText(v: Validation): string {
  const head = fixed(VALIDATION_VERDICTS, v.verdict) || 'Unrecognised verdict.';
  const why = fixed(VALIDATION_DETAILS, v.detail) || (SERVICE.test(v.detail) ? `Service ${v.detail}.` : '');
  return why ? `${head} ${why}` : head;
}
// reasonText renders comma-separated codes; unknown codes are dropped.
export function reasonText(codes: string): string {
  const known = codes.split(',').map((c) => fixed(ROLLBACK_REASONS, c) || fixed(planBlockers, c)).filter(Boolean);
  return known.length ? known.join(' ') : 'The reason was not recognised.';
}
export function rollbackText(v: Validation): string {
  const r = v.rollback;
  if (!r) return '';
  if (r.outcome === 'applied') return `Rolled back to revision ${r.revision}.`;
  if (r.outcome === 'ineligible' || r.outcome === 'failed') return `Not rolled back. ${reasonText(r.detail)}`;
  return 'Rolling back.';
}
const ROLLED_BACK = 'rolled back after a failed update';
const NOT_ROLLED_BACK = 'update failed and could not be rolled back: ';
const UNVERIFIED = 'update could not be validated: ';
// pauseText explains a pause the validation loop caused; '' for any other reason.
export function pauseText(reason: string): string {
  if (reason === ROLLED_BACK) return 'An automated update failed validation and was rolled back. Check the application, then resume.';
  if (reason.startsWith(NOT_ROLLED_BACK)) return `An automated update failed validation and was not rolled back. ${reasonText(reason.slice(NOT_ROLLED_BACK.length))} Fix the application, then resume.`;
  if (reason.startsWith(UNVERIFIED)) {
    const why = fixed(VALIDATION_DETAILS, reason.slice(UNVERIFIED.length));
    return `An automated update could not be validated.${why ? ` ${why}` : ''} Check the application, then resume.`;
  }
  return '';
}
// ValidationLine is one validation: verdict, rollback and the rollback deployment's ID prefix.
export function ValidationLine({ v }: { v: Validation }) {
  const id = v.rollback?.outcome === 'applied' ? v.rollback.deployment_id : '';
  return <span>{verdictText(v)}{v.rollback && <> {rollbackText(v)}</>}{DEPLOYMENT.test(id) && <> Deployment <code title={id}>{id.slice(0, 8)}</code>.</>}</span>;
}
