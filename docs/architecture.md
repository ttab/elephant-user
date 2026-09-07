# Architecture

How elephant-user is built: the process model, the path a write takes to the
database and out to long-polling clients, each subsystem, and the RPC surface
with the scopes that guard it. Start here before changing the service.

| Document | What it settles |
|---|---|
| [`../README.md`](../README.md) | What the repository holds, how to build and run it, every configuration flag. |
| [`ops.md`](ops.md) | Dependencies, deployment shape, bootstrap order, failure modes and their signals. |
| [`observability.md`](observability.md) | Every metric the service exports and what a change in it means. |

This document does not say what to do when something breaks (that is
`ops.md`) or what a metric means (`observability.md`). It links to the code
by file, not by line; the code is the mechanism, this is what follows from it.

## Process model

One binary, `cmd/user`, one `run` command. Startup in `cmd/user/main.go` is
strictly ordered, and everything after the pools is wired through
`internal.Run` in `internal/app.go`:

```
main.go
  pubsubPool  = pgxpool.New(CONN_STRING)           direct connection: LISTEN needs a session
  dbpool      = pubsubPool, or pgxpool.New(BOUNCER_CONN_STRING) if set
  pool metrics registered ("main", and "pubsub" when they differ)
  [--migrate-db] internal.Migrate(pubsubPool)      disposable environments only, see ops.md
  auth        = OIDC discovery + JWKS from OIDC_CONFIG
  metrics     = internal.NewMetrics(DefaultRegisterer)
  store       = internal.NewPGStore(dbpool)         FanOuts for the five NOTIFY channels
  validator   = internal.NewValidator(store)        loads active schemas, or fails startup
                └─ go reloadLoop                    NOTIFY-driven, 5 min recheck
  subscriber  = store.NewSubscriber(pubsubPool)     one LISTEN connection, feeds the FanOuts
  server      = elephantine.NewAPIServer(...)       readiness: postgres (required), schemas (optional)

internal.Run: elephantine.NewErrGroup
  Required      "server"   APIServer.ListenAndServe(grace.CancelOnQuit)
  GoWithRetries "pubsub"   subscriber.Run(grace.CancelOnStop)      5 s backoff, unlimited
  Go            "cleaner"  joblock.Run("cleaner") → sweep, then ticker(CLEANUP_INTERVAL) → sweep
```

**A failing background task stops the service; a stopping one does not.**
An error from the cleaner cancels the group and the process exits, to be
restarted, rather than serving without retention. The subscriber is the
exception: elephantine's `Subscriber.Run` reconnects by itself on ping
timeouts but returns on other connection errors, such as a database failover
resetting the LISTEN connection, and taking every replica down at that moment
would extend the outage — so it is restarted in place with a 5 s backoff,
counted in `task_restarts_total{task="pubsub"}`. Only the server is
`Required`: its exit ends everything.

The distinction matters at shutdown. On SIGTERM the subscriber and cleaner
stop at once (`CancelOnStop`, which releases the job lock so another replica
can take it), while the server keeps answering in-flight requests until the
10 s quit deadline (`CancelOnQuit`). A `Required` task cancels the group on
*any* return, nil included, and the server's quit context is a child of the
group context — so if the background tasks were `Required`, their clean exit
at stop would shut the server down at stop time and the drain window would be
lost. `stopScoped` in `internal/app.go` wraps both background tasks: it hands
them the stop context and turns a return caused by the stop into a clean nil,
so a normal shutdown exits 0.

The validator's reload loop is not in the err-group; it is a goroutine owned by
the validator, stopped through `Validator.Stop`. It reads schemas from the
store and never writes, so its failure mode is staleness, logged and counted,
not a crashed process.

There is no leader election beyond the cleaner's job lock. Every replica
serves every RPC, LISTENs on its own connection and reloads its own
validator; replicas scale reads and long-polls linearly.

## Data flow

### Settings documents

`UpdateDocument` (`internal/service.go`) is the write path with the most
rules:

1. **Scope** `user`. The target owner defaults to the caller's `sub`. Writing
   to another owner needs the `doc_admin` scope **and** membership: the owner
   must equal the caller's `org` claim or be in its `units` claim
   (`isAllowedOwner`). A `user` without `doc_admin` cannot write shared docs
   even for its own org.
2. **Validation** against the `settings` validator (see
   [Schemas and config generations](#schemas-and-config-generations)). The
   payload's UUID is overwritten with the nil UUID before validation because
   settings documents have no identity of their own; the key is
   `(owner, application, type, key)`. Validation errors are returned as
   `invalid_argument` with `err_count` and numbered meta entries.
3. **One transaction** (`PGStore.UpdateDocument`, `internal/store.go`):
   upsert the owner into `user` (the FK target), upsert the `document` row
   with `version = version + 1`, then `logAndNotify`: reserve an eventlog id,
   insert the `eventlog` row, `pg_notify('event_log_update')`. Commit.

**The document row and the eventlog entry that announces it are written in
the same transaction, and the NOTIFY is queued inside it**, so a listener is
woken only for a committed, visible row. Reads (`GetDocument`,
`ListDocuments`) span all of the caller's owners — `sub`, `org`, `units` —
and mark a document `read_only` when the caller is not its owner and lacks
`doc_admin`.

`UpdateDocument` is last-writer-wins: there is no expected-version in the
request and the response carries no version. Two tabs editing the same doc
converge through `PollEventLog`, not through conflict detection. That is
acceptable while documents are mostly per-user; for shared org and unit
documents edited by several admins an `if_match_version` field would turn the
race into a deterministic conflict, and is the change to make if that bites.

### Properties

`SetProperties`/`GetProperties`/`DeleteProperties` are flat key-value pairs
keyed `(owner, application, key)`, **private to the caller**: the owner is
always the token's `sub`, there is no shared model and no validation.
`SetProperties` writes all keys in one transaction: the rows are locked in
sorted `(application, key)` order, and the eventlog entries for all of them
are written **after** the last property row, with their ids reserved in one
call. Both orderings matter and are explained under
[Ids that tail correctly](#ids-that-tail-correctly).

### The eventlog and `PollEventLog`

`eventlog` is the change stream over documents and properties: one row per
update or delete, carrying owner, kind, application, type, key, version and
who did it — never the payload (the column exists and is always null).
`PollEventLog` is how the elephant client keeps its settings in sync:

```
PollEventLog(after_id)
  owners = sub + org + units
  register FanOut listener: owner ∈ owners && id > after_id        (goroutine, see below)
  after_id == -1 → after_id = MAX(id) over owners                   "start from now"
  read up to 10 entries with id > after_id
  if any: return them, last_id = max id seen
  else: wait for a notification, 30 s, or the client going away
        re-read, return whatever is there (possibly nothing), last_id
```

**The notification is only a wakeup.** Its payload (`{id, owner}`) is used to
filter which pollers to wake and is never trusted as data; the handler always
re-reads the table. That is why notifications are allowed to be lossy: the
FanOut hands each listener a buffer of one and drops on overflow, and a
dropped wakeup costs at most one 30-second wait.

The listener is registered from a goroutine (`go store.OnEventLogUpdate`),
so a commit landing between the initial read and the registration is not
signalled and the poll waits out its timeout. This is a latency-only window,
elephant-repository uses the identical shape, and the integration tests
carry 50 ms sleeps around long-poll triggers because of it.

### Ids that tail correctly

Every stream a client tails with `after_id` needs ids whose order equals
commit order; otherwise a slow transaction can commit id 10 after a fast one
committed id 11, the client's cursor has moved past 10, and 10 is never
delivered. Two mechanisms, same principle:

- **Eventlog ids** come from `sequence_counter` (`schema/004_sequence_counter.sql`):
  `UPDATE sequence_counter SET value = value + @count WHERE name = 'eventlog'
  RETURNING value`, executed inside the writing transaction. The `UPDATE`
  row-locks the counter until commit, so **no writer can obtain its id until
  the previous writer has committed or rolled back** — id 11 is provably
  committed after id 10 is visible, and a rollback hands the number back.
  `created` is stamped after the counter is taken, so timestamp order matches
  id order too.
- **Message ids** are per recipient, from `message_write_lock`: one atomic
  `INSERT ... ON CONFLICT DO UPDATE SET current_message_id = current + 1
  RETURNING`, which creates the row on first use and row-locks it for the
  rest of the transaction (`nextMessageID`, `internal/store.go`). Gapless and
  commit-ordered per recipient. The retention cleaner leaves the lock rows
  alone, so ids never restart after old messages are deleted.

**The counter must be the last lock a transaction acquires.** It is shared by
every eventlog writer, so a transaction holding it while it goes on to lock
data rows deadlocks against any writer holding one of those rows while
waiting for the counter. `SetProperties` and `DeleteProperties` therefore do
all their row work first and reserve the whole block of ids at the tail;
`logAndNotifyAll` documents the rule.

#### The identity column, and why it was replaced

Until v1.4.0 `eventlog.id` was `generated always as identity`. Identity values
come from a Postgres sequence, which hands out numbers at insert time and
never blocks: tx A took 10, tx B took 11 and committed first, `PollEventLog`
returned 11, A committed, and 10 was below every cursor forever. The same
race sat on the `after_id == -1` bootstrap. It bit only across *different*
documents in one reader's owner set (same-doc writes were serialised by the
document row lock), so it was rare and silent: a client that was simply stale.
Migration 004 replaced the identity with the counter; that migration needs a
service window because old and new code cannot share a schema (see
[`ops.md`](ops.md#migrations)). Do not reintroduce a sequence or identity for
any id a client tails.

The first version of the counter fix reserved ids inside the property loop and
deadlocked against single-key writers; the tail-of-transaction rule above is
the fix, and `TestConcurrentPropertyWrites` in `internal/concurrency_test.go`
fails on either ordering mistake.

### Inbox messages

Durable, per-recipient newsdoc documents with an `is_read` flag, six months of
retention. `PushInboxMessage` requires the `user` scope, a `recipient` and a
payload that validates against the `messages` validator; the caller's `sub`
is recorded as `created_by`, so a sender cannot impersonate anyone. Any
`user`-scoped token can push to any recipient. The write is one transaction:
upsert the recipient into `user`, take the next id from the recipient's lock
row, insert, `pg_notify('inbox_message_update')`.

Reads are for the caller only: `PollInboxMessages(after_id)` (same shape as
`PollEventLog`, recipient = `sub`, limit 10), `ListInboxMessages(before_id,
size)` (keyset pagination on `id DESC`, default 10, `size` uncapped),
`UpdateInboxMessage(id, is_read)` and `DeleteInboxMessage(id)`. A message
pushed to `core://unit/x` is stored but delivered to nobody — delivery is by
the token's `sub` only. The inbox API has no production callers today; its
data model is free to change, and the plan to make it org- and unit-addressed
is the next feature workstream.

### System messages

Ephemeral key-value payloads with a two-week retention: `PushMessage(recipient,
type, doc_uuid, doc_type, payload map)` and `PollMessages(after_id)`. No
validation — the elephant client's BFF pushes `type: "rpc_error"` toasts and
the clients poll them. The RPC shapes are frozen until the notification
transport that replaces them exists; do not extend them.

### Retention cleaner

`RunCleaner` (`internal/store.go`) runs under `joblock.Run` with lock name
`cleaner`, so one replica at a time owns it. **The first sweep runs as soon as
the lock is acquired**, then a ticker fires every `CLEANUP_INTERVAL` (default
12 h); each sweep deletes `message` rows older than two weeks and
`inbox_message` rows older than six months. Sweeping on acquisition is what
makes the interval a ceiling rather than a minimum: a lock that changes hands
on every deploy would otherwise restart the countdown each time and never
reach a tick. A failed sweep is logged and left for the next tick without
releasing the lock; the callback returns only when its context is cancelled
(shutdown or lock loss), and returns nil then, because the job lock counts a
returned error as a failed run. The job lock library paces restarts, recovers
panics and reports `pg_job_lock_*` metrics; the contract and the pacing are in
elephantine's `docs/joblock-restart-semantics.md`.

## Real-time plumbing

Five Postgres NOTIFY channels, one `pg.FanOut` each on `PGStore`, one LISTEN
connection per replica (`pg.Subscriber` on the direct pool):

| Channel | Published by | Woken consumers |
|---|---|---|
| `message_update` | `InsertMessage` | `PollMessages` handlers for that recipient |
| `inbox_message_update` | `InsertInboxMessage` | `PollInboxMessages` handlers for that recipient |
| `event_log_update` | every eventlog writer | `PollEventLog` handlers whose owner set contains the row's owner |
| `schema_update` | generation activation | validator reload loop, `GetActiveConfigGeneration` long-polls |
| `deprecation_update` | `UpdateDeprecation` | validator reload loop |

`pg_notify` is always issued inside the writing transaction; Postgres delivers
it at commit and discards it on rollback. **LISTEN is a session-level command
and does not survive PgBouncer's transaction pooling**, which is why the
subscriber is bound to the direct pool while queries may go through the
bouncer. The subscriber pings itself every 5 minutes and reconnects after
7 minutes of silence; a fully dead connection is therefore self-healing, and
consumers never depend on notifications for correctness — every poller
re-reads, and the validator rechecks every 5 minutes.

The FanOut recovery machinery elephantine offers (bouncing a LISTEN that is
alive but not delivering, after consecutive polls that found unannounced
work) is deliberately not wired: it needs a consumer with continuous writes
to accumulate a streak, and schema changes happen a few times a month.

## Schemas and config generations

Settings documents and inbox messages are validated against
[revisor](https://github.com/ttab/revisor) constraint sets stored in Postgres
and managed at runtime through the `Configuration` service. Tables
(`schema/003_config_generations.sql`):

| Table | Holds |
|---|---|
| `document_schema` | every schema version ever registered, `(name, version) → spec, usage` |
| `config_generation` | a named set of schemas that are active together; `identity_hash` makes registration idempotent; a partial unique index allows exactly one `active` row |
| `config_generation_schema` | which `(name, version)` each generation includes, one version per name |
| `deprecation` | `label → enforced` |

Each schema declares a **usage**, `settings` or `messages`, and the validator
builds one `revisor.Validator` per usage from the active generation. A
settings type therefore cannot validate as an inbox message or vice versa;
before v1.3.0 one shared validator accepted either.

`RegisterConfigGeneration` (scope `schema_admin`) validates every spec,
dry-runs building a validator per usage so a generation that cannot be built
is never stored, hashes the `(name, version)` set and returns the existing
generation when the hash matches. Registering and activating are separate
steps unless `activate` is set, so a diff can be inspected first.
`ActivateConfigGeneration` flips the single active row and publishes
`schema_update`. `GetActiveConfigGeneration(known_id, wait_seconds,
only_changed)` is a long-poll of at most 10 s for remote consumers that want
to live-refresh.

The validator (`internal/validator.go`) loads the active schemas and the
enforced deprecations at startup — **startup fails if the active generation
cannot be loaded**, but an environment with no active generation starts and
serves: validated writes fail with an internal error until one is activated,
and the optional `schemas` readiness entry reports it without failing the
probe, so a fresh deployment can be seeded through its own API. Reloads are
triggered by either NOTIFY channel, by a 5-minute recheck, or by
`RefreshSchemas` (tests); a failed reload keeps the previous validators and
is logged with a count key.

**Deprecations** are revisor labels toggled per label: an unenforced
deprecation is logged with the document UUID and counted in
`elephant_user_deprecations_total{label}` and
`elephant_user_docs_with_deprecations_total{doc_type}`; an enforced one turns
the use into a validation error. The counters are how you learn that no
client sends a deprecated construct any more, which is when enforcing it is
safe.

The two embedded constraint sets, `internal/se.ecms.user.settings.json` and
`internal/se.ecms.user.messages.json`, are seed fixtures: tests register them
as generation 1, and a new environment registers them through the API. The
service never seeds itself.

## Twirp APIs

All RPCs are `POST /twirp/elephant.user.<Service>/<Method>`, protobuf or
JSON, defined in `github.com/ttab/elephant-api/user`. Every request carries a
bearer JWT; authentication is HTTP middleware in front of every handler, and a
missing or invalid token is `unauthenticated` (401). Each handler then checks
its scope explicitly — an identified caller without the scope gets
`permission_denied` (403) with the accepted scopes in the
`required_any_of_scopes` error meta.

| Service | RPCs | Scope |
|---|---|---|
| `Settings` | `GetDocument`, `ListDocuments`, `UpdateDocument`, `DeleteDocument`, `GetProperties`, `SetProperties`, `DeleteProperties`, `PollEventLog` | `user`; writing a document owned by an org or unit also needs `doc_admin` and membership |
| `Messages` | `PushMessage`, `PollMessages`, `PushInboxMessage`, `PollInboxMessages`, `ListInboxMessages`, `UpdateInboxMessage`, `DeleteInboxMessage` | `user` |
| `Configuration` | `RegisterConfigGeneration`, `ActivateConfigGeneration`, `UpdateDeprecation` | `schema_admin` |
| `Configuration` | `GetActiveConfigGeneration`, `ListConfigGenerations`, `GetSchema`, `GetDeprecations` | `schema_admin` or `schema_read` |

Identity comes from the token only: `sub` is the user, `org` a single
organisation URI, `units` a list of unit URIs, all in the `core://user/…`,
`core://org/…`, `core://unit/…` form that owners and recipients use. **The
service has no membership directory**: it never learns who is in an org or a
unit, only what each caller's own token claims. Anything that looks like
group delivery must be read-side (a shared row matched against the reader's
claims at poll time, as `PollEventLog` does), never push-time fan-out to
enumerated members.

Errors are Twirp errors today; scope checks already return the
protocol-neutral `*connect.Error` from `elephantine/rpc`, which the Twirp
mount translates on the way out. The fleet is moving to Connect alongside
Twirp; the step-by-step is elephantine's `docs/migration-service.md`, and it
starts for this service when elephant-api ships the `userconnect` package.
