CREATE TABLE IF NOT EXISTS "user"(
  sub text primary key,
  created timestamptz not null default now()
);

CREATE TABLE IF NOT EXISTS message(
  recipient text not null,
  id bigint not null,
  type text null,
  created timestamptz not null default now(),
  created_by text not null,
  doc_uuid uuid null,
  doc_type text null,
  payload jsonb not null,
  primary key(recipient, id),
  foreign key(recipient) references "user"(sub)
    on delete cascade
);

CREATE TABLE IF NOT EXISTS inbox_message(
  recipient text not null,
  id bigint not null,
  created timestamptz not null default now(),
  created_by text not null,
  updated timestamptz not null default now(),
  is_read bool not null default false,
  payload jsonb not null,
  primary key(recipient, id),
  foreign key(recipient) references "user"(sub)
    on delete cascade
);

CREATE TABLE IF NOT EXISTS message_write_lock(
  recipient text not null,
  message_type text not null,
  current_message_id bigint,
  primary key(recipient, message_type),
  foreign key(recipient) references "user"(sub) 
    on delete cascade
);

---- create above / drop below ----

DROP TABLE IF EXISTS message;               
DROP TABLE IF EXISTS inbox_message;
DROP TABLE IF EXISTS message_write_lock;
DROP TABLE IF EXISTS "user";
