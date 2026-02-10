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
-- Name: eventlog_id_seq; Type: SEQUENCE; Schema: public; Owner: -
--

ALTER TABLE public.eventlog ALTER COLUMN id ADD GENERATED ALWAYS AS IDENTITY (
    SEQUENCE NAME public.eventlog_id_seq
    START WITH 1
    INCREMENT BY 1
    NO MINVALUE
    NO MAXVALUE
    CACHE 1
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
-- Name: user; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public."user" (
    sub text NOT NULL,
    created timestamp with time zone DEFAULT now() NOT NULL,
    kind public.user_kind DEFAULT 'user'::public.user_kind NOT NULL
);


--
-- Name: document document_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.document
    ADD CONSTRAINT document_pkey PRIMARY KEY (owner, application, type, key);


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
-- Name: user user_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public."user"
    ADD CONSTRAINT user_pkey PRIMARY KEY (sub);


--
-- Name: eventlog_owner_id_idx; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX eventlog_owner_id_idx ON public.eventlog USING btree (owner, id);


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

