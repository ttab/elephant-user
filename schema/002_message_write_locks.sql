CREATE TABLE IF NOT EXISTS message_write_lock(
  recipient text not null,
  message_type text not null,
  current_message_id bigint,
  primary key(recipient, message_type)
);

---- create above / drop below ----

DROP TABLE IF EXISTS message_write_lock;
