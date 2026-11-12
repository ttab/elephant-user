# elephant-user

The per-user data service of the Elephant platform: settings documents and
properties per owner, durable inbox messages addressed to users, units and
orgs, ephemeral system messages, comment threads on repository documents, and
the revisor schemas those documents are validated against. Document, NewsDoc,
version, ACL, permission, scope, eventlog and the other platform terms are
defined by
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
pushed with `PushMessage` and polled with `PollMessages`. Frozen: the
elephant client BFF and chrome depend on the shapes.
_Avoid_: Toast (that is what the client does with it), notification

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

### Comments

How threads are stored and checked is in
[`docs/architecture.md`](docs/architecture.md#comments); why access follows
the document is
[ADR-0002](docs/adr/0002-comment-access-follows-the-repository-document.md).

**Thread**:
A flat, ordered list of comments on one repository document, identified by a
uuid the client makes. Its first comment creates it; it is resolved and
reopened as a whole and never deleted. There are no replies or sub-threads.
_Avoid_: Conversation, discussion, sub-thread

**Comment**:
One entry in a thread: a NewsDoc document of type `core/comment`, validated
under the `comments` usage, written by its author's subject and edited by
that subject only. Its uuid is the client's.
_Avoid_: Note, reply, annotation

**Anchor**:
The element in a repository document's text whose id is a thread's uuid,
placing the thread on a span. It lives in the document, not here: the service
never reads it, a thread without one is about the whole document, and an
anchor without a thread is one not saved yet.
_Avoid_: Marker, highlight

**Kind**:
An open string a thread is created with, such as `factcheck`, possibly empty.
A label for clients, fixed for the thread's life; not a schema usage and not
a permission.
_Avoid_: Type, category

**Mention**:
A `rel: mention` link in a comment's text block, bound by id to a `<mention>`
element, whose `uri` is a subject. The service derives a comment's mentioned
subjects from these links on every write and never takes them from the
caller. Nothing is delivered to the mentioned subject; `ListMentions` is how
they find out.
_Avoid_: Tag, notification

**Deleted comment**:
A comment its author, or a `doc_admin`, has deleted. It stays in its thread
with `deleted` and `deleted_by` set, and no read returns its payload or its
mentions. A thread counts only its live comments; one with none left can have
its anchor removed.
_Avoid_: Tombstone, soft delete, removed comment

**Document permission**:
The caller's read, write or meta write on a repository document, as the
repository's `GetPermissions` answers it for the caller's own token. The
repository owns the ACL and the permission; this service only asks, caches
the answer for 30 s, and gates every thread on it. Not a scope, and not an
ACL kept here.
_Avoid_: Thread ACL, comment scope

**Wakeup poll**:
A long poll with no cursor that answers which documents changed, never what
changed: `WaitForComments`. The caller re-reads, and a change between two
calls is not reported. Not a cursor poll like `PollEventLog`, and not a
comment stream, which does not exist yet.
_Avoid_: Subscription, comment feed

### Ids and streams

**Commit-ordered id**:
An id handed out by a `sequence_counter` row inside the writing transaction,
so that id order equals commit order and a cursor never skips an entry. Both
the eventlog and the inbox use one; system messages use a per-recipient
counter; comments have none yet, since nothing tails them.
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
Which kind of document a schema validates: `SETTINGS`, `MESSAGES` or
`COMMENTS`. A schema validates only its own usage.
_Avoid_: Kind (that is a thread's label), category
