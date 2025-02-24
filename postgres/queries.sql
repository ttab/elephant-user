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
ORDER BY id DESC
LIMIT sqlc.arg('limit')::bigint;

-- name: ListMessagesAfterId :many
SELECT recipient, id, type, created, created_by, doc_uuid, doc_type, payload
FROM message
WHERE recipient = @recipient
      AND id > @after_id
ORDER BY id DESC
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
DO UPDATE
SET current_message_id = EXCLUDED.current_message_id;

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
      sub, created
) VALUES (
      @sub, @created
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