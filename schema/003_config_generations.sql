-- Extending schema_usage is a plain `ALTER TYPE schema_usage ADD VALUE`
-- migration, but the added value cannot be used in the same migration.
CREATE TYPE schema_usage AS ENUM ('settings', 'messages');

CREATE TABLE IF NOT EXISTS document_schema (
  name text not null,
  version text not null,
  spec jsonb not null,
  usage schema_usage not null,
  primary key (name, version)
);

CREATE TABLE IF NOT EXISTS config_generation (
  id bigint generated always as identity primary key,
  identity_hash text unique not null,
  description text not null default '',
  created_at timestamptz not null default now(),
  activated_at timestamptz,
  active boolean not null default false
);

CREATE UNIQUE INDEX IF NOT EXISTS config_generation_single_active
  ON config_generation (active) WHERE active;

CREATE TABLE IF NOT EXISTS config_generation_schema (
  generation_id bigint not null references config_generation(id)
    on delete cascade,
  name text not null,
  version text not null,
  ordinal integer not null,
  -- One version of a schema name per generation.
  primary key (generation_id, name),
  foreign key (name, version) references document_schema(name, version)
);

CREATE TABLE IF NOT EXISTS deprecation (
  label text primary key,
  enforced boolean not null default false
);

---- create above / drop below ----

DROP TABLE IF EXISTS deprecation;
DROP TABLE IF EXISTS config_generation_schema;
DROP TABLE IF EXISTS config_generation;
DROP TABLE IF EXISTS document_schema;

DROP TYPE IF EXISTS schema_usage;
