-- Comment threads on repository documents, and the comments in them. A
-- thread is anchored to a document by its uuid; a comment's payload is a
-- newsdoc document validated under the 'comments' schema usage, and
-- 'mentioned' holds the subject uris the service derives from its mention
-- links.
--
-- There is no foreign key to the document: it lives in the repository, in
-- another service. There is none to the "user" table either: comments are
-- keyed by the subject claims of the token, as the inbox is, and a thread
-- outlives any user row. The uuids have no default because the client makes
-- them, which is what makes a retried write idempotent; a missing uuid is
-- the caller's error, never a server value. There is no sequence id because
-- nothing tails comments yet; a commit-ordered id from sequence_counter
-- comes with an eventlog if one is added.
--
-- The new schema_usage value cannot be used in this migration, which is
-- fine: it is only referenced when a generation is registered.

CREATE TABLE comment_thread (
  uuid uuid primary key,
  document_uuid uuid not null,
  kind text not null default '',
  created timestamptz not null,
  created_by text not null,
  updated timestamptz not null,
  resolved timestamptz,
  resolved_by text
);

CREATE INDEX comment_thread_document_idx
  ON comment_thread (document_uuid, updated);

CREATE TABLE comment (
  uuid uuid primary key,
  thread_uuid uuid not null
    references comment_thread (uuid) on delete cascade,
  author text not null,
  author_name text not null default '',
  created timestamptz not null,
  updated timestamptz not null,
  deleted timestamptz,
  deleted_by text,
  mentioned text[] not null default '{}',
  payload jsonb not null
);

CREATE INDEX comment_thread_created_idx
  ON comment (thread_uuid, created, uuid);

CREATE INDEX comment_mentioned_idx
  ON comment USING gin (mentioned);

ALTER TYPE schema_usage ADD VALUE IF NOT EXISTS 'comments';

---- create above / drop below ----

-- Postgres cannot remove a value from an enum, so 'comments' stays in
-- schema_usage after a rollback. That is a known limitation; the value is
-- harmless once unused, and the IF NOT EXISTS above lets this migration run
-- again on top of it.

DROP TABLE comment;
DROP TABLE comment_thread;
