# Getting started

## First start

Follow the published-image installation in the product reference. Open `http://localhost:9273` on the Docker host and retrieve the temporary administrator password using the documented container command. Replace it at first sign-in. Compose keeps data in the `kyyard-data` volume.

Containers shows Local Docker automatically on a standard installation. For remote browser access, configure HTTPS and trust only the real proxy address. Existing bind-mount installations need the documented bind overlay so an update does not start an empty instance.

## Connect another host

Use the host/agent enrollment workflow in the product reference and verify the remote fingerprint. Confirm that inventory and logs come from the intended host before enabling lifecycle controls. Configure SSO in Settings when required; SAML login is not implemented.
