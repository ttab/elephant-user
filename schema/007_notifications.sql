-- The notification feed: a keyed log, one row per event per key, where a key
-- is a user, unit or org uri. Readers tail each of their keys with its own
-- id, so ids are per key and commit-ordered: they come from the key's
-- notification_sequence row, which the writer upserts and row-locks for the
-- rest of its transaction, the same way message_write_lock hands out system
-- message ids. Never an identity or serial column; see "Ids that tail
-- correctly" in docs/architecture.md. The unique (key, event_uuid) makes a
-- repeated write for the same event under the same key a no-op. refs is
-- always a JSON array and meta always a JSON object: the store writes them
-- that way, empty when there is nothing to say.
--
-- notification_cursor is a reader's delivered position per key. It is a
-- separate row from the sequence on purpose: writes lock the sequence,
-- delivery bumps the cursor, and the two never meet.
--
-- notification_subscription is a user's interest in a document; a null
-- expires_at is a permanent subscription.
--
-- There is no foreign key to the document: it lives in the repository, in
-- another service. There is none to the "user" table either: keys and
-- subjects are the token's claims, as in the inbox.

CREATE TABLE notification (
  key text not null,
  id bigint not null,
  kind text not null,
  document_uuid uuid not null,
  document_type text not null,
  refs jsonb not null default '[]'::jsonb,
  actor text not null,
  event_uuid uuid not null,
  created timestamptz not null,
  meta jsonb not null default '{}'::jsonb,
  primary key (key, id),
  unique (key, event_uuid)
);

CREATE INDEX notification_created_idx ON notification (created);

CREATE TABLE notification_sequence (
  key text primary key,
  last_id bigint not null
);

CREATE TABLE notification_cursor (
  subject text not null,
  key text not null,
  position bigint not null,
  primary key (subject, key)
);

CREATE TABLE notification_subscription (
  subject text not null,
  document_uuid uuid not null,
  expires_at timestamptz null,
  primary key (subject, document_uuid)
);

CREATE INDEX notification_subscription_document_idx
  ON notification_subscription (document_uuid);

---- create above / drop below ----

DROP TABLE notification_subscription;
DROP TABLE notification_cursor;
DROP TABLE notification_sequence;
DROP TABLE notification;
