CREATE TABLE IF NOT EXISTS message(
       recepient text not null,
       id bigint not null,
       type text,
       created timestamptz not null default NOW(),
       created_by text not null,
       doc_uuid uuid null,
       doc_type text null,
       payload jsonb not null,
       primary key(recepient, id)
);

CREATE TABLE IF NOT EXISTS inbox_message(
       recepient text not null,
       id bigint not null,
       created timestamptz not null default NOW(),
       created_by text not null,
       updated timestamptz not null default NOW(),
       payload jsonb not null,
       primary key(recepient, id)
);

---- create above / drop below ----

DROP TABLE IF EXISTS message;
DROP TABLE IF EXISTS inbox_message;
