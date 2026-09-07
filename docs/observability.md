# Observability

Every metric elephant-user exports and what a change in it means. How to read
them together during an incident is in [`ops.md`](ops.md#what-to-watch-in-order);
what the subsystems are is in [`architecture.md`](architecture.md).

The service declares its own collectors in one place, `internal/metrics.go`
(`NewMetrics(reg)`), and `TestMetricsLint` runs the Prometheus linter over the
full set. Everything else comes from elephantine and keeps that library's
names; the service registers its pool collectors where the pools are created
(`cmd/user/main.go`) and passes the registerer to the job lock, the RPC hooks
and the err-group. All of it is served on `GET /metrics` on the profile port
(default `:1081`).

Names are prefixed `elephant_user_` rather than the convention's `user_`. They
are live in production, and renaming a metric breaks dashboards, so they stay.
New metrics keep the existing prefix for consistency.

## Schema validation

The pair to watch: `elephant_user_deprecations_total` by `label` against the
list in `GetDeprecations`. A label that has stopped counting is one whose
deprecation can be enforced; a label that keeps counting after being enforced
means a client is now being refused.

- `elephant_user_deprecations_total{label}` — uses of a deprecated schema
  construct that were allowed because the label is not enforced. Rising is not
  wrong by itself; it names the clients that have not moved. Enforced labels
  do not count here: their uses become validation errors and show up as
  `invalid_argument` responses instead.
- `elephant_user_docs_with_deprecations_total{doc_type}` — documents that
  used at least one deprecated construct, by document type. The same signal
  grouped by what is being written rather than by which rule it broke.

Validator reload failures are not a metric. They are logged at error level
with the elephantine count keys `elephant_user_schema_refresh_failure_count`
and `elephant_user_deprecation_refresh_failure_count`, which the log pipeline
turns into counts. A run of them means the active generation cannot be loaded
and the replica is validating against the previous one.

## RPC

From elephantine; identical labels on every service in the fleet. `service`
is the short name (`Settings`, `Messages`, `Configuration`), `method` the RPC,
`customer` the empty string (this service runs in an isolated environment per
customer and does not label by org).

- `rpc_requests_total{service,method,customer}` — calls that reached a
  handler. A refused authentication is not counted here.
- `rpc_responses_total{service,method,status,customer}` — every response,
  including the ones the authentication middleware refuses. `status="401"`
  growing without a deploy is a client with an expired or wrong token;
  `status="403"` is a client without the scope; `status="500"` is ours.
  Long-polls answer `200` on timeout as well as on data, so their volume is
  driven by client count, not by writes.
- `rpc_duration_seconds{service,method,customer}` — histogram. `PollEventLog`,
  `PollMessages`, `PollInboxMessages` and `GetActiveConfigGeneration` are
  long-polls and legitimately sit at their timeouts (30 s and 10 s); read
  their p50 as "how often something happened", not as latency. Every other
  method should be milliseconds.
- `rpc_protocol_responses_total{service,method,protocol,code,client_id}` —
  the same responses broken down by RPC code and by the calling application
  (the token's client id, empty for anonymous or refused calls). `protocol`
  is `twirp` for every call today; when Connect is mounted, `protocol="twirp"`
  going to zero for a method is what says its Twirp mount can be retired.
  `code` is where to look for the error breakdown `status` cannot give.

## Database

From elephantine's `pg.NewPoolStatCollector`, one series set per pool: `main`
always, `pubsub` only when a bouncer connection string is configured and the
pools differ. Pool size is not configured explicitly and defaults to the host
CPU count, so `max_conns` reports the node the pod landed on, not a decision.

- `pgxpool_acquired_conns{pool}` against `pgxpool_max_conns{pool}` — the
  saturation pair. `acquired` sitting at `max` while
  `pgxpool_empty_acquire_wait_seconds_total` climbs means requests are queuing
  for connections; long-polls do not hold a connection while waiting, so this
  is write or read volume, not client count.
- `pgxpool_empty_acquires_total{pool}` and
  `pgxpool_empty_acquire_wait_seconds_total{pool}` — how often and how long
  callers waited for a free connection. Lag, not loss.
- `pgxpool_canceled_acquires_total{pool}` — callers that gave up waiting;
  each one was an RPC that failed.
- `pgxpool_new_conns_total`, `pgxpool_max_lifetime_destroys_total`,
  `pgxpool_max_idle_destroys_total` — connection churn; a steady rate is the
  pool's normal lifecycle, a spike is the database dropping connections.
- `pgxpool_total_conns`, `pgxpool_idle_conns`, `pgxpool_constructing_conns` —
  state gauges for the above.

## Background tasks

- `task_restarts_total{task="pubsub"}` — the LISTEN subscriber returned an
  error and was restarted after a 5 s backoff. It is the only task that is
  restarted in place, so it is the only label value that exists. A steady rate
  means the direct database connection is being reset repeatedly; long-polls
  fall back to their timeouts between restarts. The server and the cleaner are
  not counted here: their failure exits the process, which shows as a pod
  restart.
- `pg_job_lock_held{name="cleaner"}` — 1 on the replica that owns the
  retention cleaner, 0 elsewhere. Summed across replicas it should be exactly
  1; 0 for more than a minute means no replica is cleaning, more than 1 is a
  stale lock being stolen.
- `pg_job_lock_transitions_total{name,state}` — lock state changes
  (`held`, `lost`, `released`). A sustained rate is lock churn: the lock
  ping-ponging between replicas or a replica repeatedly losing its database
  connection.
- `pg_job_lock_restarts_total{name="cleaner"}` — the cleaner function
  returned an error and was restarted with backoff. The function returns nil
  on cancellation and never returns otherwise, so any increment here is
  unexpected and worth reading the logs for. A failed sweep is retried on the
  next tick without counting here.

## Readiness

- `health_check_up{name}` — 1 while the named readiness check passes.
  `name="postgres"` is required: 0 turns `/health/ready` into a 500 and the
  pod out of the load balancer. `name="schemas"` is optional: 0 means the
  active generation lacks a `settings` or `messages` schema, every validated
  write is failing, and the probe stays green on purpose so the generation can
  be registered through the service's own API.

## State gauges

`pgxpool_*` gauges are read on scrape from the pool's own statistics.
`pg_job_lock_held` is set on every lock state change and reports 0 on a
replica that does not hold the lock, not absence; the sum across replicas is
the meaningful number. `health_check_up` is set each time the readiness
handler runs, so it lags the underlying state by one probe interval.

## Not in place yet

There is no metric for how long pollers waited or how often a poll returned
on the timeout rather than on a notification, so a LISTEN connection that is
alive but not delivering shows only as `rpc_duration_seconds` for the poll
methods flattening against their timeouts. There is no eventlog lag or size
gauge. The log-derived validator reload counts are the only reload signal.
