# Changelog

All notable changes to elephant-user from v1.0.0 onwards are documented here.
Entries are derived from the release tags; the linked pull requests hold the
detail. Earlier history is not reconstructed.

## [v1.4.0] - Unreleased

**Behaviour change (authentication):** a request with an invalid or missing
token is answered `unauthenticated` (HTTP 401) where it was answered
`permission_denied` (403). `permission_denied` now only means an identified
caller that lacks a scope. Anything keyed on 403 for a bad token, such as an
ingress rule, a dashboard panel or a client's retry logic, reads 401 after the
upgrade. Inherited from elephantine v0.29.0. (#76)

**Behaviour change (request bodies):** request bodies are capped at 8 MiB. A
request that declares a larger `Content-Length` is refused with 413 before it
reaches a handler; a chunked body fails on the read that passes the limit.
Inherited from elephantine v0.28.0. (#76)

**Behaviour change (delete events):** `DeleteDocument` on a document that does
not exist no longer emits a delete event to the eventlog. An idempotent
re-delete used to produce a second `delete` entry; consumers of `PollEventLog`
should not rely on one. (#75)

**Breaking (configuration):** the `PG_CONN_URI` environment variable is no
longer read. Set the database connection string with `CONN_STRING` (or the
`--db` flag), which the platform deployment templates already use. (#76)

**Build (Go 1.27.1):** the module's `go` directive is 1.27.1, which elephantine
v0.29.0 requires. A build box pinned to an older toolchain with
`GOTOOLCHAIN=local` fails on the upgrade; `GOTOOLCHAIN=auto` downloads it. The
Docker build image moves to `golang:1.27.1-alpine3.24`. (#76)

**Migrations:**

- `schema/004_sequence_counter.sql` creates the `sequence_counter` table seeded
  from the current eventlog, drops the identity from `eventlog.id`, and makes
  `message_write_lock.current_message_id` `NOT NULL DEFAULT 0`. Old and new code
  are mutually incompatible with the other's schema, so it must run in a
  service window: scale to 0, migrate, verify
  `schema_version = 4` and that `sequence_counter` matches `MAX(eventlog.id)`,
  deploy, scale up. Rollback needs a window as well. (#75)

Changes:

- Fixed `PollEventLog` permanently skipping an entry when a slower write
  committed after a faster one: eventlog ids are now assigned from a row-locked
  counter inside the writing transaction, so id order equals commit order.
  Clients need no change; ids remain monotonically increasing. (#75)
- Fixed a race where concurrent first-ever pushes to a recipient failed with an
  internal error instead of being serialised. (#75)
- Fixed a deadlock between concurrent property writes for the same owner; property
  rows are now locked in a stable order and eventlog ids are reserved as the
  transaction's last lock. (#75)
- New `--cleanup-interval` flag (`CLEANUP_INTERVAL`, default `12h`) controls how
  often expired messages and inbox messages are removed. (#76)
- New `--db-max-conns` flag (`DB_MAX_CONNS`, default `16`) sets the query pool
  size explicitly instead of letting it follow the node's CPU count; `0`
  restores that default. With a bouncer configured the direct pool is fixed at
  2. (#76)
- `/health/ready` reports a `postgres` entry alongside `schemas`. Both are
  optional: they show in the body and in `health_check_up{name}` without
  failing the probe, so a starved pool cannot pull every replica from the load
  balancer at once. (#76)
- `GET /version` reports the build version (set via `-ldflags "-X main.version"`,
  Dockerfile build arg `VERSION`) and the elephant-api/elephantine module versions.
  (#76)
- New metrics: `pgxpool_*` connection pool statistics (`pool="main"`, and
  `pool="pubsub"` when a bouncer pool is configured), `pg_job_lock_*` for the
  cleaner's job lock, `task_restarts_total{task="pubsub"}` for LISTEN
  subscriber restarts, and
  `rpc_protocol_responses_total{service,method,protocol,code,client_id}` from
  elephantine, which breaks RPC responses down by error code and calling
  application. (#76)
- The plaintext listener serves HTTP/2 alongside HTTP/1.1; HTTP/1.1 callers are
  unaffected. (#76)
- The pg LISTEN subscriber and the retention cleaner run as supervised tasks
  next to the API server. A subscriber that loses its connection for a reason
  the library does not reconnect from is restarted with a 5 s backoff; a
  cleaner failure stops the service so it is restarted rather than running
  without retention. On SIGTERM both stop immediately while the server keeps
  serving in-flight requests for up to 10 s. The cleaner sweeps once as soon as
  it acquires its lock, then every interval. (#76)
- Dependency upgrades: elephantine to v0.29.1, ttab/mage to v0.13.1,
  golangci-lint to 2.13, `golang.org/x/crypto` to v0.56.0. (#76)

## [v1.3.0] - 2026-07-20

**Breaking (schemas):** revisor schemas are no longer embedded in the binary.
They live in Postgres and are managed through the new `Configuration` Twirp
service as config generations. A freshly migrated environment has no active
generation, and validated writes (`UpdateDocument`, `PushInboxMessage`) fail
until one is registered and activated. Register the seed schemas
`se.ecms.user.settings` and `se.ecms.user.messages` (version `v1.0.0`) with
`RegisterConfigGeneration` and activate them before routing traffic. Requires
elephant-api v0.24.2 or later for the `Configuration` client. (#65, #66)

**Behaviour change (validation):** each schema declares a usage, `settings` or
`messages`, and validation is per usage. A settings document type no longer
validates as an inbox message and vice versa. (#65)

**Migrations:**

- `schema/003_config_generations.sql` adds `document_schema`,
  `config_generation`, `config_generation_schema` and `deprecation`. Run before
  the deploy; no data is touched and no maintenance window is needed. (#65)

Changes:

- New `Configuration` service: `RegisterConfigGeneration`,
  `ActivateConfigGeneration`, `GetActiveConfigGeneration` (long-poll),
  `ListConfigGenerations`, `GetSchema`, `GetDeprecations`, `UpdateDeprecation`.
  Requires the `schema_admin` scope for writes and `schema_read` for reads. (#65)
- Schema deprecations can be toggled per label: unenforced uses are logged and
  counted in `elephant_user_deprecations_total` and
  `elephant_user_docs_with_deprecations_total`; enforced ones block writes. (#65)
- Running instances hot-reload their validators when the active generation or a
  deprecation changes, with a five-minute periodic fallback. (#65)
- New `--migrate-db` flag (`MIGRATE_DB`) applies pending migrations at startup.
  Intended for disposable environments only; production migrations are run by
  the platform tooling. (#65)
- Optional `schemas` entry in `/health/ready` reports whether the active
  generation covers both usages without failing the probe. (#65)
- Dependency upgrades: Go to 1.26.4, elephant-api to v0.24.2, GitHub Actions and
  golangci-lint to 2.12.2. (#66)

## [v1.2.1] - 2026-04-30

Changes:

- New TLS flags on `run`: `--tls-addr` (`TLS_ADDR`/`TLS_LISTEN_ADDR`, default
  `:1443`), `--cert-file` (`TLS_CERT_PATH`) and `--key-file` (`TLS_KEY_PATH`).
  When a certificate is configured the API server listens on both the HTTP and
  HTTPS ports. (#50)
- Dependency upgrades: elephantine to v0.26.1. (#50)

## [v1.2.0] - 2026-04-17

Changes:

- New settings document type `ntb/nynorsk` for storing NTB translation
  preferences. (#42)
- Dependency upgrades: Go to 1.26.2. (#42)

## [v1.1.1] - 2026-03-23

Changes:

- Poll handlers return `twirp.Canceled` instead of an internal error when the
  client cancels the request.

## [v1.1.0] - 2026-03-23

**Breaking (configuration):** the `--pg-conn-uri` flag is renamed `--db`. The
`PG_CONN_URI` and `CONN_STRING` environment variables are unchanged.

Changes:

- New `--db-bouncer` flag (`BOUNCER_CONN_STRING`) for a PgBouncer connection
  string. When set, queries use the bouncer pool while LISTEN/NOTIFY keeps a
  direct connection, which transaction pooling does not support.
- Dependency upgrades: Go to 1.26.1, eltest, golangci-lint to 2.11.3, GitHub
  Actions.

## [v1.0.6] - 2026-03-02

**Breaking (inbox messages):** the inbox message document type is renamed from
`tt/inbox-message` to `core/inbox-message`. Producers must push documents with
the new type; the old type no longer validates.

Changes:

- Dependency upgrades.

## [v1.0.5] - 2026-02-24

Changes:

- The `core/wire-pane` content block in `core/wire-panes-setting` documents no
  longer requires at least one `core://view-setting-filter` meta block.
- Dependency upgrades: Go to 1.26.0, golangci-lint to 2.10.

## [v1.0.4] - 2026-02-16

**Migrations:**

- `schema/002_settings.sql` adds the `document`, `property` and `eventlog`
  tables and the `user.kind` column. Run before the deploy; no maintenance
  window is needed.

Changes:

- New `Settings` service. `GetDocument`, `ListDocuments`, `UpdateDocument` and
  `DeleteDocument` manage schema-validated newsdoc settings documents keyed by
  owner, application, type and key, for view settings such as filters and
  searches. `GetProperties`, `SetProperties` and `DeleteProperties` store flat
  key-value preferences. `PollEventLog` long-polls a change stream over both.
  Documents can be owned by the user, a unit or an organisation; writing to a
  shared owner requires the `doc_admin` scope and membership. Requires
  elephant-api v0.20.0 or later. (#27)
- Fixed the owner kind recorded when a unit or organisation owner is first seen.
- Dependency upgrades: Go to 1.25.6, Alpine 3.23, GitHub Actions.

## [v1.0.3] - 2025-11-24

Changes:

- The database connection string can also be supplied as `CONN_STRING`, the
  name used by the platform deployment templates, in addition to `PG_CONN_URI`.

## [v1.0.2] - 2025-06-18

Changes:

- Dependency upgrades.

## [v1.0.1] - 2025-05-10

Changes:

- Dependency upgrades: elephantine bug fix release.

## [v1.0.0] - 2025-05-10

Changes:

- CORS hosts are configurable with the repeatable `--cors-host` flag
  (`CORS_HOSTS`); wildcards are supported. (#26)
