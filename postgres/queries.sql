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

-- name: NextMessageID :one
INSERT INTO message_write_lock(
      recipient, message_type, current_message_id
) VALUES (
      @recipient, @message_type, 1
)
ON CONFLICT(recipient, message_type)
DO UPDATE SET
  current_message_id = message_write_lock.current_message_id + 1
RETURNING current_message_id;

-- name: ReserveSequenceValues :one
UPDATE sequence_counter
SET value = value + @count::bigint
WHERE name = @name
RETURNING value;

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
WHERE owner = ANY(@owners::text[])
      AND (sqlc.narg('application')::text IS NULL OR application = sqlc.narg('application')::text)
      AND (sqlc.narg('type')::text IS NULL OR type = sqlc.narg('type')::text)
ORDER BY application ASC, type ASC, key ASC;

-- name: ListDocumentsFull :many
SELECT owner, application, type, key, version, schema_version,
       title, created, updated, updated_by, payload
FROM document
WHERE owner = ANY(@owners::text[])
      AND (sqlc.narg('application')::text IS NULL OR application = sqlc.narg('application')::text)
      AND (sqlc.narg('type')::text IS NULL OR type = sqlc.narg('type')::text)
ORDER BY application ASC, type ASC, key ASC;

-- name: UpsertDocument :one
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
  payload = EXCLUDED.payload
RETURNING version;

-- name: DeleteDocument :one
DELETE FROM document
WHERE owner = @owner
      AND application = @application
      AND type = @type
      AND key = @key
RETURNING 1;

-- name: GetLatestEventLogId :one
SELECT COALESCE(MAX(id), 0)::bigint
FROM eventlog
WHERE owner = ANY(@owners::text[]);

-- name: InsertEventLog :exec
INSERT INTO eventlog (
      id, owner, type, resource_kind, application, document_type,
      key, version, updated_by, created, payload
) VALUES (
      @id,
      @owner,
      @type, -- type (update/delete)
      @resource_kind, -- resource_kind (document/property)
      @application,
      @document_type, -- empty if resource_kind is 'property'
      @key,
      @version,
      @updated_by,
      @created,
      @payload
);

-- name: GetEventLogEntriesAfterId :many
SELECT id, owner, type, resource_kind, application, document_type,
       key, version, updated_by, created, payload
FROM eventlog
WHERE owner = ANY(@owners::text[])
      AND id > @after_id
ORDER BY id ASC
LIMIT sqlc.arg('limit')::bigint;

-- name: EnsureSchema :exec
INSERT INTO document_schema (name, version, spec, usage)
VALUES (@name, @version, @spec, @usage)
ON CONFLICT (name, version) DO NOTHING;

-- name: GetSchema :one
SELECT name, version, spec, usage
FROM document_schema
WHERE name = @name AND version = @version;

-- name: GetActiveSchema :one
SELECT ds.name, ds.version, ds.spec, ds.usage
FROM config_generation AS cg
     INNER JOIN config_generation_schema AS cgs
           ON cgs.generation_id = cg.id
     INNER JOIN document_schema AS ds
           ON ds.name = cgs.name AND ds.version = cgs.version
WHERE cg.active AND cgs.name = @name;

-- name: InsertConfigGeneration :one
INSERT INTO config_generation (identity_hash, description)
VALUES (@identity_hash, @description)
RETURNING id, identity_hash, description, created_at, activated_at, active;

-- name: GetConfigGenerationByIdentityHash :one
SELECT id, identity_hash, description, created_at, activated_at, active
FROM config_generation
WHERE identity_hash = @identity_hash;

-- name: InsertConfigGenerationSchema :exec
INSERT INTO config_generation_schema (generation_id, name, version, ordinal)
VALUES (@generation_id, @name, @version, @ordinal);

-- name: DeactivateActiveConfigGeneration :exec
UPDATE config_generation SET active = false
WHERE active AND id != @exclude_id;

-- name: ActivateConfigGeneration :exec
UPDATE config_generation SET active = true, activated_at = now()
WHERE id = @id;

-- name: GetActiveConfigGeneration :one
SELECT id, identity_hash, description, created_at, activated_at, active
FROM config_generation
WHERE active;

-- name: GetConfigGeneration :one
SELECT id, identity_hash, description, created_at, activated_at, active
FROM config_generation
WHERE id = @id;

-- name: ListConfigGenerations :many
SELECT id, identity_hash, description, created_at, activated_at, active
FROM config_generation
WHERE (sqlc.arg(before)::bigint = 0 OR id < sqlc.arg(before)::bigint)
ORDER BY id DESC
LIMIT sqlc.arg(page_size)::bigint;

-- name: GetConfigGenerationSchemas :many
SELECT cgs.name, cgs.version, ds.usage
FROM config_generation_schema AS cgs
     INNER JOIN document_schema AS ds
           ON ds.name = cgs.name AND ds.version = cgs.version
WHERE cgs.generation_id = @generation_id
ORDER BY cgs.ordinal;

-- name: GetActiveSchemas :many
SELECT cgs.name, cgs.version, ds.spec, ds.usage
FROM config_generation AS cg
     INNER JOIN config_generation_schema AS cgs
           ON cgs.generation_id = cg.id
     INNER JOIN document_schema AS ds
           ON ds.name = cgs.name AND ds.version = cgs.version
WHERE cg.active
ORDER BY cgs.ordinal;

-- name: GetDeprecations :many
SELECT label, enforced
FROM deprecation
ORDER BY label;

-- name: UpsertDeprecation :exec
INSERT INTO deprecation (label, enforced)
VALUES (@label, @enforced)
ON CONFLICT (label) DO UPDATE SET enforced = excluded.enforced;

-- name: GetEnforcedDeprecations :many
SELECT label
FROM deprecation
WHERE enforced;

-- name: GetConfigGenerationSchemasWithSpec :many
SELECT cgs.name, cgs.version, ds.spec, ds.usage
FROM config_generation_schema AS cgs
     INNER JOIN document_schema AS ds
           ON ds.name = cgs.name AND ds.version = cgs.version
WHERE cgs.generation_id = @generation_id
ORDER BY cgs.ordinal;
