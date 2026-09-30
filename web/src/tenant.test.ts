import { describe, expect, it } from 'vitest';
import { canConfigure } from './tenant';

describe('canConfigure', () => {
  it('is organization admin only', () => {
    expect(canConfigure('organization_admin')).toBe(true);
    for (const role of ['environment_admin', 'operator', 'developer', 'read_only', undefined]) {
      expect(canConfigure(role)).toBe(false);
    }
  });
});
