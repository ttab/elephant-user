-- name: GetLatestInboxMessageId :one
SELECT COALESCE(MAX(id), 0)::bigint AS latest_id
FROM inbox_message
WHERE recipient = @recipient;

-- name: ListInboxMessagesBeforeId :many
SELECT recipient, id, created, created_by, updated, is_read, payload
FROM inbox_message
WHERE recipient = @recipient
      AND (@before_id::bigint = 0 OR id < @before_id)
ORDER BY id DESC
LIMIT sqlc.arg('limit')::bigint;

-- name: ListInboxMessagesAfterId :many
SELECT recipient, id, created, created_by, updated, is_read, payload
FROM inbox_message
WHERE recipient = @recipient
      AND id > @after_id
ORDER BY id ASC
LIMIT sqlc.arg('limit')::bigint;

-- name: GetLatestMessageId :one
SELECT COALESCE(MAX(id), 0)::bigint AS latest_id
FROM message
WHERE recipient = @recipient;

-- name: ListMessagesAfterId :many
SELECT recipient, id, type, created, created_by, doc_uuid, doc_type, payload
FROM message
WHERE recipient = @recipient
      AND id > @after_id
ORDER BY id ASC
LIMIT sqlc.arg('limit')::bigint;

-- name: GetMessageWriteLock :one
SELECT recipient, message_type, current_message_id
FROM message_write_lock
WHERE recipient = @recipient
      AND message_type = @message_type
FOR UPDATE;

-- name: UpsertMessageWriteLock :exec
INSERT INTO message_write_lock(
      recipient, message_type, current_message_id
) VALUES (
      @recipient, @message_type, @current_message_id
)
ON CONFLICT(recipient, message_type)
DO UPDATE SET
  current_message_id = EXCLUDED.current_message_id;

-- name: InsertInboxMessage :exec
INSERT INTO inbox_message(
      recipient, id, created, created_by, updated, is_read, payload
) VALUES (
      @recipient, @id, @created, @created_by, @updated, @is_read, @payload
);

-- name: InsertMessage :exec
INSERT INTO message(
      recipient, id, type, created, created_by, doc_uuid, doc_type, payload
) VALUES (
      @recipient, @id, @type, @created, @created_by, @doc_uuid, @doc_type, @payload
);

-- name: UpsertUser :exec
INSERT INTO "user"(
      sub, created, kind
) VALUES (
      @sub, @created, @kind
)
ON CONFLICT (sub)
DO NOTHING;

-- name: UpdateInboxMessage :exec
UPDATE inbox_message
SET is_read = @is_read
WHERE recipient = @recipient
      AND id = @id;

-- name: DeleteInboxMessage :exec
DELETE FROM inbox_message
WHERE recipient = @recipient
      AND id = @id;

-- name: Notify :exec
SELECT pg_notify(@channel::text, @message::text);

-- name: DeleteOldInboxMessages :exec
DELETE FROM inbox_message
WHERE created < now() - INTERVAL '6 months';

-- name: DeleteOldMessages :exec
DELETE FROM message
WHERE created < now() - INTERVAL '2 weeks';

-- name: GetProperties :many
SELECT owner, application, key, value, created, updated
FROM property
WHERE owner = @owner
      AND (sqlc.narg('application')::text IS NULL OR application = sqlc.narg('application')::text)
      AND (sqlc.slice('keys')::text[] IS NULL OR key = ANY(sqlc.slice('keys')::text[]));

-- name: UpsertProperty :exec
INSERT INTO property (
      owner, application, key, value, updated
) VALUES (
      @owner, @application, @key, @value, now()
)
ON CONFLICT (owner, application, key)
DO UPDATE SET
  value = EXCLUDED.value,
  updated = now();

-- name: DeleteProperty :one
DELETE FROM property
WHERE owner = @owner
      AND application = @application
      AND key = @key
RETURNING 1;

-- name: GetDocument :one
SELECT owner, application, type, key, version, schema_version,
       title, created, updated, updated_by, payload
FROM document
WHERE owner = @owner
      AND application = @application
      AND type = @type
      AND key = @key;

-- name: ListDocumentsMetadata :many
SELECT owner, application, type, key, version, schema_version,
       title, created, updated, updated_by
FROM document
WHERE owner = @owner
      AND (sqlc.narg('application')::text IS NULL OR application = sqlc.narg('application')::text)
      AND (sqlc.narg('type')::text IS NULL OR type = sqlc.narg('type')::text)
ORDER BY application ASC, type ASC, key ASC;

-- name: ListDocumentsFull :many
SELECT owner, application, type, key, version, schema_version,
       title, created, updated, updated_by, payload
FROM document
WHERE owner = @owner
      AND (sqlc.narg('application')::text IS NULL OR application = sqlc.narg('application')::text)
      AND (sqlc.narg('type')::text IS NULL OR type = sqlc.narg('type')::text)
ORDER BY application ASC, type ASC, key ASC;

-- name: UpsertDocument :exec
INSERT INTO document (
      owner, application, type, key,
      version, schema_version, title, created,
      updated, updated_by, payload
) VALUES (
      @owner, @application, @type, @key,
      1, @schema_version, @title, now(),
      now(), @updated_by, @payload
)
ON CONFLICT (owner, application, type, key)
DO UPDATE SET
  version = document.version + 1,
  schema_version = EXCLUDED.schema_version,
  title = EXCLUDED.title,
  updated_by = EXCLUDED.updated_by,
  updated = now(),
  payload = EXCLUDED.payload;

-- name: DeleteDocument :one
DELETE FROM document
WHERE owner = @owner
      AND application = @application
      AND type = @type
      AND key = @key
RETURNING 1;

-- name: InsertEventLog :exec
INSERT INTO eventlog (
  owner, created, type, resource_kind, application,
  document_type, updated_by, key, payload
) VALUES (
  @owner,
  now(),
  @type, -- type (update/delete)
  @resource_kind, -- resource_kind (document/property)
  @application,
  @document_type, -- empty if resource_kind is 'property'
  @updated_by,
  @key,
  @payload
);