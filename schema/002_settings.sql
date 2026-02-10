CREATE TYPE user_kind AS ENUM ('user', 'unit', 'org');

ALTER TABLE "user" ADD COLUMN kind user_kind not null default 'user';

CREATE TABLE IF NOT EXISTS document (
  owner text not null,
  application text NOT NULL,
  type text NOT NULL,
  key text NOT NULL,
  version bigint not null default 1,
  schema_version text not null,
  title text not null,
  created timestamptz not null default now(),
  updated timestamptz not null default now(),
  updated_by text not null,
  payload jsonb NOT NULL,
  primary key (owner, application, type, key),
  foreign key(owner) references "user"(sub)
    on delete cascade
);

CREATE TABLE IF NOT EXISTS property (
  owner text not null,
  application text NOT NULL,
  key text NOT NULL,
  value text NOT NULL,
  created timestamptz not null default now(),
  updated timestamptz not null default now(),
  primary key (owner, application, key),
  foreign key(owner) references "user"(sub)
    on delete cascade
);

CREATE TYPE event_type AS ENUM ('update', 'delete');
CREATE TYPE resource_kind AS ENUM ('document', 'property');

CREATE TABLE IF NOT EXISTS eventlog (
  id bigint generated always as identity primary key,
  owner text not null,
  type event_type NOT NULL,
  resource_kind resource_kind NOT NULL,
  application text NOT NULL,
  document_type text, -- empty if resource_kind is 'property'
  key text not null,
  version bigint,
  updated_by text not null,
  created timestamptz not null default now(),
  payload jsonb
);

-- give me events for owner X (user, matching unit and org) after id Y
-- owner in array operation for query
CREATE INDEX IF NOT EXISTS eventlog_owner_id_idx ON eventlog (owner, id);

---- create above / drop below ----

DROP TABLE IF EXISTS eventlog;
DROP TABLE IF EXISTS property;
DROP TABLE IF EXISTS document;

DROP TYPE IF EXISTS resource_kind;
DROP TYPE IF EXISTS event_type;

ALTER TABLE "user" DROP COLUMN IF EXISTS kind;

DROP TYPE IF EXISTS user_kind;