# The Elephant User API

elephant-user is the per-user data service of the Elephant editorial system,
written in Go. It stores what belongs to a person rather than to a document:
**settings documents** (view configurations such as filters and searches,
validated newsdoc payloads keyed by owner, application, type and key),
**properties** (flat key-value preferences), **inbox messages** (durable
newsdoc documents with a read flag) and **system messages** (ephemeral
notifications such as error toasts). Everything lives in Postgres and is served
over Twirp.

Two things make it more than a CRUD service. Settings documents can be owned by
an organisation or a unit as well as a user, so a filter set can be shared
across a desk, and changes to documents and properties are written to an
eventlog that clients tail with long-polling `PollEventLog` to keep open tabs
in sync. Validation schemas are not baked in: they live in Postgres as
**config generations**, are managed through the `Configuration` API, and every
replica hot-reloads its validators when the active generation changes.

## Documentation

This README is the working reference: what the repository holds, how to build
and run it, and what every configuration flag does. The design lives in
`docs/`.

| Document | What it settles |
|---|---|
| [`docs/architecture.md`](docs/architecture.md) | How the service is built: the process model, the write and long-poll paths, id assignment, schemas and config generations, the RPC surface and scopes. Start here to understand the system. |
| [`docs/ops.md`](docs/ops.md) | The operator's-eye view: dependencies, deployment shape, data flows, the bootstrap order, migrations, and the failure modes with the signal that shows each one. |
| [`docs/observability.md`](docs/observability.md) | Every metric the service exports and what a change in it means. |
| [`CHANGELOG.md`](CHANGELOG.md) | What each release changed for a consumer, with the deploy procedure where one is needed. |

The documents link to each other by heading, and a renamed heading otherwise
breaks a link silently, so `mage docs:links` checks that every relative link
and `#anchor` resolves. The lint workflow runs it.

## Repository layout

```
cmd/user/                  the service binary: flags, pools, wiring
internal/
  service.go               Settings and Messages Twirp handlers, access control
  config_service.go        Configuration Twirp handlers
  config.go                config feature contract: types, events, errors
  store.go                 PGStore: messages, settings, eventlog, subscriber, cleaner
  config_store.go          PGStore: config generations, schemas, deprecations
  validator.go             hot-reloaded per-usage revisor validators
  metrics.go               every Prometheus collector the service owns
  app.go                   the err-group: API server, subscriber, cleaner
  migrate.go               tern helper behind the --migrate-db flag
  se.ecms.user.*.json      the seed constraint sets (tests and bootstrapping)
postgres/                  sqlc-generated query code; queries.sql is the source
schema/                    tern migrations, vendor.json for library migrations
testdata/                  golden files for the integration tests
magefiles/                 mage targets from github.com/ttab/mage (sql, docs)
```

## Build & development tools

Go 1.27 (the version in `go.mod`), Docker (tests start their own Postgres,
and the sqlc/tern generators run in a pinned image), and
[mage](https://magefile.org/) for the repository tasks. Never install sqlc or
tern locally: the generated code is a committed artifact and the mage targets
pin the versions that produce it.

```sh
go build -o /dev/null ./...          # compile check; go run ./cmd/user to run
golangci-lint run                    # lint
go test ./...                        # integration tests, each with its own Postgres container
REGENERATE=true go test ./...        # rewrite the golden files after an intended change
go run golang.org/x/vuln/cmd/govulncheck@latest ./...

mage sql:db                          # create the local database and role
mage sql:migrate                     # apply ./schema
mage sql:rollback 3                  # roll back to a schema version
mage sql:generate                    # postgres/queries.sql -> postgres/queries.sql.go
mage sql:dumpSchema                  # refresh postgres/schema.sql from the local database
mage sql:vendorCheck                 # library migrations declared in schema/vendor.json are covered
mage docs:links                      # relative links and anchors in the markdown resolve
```

The protobuf definitions live in `github.com/ttab/elephant-api`, so there is
no code generation for the API here; bumping that module is how a new RPC
arrives.

## Running a local dev instance

The service needs Postgres and an OIDC provider whose tokens it can validate.

```sh
# Once: database and schema.
mage sql:db
mage sql:migrate

# Run. CONN_STRING defaults to the database mage sql:db created; OIDC_CONFIG is
# the discovery URL of the provider that will issue your tokens.
OIDC_CONFIG=https://<provider>/.well-known/openid-configuration go run ./cmd/user run
```

`ttrun.env` holds the environment for running against the team's providers
(`ttrun -- go run ./cmd/user run`). The service starts with no active config
generation: `GET :1081/health/ready` is green with `schemas` reported as
failing, and `UpdateDocument`/`PushInboxMessage` fail until a generation is
registered and activated with a `schema_admin` token — the two seed schemas in
`internal/se.ecms.user.*.json` are the ones to register. `docs/ops.md`
[Common operations](docs/ops.md#common-operations) has the call.

Tests do not touch the local database: `eltest` starts a Postgres container
per test run and migrates a fresh database for each test, and `TestMain`
purges the containers afterwards.

### Resetting a local dev environment

```sh
mage sql:dropDB && mage sql:db && mage sql:migrate
```

Nothing else holds state.

## Configuration reference

All flags belong to the `run` command and read the environment variable in
the second column.

### Server

| Flag | Env | Default | What it does |
|---|---|---|---|
| `--addr` | `ADDR` | `:1080` | Plain HTTP listener: the Twirp APIs, `/health/alive`, `/version`. Serves HTTP/1.1 and HTTP/2. |
| `--profile-addr` | `PROFILE_ADDR` | `:1081` | Internal listener: `/health/ready`, `/metrics`, `/debug/pprof`, `/debug/bom`. Unauthenticated; never expose it. |
| `--tls-addr` | `TLS_ADDR`, `TLS_LISTEN_ADDR` | `:1443` | HTTPS listener, only opened when `--cert-file` is set. |
| `--cert-file` | `TLS_CERT_PATH` | | PEM certificate. Setting it is the switch for the TLS listener. |
| `--key-file` | `TLS_KEY_PATH` | | PEM private key for the certificate. |
| `--cors-host` | `CORS_HOSTS` | | Allowed browser origins, repeatable, wildcards supported. The elephant client's origin goes here. |
| `--log-level` | `LOG_LEVEL` | `debug` | slog level. `debug` logs every RPC; `info` is the production setting. |

### Database

| Flag | Env | Default | What it does |
|---|---|---|---|
| `--db` | `CONN_STRING` | local dev database | Direct Postgres connection. Used for LISTEN/NOTIFY, migrations and, without a bouncer, all queries. Must not point at PgBouncer in transaction-pooling mode: LISTEN is a session-level command and silently never fires through it. |
| `--db-bouncer` | `BOUNCER_CONN_STRING` | | PgBouncer connection string. When set and different from `--db`, all queries go through it and only the LISTEN connection stays direct. |
| `--migrate-db` | `MIGRATE_DB` | `false` | Apply pending migrations at startup. For disposable environments only; production migrations run through elephant-platform, and this flag is slated for removal. |

Neither pool sets `MaxConns`, so the pool size is `max(4, NumCPU)` of the
host. Add `pool_max_conns=<n>` to the connection string to pin it.

### Authentication

Provided by elephantine's `AuthenticationCLIFlags`.

| Flag | Env | What it does |
|---|---|---|
| `--oidc-config` | `OIDC_CONFIG` | OIDC discovery URL. Fetched at startup for the issuer and JWKS; startup fails without it. |
| `--jwt-audience` | `JWT_AUDIENCE` | Required `aud` claim, if the provider sets one. |
| `--jwt-scope-prefix` | `JWT_SCOPE_PREFIX` | Prefix stripped from scope values before matching `user`, `doc_admin`, `schema_admin`, `schema_read`. |
| `--client-id`, `--client-secret` | `CLIENT_ID`, `CLIENT_SECRET` | Confidential client credentials for providers that require them to read the discovery document. |

### Background work

| Flag | Env | Default | What it does |
|---|---|---|---|
| `--cleanup-interval` | `CLEANUP_INTERVAL` | `12h` | How often the retention cleaner deletes system messages older than two weeks and inbox messages older than six months. Runs on one replica at a time under the `cleaner` job lock. The sweep is cheap; hourly is the sensible lower bound. |

## Pending work

**`--migrate-db` should go.** Services in this fleet never migrate their own
schema; the flag came in with the schema work in v1.3.0 and the PR #75 review
asked for it to stay out of production. Removing it also removes
`internal/migrate.go`. Embedding the migrations stays, because the tests and
the platform tooling read them.

**Pool sizing is undecided.** Both pools take pgxpool's default of the host
CPU count, which changes with the node the pod lands on. A number sized for
the workload belongs in `pool_max_conns` on both connection strings; it needs
a look at `pgxpool_empty_acquire_wait_seconds_total` under real load first.

**Connect dual-stack.** The fleet is moving from Twirp to Connect and this
service already carries the shared infrastructure (authentication middleware,
shared RPC metrics, the Twirp error translator). Mounting the Connect handlers
next to the Twirp ones waits for elephant-api to ship the `userconnect`
package; the playbook is elephantine's `docs/migration-service.md`.

**Inbox to orgs and units.** Messages are stored and delivered per recipient
`sub`; a message addressed to a unit or org is stored and reaches nobody. The
redesign — one shared row per broadcast, a per-reader read-state table, a
global inbox id from `sequence_counter` so one cursor spans a reader's whole
owner set, and an idempotency key on push — is planned but not started, and
nothing depends on the inbox API today.

**Notifications over a websocket.** The long-poll transports (`PollMessages`,
`PollInboxMessages`, `PollEventLog`) are meant to be replaced by a websocket
subscription API with an ingest endpoint for producers such as elephant-wires.
The routing model is not decided; the `Messages` RPC shapes are frozen until
that exists.
