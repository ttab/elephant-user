package internal_test

import (
	"encoding/json"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-user/internal"
	"github.com/ttab/elephantine/test"
)

const (
	commentAuthor = "core://user/author"
	commentOther  = "core://user/other"
)

// commentPayload builds a core/comment body with a text block that links
// the given subjects as mentions.
func commentPayload(t *testing.T, text string, mentions ...string) json.RawMessage {
	t.Helper()

	block := &newsdoc.Block{
		Type: "core/text",
		Data: map[string]string{"text": text},
	}

	for i, m := range mentions {
		block.Links = append(block.Links, &newsdoc.Block{
			Id:   "m" + string(rune('a'+i)),
			Rel:  "mention",
			Type: "tt/keycloak",
			Uri:  m,
		})
	}

	payload, err := json.Marshal(&newsdoc.Document{
		Uuid:    uuid.NewString(),
		Type:    "core/comment",
		Content: []*newsdoc.Block{block},
	})
	test.Mustf(t, err, "marshal comment payload")

	return payload
}

// commentEvents subscribes to comment changes on the documents and returns
// the channel they arrive on. The subscription is registered from a
// goroutine, so wait for it before the first write.
func commentEvents(
	t *testing.T, eu TestElephantUser, docs ...uuid.UUID,
) chan internal.CommentEvent {
	t.Helper()

	ch := make(chan internal.CommentEvent, 64)

	eu.Store.OnCommentUpdate(t.Context(), ch, docs)

	time.Sleep(50 * time.Millisecond)

	return ch
}

// expectCommentEvents reads one event per wanted type, in order, then
// checks that nothing else arrives.
func expectCommentEvents(
	t *testing.T, ch chan internal.CommentEvent,
	want ...internal.CommentEventType,
) []internal.CommentEvent {
	t.Helper()

	got := make([]internal.CommentEvent, 0, len(want))

	for i, w := range want {
		select {
		case e := <-ch:
			if e.Type != w {
				t.Fatalf("event %d: got %q, want %q", i, e.Type, w)
			}

			got = append(got, e)
		case <-time.After(5 * time.Second):
			t.Fatalf("event %d: timed out waiting for %q", i, w)
		}
	}

	select {
	case e := <-ch:
		t.Fatalf("unexpected event %q after %v", e.Type, want)
	case <-time.After(100 * time.Millisecond):
	}

	return got
}

func TestCommentsCreate(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	docUUID := uuid.New()
	threadUUID := uuid.New()
	events := commentEvents(t, eu, docUUID)

	first := internal.Comment{
		UUID:       uuid.New(),
		ThreadUUID: threadUUID,
		Author:     commentAuthor,
		AuthorName: "Author",
		Payload: commentPayload(t, "First",
			"core://user/b", "core://user/a", "core://user/b"),
	}

	comment, thread, err := eu.Store.CreateComment(ctx, first, docUUID, "factcheck")
	test.Mustf(t, err, "create the first comment")

	test.Equalf(t, threadUUID, thread.UUID, "thread uuid")
	test.Equalf(t, docUUID, thread.DocumentUUID, "thread document")
	test.Equalf(t, "factcheck", thread.Kind, "thread kind")
	test.Equalf(t, commentAuthor, thread.CreatedBy, "thread created by")
	test.Equalf(t, int64(1), thread.LiveComments, "live comments")
	test.Equalf(t, "Author", comment.AuthorName, "author name")
	test.Equalf(t, true, comment.Updated.Equal(comment.Created), "a new comment is not edited")

	if !slices.Equal([]string{"core://user/a", "core://user/b"}, comment.Mentioned) {
		t.Fatalf("got mentions %v", comment.Mentioned)
	}

	got := expectCommentEvents(t, events,
		internal.CommentEventThreadCreated,
		internal.CommentEventCommentCreated)

	test.Equalf(t, threadUUID, got[0].ThreadUUID, "thread event thread")
	test.Equalf(t, docUUID, got[0].DocumentUUID, "thread event document")
	test.Equalf(t, "factcheck", got[0].Kind, "thread event kind")
	test.Equalf(t, commentAuthor, got[0].Actor, "thread event actor")

	if got[0].CommentUUID != nil {
		t.Fatal("a thread event carries no comment uuid")
	}

	if got[1].CommentUUID == nil || *got[1].CommentUUID != first.UUID {
		t.Fatalf("comment event names comment %v", got[1].CommentUUID)
	}

	if !slices.Equal(comment.Mentioned, got[1].Mentioned) {
		t.Fatalf("comment event mentions %v", got[1].Mentioned)
	}

	// An idempotent repeat answers with the stored comment, writes
	// nothing and announces nothing. Re-encoding the payload with
	// whitespace does not make it different.
	var indented json.RawMessage

	indented, err = json.MarshalIndent(first.Payload, "", "  ")
	test.Mustf(t, err, "indent payload")

	repeat := first
	repeat.Payload = indented

	again, againThread, err := eu.Store.CreateComment(ctx, repeat, docUUID, "factcheck")
	test.Mustf(t, err, "repeat the first comment")

	test.Equalf(t, comment.UUID, again.UUID, "repeat answers the same comment")
	test.Equalf(t, true, comment.Created.Equal(again.Created), "repeat keeps created")
	test.Equalf(t, int64(1), againThread.LiveComments, "repeat adds no comment")

	expectCommentEvents(t, events)

	// The same uuid with another payload, or in another thread, is a
	// conflict.
	conflict := first
	conflict.Payload = commentPayload(t, "Something else")

	_, _, err = eu.Store.CreateComment(ctx, conflict, docUUID, "")
	if !errors.Is(err, internal.ErrCommentConflict) {
		t.Fatalf("different payload: got %v, want ErrCommentConflict", err)
	}

	elsewhere := first
	elsewhere.ThreadUUID = uuid.New()

	_, _, err = eu.Store.CreateComment(ctx, elsewhere, docUUID, "")
	if !errors.Is(err, internal.ErrCommentConflict) {
		t.Fatalf("different thread: got %v, want ErrCommentConflict", err)
	}

	_, err = eu.Store.GetThread(ctx, elsewhere.ThreadUUID)
	if !errors.Is(err, internal.ErrCommentThreadNotFound) {
		t.Fatalf("the conflicting post created its thread: %v", err)
	}

	expectCommentEvents(t, events)

	// A post to an existing thread needs no document, and a post that
	// names another document for it is refused.
	second := internal.Comment{
		UUID:       uuid.New(),
		ThreadUUID: threadUUID,
		Author:     commentOther,
		Payload:    commentPayload(t, "Second"),
	}

	_, _, err = eu.Store.CreateComment(ctx, second, uuid.New(), "")
	if !errors.Is(err, internal.ErrCommentThreadConflict) {
		t.Fatalf("other document: got %v, want ErrCommentThreadConflict", err)
	}

	secondComment, thread2, err := eu.Store.CreateComment(ctx, second, uuid.Nil, "")
	test.Mustf(t, err, "post to the existing thread")

	test.Equalf(t, int64(2), thread2.LiveComments, "live comments after the second post")
	test.Equalf(t, commentAuthor, thread2.CreatedBy, "the thread keeps its creator")
	test.Equalf(t, true, thread2.Updated.After(thread.Updated), "the post bumps the thread")
	test.Equalf(t, 0, len(secondComment.Mentioned), "no mentions")

	got = expectCommentEvents(t, events, internal.CommentEventCommentCreated)
	test.Equalf(t, commentOther, got[0].Actor, "second post actor")

	// An unknown thread without a document cannot be created.
	_, _, err = eu.Store.CreateComment(ctx, internal.Comment{
		UUID:       uuid.New(),
		ThreadUUID: uuid.New(),
		Author:     commentAuthor,
		Payload:    commentPayload(t, "Orphan"),
	}, uuid.Nil, "")
	if !errors.Is(err, internal.ErrCommentThreadNotFound) {
		t.Fatalf("unknown thread: got %v, want ErrCommentThreadNotFound", err)
	}

	// Posting to a resolved thread reopens it.
	_, err = eu.Store.ResolveThread(ctx, threadUUID, commentOther)
	test.Mustf(t, err, "resolve the thread")

	expectCommentEvents(t, events, internal.CommentEventThreadResolved)

	_, reopened, err := eu.Store.CreateComment(ctx, internal.Comment{
		UUID:       uuid.New(),
		ThreadUUID: threadUUID,
		Author:     commentAuthor,
		Payload:    commentPayload(t, "Third"),
	}, docUUID, "")
	test.Mustf(t, err, "post to the resolved thread")

	if reopened.Resolved != nil || reopened.ResolvedBy != "" {
		t.Fatalf("the post left the thread resolved: %v by %q",
			reopened.Resolved, reopened.ResolvedBy)
	}

	expectCommentEvents(t, events,
		internal.CommentEventCommentCreated,
		internal.CommentEventThreadReopened)

	full, err := eu.Store.GetThread(ctx, threadUUID)
	test.Mustf(t, err, "get the thread")

	test.Equalf(t, 3, len(full.Comments), "comments in the thread")
	test.Equalf(t, first.UUID, full.Comments[0].UUID, "oldest first")
	test.Equalf(t, second.UUID, full.Comments[1].UUID, "then the second")

	_, err = eu.Store.GetThread(ctx, uuid.New())
	if !errors.Is(err, internal.ErrCommentThreadNotFound) {
		t.Fatalf("unknown thread: got %v, want ErrCommentThreadNotFound", err)
	}
}

func TestCommentsUpdateDelete(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	docUUID := uuid.New()
	threadUUID := uuid.New()

	mine := internal.Comment{
		UUID:       uuid.New(),
		ThreadUUID: threadUUID,
		Author:     commentAuthor,
		Payload:    commentPayload(t, "Mine", "core://user/a"),
	}

	events := commentEvents(t, eu, docUUID)

	_, before, err := eu.Store.CreateComment(ctx, mine, docUUID, "")
	test.Mustf(t, err, "create my comment")

	expectCommentEvents(t, events,
		internal.CommentEventThreadCreated,
		internal.CommentEventCommentCreated)

	// The author edits; mentions are derived again.
	edited, err := eu.Store.UpdateComment(ctx, mine.UUID, commentAuthor,
		commentPayload(t, "Edited", "core://user/c", "core://application/bot"))
	test.Mustf(t, err, "edit my comment")

	if !slices.Equal([]string{"core://application/bot", "core://user/c"}, edited.Mentioned) {
		t.Fatalf("edited mentions %v", edited.Mentioned)
	}

	test.Equalf(t, true, edited.Updated.After(edited.Created), "an edit marks the comment edited")

	got := expectCommentEvents(t, events, internal.CommentEventCommentUpdated)

	test.Equalf(t, docUUID, got[0].DocumentUUID, "update event document")
	test.Equalf(t, commentAuthor, got[0].Actor, "update event actor")

	if !slices.Equal(edited.Mentioned, got[0].Mentioned) {
		t.Fatalf("update event mentions %v", got[0].Mentioned)
	}

	thread, err := eu.Store.GetThread(ctx, threadUUID)
	test.Mustf(t, err, "get the thread")

	test.Equalf(t, true, thread.Updated.After(before.Updated), "an edit bumps the thread")

	var body struct {
		Content []struct {
			Data map[string]string `json:"data"`
		} `json:"content"`
	}

	err = json.Unmarshal(thread.Comments[0].Payload, &body)
	test.Mustf(t, err, "decode the stored payload")
	test.Equalf(t, "Edited", body.Content[0].Data["text"], "the payload is replaced")

	mentions, err := eu.Store.ListMentions(ctx, "core://user/a", time.Time{}, 10)
	test.Mustf(t, err, "list the old mention")
	test.Equalf(t, 0, len(mentions), "the edit removed the old mention")

	mentions, err = eu.Store.ListMentions(ctx, "core://user/c", time.Time{}, 10)
	test.Mustf(t, err, "list the new mention")
	test.Equalf(t, 1, len(mentions), "the edit added the new mention")

	// Somebody else may not edit it.
	_, err = eu.Store.UpdateComment(ctx, mine.UUID, commentOther,
		commentPayload(t, "Hijacked"))
	if !errors.Is(err, internal.ErrNotCommentAuthor) {
		t.Fatalf("edit by another: got %v, want ErrNotCommentAuthor", err)
	}

	_, err = eu.Store.UpdateComment(ctx, uuid.New(), commentAuthor,
		commentPayload(t, "Nothing"))
	if !errors.Is(err, internal.ErrCommentNotFound) {
		t.Fatalf("edit of an unknown comment: got %v, want ErrCommentNotFound", err)
	}

	// Nor delete it without the flag.
	err = eu.Store.DeleteComment(ctx, mine.UUID, commentOther, false)
	if !errors.Is(err, internal.ErrNotCommentAuthor) {
		t.Fatalf("delete by another: got %v, want ErrNotCommentAuthor", err)
	}

	expectCommentEvents(t, events)

	// The author deletes; a second delete is a no-op.
	err = eu.Store.DeleteComment(ctx, mine.UUID, commentAuthor, false)
	test.Mustf(t, err, "delete my comment")

	got = expectCommentEvents(t, events, internal.CommentEventCommentDeleted)
	test.Equalf(t, commentAuthor, got[0].Actor, "delete event actor")
	test.Equalf(t, 0, len(got[0].Mentioned), "delete event mentions")

	err = eu.Store.DeleteComment(ctx, mine.UUID, commentAuthor, false)
	test.Mustf(t, err, "delete my comment again")

	expectCommentEvents(t, events)

	thread, err = eu.Store.GetThread(ctx, threadUUID)
	test.Mustf(t, err, "get the thread after the delete")

	deleted := thread.Comments[0]

	test.Equalf(t, int64(0), thread.LiveComments, "no live comments")
	test.Equalf(t, 1, len(thread.Comments), "the deleted comment stays")
	test.Equalf(t, commentAuthor, deleted.DeletedBy, "deleted by")
	test.Equalf(t, 0, len(deleted.Payload), "a deleted comment has no payload")
	test.Equalf(t, 0, len(deleted.Mentioned), "a deleted comment has no mentions")

	if deleted.Deleted == nil {
		t.Fatal("the comment is not marked deleted")
	}

	mentions, err = eu.Store.ListMentions(ctx, "core://user/c", time.Time{}, 10)
	test.Mustf(t, err, "list mentions of a deleted comment")
	test.Equalf(t, 0, len(mentions), "a deleted comment mentions nobody")

	// An edit of a deleted comment is refused.
	_, err = eu.Store.UpdateComment(ctx, mine.UUID, commentAuthor,
		commentPayload(t, "Resurrected"))
	if !errors.Is(err, internal.ErrCommentDeleted) {
		t.Fatalf("edit of a deleted comment: got %v, want ErrCommentDeleted", err)
	}

	// With the flag, another's comment may be deleted.
	theirs := internal.Comment{
		UUID:       uuid.New(),
		ThreadUUID: threadUUID,
		Author:     commentAuthor,
		Payload:    commentPayload(t, "Theirs"),
	}

	_, _, err = eu.Store.CreateComment(ctx, theirs, docUUID, "")
	test.Mustf(t, err, "create another comment")

	expectCommentEvents(t, events, internal.CommentEventCommentCreated)

	err = eu.Store.DeleteComment(ctx, theirs.UUID, commentOther, true)
	test.Mustf(t, err, "delete another's comment with the flag")

	got = expectCommentEvents(t, events, internal.CommentEventCommentDeleted)
	test.Equalf(t, commentOther, got[0].Actor, "admin delete actor")

	err = eu.Store.DeleteComment(ctx, uuid.New(), commentAuthor, true)
	if !errors.Is(err, internal.ErrCommentNotFound) {
		t.Fatalf("delete of an unknown comment: got %v, want ErrCommentNotFound", err)
	}
}

func TestCommentsResolveReopen(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	docUUID := uuid.New()
	threadUUID := uuid.New()

	events := commentEvents(t, eu, docUUID)

	_, _, err := eu.Store.CreateComment(ctx, internal.Comment{
		UUID:       uuid.New(),
		ThreadUUID: threadUUID,
		Author:     commentAuthor,
		Payload:    commentPayload(t, "Resolve me"),
	}, docUUID, "")
	test.Mustf(t, err, "create a comment")

	expectCommentEvents(t, events,
		internal.CommentEventThreadCreated,
		internal.CommentEventCommentCreated)

	// Reopening an open thread is a no-op.
	open, err := eu.Store.ReopenThread(ctx, threadUUID, commentOther)
	test.Mustf(t, err, "reopen an open thread")

	if open.Resolved != nil {
		t.Fatal("the thread is resolved")
	}

	expectCommentEvents(t, events)

	resolved, err := eu.Store.ResolveThread(ctx, threadUUID, commentOther)
	test.Mustf(t, err, "resolve the thread")

	if resolved.Resolved == nil {
		t.Fatal("the thread is not resolved")
	}

	test.Equalf(t, commentOther, resolved.ResolvedBy, "resolved by")
	test.Equalf(t, true, resolved.Updated.Equal(*resolved.Resolved), "resolve bumps the thread")

	got := expectCommentEvents(t, events, internal.CommentEventThreadResolved)
	test.Equalf(t, commentOther, got[0].Actor, "resolve event actor")

	// Resolving again is a no-op that keeps the first resolve.
	again, err := eu.Store.ResolveThread(ctx, threadUUID, commentAuthor)
	test.Mustf(t, err, "resolve the thread again")

	test.Equalf(t, commentOther, again.ResolvedBy, "the first resolve stands")

	expectCommentEvents(t, events)

	reopened, err := eu.Store.ReopenThread(ctx, threadUUID, commentAuthor)
	test.Mustf(t, err, "reopen the thread")

	if reopened.Resolved != nil || reopened.ResolvedBy != "" {
		t.Fatal("the thread is still resolved")
	}

	test.Equalf(t, true, reopened.Updated.After(resolved.Updated), "reopen bumps the thread")

	got = expectCommentEvents(t, events, internal.CommentEventThreadReopened)
	test.Equalf(t, commentAuthor, got[0].Actor, "reopen event actor")

	_, err = eu.Store.ResolveThread(ctx, uuid.New(), commentAuthor)
	if !errors.Is(err, internal.ErrCommentThreadNotFound) {
		t.Fatalf("resolve an unknown thread: got %v, want ErrCommentThreadNotFound", err)
	}
}

func TestCommentsListThreadsAndMentions(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	docUUID := uuid.New()
	otherDoc := uuid.New()
	mentioned := "core://user/mentioned"

	threads := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}

	var commentUUIDs []uuid.UUID

	post := func(thread uuid.UUID, doc uuid.UUID, mentions ...string) uuid.UUID {
		t.Helper()

		c := internal.Comment{
			UUID:       uuid.New(),
			ThreadUUID: thread,
			Author:     commentAuthor,
			AuthorName: "Author",
			Payload:    commentPayload(t, "Hello", mentions...),
		}

		_, _, err := eu.Store.CreateComment(ctx, c, doc, "")
		test.Mustf(t, err, "post a comment")

		return c.UUID
	}

	// Threads 0, 1, 2 created in order, then thread 0 gets a second
	// comment, which makes it the most recently updated.
	for _, th := range threads {
		commentUUIDs = append(commentUUIDs, post(th, docUUID, mentioned))
	}

	commentUUIDs = append(commentUUIDs, post(threads[0], docUUID, mentioned))

	// A thread on another document does not show up.
	otherComment := post(uuid.New(), otherDoc, mentioned)

	// Thread 1 is resolved, and its only comment deleted.
	err := eu.Store.DeleteComment(ctx, commentUUIDs[1], commentAuthor, false)
	test.Mustf(t, err, "delete the comment in thread 1")

	_, err = eu.Store.ResolveThread(ctx, threads[1], commentAuthor)
	test.Mustf(t, err, "resolve thread 1")

	open, err := eu.Store.ListThreads(ctx, docUUID, false)
	test.Mustf(t, err, "list open threads")

	test.Equalf(t, 2, len(open), "open threads")
	test.Equalf(t, threads[0], open[0].UUID, "most recently updated first")
	test.Equalf(t, threads[2], open[1].UUID, "then the next")
	test.Equalf(t, int64(2), open[0].LiveComments, "live comments in thread 0")
	test.Equalf(t, 2, len(open[0].Comments), "comments in thread 0")
	test.Equalf(t, commentUUIDs[0], open[0].Comments[0].UUID, "oldest comment first")
	test.Equalf(t, commentUUIDs[3], open[0].Comments[1].UUID, "newest comment last")

	all, err := eu.Store.ListThreads(ctx, docUUID, true)
	test.Mustf(t, err, "list all threads")

	test.Equalf(t, 3, len(all), "all threads")
	test.Equalf(t, threads[1], all[0].UUID, "the resolve bumped thread 1 to the top")
	test.Equalf(t, int64(0), all[0].LiveComments, "thread 1 has no live comments")
	test.Equalf(t, 1, len(all[0].Comments), "thread 1 keeps its deleted comment")
	test.Equalf(t, 0, len(all[0].Comments[0].Payload), "without its payload")

	none, err := eu.Store.ListThreads(ctx, uuid.New(), true)
	test.Mustf(t, err, "list threads on a document without any")
	test.Equalf(t, 0, len(none), "no threads")

	// Mentions: newest first, live comments only, paged on created.
	page, err := eu.Store.ListMentions(ctx, mentioned, time.Time{}, 2)
	test.Mustf(t, err, "list the first page of mentions")

	test.Equalf(t, 2, len(page), "first page")
	test.Equalf(t, otherComment, page[0].CommentUUID, "newest mention first")
	test.Equalf(t, otherDoc, page[0].DocumentUUID, "mention document")
	test.Equalf(t, commentUUIDs[3], page[1].CommentUUID, "then the next")
	test.Equalf(t, threads[0], page[1].ThreadUUID, "mention thread")
	test.Equalf(t, commentAuthor, page[1].Author, "mention author")
	test.Equalf(t, "Author", page[1].AuthorName, "mention author name")

	rest, err := eu.Store.ListMentions(ctx, mentioned, page[1].Created, 10)
	test.Mustf(t, err, "list the rest of the mentions")

	var restUUIDs []uuid.UUID
	for _, m := range rest {
		restUUIDs = append(restUUIDs, m.CommentUUID)
	}

	// Thread 1's comment is deleted and left out.
	want := []uuid.UUID{commentUUIDs[2], commentUUIDs[0]}

	if !slices.Equal(want, restUUIDs) {
		t.Fatalf("got the rest %v, want %v", restUUIDs, want)
	}

	// A mention is a reference: the type has no payload, and the JSON a
	// handler would build from it carries none.
	data, err := json.Marshal(page[0])
	test.Mustf(t, err, "marshal a mention")

	var fields map[string]json.RawMessage

	err = json.Unmarshal(data, &fields)
	test.Mustf(t, err, "decode a mention")

	if _, ok := fields["Payload"]; ok {
		t.Fatal("a mention carries a payload")
	}

	nobody, err := eu.Store.ListMentions(ctx, "core://user/nobody", time.Time{}, 10)
	test.Mustf(t, err, "list mentions of somebody never mentioned")
	test.Equalf(t, 0, len(nobody), "no mentions")
}
