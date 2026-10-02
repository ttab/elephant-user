# Inbox messages are one shared row with per-reader state

An inbox message addressed to a unit or an org is stored once, in
`inbox_message`, and matched against each reader's `sub`, `org` and `units`
claims when they poll or list. Each reader's `is_read` and `hidden` flags live
in `inbox_message_state(message_id, subject)`, absent until the reader touches
the message. We chose this over writing one row per member because the service
has no membership directory: it only ever learns a caller's org and units from
that caller's own token, so it cannot enumerate who a broadcast should reach,
and a fan-out would also have to be replayed whenever membership changed. The
settings eventlog already delivered shared documents this way, so the inbox
follows the same read-side rule rather than introducing a second model.

Two consequences shape the API. Ids come from one `sequence_counter` row
across every recipient, because a reader tails their whole owner set with a
single `after_id` and per-recipient sequences cannot compose into one cursor;
the old per-recipient `inbox_message` table was dropped rather than migrated,
since its ids could not be renumbered without inventing an order and the inbox
had no callers. And a delete hides the message for the caller only, by setting
`hidden` on their state row: the shared row belongs to every reader, so no
single reader may remove it, and for a user-addressed message the reader
cannot tell hide from delete anyway. Sender retraction is a separate RPC if it
is ever needed, keyed on the creator or a `doc_admin` of the recipient group.

Idempotency reuses the payload document's `uuid`, with `(recipient, uuid)`
unique: a retried push answers with the existing id, and the same uuid with a
different payload is refused as `already_exists`. An explicit idempotency key
field was rejected as a second identity every caller would have to mint next
to the one the document already carries.

## Consequences

- A reader in two recipients of the same document sees two rows with one
  payload uuid; clients collapse on the uuid.
- `InboxMessage.updated` is message-level and equals `created` while messages
  are immutable; a reader's "read at" would be a new named field, not a
  reinterpretation of this one.
- The push takes the counter as its first and only lock, unlike the eventlog
  writers, which take it last. That is safe because the push locks no data
  row, and it is what makes the uuid check under the counter race-free. Do
  not add a data-row lock to the push without revisiting this.
- Read-state changes emit `inbox_state_update` but the poll cannot carry
  them, since a flag change mints no id; cross-tab sync waits for the
  notification stream.
