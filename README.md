# syngo

[![License: AGPL v3+](https://img.shields.io/badge/License-AGPL_v3%2B-blue.svg)](https://www.gnu.org/licenses/agpl-3.0.html)

Русская версия: [README.ru.md](README.ru.md) (full documentation, in Russian).

syngo is a [Matrix](https://matrix.org) homeserver written in Go. It implements the
Client-Server API without a Python runtime, follows the behaviour of the reference
implementation [Synapse](https://github.com/element-hq/synapse) and is checked against the
Matrix specification and the [Complement](https://github.com/matrix-org/complement)
conformance suite.

## Features

- **Client compatibility:** works with Element Web/Desktop and other clients that use the
  Client-Server API (v1.1 - v1.11): registration and login (password, refresh tokens, UIA),
  rooms, state, redactions, receipts, typing, account data, push rules and pushers, search,
  media (v1 and legacy v3, thumbnails, URL previews), E2EE transport (`/keys/*`, cross-signing,
  key backup, to-device).
- **Sync:** `/sync` with long-poll and simplified sliding sync (MSC4186). The number of SQL
  queries per `/sync` does not grow with the number of rooms; this is enforced by tests.
- **Federation:** server keys and X-Matrix signatures, server discovery (well-known, SRV,
  8448), send/state/backfill/missing events, remote invite/join/leave, auth rules and state
  resolution v2 (room versions 10, 11 and 12), persistent outbound queue with retries.
  Interoperability with a live Synapse is verified (`docs/interop-report.md`).
- **Migration from Synapse:** export from a Synapse PostgreSQL database
  (`tools/synapse-export/export.sql`) and import with `syngo-import`: users, rooms and full
  event history (event IDs preserved), devices and access tokens, E2EE keys, account data,
  push rules, media files and metadata. Synapse password hashes (bcrypt, pepper) are accepted.
  There is also an exit path back to JSONL (`syngo-export`).
- **Authentication:** local passwords (argon2id), LDAP (configuration compatible with
  synapse-ldap3) and SSO via OpenID Connect (`oidc_providers` format of Synapse).
- **Operations:** `/health` and `/health/ready`, Prometheus metrics on a separate listener,
  alert rules and a Grafana dashboard (`deploy/monitoring/`), backup/restore scripts, a
  production docker-compose setup and a systemd unit, admin API.

## Status

The core of the Client-Server API, `/sync`, sliding sync, federation and migration from
Synapse are implemented. The November 25 Complement run matched all 553 results from
November 21. Of those, 340 passed, 206 failed and 7 were skipped. The test image lacked an
HTTPS listener on port 8448. The federation test client stopped before another 77 scenarios
returned results. [Complement results](test/complement/results.jsonl)
One `syngod` process per database is supported (a second instance refuses to start, see
`docs/adr/0009-single-instance.md`). Rehearse a migration on a copy of the data before moving
a production installation (`docs/migration-runbook.md`).

## Versions and compatibility

Versions follow `v1.<Synapse minor>.<syngo patch>`: `v1.165.0` is compatible with Synapse 1.165.
The minor number is the Synapse release syngo is verified against; patch releases are syngo
releases on the same baseline. Moving to a newer Synapse (after re-verification) bumps the
minor: `v1.<N>.0`.

| syngo | Synapse | Verified |
|---|---|---|
| v1.165.x | 1.165 | database migration (`syngo-export`, `syngo-import`, Synapse 1.165 fixtures), federation (interop stand with Synapse 1.165), Client-Server API (Element Web, Element X via simplified sliding sync) |

The version is reported by `syngod -version`, `GET /_matrix/federation/v1/version`
(`{"name": "syngo", "version": "1.165.0"}`) and the `syngo_build_info` metric. See
[CHANGELOG.md](CHANGELOG.md).

## Requirements

- Go 1.27+
- PostgreSQL 16+ (17 is used in CI and in the compose files)
- Docker with compose v2 for the containerised setup, the federation stand and Complement

## Build

```bash
git clone https://github.com/aisa-it/syngo.git
cd syngo
make build        # static binary bin/syngod
make docker       # image syngo:dev (multi-stage, distroless)
```

`make help` lists all targets.

## Run

### Docker Compose

```bash
cp deploy/prod/.env.example deploy/prod/.env
# set SYNGOD_SERVER_NAME (public domain), POSTGRES_PASSWORD,
# SYNGOD_SYNC_TOKEN_SECRET and SYNGOD_REGISTRATION_SHARED_SECRET (openssl rand -hex 32)
docker compose -f deploy/prod/docker-compose.yml up -d --build --wait
```

The compose file starts PostgreSQL, applies migrations with a one-shot `migrate` container and
then starts `syngod` on `127.0.0.1:8008`. Put a TLS-terminating reverse proxy in front of it.
Registration is closed by default; create the first (admin) user with the shared secret,
compatible with Synapse's `register_new_matrix_user`:

```bash
register_new_matrix_user -k "$SYNGOD_REGISTRATION_SHARED_SECRET" -u admin -a http://127.0.0.1:8008
```

### Binary

```bash
cp config.example.yaml config.yaml   # edit server_name, database DSN, secrets
bin/syngo-migrate -dsn "$SYNGOD_DATABASE_DSN" -up   # or: make migrate PSQL_DSN=...
bin/syngod -config config.yaml
```

`syngo-migrate` is built from `cmd/migrate` (`go build -o bin/syngo-migrate ./cmd/migrate`);
SQL migrations are embedded into both binaries. A systemd unit is provided in
`deploy/prod/syngod.service`.

For local development `make run` starts the server on `:8008` in dev mode
(`server_name` localhost, open registration):

```bash
make run
curl -s localhost:8008/_matrix/client/versions
```

## Configuration

Values are applied in this order: defaults, YAML file (`-config` or `$SYNGOD_CONFIG`),
environment variables (`SYNGOD_*`). See `config.example.yaml` and `.env.example` for all
options. Unknown YAML keys are rejected and the configuration is validated before start.
The default environment is `prod`: it requires a real `server_name`, a database DSN and a
`/sync` token secret of at least 32 characters, and forbids insecure test-only switches.

## Tests

```bash
make test-race                                   # unit tests with -race
make coverage                                    # tests + per-package coverage ratchet
make gates-test                                  # self-tests of the ratchet gates
make lint                                        # golangci-lint v2 (make tools installs it)
SYNGOD_TEST_DSN='postgres://syngo:syngo@127.0.0.1:5432/syngo_test?sslmode=disable' \
  make test-integration                          # integration tests on a live PostgreSQL
make test-complement                             # Complement + ratchet (needs Docker)
make stand-up && make stand-smoke                # two-server federation stand (a.test, b.test)
make stand-interop                               # federation interop with a live Synapse
```

The integration test database name must contain `test`: the tests drop and recreate its schema.
CI runs on GitHub Actions: `.github/workflows/ci.yml` (build, lint, gate self-tests, unit tests
with coverage ratchet, integration tests on PostgreSQL 17, media backup and restore, alert rules, shellcheck,
govulncheck) and `.github/workflows/complement.yml` (Complement, manual run).
Load testing against syngo and Synapse is described in `docs/load-testing.md`.

## Documentation

- `README.ru.md` - full documentation (Russian)
- `docs/adr/` - architecture decision records
- `docs/operations.md` - backup, restore, monitoring, alerts
- `docs/migration-runbook.md`, `docs/migration-rehearsal.md` - migration from Synapse
- `docs/api-scope.md` - implemented API scope
- `docs/load-testing.md`, `docs/load-report-2026-11-22.md` - load testing and results

## License and source availability

syngo is free software licensed under the
[GNU Affero General Public License, version 3 or (at your option) any later version](LICENSE)
(`AGPL-3.0-or-later`). Portions derived from Synapse and their upstream copyright notices are
listed in [NOTICE](NOTICE).

The corresponding source is available at <https://github.com/aisa-it/syngo>.
`GET /_syngo/v1/source` publicly reports the SPDX license identifier and a source URL, including
when federation is disabled; the federation version endpoint reports the same fields. Release
builds must set `COMMIT` and publish that revision so the URL identifies the exact source.
Operators deploying modified versions must keep the corresponding source available to their users under
AGPL section 13.
