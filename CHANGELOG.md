# Changelog

Versions follow `v1.<Synapse minor>.<syngo patch>`: the minor number is the Synapse release
that syngo is verified against (database migration with `syngo-export`/`syngo-import`,
federation interop, Client-Server API behaviour). Patch releases are syngo releases on the
same Synapse baseline. See "Versions and compatibility" in the README.

## Unreleased

- Changed the project license from MPL-2.0 to `AGPL-3.0-or-later`.
- Added Synapse copyright and derivation notices for the ported state resolution code and tests.
- Added the public `/_syngo/v1/source` license/source offer; the federation version endpoint
  reports the same fields. Container images include `LICENSE` and `NOTICE` in `/licenses`.

## v1.165.0 - 2026-11-23

First versioned release. Compatible with Synapse 1.165.

- Client-Server API v1.1-v1.11 for Element Web/Desktop and other clients: registration and
  login (password, UIA, refresh tokens, OIDC/SSO, LDAP), rooms and state, redactions,
  receipts, typing, account data, push rules and pushers, search, media, E2EE transport
  (keys, cross-signing, key backup, to-device).
- `/sync` with long-poll and simplified sliding sync (MSC4186) for Element X.
- Federation: server keys and request signing, server discovery, send/state/backfill/
  missing events, remote invite/join/leave, auth rules and state resolution v2/v2.1
  (room versions 10, 11, 12), EDUs, remote E2EE and media, persistent outbound queue.
- Migration from Synapse 1.165: users, devices, E2EE keys, account data, push rules,
  receipts, rooms and events, media; clients keep working with their old Synapse tokens
  (sync and pagination tokens are accepted after the switch).
- Load-tested against Synapse 1.165 on the same hardware (`docs/load-report-2026-11-22.md`).
