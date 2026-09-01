-- Named counters that hand out gapless, commit-ordered ids. A writer takes
-- the next value with UPDATE ... RETURNING inside its own transaction, which
-- row-locks the counter until commit. The next writer therefore cannot obtain
-- its id before the previous one is visible, so tailing readers using
-- "id > after_id" can never skip past an uncommitted lower id.
CREATE TABLE sequence_counter (
  name text primary key,
  value bigint not null
);

-- The eventlog id is now assigned by the application from the counter above.
-- The ALTER takes an ACCESS EXCLUSIVE lock on eventlog that is held for the
-- rest of this transaction, so the MAX(id) seed below cannot be outrun by a
-- concurrent writer.
ALTER TABLE eventlog ALTER COLUMN id DROP IDENTITY;

INSERT INTO sequence_counter (name, value)
SELECT 'eventlog', COALESCE(MAX(id), 0) FROM eventlog;

UPDATE message_write_lock SET current_message_id = 0
WHERE current_message_id IS NULL;

ALTER TABLE message_write_lock
  ALTER COLUMN current_message_id SET DEFAULT 0,
  ALTER COLUMN current_message_id SET NOT NULL;

---- create above / drop below ----

ALTER TABLE message_write_lock
  ALTER COLUMN current_message_id DROP NOT NULL,
  ALTER COLUMN current_message_id DROP DEFAULT;

ALTER TABLE eventlog ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY;

SELECT setval(
  pg_get_serial_sequence('eventlog', 'id'),
  COALESCE((SELECT MAX(id) FROM eventlog), 0) + 1,
  false
);

DROP TABLE sequence_counter;
