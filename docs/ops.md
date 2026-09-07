# elephant-user — operations

For whoever is holding the pager or bringing up an environment: what the
service depends on, how it is deployed, what its data flows cost when they
stop, the order things have to happen in, and the failure modes with the
signal that shows each one. It does not explain how the code is built
([`architecture.md`](architecture.md)) or define the metrics
([`observability.md`](observability.md)).

| Document | What it settles |
|---|---|
| [`../README.md`](../README.md) | What the repository holds, how to build and run it, every configuration flag. |
| [`architecture.md`](architecture.md) | How the service is built: process model, data flow, subsystems, RPC surface and scopes. |
| [`observability.md`](observability.md) | Every metric the service exports and what a change in it means. |

## What the service is

One process with three halves that fail independently:

- **The API** — Twirp services for settings documents, key-value properties,
  system messages, inbox messages and schema configuration, all against
  Postgres. Stateless; every replica serves everything.
- **The LISTEN connection** — one per replica, on a direct Postgres
  connection, waking long-poll handlers and the schema validator. Without it
  the API keeps working but every long-poll takes its full timeout and a
  schema change takes up to five minutes to reach the replica.
- **The retention cleaner** — one replica at a time, under a job lock,
  deleting old messages twice a day. Without it nothing is lost; tables grow.

A cleaner failure stops the process, which is restarted rather than left
serving without retention. The LISTEN connection is restarted in place with a
5 s backoff when it fails, so a database blip does not take every replica
down at once.

## Components

| Repository | What it is to us |
|---|---|
| `ttab/elephant-user` | this service |
| `ttab/elephant-api` | the protobuf definitions and generated Twirp code for the `user` package; the elephant client's TypeScript client is generated from the same source |
| `ttab/elephantine` | the shared framework: API server, auth middleware, job lock, LISTEN subscriber, metrics, graceful shutdown |
| `ttab/revisor` | the newsdoc schema validator the settings and message documents are checked with |
| `ttab/elephant-platform` | `setup db migrate`, the tool that applies this repository's migrations in hosted environments |
| Keycloak | the OIDC provider that issues the JWTs and defines the scopes |

## Deployment shape

| Role | Configuration | Runs |
|---|---|---|
| API replica | `CONN_STRING` (direct), optional `BOUNCER_CONN_STRING`, `OIDC_CONFIG`, `CORS_HOSTS`, TLS paths | the API, its own LISTEN connection, and a candidate for the cleaner lock |

There is one role. Replicas scale RPC throughput and long-poll fan-out; each
replica adds one LISTEN session to the direct database. The cleaner does not
scale — exactly one replica holds the `cleaner` lock and the rest wait — and
does not need to.

## Runtime dependencies

| Dependency | Needed for | Without it |
|---|---|---|
| Postgres, direct connection (`CONN_STRING`) | LISTEN/NOTIFY, migrations, and all queries unless a bouncer is configured | startup fails on the initial ping; at runtime the `postgres` readiness check fails and the replica leaves the load balancer |
| Postgres via PgBouncer (`BOUNCER_CONN_STRING`, optional) | all queries when configured | same as above for queries; LISTEN is unaffected because it never goes through the bouncer |
| OIDC provider (`OIDC_CONFIG`) | discovery document and JWKS at startup, key refresh afterwards | startup fails; a running replica keeps validating with cached keys until a key rotates |
| An active config generation | validating `UpdateDocument` and `PushInboxMessage` | those two RPCs fail with an internal error; everything else works; readiness reports `schemas` as failing but stays green |

Truly required to start: Postgres and the OIDC provider. Truly required to
serve every RPC: an active config generation.

## Endpoints and ports

| Port | Default | What is on it |
|---|---|---|
| API | `:1080` (`ADDR`) | `POST /twirp/elephant.user.{Settings,Messages,Configuration}/<Method>`, `GET /health/alive`, `GET /version` |
| API, TLS | `:1443` (`TLS_ADDR`), only when `TLS_CERT_PATH` is set | the same, over TLS |
| Profile | `:1081` (`PROFILE_ADDR`) | `GET /health/ready`, `GET /metrics`, `/debug/pprof/*`, `/debug/vars`, `/debug/bom` |

The plain listener speaks HTTP/1.1 and HTTP/2. The profile port is internal
and unauthenticated; it must not be exposed.

## Data flows

### 1. A settings write, and the client that sees it

```
elephant client ── UpdateDocument ──► replica A
                                       │ scope user (+ doc_admin for shared owner)
                                       │ validate against the settings schema
                                       │ BEGIN
                                       │   upsert user, upsert document (version+1)
                                       │   UPDATE sequence_counter 'eventlog' +1 RETURNING  ← row lock
                                       │   INSERT eventlog (id, owner, kind, key, version)
                                       │   pg_notify('event_log_update', {id, owner})
                                       │ COMMIT ── notification delivered to every LISTENer
                                       ▼
   Postgres ──NOTIFY──► replica B's subscriber ──► FanOut ──► the PollEventLog handler
                                                              whose owner set contains the owner
                                                              and whose cursor is below the id
                                                              └─ re-reads eventlog, returns entries
```

The operational weight is on the counter row: every settings and property
write in the whole system serialises on it for the tail of its transaction.
That is microseconds at this service's volume, and it is what makes the
`after_id` cursor safe. If writes ever back up, this lock is the first thing
to look at in `pg_stat_activity`.

The notification is a wakeup only. Replica B re-reads before answering, so a
lost notification costs one 30-second wait and nothing else.

### 2. An inbox push

```
BFF / service token ── PushInboxMessage ──► replica
                                              │ scope user; validate against the messages schema
                                              │ BEGIN
                                              │   upsert recipient into user
                                              │   INSERT message_write_lock ... ON CONFLICT DO UPDATE +1 RETURNING  ← per-recipient lock
                                              │   INSERT inbox_message (recipient, id, payload)
                                              │   pg_notify('inbox_message_update', {id, recipient})
                                              │ COMMIT
```

Ids are per recipient, so a burst to many recipients does not contend. A
retried push after a timeout is a duplicate — there is no idempotency key —
which matters for durable inbox messages and not for ephemeral system
messages.

### 3. Schema activation

```
operator (schema_admin) ── RegisterConfigGeneration(schemas, activate) ──► any replica
                                                                            │ dry-run build a validator per usage
                                                                            │ store schemas + generation (idempotent on the set)
                                                                            │ if activate: flip the single active row
                                                                            │ pg_notify('schema_update')
   every replica's validator reload loop ── wakes ── loads active schemas ── swaps validators
                                          (or the 5-minute recheck, if the notification was missed)
```

Activation is atomic across the fleet in the sense that every replica swaps
to the same generation; it is not simultaneous. For up to five minutes, in
the worst case, two replicas can validate against different generations.

### 4. Retention

```
every CLEANUP_INTERVAL (12h), on the replica holding job lock "cleaner":
  DELETE FROM message        WHERE created < now() - 2 weeks
  DELETE FROM inbox_message  WHERE created < now() - 6 months
```

The first sweep runs as soon as a replica acquires the lock, so a deploy
never postpones retention. `message_write_lock` rows are never deleted, so a
recipient's ids keep counting up after their old messages are gone. Consumers should not treat
`after_id` gaps at the low end as missing messages.

## Single-leader work

| Lock | Does | When nobody holds it |
|---|---|---|
| `cleaner` | the retention sweep above | tables grow; nothing is lost; `sum(pg_job_lock_held{name="cleaner"})` is 0 |

## Where state lives

Everything is in Postgres and Postgres is authoritative for all of it.

| Table | Holds | Notes |
|---|---|---|
| `user` | every owner or recipient ever seen, with its kind | FK target; rows are never deleted |
| `document`, `property` | settings state | current version only, no history |
| `eventlog` | the change stream over documents and properties | ids from `sequence_counter`; never contains payloads |
| `sequence_counter` | the `eventlog` id counter | one row; must equal `MAX(eventlog.id)` after migration |
| `message`, `inbox_message` | messages by `(recipient, id)` | retention 2 weeks / 6 months |
| `message_write_lock` | per-recipient id counters | never cleaned |
| `document_schema`, `config_generation`, `config_generation_schema`, `deprecation` | schema configuration | exactly one generation is active |
| `job_lock` | the cleaner lock | rows are transient |
| `schema_version` | tern's migration marker | |

Nothing is cached outside the process except the validators, which are
rebuilt from `document_schema` on every reload.

## Migrations

Migrations live in `schema/` as tern files and are applied by elephant-platform
(`go run ./cmd/setup db migrate`) in hosted environments and by `mage
sql:migrate` locally. **The service must not migrate its own schema at
startup.** The `--migrate-db` flag exists for disposable environments and is
kept for now; do not set it in production.

`schema/004_sequence_counter.sql` (v1.4.0) is the one migration that needs a
service window, because old and new code cannot share the schema in either
direction: old code inserts without an id and fails on the new table, new
code inserts an explicit id and fails on the old identity column.

```
1. scale the deployment to 0
2. go run ./cmd/setup db migrate                       (elephant-platform)
3. gate:
     SELECT version FROM schema_version;               -- 4
     SELECT value = (SELECT COALESCE(MAX(id), 0) FROM eventlog)
     FROM sequence_counter WHERE name = 'eventlog';    -- true
4. deploy the new version, scale up
5. smoke-test: one settings write, one PollEventLog
```

The gate in step 3 exists because a counter seeded below the real maximum
wedges every eventlog write permanently (each one retries the same duplicate
id). The migration takes the table lock before seeding, so it cannot happen
when nothing is writing; the check is belt and braces. Rollback (`mage
sql:rollback 3` locally, the platform equivalent hosted) needs a window for
the same reason. Rehearse both directions on staging in one session.

The `job_lock` table is elephantine's; `schema/vendor.json` declares that and
`001_messages.sql` asserts it created the table by hand
(`-- covers:`). `go test ./schema/` fails when elephantine ships a migration
this service has not taken.

## Bootstrap order — read this before starting a new environment

1. Postgres exists and the migrations are applied (step 2 above, or `mage
   sql:db && mage sql:migrate` locally).
2. Keycloak knows the scopes: `user`, `doc_admin`, `schema_admin`,
   `schema_read`.
3. Deploy. The service starts with no active generation; `/health/ready` is
   green with `schemas` reported as failing.
4. Register and activate the initial generation with a `schema_admin` token:
   `RegisterConfigGeneration` with the two seed schemas
   `se.ecms.user.settings@v1.0.0` and `se.ecms.user.messages@v1.0.0` (the
   specs are the embedded files in `internal/`) and `activate: true`. See
   [Common operations](#common-operations).
5. Route traffic.

Out of order: traffic before step 4 means every `UpdateDocument` and
`PushInboxMessage` fails with an internal error until the generation is
active. Nothing is lost — the writes are refused, not dropped — but the
elephant client shows failed saves.

## Failure modes

### Settings saves fail with an internal error, everything else works

`UpdateDocument` and `PushInboxMessage` return `internal` and the log says
`no active schema for usage "settings"` (or `"messages"`). No generation is
active, or the active one lacks a usage.

Signal: `health_check_up{name="schemas"} == 0`; `rpc_responses_total{method="UpdateDocument",status="500"}`.

Action: `GetActiveConfigGeneration` to see what is active;
`ListConfigGenerations` to find the intended one; `ActivateConfigGeneration`
or register a generation that covers both usages.

### A settings change does not show up in another tab for 30 seconds

Long-polls are returning on timeout instead of on notification. Either the
replica's LISTEN connection is down (the subscriber logs `listener ping
timeout, reconnecting` and recovers by itself within 7 minutes), or the
subscriber is going through PgBouncer in transaction-pooling mode, where
LISTEN never works.

Signal: `rpc_duration_seconds{method="PollEventLog"}` p50 pinned at 30 s
across all pollers while writes are happening; `pg_job_lock_transitions_total`
churning at the same time points at the database connection rather than the
listener.

Action: check that `CONN_STRING` is a direct connection and not the bouncer's.
If it is, restart the replica; if the ping timeouts recur, the network path
between the replica and Postgres is dropping idle connections.

### A schema was activated and one replica still rejects the new type

The replica missed the `schema_update` notification and has not hit its
5-minute recheck yet. Expected for up to five minutes; a problem after that.

Signal: `elephant_user_schema_refresh_failure_count` in the logs for that
replica means the reload runs and fails (the generation cannot be loaded);
nothing in the logs means the reload never ran, which is the LISTEN failure
above.

Action: wait out five minutes, then treat as the LISTEN failure. If reloads
fail, the generation itself is broken; the dry-run at registration should
have prevented it, so read the error — it names the schema.

### `/health/ready` is red, `postgres` failing

The replica cannot ping the database it runs queries against. It has left the
load balancer, which is correct.

Signal: `health_check_up{name="postgres"} == 0`; `pgxpool_canceled_acquires_total` rising just before.

Action: this is Postgres or the bouncer, not the service. If only one replica
shows it, its node's network.

### Readiness is green but the pod keeps restarting

The cleaner task returned an error (the job lock could not be created or gave
up), or the API server itself failed to listen. The process exits by design so
the failure is loud.

Signal: pod restart count; the last log line before exit, `failed to run
application`, names the task. `task_restarts_total` does not move for these:
it counts only the subscriber, which is restarted in place.

Action: read that log line. `run cleaner job lock` errors are the job lock's
ping or acquire failing against the database.

### Long-polls time out in bursts and `task_restarts_total{task="pubsub"}` climbs

The LISTEN connection keeps failing with an error the subscriber does not
reconnect from by itself (a reset connection rather than a silent one), and
the err-group restarts it every 5 s. Between restarts, pollers on this replica
wait out their timeouts. Usually a database failover or restart in progress.

Signal: `task_restarts_total{task="pubsub"}` rate; `listener` errors in the
log; `pgxpool_new_conns_total` spiking at the same time.

Action: none if it stops within a minute of the database recovering. If it
continues, the direct connection string points at something that keeps
dropping sessions.

### Every write is failing with a duplicate key on `eventlog_pkey`

The `sequence_counter` value is below `MAX(eventlog.id)`. This can only
happen if a migration was run against a live database without the window, or
someone edited the counter. It is permanent until fixed: each write retries
the same duplicate id.

Signal: `rpc_responses_total{status="500"}` for `UpdateDocument`,
`SetProperties`, `DeleteDocument`, `DeleteProperties` all at once; logs say
`duplicate key value violates unique constraint "eventlog_pkey"`.

Action:

```sql
UPDATE sequence_counter SET value = (SELECT COALESCE(MAX(id), 0) FROM eventlog)
WHERE name = 'eventlog';
```

### A client gets 401 where it used to get 403

Since v1.4.0 an invalid or missing token is `unauthenticated` (401);
`permission_denied` (403) is reserved for a valid token without the scope.
An ingress rule, dashboard or client retry policy keyed on 403 for bad tokens
reads 401 now. Not a fault; a contract change to update the consumer for.

### Two admins edited the same shared document and one edit vanished

`UpdateDocument` is last-writer-wins with no version check. The second save
overwrote the first; the eventlog shows both versions. Known limitation; an
`if_match_version` request field is the fix if shared-document editing grows.

## What to watch, in order

1. `rpc_responses_total{status="500"}` rate — the service's own failures;
   everything in the catalogue above shows up here first.
2. `health_check_up{name="schemas"}` — 0 means validated writes are failing
   while the pod looks healthy.
3. `sum(pg_job_lock_held{name="cleaner"})` — must be 1; 0 for long means the
   cleaner is not running anywhere.
4. `rpc_duration_seconds{method=~"Poll.*"}` p50 against the 30 s timeout —
   pinned at the timeout while writes flow means notifications are not
   arriving; `task_restarts_total{task="pubsub"}` says whether the subscriber
   is fighting to reconnect.
5. `pgxpool_empty_acquire_wait_seconds_total{pool="main"}` — connection
   queuing; the pool is sized by CPU count, not by decision, so this is the
   number that says the default is wrong.

## Common operations

**Register and activate the seed schemas.** Needs a token with `schema_admin`.
The request is a `RegisterConfigGenerationRequest` with the two embedded
constraint sets as `spec` strings, `usage` set to `SCHEMA_USAGE_SETTINGS` and
`SCHEMA_USAGE_MESSAGES`, and `activate: true`:

```sh
curl -sS -X POST "$USER_API/twirp/elephant.user.Configuration/RegisterConfigGeneration" \
  -H "Authorization: Bearer $TOKEN" -H "Content-Type: application/json" \
  -d @generation.json
```

Registering the same set again returns the existing generation; it is safe to
re-run.

**See what is active.** `GetActiveConfigGeneration` with an empty request
(`schema_read` suffices); `ListConfigGenerations` for the history.

**Enforce a deprecation.** Check `elephant_user_deprecations_total{label}` has
been flat for as long as you need, then `UpdateDeprecation{label, enforced:
true}`. Uses of the construct become validation errors on every replica
within seconds (notification) or five minutes (recheck).

**Run a migration.** See [Migrations](#migrations); 004 needs a window.

**Change how often retention runs.** `CLEANUP_INTERVAL` (a Go duration,
default `12h`). The sweep is cheap; there is no reason to make it more
frequent than hourly.

## Security

Inbound: every RPC needs a bearer JWT from the configured OIDC provider.
Scopes and what they grant:

| Scope | Grants |
|---|---|
| `user` | all `Settings` and `Messages` RPCs on the caller's own data; reading shared docs the caller's org or units own; pushing messages to any recipient |
| `doc_admin` | writing settings documents owned by an org or unit the caller belongs to |
| `schema_admin` | registering and activating config generations, toggling deprecations |
| `schema_read` | reading generations, schemas and deprecations |

A `schema_admin` token also administers schemas on elephant-repository; the
scope is shared by design. Identity is claims only — the service has no
directory and trusts `org` and `units` as the token states them. Pushed
messages record the token's `sub` as `created_by`, so a sender cannot
impersonate another sender, but any `user` token can push to any recipient.

Outbound: the OIDC discovery document and JWKS at startup and on key
rotation. Nothing else leaves the process. Secrets are the database
connection strings and, if a confidential client is configured, the
OIDC client secret; all arrive as environment variables. The profile port is
unauthenticated and internal only.

## Not in place yet

- **`--migrate-db` still exists.** It contradicts the rule that services do
  not migrate themselves and is slated for removal; until then it must stay
  off in production.
- **Pool size is the CPU-count default.** `pgxpool` picks
  `max(4, NumCPU)` from the cpuset, which tracks the node, so the pool changes
  size on reschedule. Set `pool_max_conns` in the connection strings once a
  workload-based number is agreed.
- **No idempotency on pushes.** A retried `PushInboxMessage` duplicates.
  Planned with the inbox redesign.
- **Connect is not mounted yet.** Only `/twirp/` paths exist; the dual-stack
  mount waits for elephant-api to ship the `userconnect` package.
