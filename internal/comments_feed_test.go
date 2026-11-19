package internal_test

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"
	userv1 "github.com/ttab/elephant-api/elephant/user/v1"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephantine/test"
)

// feedWatcher reads one user's feed through ListNotifications and hands
// out the rows it has not seen before, oldest first.
type feedWatcher struct {
	eu   TestElephantUser
	ctx  context.Context
	seen map[string]bool
}

func newFeedWatcher(
	ctx context.Context, t *testing.T, eu TestElephantUser, subject string,
) *feedWatcher {
	t.Helper()

	return &feedWatcher{
		eu: eu,
		ctx: bearerContext(ctx, eu.AccessToken(t,
			commentClaims(subject, "user", ""))),
		seen: make(map[string]bool),
	}
}

// next returns the rows written since the last call, oldest first.
func (w *feedWatcher) next(t *testing.T) []*userv1.Notification {
	t.Helper()

	res, err := w.eu.Notifications.ListNotifications(w.ctx,
		connect.NewRequest(&userv1.ListNotificationsRequest{Limit: 200}))
	test.Mustf(t, err, "list notifications")

	var fresh []*userv1.Notification

	for _, n := range res.Msg.Notifications {
		id := n.Key + "/" + n.EventUuid
		if w.seen[id] {
			continue
		}

		w.seen[id] = true

		fresh = append(fresh, n)
	}

	slices.Reverse(fresh)

	return fresh
}

// expectNone checks that nothing new is in the feed.
func (w *feedWatcher) expectNone(t *testing.T, msg string) {
	t.Helper()

	rows := w.next(t)
	if len(rows) != 0 {
		t.Fatalf("%s: got %d new rows, the first %s", msg, len(rows), rows[0].Kind)
	}
}

// expectOne checks that exactly one new row is in the feed and returns it.
func (w *feedWatcher) expectOne(t *testing.T, msg string) *userv1.Notification {
	t.Helper()

	rows := w.next(t)
	if len(rows) != 1 {
		t.Fatalf("%s: got %d new rows, want 1", msg, len(rows))
	}

	return rows[0]
}

// feedDoc is the document the comment feed test comments on.
const feedDoc = "a7000000-0000-4000-8000-000000000001"

// expectCommentRow checks a comment feed row: its key, kind, actor and
// refs, given as "rel=uuid" in order, and that it is about feedDoc.
func expectCommentRow(
	t *testing.T, n *userv1.Notification,
	key string, kind string, actor string, refs ...string,
) {
	t.Helper()

	test.Equalf(t, key, n.Key, "row key")
	test.Equalf(t, kind, n.Kind, "row kind")
	test.Equalf(t, feedDoc, n.DocumentUuid, "row document")
	test.Equalf(t, "", n.DocumentType, "no document type for comment rows")
	test.Equalf(t, actor, n.Actor, "row actor")
	test.Equalf(t, 0, len(n.Meta), "no meta on comment rows")

	got := make([]string, len(n.Refs))
	for i, r := range n.Refs {
		got[i] = r.Rel + "=" + r.Uuid
	}

	expectStrings(t, refs, got, "row refs")
}

// TestCommentsFeed checks the feed rows comment writes leave: the kind,
// keys and refs of a create, a mention, edits without and with a new
// mention, a delete, a resolve, a reopen and a reopen by posting, that the
// actor never gets a row for their own write, and that a reader who is
// both subscribed and mentioned gets one row.
func TestCommentsFeed(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	const (
		doc    = feedDoc
		thread = "b7000000-0000-4000-8000-000000000001"

		comment1 = "c7000000-0000-4000-8000-000000000001"
		comment2 = "c7000000-0000-4000-8000-000000000002"
		comment3 = "c7000000-0000-4000-8000-000000000003"
		comment4 = "c7000000-0000-4000-8000-000000000004"

		alice  = "core://user/alice"
		bob    = "core://user/bob"
		reader = "core://user/reader"
		anna   = "core://user/anna"
		bo     = "core://user/bo"

		created  = "se.ecms.user.comments.created"
		updated  = "se.ecms.user.comments.updated"
		deleted  = "se.ecms.user.comments.deleted"
		resolved = "se.ecms.user.comments.resolved"
		reopened = "se.ecms.user.comments.reopened"
	)

	aliceCtx := bearerContext(ctx, eu.AccessToken(t,
		commentClaims("alice", "user", "Alice")))
	bobCtx := bearerContext(ctx, eu.AccessToken(t,
		commentClaims("bob", "user", "Bob")))

	aliceFeed := newFeedWatcher(ctx, t, eu, "alice")
	readerFeed := newFeedWatcher(ctx, t, eu, "reader")
	annaFeed := newFeedWatcher(ctx, t, eu, "anna")
	boFeed := newFeedWatcher(ctx, t, eu, "bo")
	bobFeed := newFeedWatcher(ctx, t, eu, "bob")

	// Alice and the reader follow the document, the others do not.
	for _, w := range []*feedWatcher{aliceFeed, readerFeed} {
		_, err := eu.Notifications.Subscribe(w.ctx,
			connect.NewRequest(&userv1.SubscribeRequest{
				Items: []*userv1.SubscriptionItem{{DocumentUuid: doc}},
			}))
		test.Mustf(t, err, "subscribe to the document")
	}

	t.Run("create with mentions", func(t *testing.T) {
		_, err := eu.Comments.CreateComment(aliceCtx, &user.CreateCommentRequest{
			Uuid:         comment1,
			ThreadUuid:   thread,
			DocumentUuid: doc,
			Payload:      rpcCommentPayload("Check with", anna, reader),
		})
		test.Mustf(t, err, "create the first comment")

		refs := []string{
			"thread=" + thread, "comment=" + comment1,
			"mention=" + anna, "mention=" + reader,
		}

		r := readerFeed.expectOne(t, "subscribed and mentioned reader")
		expectCommentRow(t, r, reader, created, alice, refs...)

		a := annaFeed.expectOne(t, "mentioned user")
		expectCommentRow(t, a, anna, created, alice, refs...)

		test.Equalf(t, r.EventUuid, a.EventUuid, "one event under both keys")

		aliceFeed.expectNone(t, "the actor's own create")
		boFeed.expectNone(t, "neither subscribed nor mentioned")
	})

	t.Run("repeat of the create", func(t *testing.T) {
		_, err := eu.Comments.CreateComment(aliceCtx, &user.CreateCommentRequest{
			Uuid:         comment1,
			ThreadUuid:   thread,
			DocumentUuid: doc,
			Payload:      rpcCommentPayload("Check with", anna, reader),
		})
		test.Mustf(t, err, "repeat the first comment")

		readerFeed.expectNone(t, "a repeat writes nothing")
		annaFeed.expectNone(t, "a repeat writes nothing")
	})

	t.Run("edit without a new mention", func(t *testing.T) {
		_, err := eu.Comments.UpdateComment(aliceCtx, &user.UpdateCommentRequest{
			Uuid:    comment1,
			Payload: rpcCommentPayload("Please check with", anna),
		})
		test.Mustf(t, err, "edit the comment")

		expectCommentRow(t, readerFeed.expectOne(t, "subscriber"),
			reader, updated, alice,
			"thread="+thread, "comment="+comment1)

		annaFeed.expectNone(t, "an edit does not mention anna again")
		aliceFeed.expectNone(t, "the actor's own edit")
	})

	t.Run("edit with a new mention", func(t *testing.T) {
		_, err := eu.Comments.UpdateComment(aliceCtx, &user.UpdateCommentRequest{
			Uuid:    comment1,
			Payload: rpcCommentPayload("Please check with", anna, bo),
		})
		test.Mustf(t, err, "edit the comment")

		refs := []string{
			"thread=" + thread, "comment=" + comment1, "mention=" + bo,
		}

		expectCommentRow(t, readerFeed.expectOne(t, "subscriber"),
			reader, updated, alice, refs...)
		expectCommentRow(t, boFeed.expectOne(t, "newly mentioned"),
			bo, updated, alice, refs...)

		annaFeed.expectNone(t, "already mentioned")
		aliceFeed.expectNone(t, "the actor's own edit")
	})

	t.Run("another author posts", func(t *testing.T) {
		_, err := eu.Comments.CreateComment(bobCtx, &user.CreateCommentRequest{
			Uuid:       comment2,
			ThreadUuid: thread,
			Payload:    rpcCommentPayload("On it."),
		})
		test.Mustf(t, err, "answer in the thread")

		refs := []string{"thread=" + thread, "comment=" + comment2}

		expectCommentRow(t, aliceFeed.expectOne(t, "subscribed alice"),
			alice, created, bob, refs...)
		expectCommentRow(t, readerFeed.expectOne(t, "subscriber"),
			reader, created, bob, refs...)

		bobFeed.expectNone(t, "the actor's own post")
		annaFeed.expectNone(t, "not mentioned this time")
	})

	t.Run("delete", func(t *testing.T) {
		_, err := eu.Comments.DeleteComment(aliceCtx,
			&user.DeleteCommentRequest{Uuid: comment1})
		test.Mustf(t, err, "delete the comment")

		expectCommentRow(t, readerFeed.expectOne(t, "subscriber"),
			reader, deleted, alice,
			"thread="+thread, "comment="+comment1)

		aliceFeed.expectNone(t, "the actor's own delete")
		annaFeed.expectNone(t, "a delete addresses subscribers only")
		boFeed.expectNone(t, "a delete addresses subscribers only")

		_, err = eu.Comments.DeleteComment(aliceCtx,
			&user.DeleteCommentRequest{Uuid: comment1})
		test.Mustf(t, err, "delete the comment again")

		readerFeed.expectNone(t, "a repeated delete writes nothing")
	})

	t.Run("resolve and reopen", func(t *testing.T) {
		_, err := eu.Comments.ResolveThread(aliceCtx,
			&user.ResolveThreadRequest{Uuid: thread})
		test.Mustf(t, err, "resolve the thread")

		expectCommentRow(t, readerFeed.expectOne(t, "subscriber"),
			reader, resolved, alice, "thread="+thread)
		aliceFeed.expectNone(t, "the actor's own resolve")

		_, err = eu.Comments.ResolveThread(aliceCtx,
			&user.ResolveThreadRequest{Uuid: thread})
		test.Mustf(t, err, "resolve the thread again")

		readerFeed.expectNone(t, "a repeated resolve writes nothing")

		_, err = eu.Comments.ReopenThread(bobCtx,
			&user.ReopenThreadRequest{Uuid: thread})
		test.Mustf(t, err, "reopen the thread")

		expectCommentRow(t, readerFeed.expectOne(t, "subscriber"),
			reader, reopened, bob, "thread="+thread)
		expectCommentRow(t, aliceFeed.expectOne(t, "subscribed alice"),
			alice, reopened, bob, "thread="+thread)
		bobFeed.expectNone(t, "the actor's own reopen")
	})

	t.Run("reopen by posting", func(t *testing.T) {
		_, err := eu.Comments.ResolveThread(aliceCtx,
			&user.ResolveThreadRequest{Uuid: thread})
		test.Mustf(t, err, "resolve the thread")

		readerFeed.next(t)

		_, err = eu.Comments.CreateComment(bobCtx, &user.CreateCommentRequest{
			Uuid:       comment3,
			ThreadUuid: thread,
			Payload:    rpcCommentPayload("One more thing", anna),
		})
		test.Mustf(t, err, "post to the resolved thread")

		rows := readerFeed.next(t)
		if len(rows) != 2 {
			t.Fatalf("got %d rows for the subscriber, want 2", len(rows))
		}

		expectCommentRow(t, rows[0], reader, created, bob,
			"thread="+thread, "comment="+comment3, "mention="+anna)
		expectCommentRow(t, rows[1], reader, reopened, bob,
			"thread="+thread)

		if rows[0].Id >= rows[1].Id {
			t.Fatalf("created row %d is not before reopened row %d",
				rows[0].Id, rows[1].Id)
		}

		if rows[0].EventUuid == rows[1].EventUuid {
			t.Fatal("the two events share an event uuid")
		}

		expectCommentRow(t, annaFeed.expectOne(t, "mentioned, not subscribed"),
			anna, created, bob,
			"thread="+thread, "comment="+comment3, "mention="+anna)

		bobFeed.expectNone(t, "the actor's own post")
	})

	t.Run("live on a stream", func(t *testing.T) {
		stream := openStream(readerFeed.ctx, t, eu, &userv1.StreamRequest{})

		// Drain the replay of the rows above.
		for range readerFeed.seen {
			stream.row(t)
		}

		stream.quiet(t)

		_, err := eu.Comments.CreateComment(aliceCtx, &user.CreateCommentRequest{
			Uuid:       comment4,
			ThreadUuid: thread,
			Payload:    rpcCommentPayload("Live", reader),
		})
		test.Mustf(t, err, "post while the reader streams")

		expectCommentRow(t, stream.row(t), reader, created, alice,
			"thread="+thread, "comment="+comment4, "mention="+reader)
		stream.quiet(t)
	})
}
