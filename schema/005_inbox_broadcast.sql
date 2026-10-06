-- Inbox messages become addressable to a unit or an org, and a message
-- addressed to a group is one row read by every member. The per-reader
-- state (read, hidden) therefore moves out of the message row into its own
-- table, and ids come from sequence_counter('inbox'): one commit-ordered id
-- space across every recipient, so a single after_id cursor spans a
-- reader's sub, org and units. The payload document's uuid identifies a
-- message per recipient, which is what makes a retried push idempotent.
--
-- The old table is dropped rather than migrated: its ids were per
-- recipient and cannot be renumbered into one space without inventing an
-- order, and the inbox has no callers. The message_write_lock rows that
-- handed out the old per-recipient inbox ids go with it; the lock keeps
-- serving the system message table.

DROP TABLE inbox_message;

CREATE TABLE inbox_message (
  id bigint primary key,
  uuid uuid not null,
  recipient text not null,
  created timestamptz not null,
  created_by text not null,
  payload jsonb not null,
  unique (recipient, uuid)
);

CREATE INDEX inbox_message_recipient_id_idx ON inbox_message (recipient, id);

CREATE TABLE inbox_message_state (
  message_id bigint not null references inbox_message (id) on delete cascade,
  subject text not null,
  is_read bool not null default false,
  hidden bool not null default false,
  updated timestamptz not null,
  primary key (message_id, subject)
);

INSERT INTO sequence_counter (name, value) VALUES ('inbox', 0)
ON CONFLICT (name) DO NOTHING;

DELETE FROM message_write_lock WHERE message_type = 'inbox';

---- create above / drop below ----

DROP TABLE inbox_message_state;
DROP TABLE inbox_message;

DELETE FROM sequence_counter WHERE name = 'inbox';

CREATE TABLE inbox_message (
  recipient text not null,
  id bigint not null,
  created timestamptz not null default now(),
  created_by text not null,
  updated timestamptz not null default now(),
  is_read bool not null default false,
  payload jsonb not null,
  primary key (recipient, id),
  foreign key (recipient) references "user"(sub) on delete cascade
);
