CREATE TABLE IF NOT EXISTS message(
       recipient text not null,
       id bigint not null,
       type text null,
       created timestamptz not null default NOW(),
       created_by text not null,
       doc_uuid uuid null,
       doc_type text null,
       payload jsonb not null,
       primary key(recipient, id)
);

CREATE TABLE IF NOT EXISTS inbox_message(
       recipient text not null,
       id bigint not null,
       created timestamptz not null default NOW(),
       created_by text not null,
       updated timestamptz not null default NOW(),
       is_read bool not null default false,
       payload jsonb not null,
       primary key(recipient, id)
);

---- create above / drop below ----

DROP TABLE IF EXISTS message;               
DROP TABLE IF EXISTS inbox_message;
