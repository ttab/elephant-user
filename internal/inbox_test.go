package internal_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-api/user"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/test"
)

// TestInboxBroadcast covers the shared-row inbox: a message addressed to a
// unit or an org is one row read by every member under their own read
// state, a reader's poll and list span their sub, org and units in one id
// order, a retried push is answered with the existing message, and the
// scope rules for group recipients mirror shared settings documents.
func TestInboxBroadcast(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	const (
		desk  = "core://unit/desk"
		acme  = "core://org/acme"
		other = "core://unit/other"
	)

	claims := func(subject, scope, org string, units ...string) elephantine.JWTClaims {
		return elephantine.JWTClaims{
			Scope: scope,
			RegisteredClaims: jwt.RegisteredClaims{
				Issuer:  "test",
				Subject: subject,
			},
			Org:   org,
			Units: units,
		}
	}

	memberA := bearerContext(ctx, eu.AccessToken(t,
		claims("member-a", "user", acme, desk)))
	memberB := bearerContext(ctx, eu.AccessToken(t,
		claims("member-b", "user", acme, desk)))
	deskAdmin := bearerContext(ctx, eu.AccessToken(t,
		claims("desk-admin", "user doc_admin", acme, desk)))
	outsider := bearerContext(ctx, eu.AccessToken(t,
		claims("outsider", "user", "core://org/elsewhere", other)))
	otherAdmin := bearerContext(ctx, eu.AccessToken(t,
		claims("other-admin", "user doc_admin", "core://org/elsewhere", other)))

	push := func(
		ctx context.Context, recipient, id, title string,
	) (*user.PushInboxMessageResponse, error) {
		return eu.Messages.PushInboxMessage(ctx, &user.PushInboxMessageRequest{
			Recipient: recipient,
			Payload: &newsdoc.Document{
				Uuid:  "3b482036-39fb-584d-8477-" + id,
				Type:  "core/inbox-message",
				Uri:   "message://inbox/" + id,
				Title: title,
			},
		})
	}

	list := func(ctx context.Context, size int64) []*user.InboxMessage {
		t.Helper()

		res, err := eu.Messages.ListInboxMessages(ctx, &user.ListInboxMessagesRequest{
			Size: size,
		})
		test.Mustf(t, err, "list inbox messages")

		return res.Messages
	}

	ids := func(msgs []*user.InboxMessage) []int64 {
		out := make([]int64, len(msgs))
		for i, m := range msgs {
			out[i] = m.Id
		}

		return out
	}

	expectIDs := func(want []int64, got []int64, msg string) {
		t.Helper()

		if !slices.Equal(want, got) {
			t.Fatalf("%s: got %v, want %v", msg, got, want)
		}
	}

	// Group recipients follow the shared-write rule: doc_admin plus
	// membership. A plain member and an admin of another unit are both
	// refused.
	_, err := push(memberA, desk, "000000000001", "Desk notice")
	test.IsRPCError(t, err, connect.CodePermissionDenied)

	_, err = push(otherAdmin, desk, "000000000001", "Desk notice")
	test.IsRPCError(t, err, connect.CodePermissionDenied)

	deskMsg, err := push(deskAdmin, desk, "000000000001", "Desk notice")
	test.Mustf(t, err, "push to the unit")

	orgMsg, err := push(deskAdmin, acme, "000000000002", "Org notice")
	test.Mustf(t, err, "push to the org")

	// A user recipient needs only the user scope, as before.
	directMsg, err := push(memberA, "core://user/member-b", "000000000003", "Hello B")
	test.Mustf(t, err, "push a direct message")

	expectIDs([]int64{1, 2, 3}, []int64{deskMsg.Id, orgMsg.Id, directMsg.Id},
		"ids come from one commit-ordered counter across recipients")

	// A retried push is answered with the existing message; the same uuid
	// with a different payload is a conflict.
	again, err := push(deskAdmin, desk, "000000000001", "Desk notice")
	test.Mustf(t, err, "retry the unit push")
	test.Equalf(t, deskMsg.Id, again.Id, "retry answers with the existing id")

	_, err = push(deskAdmin, desk, "000000000001", "Desk notice, edited")
	test.IsRPCError(t, err, connect.CodeAlreadyExists)

	// Each reader sees what is addressed to their sub, org or units.
	expectIDs([]int64{3, 2, 1}, ids(list(memberB, 0)), "member B sees the direct, org and unit messages, newest first")
	expectIDs([]int64{2, 1}, ids(list(memberA, 0)), "member A sees the org and unit messages")
	expectIDs([]int64(nil), ids(list(outsider, 0)), "an outsider sees nothing")

	for _, m := range list(memberB, 0) {
		test.Equalf(t, false, m.IsRead, "message %d starts unread", m.Id)
		test.Equalf(t, m.Created, m.Updated, "message %d is unchanged since creation", m.Id)
	}

	test.Equalf(t, desk, list(memberB, 0)[2].Recipient,
		"a message names the recipient it was addressed to")

	// Read state is per reader: B marking the unit message read does not
	// touch A's view of the same row.
	_, err = eu.Messages.UpdateInboxMessage(memberB, &user.UpdateInboxMessageRequest{
		Id:     deskMsg.Id,
		IsRead: true,
	})
	test.Mustf(t, err, "mark the unit message read as B")

	test.Equalf(t, true, list(memberB, 0)[2].IsRead, "B sees the unit message as read")
	test.Equalf(t, false, list(memberA, 0)[1].IsRead, "A still sees it unread")

	// Delete hides for the caller only.
	_, err = eu.Messages.DeleteInboxMessage(memberB, &user.DeleteInboxMessageRequest{
		Id: orgMsg.Id,
	})
	test.Mustf(t, err, "delete the org message as B")

	expectIDs([]int64{3, 1}, ids(list(memberB, 0)), "B no longer sees the org message")
	expectIDs([]int64{2, 1}, ids(list(memberA, 0)), "A still does")

	// A message outside the caller's owners is not found, whatever it is.
	_, err = eu.Messages.UpdateInboxMessage(outsider, &user.UpdateInboxMessageRequest{
		Id:     deskMsg.Id,
		IsRead: true,
	})
	test.IsRPCError(t, err, connect.CodeNotFound)

	_, err = eu.Messages.DeleteInboxMessage(outsider, &user.DeleteInboxMessageRequest{
		Id: deskMsg.Id,
	})
	test.IsRPCError(t, err, connect.CodeNotFound)

	// The poll spans the same owners and skips what the caller hid; B has
	// two visible messages, so size 1 returns one and the cursor stops on
	// it, and a second poll after that id returns the other.
	polled, err := eu.Messages.PollInboxMessages(memberB, &user.PollInboxMessagesRequest{
		AfterId: 0,
		Size:    1,
	})
	test.Mustf(t, err, "poll as B with size 1")
	expectIDs([]int64{1}, ids(polled.Messages), "size limits the poll")
	test.Equalf(t, int64(1), polled.LastId, "last id is the last returned")

	polled, err = eu.Messages.PollInboxMessages(memberB, &user.PollInboxMessagesRequest{
		AfterId: polled.LastId,
		Size:    1,
	})
	test.Mustf(t, err, "poll as B after the first page")
	expectIDs([]int64{3}, ids(polled.Messages), "the next page skips the hidden message")

	// The list takes the same size; a value over the ceiling is served the
	// ceiling rather than refused.
	expectIDs([]int64{3}, ids(list(memberB, 1)), "size limits the list")
	expectIDs([]int64{3, 1}, ids(list(memberB, 500)), "an oversized size is clamped, not refused")

	// A poll started before a group push wakes up on it.
	woke := make(chan *user.PollInboxMessagesResponse, 1)

	go func() {
		res, err := eu.Messages.PollInboxMessages(memberA, &user.PollInboxMessagesRequest{
			AfterId: -1,
		})
		if err != nil {
			t.Errorf("poll as A: %v", err)
		}

		woke <- res
	}()

	time.Sleep(50 * time.Millisecond)

	lateMsg, err := push(deskAdmin, desk, "000000000004", "Late notice")
	test.Mustf(t, err, "push a late unit message")

	select {
	case res := <-woke:
		expectIDs([]int64{lateMsg.Id}, ids(res.Messages), "the waiting poll got the new message")
	case <-time.After(5 * time.Second):
		t.Fatal("the poll did not wake up on the unit push")
	}

	// PollEventLog takes the same size argument.
	for _, key := range []string{"one", "two"} {
		_, err = eu.Settings.UpdateDocument(memberA, &user.UpdateDocumentRequest{
			Application:   "se.ecms.local.test.inbox",
			Type:          "core/view-setting",
			Key:           key,
			SchemaVersion: "v1.0.0",
			Payload: &newsdoc.Document{
				Type:  "core/view-setting",
				Title: key,
			},
		})
		test.Mustf(t, err, "write settings document %q", key)
	}

	events, err := eu.Settings.PollEventLog(memberA, &user.PollEventLogRequest{
		AfterId: 0,
		Size:    1,
	})
	test.Mustf(t, err, "poll the eventlog with size 1")
	test.Equalf(t, 1, len(events.Entries), "size limits the eventlog poll")
}
