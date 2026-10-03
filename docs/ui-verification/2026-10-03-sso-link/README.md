# Explicit SSO linking UI verification

The committed production frontend was served on loopback with synthetic authenticated admin/provider responses. These screenshots prove layout, not production account linking.

- Desktop: 1280 × 800. Current-password input, provider choice and connect action are readable; the completion status opens the Sign-in tab.
- Mobile: 390 × 844. Controls fit the viewport; the provider callback wraps without horizontal overflow.
- Frontend test proves a CSRF-bearing request and password-input clearing on a refused attempt.
- Go HTTP/store tests prove password, session, browser and provider proofs, one-use state, identity uniqueness, legacy backfill, and a later SSO session on the original account.
- Production callback registration is corrected; the new linking flow still needs release deployment and operator verification.
