# elephant-user

The per-user data service of the Elephant platform: settings documents and
properties per owner, durable inbox messages addressed to users, units and
orgs, ephemeral system messages, and the revisor schemas those documents are
validated against. Document, NewsDoc, version, scope, eventlog and the other
platform terms are defined by
[elephant-repository's glossary](https://github.com/ttab/elephant-repository/blob/main/CONTEXT.md)
and are not redefined here; where a term below narrows one of them, it says
so.

## Language

### Identity

**Owner**:
A URI that settings state and inbox messages are addressed to: a user
(`core://user/<id>`), a unit (`core://unit/<id>`) or an org
(`core://org/<id>`). A caller's owners are the `sub`, `org` and `units`
claims of their token, and nothing else.
_Avoid_: Target, principal, account

**Subject**:
The caller themselves, the `sub` claim as elephantine normalises it to a URI.
A subject is one of the caller's owners; it is the one their read state is
kept under.
_Avoid_: User id, current user

**Recipient**:
The owner an inbox or system message was addressed to. For a system message
always a subject; for an inbox message any owner.
_Avoid_: Addressee, destination

**Reader**:
A caller reading the inbox: their owners decide what is addressed to them,
their subject decides whose read state applies. In code, `InboxReader`.
_Avoid_: Consumer, viewer

**Group recipient**:
A unit or an org as recipient. Pushing to one requires the `doc_admin` scope
and membership; what the push creates is a broadcast.
_Avoid_: Team, department

**Membership**:
Having a unit or org among one's own claims. The service has no membership
directory and can never enumerate the members of a group; it only knows what
each caller's token says about that caller.
_Avoid_: Group lookup, directory

### Inbox

**Inbox message**:
A durable NewsDoc document of type `core/inbox-message` addressed to a
recipient, kept six months, with one row however many readers it has. Its
identity per recipient is the document's uuid.
_Avoid_: Notification, mail, inbox item

**Broadcast**:
An inbox message addressed to a group recipient. One row; every member reads
it under their own read state. Never fanned out.
_Avoid_: Mass message, announcement (that is a use, not the term)

**Read state**:
One reader's `is_read` and `hidden` flags for one message, in
`inbox_message_state`. Absent means unread and visible.
_Avoid_: Message status, flags

**Hide**:
What `DeleteInboxMessage` does: set `hidden` on the caller's read state so
the message leaves their polls and lists while staying for every other
reader. There is no un-hide and no deletion of the shared row by a reader.
_Avoid_: Delete (in prose about what happens to the row), dismiss

**Idempotent push**:
A `PushInboxMessage` whose `(recipient, uuid)` already exists with the same
payload is answered with the existing id and stores nothing. The same uuid
with a different payload is a conflict, `already_exists`.
_Avoid_: Deduplication, upsert

**System message**:
An ephemeral typed key-value message to one subject, two weeks of retention,
pushed with `PushMessage` and polled with `PollMessages`. Frozen; to be
replaced by the notification stream.
_Avoid_: Toast (that is what the client does with it), notification

**Notification**:
Not an inbox message. A system-emitted event about a resource, delivered by
the planned notification stream, subscribed to rather than addressed, with no
read state. Reserved for that work; do not use it for inbox messages.
_Avoid_: Using it interchangeably with inbox message

### Settings

**Settings document**:
A schema-validated NewsDoc document owned by an owner and keyed by
application, type and key. Shared when its owner is a unit or org; writing a
shared one needs `doc_admin` and membership.
_Avoid_: Setting, preference document, config

**Property**:
A flat key-value preference, private to its subject, keyed by application and
key.
_Avoid_: Setting, attribute

**Eventlog**:
Narrower than the platform's: this service's change stream over settings
documents and properties only, tailed with `PollEventLog`. Inbox messages do
not enter it; the inbox table is its own log.
_Avoid_: Audit log, history

### Ids and streams

**Commit-ordered id**:
An id handed out by a `sequence_counter` row inside the writing transaction,
so that id order equals commit order and a cursor never skips an entry. Both
the eventlog and the inbox use one; system messages use a per-recipient
counter.
_Avoid_: Serial, sequence (the Postgres object, which is what this replaces)

**Cursor**:
The `after_id` a client carries between polls, valid across everything the
caller can read because the ids it moves over are commit-ordered in one
space.
_Avoid_: Offset, position

**Wakeup**:
What a NOTIFY is: a signal that a waiting poll should re-read, never the data
itself. Delivery is best effort and every poller re-reads.
_Avoid_: Event delivery, push

### Schemas

**Config generation**:
A named, immutable set of revisor schema versions that is active together;
exactly one is active. Registered and activated through the Configuration
service, never seeded by the service itself.
_Avoid_: Schema set, schema version (that is one schema's version)

**Usage**:
Which kind of document a schema validates: `SETTINGS` or `MESSAGES`. A
schema validates only its own usage.
_Avoid_: Kind, category
