# Comment access follows the repository document

Decided 2026-11-03.

A comment thread is on one repository document, and whoever may read that
document may read its threads, and whoever may write it may comment, edit
their own comments, and resolve and reopen threads. The service does not keep
an ACL of its own: it forwards the caller's bearer token to elephant-repository's
`Documents.GetPermissions` and reads the answer, which is already per caller
and already resolves unit membership and elevated document scopes, so an
agent with `doc_read_all` or `doc_write` needs no special handling. We chose
this because the service has no membership directory and only ever learns a
caller's org and units from that caller's own token: it cannot evaluate a
document ACL itself, and anything it stored would be a second ACL drifting
from the one editors actually manage. The only scope the `Comments` RPCs check
here is `user`; `doc_admin` additionally lets a caller delete another
author's comment, because write access to an article does not make an editor
a moderator.

The rest of the model follows from the same reluctance to own what the
platform or the client already owns. Thread and comment uuids are made by the
client: the editor mints the thread uuid when it inserts the anchor, which
reaches other editors through the collaborative document before any comment
is saved, and a client-made comment uuid makes a retried create idempotent
and lets the client render optimistically. A delete keeps the row, sets
`deleted` and `deleted_by`, and stops every read from returning the payload
and mentions, so a thread keeps its shape and an anchor can be cleaned up when
no live comment is left. And there is no commit-ordered id yet: nothing tails
comments, so threads are ordered by timestamp, and `WaitForComments` is a
cursor-less wakeup poll until a comment log exists.

## Considered options

- **A per-thread ACL table with its own scopes**, as an earlier prototype
  had: threads carried no document reference, a `comment_thread_acl` table
  listed URIs and permissions, and three comment scopes gated the RPCs.
  Rejected because it asks every client to maintain a second ACL beside the
  document's, cannot follow a unit membership change without a directory, and
  needs Keycloak provisioning for scopes that say nothing the document ACL
  does not. A thread without a document also cannot be listed by document.
- **Holding service credentials and evaluating the ACL here.** Workable, but
  it reimplements the repository's grant logic, elevated scopes included, and
  makes this service a privileged reader of every document's ACL. Forwarding
  the caller's token keeps the repository the only place that decides.
- **Server-generated uuids.** Rejected: the thread uuid has to exist in the
  document text before the thread exists here, and a server id would make a
  retry after a lost response create a duplicate.
- **Hard delete.** Rejected: removing the row would leave an anchor pointing
  at nothing with no way to tell a deleted thread from one never saved, and
  would lose who deleted what. Nothing is purged.
- **A sequence id now.** Rejected for the first version: a comment log is one
  more table and one more insert in the write transaction, and its shape
  belongs with the notification work that will consume it. The
  `comment_update` notification already carries the record a log row would
  hold, so adding one later changes no write path but its tail.

## Consequences

- The repository is a runtime dependency of every `Comments` RPC but
  `ListMentions`: unset, they answer `permission_denied`; unreachable,
  `internal`. `ListMentions` checks no document, which is why it returns
  references without payloads.
- A calling token needs a repository document scope, such as `doc_read`, as
  well as `user`; the repository refuses `GetPermissions` without one, and the
  refusal is answered as nothing granted.
- Answers are cached for 30 s per subject and document, refusals included, so
  an ACL change reaches comments within that window, in both directions.
- "May comment but may not edit the article" cannot be expressed. If it is
  ever needed, a `comment_write` scope checked here is the way to add it.
- A deleted comment's payload stays in the database; removing it is a
  retention decision that does not exist yet.
- When a comment log is added, its ids come from a `sequence_counter` row
  taken last in the writing transaction, after the thread lock, as the
  eventlog rule requires. Do not order a tailed comment stream by timestamp.
