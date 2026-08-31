--
-- PostgreSQL database dump
--


-- Dumped from database version 17.7 (Debian 17.7-3.pgdg12+1)
-- Dumped by pg_dump version 17.7 (Debian 17.7-3.pgdg12+1)

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET transaction_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

--
-- Name: event_type; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.event_type AS ENUM (
    'update',
    'delete'
);


--
-- Name: resource_kind; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.resource_kind AS ENUM (
    'document',
    'property'
);


--
-- Name: schema_usage; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.schema_usage AS ENUM (
    'settings',
    'messages'
);


--
-- Name: user_kind; Type: TYPE; Schema: public; Owner: -
--

CREATE TYPE public.user_kind AS ENUM (
    'user',
    'unit',
    'org'
);


SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: config_generation; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.config_generation (
    id bigint NOT NULL,
    identity_hash text NOT NULL,
    description text DEFAULT ''::text NOT NULL,
    created_at timestamp with time zone DEFAULT now() NOT NULL,
    activated_at timestamp with time zone,
    active boolean DEFAULT false NOT NULL
);


--
-- Name: config_generation_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.config_generation ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.config_generation_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
);


--
-- Name: config_generation_schema; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.config_generation_schema (
    generation_id bigint NOT NULL,
    name text NOT NULL,
    version text NOT NULL,
    ordinal integer NOT NULL
);


--
-- Name: deprecation; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.deprecation (
    label text NOT NULL,
    enforced boolean DEFAULT false NOT NULL
);


--
-- Name: document; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.document (
    owner text NOT NULL,
    application text NOT NULL,
    type text NOT NULL,
    key text NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    schema_version text NOT NULL,
    title text NOT NULL,
    created timestamp with time zone DEFAULT now() NOT NULL,
    updated timestamp with time zone DEFAULT now() NOT NULL,
    updated_by text NOT NULL,
    payload jsonb NOT NULL
);


--
-- Name: document_schema; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.document_schema (
    name text NOT NULL,
    version text NOT NULL,
    spec jsonb NOT NULL,
    usage public.schema_usage NOT NULL
);


--
-- Name: eventlog; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.eventlog (
    id bigint NOT NULL,
    owner text NOT NULL,
    type public.event_type NOT NULL,
    resource_kind public.resource_kind NOT NULL,
    application text NOT NULL,
    document_type text,
    key text NOT NULL,
    version bigint,
    updated_by text NOT NULL,
    created timestamp with time zone DEFAULT now() NOT NULL,
    payload jsonb
);


--
-- Name: inbox_message; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.inbox_message (
    recipient text NOT NULL,
    id bigint NOT NULL,
    created timestamp with time zone DEFAULT now() NOT NULL,
    created_by text NOT NULL,
    updated timestamp with time zone DEFAULT now() NOT NULL,
    is_read boolean DEFAULT false NOT NULL,
    payload jsonb NOT NULL
);


--
-- Name: job_lock; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.job_lock (
    name text NOT NULL,
    holder text NOT NULL,
    touched timestamp with time zone NOT NULL,
    iteration bigint NOT NULL
);


--
-- Name: message; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.message (
    recipient text NOT NULL,
    id bigint NOT NULL,
    type text,
    created timestamp with time zone DEFAULT now() NOT NULL,
    created_by text NOT NULL,
    doc_uuid uuid,
    doc_type text,
    payload jsonb NOT NULL
);


--
-- Name: message_write_lock; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.message_write_lock (
    recipient text NOT NULL,
    message_type text NOT NULL,
    current_message_id bigint
);


--
-- Name: property; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.property (
    owner text NOT NULL,
    application text NOT NULL,
    key text NOT NULL,
    value text NOT NULL,
    created timestamp with time zone DEFAULT now() NOT NULL,
    updated timestamp with time zone DEFAULT now() NOT NULL
);


--
-- Name: schema_version; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.schema_version (
    version integer NOT NULL
);


--
-- Name: sequence_counter; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sequence_counter (
    name text NOT NULL,
    value bigint NOT NULL
);


--
-- Name: user; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."user" (
    sub text NOT NULL,
    created timestamp with time zone DEFAULT now() NOT NULL,
    kind public.user_kind DEFAULT 'user'::public.user_kind NOT NULL
);


--
-- Name: config_generation config_generation_identity_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.config_generation
    ADD CONSTRAINT config_generation_identity_hash_key UNIQUE (identity_hash);


--
-- Name: config_generation config_generation_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.config_generation
    ADD CONSTRAINT config_generation_pkey PRIMARY KEY (id);


--
-- Name: config_generation_schema config_generation_schema_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.config_generation_schema
    ADD CONSTRAINT config_generation_schema_pkey PRIMARY KEY (generation_id, name);


--
-- Name: deprecation deprecation_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.deprecation
    ADD CONSTRAINT deprecation_pkey PRIMARY KEY (label);


--
-- Name: document document_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.document
    ADD CONSTRAINT document_pkey PRIMARY KEY (owner, application, type, key);


--
-- Name: document_schema document_schema_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.document_schema
    ADD CONSTRAINT document_schema_pkey PRIMARY KEY (name, version);


--
-- Name: eventlog eventlog_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.eventlog
    ADD CONSTRAINT eventlog_pkey PRIMARY KEY (id);


--
-- Name: inbox_message inbox_message_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.inbox_message
    ADD CONSTRAINT inbox_message_pkey PRIMARY KEY (recipient, id);


--
-- Name: job_lock job_lock_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.job_lock
    ADD CONSTRAINT job_lock_pkey PRIMARY KEY (name);


--
-- Name: message message_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.message
    ADD CONSTRAINT message_pkey PRIMARY KEY (recipient, id);


--
-- Name: message_write_lock message_write_lock_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.message_write_lock
    ADD CONSTRAINT message_write_lock_pkey PRIMARY KEY (recipient, message_type);


--
-- Name: property property_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.property
    ADD CONSTRAINT property_pkey PRIMARY KEY (owner, application, key);


--
-- Name: sequence_counter sequence_counter_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sequence_counter
    ADD CONSTRAINT sequence_counter_pkey PRIMARY KEY (name);


--
-- Name: user user_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."user"
    ADD CONSTRAINT user_pkey PRIMARY KEY (sub);


--
-- Name: config_generation_single_active; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX config_generation_single_active ON public.config_generation USING btree (active) WHERE active;


--
-- Name: eventlog_owner_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX eventlog_owner_id_idx ON public.eventlog USING btree (owner, id);


--
-- Name: config_generation_schema config_generation_schema_generation_id_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.config_generation_schema
    ADD CONSTRAINT config_generation_schema_generation_id_fkey FOREIGN KEY (generation_id) REFERENCES public.config_generation(id) ON DELETE CASCADE;


--
-- Name: config_generation_schema config_generation_schema_name_version_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.config_generation_schema
    ADD CONSTRAINT config_generation_schema_name_version_fkey FOREIGN KEY (name, version) REFERENCES public.document_schema(name, version);


--
-- Name: document document_owner_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.document
    ADD CONSTRAINT document_owner_fkey FOREIGN KEY (owner) REFERENCES public."user"(sub) ON DELETE CASCADE;


--
-- Name: inbox_message inbox_message_recipient_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.inbox_message
    ADD CONSTRAINT inbox_message_recipient_fkey FOREIGN KEY (recipient) REFERENCES public."user"(sub) ON DELETE CASCADE;


--
-- Name: message message_recipient_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.message
    ADD CONSTRAINT message_recipient_fkey FOREIGN KEY (recipient) REFERENCES public."user"(sub) ON DELETE CASCADE;


--
-- Name: message_write_lock message_write_lock_recipient_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.message_write_lock
    ADD CONSTRAINT message_write_lock_recipient_fkey FOREIGN KEY (recipient) REFERENCES public."user"(sub) ON DELETE CASCADE;


--
-- Name: property property_owner_fkey; Type: FK CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.property
    ADD CONSTRAINT property_owner_fkey FOREIGN KEY (owner) REFERENCES public."user"(sub) ON DELETE CASCADE;


--
-- PostgreSQL database dump complete
--


