package internal_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	userv1 "github.com/ttab/elephant-api/elephant/user/v1"
	"github.com/ttab/elephant-user/internal"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/rpc"
	"github.com/ttab/elephantine/test"
)

const notificationsPath = "/elephant.user.v1.NotificationsService/"

func notificationClaims(
	subject string, scope string, org string, units ...string,
) elephantine.JWTClaims {
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

// expectInvalidArgument checks that err is invalid_argument naming the
// argument in its error metadata, the contract the rpc helpers keep on
// both stacks.
func expectInvalidArgument(t *testing.T, err error, argument string) {
	t.Helper()

	test.IsRPCError(t, err, connect.CodeInvalidArgument)
	test.Equalf(t, argument, rpc.Meta(err)["argument"],
		"the error names the argument")
}

func subscriptionDocuments(subs []*userv1.Subscription) []string {
	out := make([]string, len(subs))
	for i, s := range subs {
		out[i] = s.DocumentUuid
	}

	return out
}

func notificationEvents(rows []*userv1.Notification) []string {
	out := make([]string, len(rows))
	for i, n := range rows {
		out[i] = n.EventUuid
	}

	return out
}

func expiresNear(t *testing.T, sub *userv1.Subscription, want time.Time) {
	t.Helper()

	got, err := time.Parse(time.RFC3339Nano, sub.Expires)
	test.Mustf(t, err, "parse expires of %s", sub.DocumentUuid)

	if d := got.Sub(want).Abs(); d > time.Minute {
		t.Fatalf("expires of %s is %s, want about %s",
			sub.DocumentUuid, sub.Expires, want.Format(time.RFC3339))
	}
}

func TestNotificationsSubscribe(t *testing.T) {
	regenerate := os.Getenv("REGENERATE") == "true"
	testData := filepath.Join("..", "testdata")

	eu := startElephantUser(t)
	ctx := t.Context()
	client := eu.Notifications

	const (
		alice = "core://user/alice"
		bob   = "core://user/bob"

		docDefault   = "a1000000-0000-4000-8000-000000000001"
		docTTL       = "a1000000-0000-4000-8000-000000000002"
		docUntil     = "a1000000-0000-4000-8000-000000000003"
		docPermanent = "a1000000-0000-4000-8000-000000000004"
		docAllowed   = "a1000000-0000-4000-8000-000000000005"
		docRefused   = "a1000000-0000-4000-8000-000000000006"
	)

	aliceCtx := bearerContext(ctx, eu.AccessToken(t,
		notificationClaims("alice", "user", "core://org/test")))
	adminCtx := bearerContext(ctx, eu.AccessToken(t,
		notificationClaims("admin", "user doc_admin", "core://org/test")))
	producerCtx := bearerContext(ctx, eu.AccessToken(t,
		notificationClaims("producer", "user_notify", "")))

	until := time.Now().Add(2 * time.Hour).UTC().Truncate(time.Second)

	t.Run("self with each lifetime", func(t *testing.T) {
		res, err := client.Subscribe(aliceCtx, connect.NewRequest(&userv1.SubscribeRequest{
			Items: []*userv1.SubscriptionItem{
				{DocumentUuid: docDefault},
				{
					DocumentUuid: docTTL,
					Lifetime:     &userv1.SubscriptionItem_TtlSeconds{TtlSeconds: 3600},
				},
				{
					DocumentUuid: docUntil,
					Lifetime: &userv1.SubscriptionItem_Until{
						Until: until.Format(time.RFC3339),
					},
				},
				{
					DocumentUuid: docPermanent,
					Lifetime:     &userv1.SubscriptionItem_Permanent{Permanent: true},
				},
			},
		}))
		test.Mustf(t, err, "subscribe with each lifetime")

		subs := res.Msg.Subscriptions

		expectStrings(t,
			[]string{docDefault, docTTL, docUntil, docPermanent},
			subscriptionDocuments(subs), "subscriptions by document")

		for _, s := range subs {
			test.Equalf(t, alice, s.User, "the caller is subscribed")
		}

		now := time.Now()

		expiresNear(t, subs[0], now.Add(internal.DefaultSubscriptionTTL))
		expiresNear(t, subs[1], now.Add(time.Hour))
		test.Equalf(t, until.Format(time.RFC3339Nano), subs[2].Expires,
			"until is stored as given")
		test.Equalf(t, "", subs[3].Expires, "a permanent subscription has no expiry")

		test.MessageAgainstGolden(t, regenerate, res.Msg,
			filepath.Join(testData, "subscribe_1.json"),
			test.IgnoreTimestamps{Fields: []string{"expires"}})

		listed, err := client.ListSubscriptions(aliceCtx,
			connect.NewRequest(&userv1.ListSubscriptionsRequest{}))
		test.Mustf(t, err, "list own subscriptions")

		expectStrings(t, subscriptionDocuments(subs),
			subscriptionDocuments(listed.Msg.Subscriptions),
			"the list answers the stored subscriptions")

		test.MessageAgainstGolden(t, regenerate, listed.Msg,
			filepath.Join(testData, "list_subscriptions_1.json"),
			test.IgnoreTimestamps{Fields: []string{"expires"}})

		// Naming oneself is the same as leaving user empty.
		named, err := client.ListSubscriptions(aliceCtx,
			connect.NewRequest(&userv1.ListSubscriptionsRequest{User: alice}))
		test.Mustf(t, err, "list own subscriptions by name")
		test.Equalf(t, 4, len(named.Msg.Subscriptions), "own subscriptions by name")
	})

	t.Run("invalid lifetimes and arguments", func(t *testing.T) {
		refused := func(argument string, items ...*userv1.SubscriptionItem) {
			t.Helper()

			_, err := client.Subscribe(aliceCtx,
				connect.NewRequest(&userv1.SubscribeRequest{Items: items}))
			expectInvalidArgument(t, err, argument)
		}

		refused("items.0.ttl_seconds", &userv1.SubscriptionItem{
			DocumentUuid: docDefault,
			Lifetime:     &userv1.SubscriptionItem_TtlSeconds{TtlSeconds: -1},
		})

		refused("items.1.ttl_seconds", &userv1.SubscriptionItem{DocumentUuid: docDefault},
			&userv1.SubscriptionItem{
				DocumentUuid: docTTL,
				Lifetime:     &userv1.SubscriptionItem_TtlSeconds{TtlSeconds: 0},
			})

		refused("items.0.until", &userv1.SubscriptionItem{
			DocumentUuid: docDefault,
			Lifetime: &userv1.SubscriptionItem_Until{
				Until: time.Now().Add(-time.Minute).Format(time.RFC3339),
			},
		})

		refused("items.0.until", &userv1.SubscriptionItem{
			DocumentUuid: docDefault,
			Lifetime:     &userv1.SubscriptionItem_Until{Until: "tomorrow"},
		})

		refused("items.0.document_uuid", &userv1.SubscriptionItem{})

		refused("items.0.document_uuid", &userv1.SubscriptionItem{DocumentUuid: "nope"})

		refused("items")

		_, err := client.Subscribe(aliceCtx, connect.NewRequest(&userv1.SubscribeRequest{
			User:  "core://unit/test",
			Items: []*userv1.SubscriptionItem{{DocumentUuid: docDefault}},
		}))
		expectInvalidArgument(t, err, "user")

		// Nothing of the refused requests was written.
		listed, err := client.ListSubscriptions(aliceCtx,
			connect.NewRequest(&userv1.ListSubscriptionsRequest{}))
		test.Mustf(t, err, "list own subscriptions")

		expectStrings(t,
			[]string{docDefault, docTTL, docUntil, docPermanent},
			subscriptionDocuments(listed.Msg.Subscriptions),
			"subscriptions after the refused requests")
	})

	t.Run("another user", func(t *testing.T) {
		eu.Repository.Grant(docRefused, alice, "r")

		_, err := client.Subscribe(aliceCtx, connect.NewRequest(&userv1.SubscribeRequest{
			User: bob,
			Items: []*userv1.SubscriptionItem{
				{DocumentUuid: docAllowed},
				{DocumentUuid: docRefused},
			},
		}))
		test.IsRPCError(t, err, connect.CodePermissionDenied)

		listed, err := client.ListSubscriptions(adminCtx,
			connect.NewRequest(&userv1.ListSubscriptionsRequest{User: bob}))
		test.Mustf(t, err, "list bob's subscriptions as admin")
		test.Equalf(t, 0, len(listed.Msg.Subscriptions),
			"one refused document writes nothing")

		res, err := client.Subscribe(aliceCtx, connect.NewRequest(&userv1.SubscribeRequest{
			User: bob,
			Items: []*userv1.SubscriptionItem{
				{
					DocumentUuid: docAllowed,
					Lifetime:     &userv1.SubscriptionItem_Permanent{Permanent: true},
				},
			},
		}))
		test.Mustf(t, err, "subscribe bob where alice may write")

		test.Equalf(t, 1, len(res.Msg.Subscriptions), "bob's subscriptions")
		test.Equalf(t, bob, res.Msg.Subscriptions[0].User, "bob is subscribed")

		_, err = client.ListSubscriptions(aliceCtx,
			connect.NewRequest(&userv1.ListSubscriptionsRequest{User: bob}))
		test.IsRPCError(t, err, connect.CodePermissionDenied)

		listed, err = client.ListSubscriptions(adminCtx,
			connect.NewRequest(&userv1.ListSubscriptionsRequest{User: bob}))
		test.Mustf(t, err, "list bob's subscriptions as admin")

		expectStrings(t, []string{docAllowed},
			subscriptionDocuments(listed.Msg.Subscriptions),
			"bob's subscriptions as admin sees them")

		_, err = client.Unsubscribe(aliceCtx, connect.NewRequest(&userv1.UnsubscribeRequest{
			User:          bob,
			DocumentUuids: []string{docAllowed, docRefused},
		}))
		test.IsRPCError(t, err, connect.CodePermissionDenied)

		_, err = client.Unsubscribe(aliceCtx, connect.NewRequest(&userv1.UnsubscribeRequest{
			User:          bob,
			DocumentUuids: []string{docAllowed},
		}))
		test.Mustf(t, err, "unsubscribe bob where alice may write")

		listed, err = client.ListSubscriptions(adminCtx,
			connect.NewRequest(&userv1.ListSubscriptionsRequest{User: bob}))
		test.Mustf(t, err, "list bob's subscriptions after the unsubscribe")
		test.Equalf(t, 0, len(listed.Msg.Subscriptions), "bob is unsubscribed")
	})

	t.Run("another user in the other uri forms", func(t *testing.T) {
		// A token's subject can be a keycloak user or an application as
		// well as a core user; all three are users to subscribe.
		for _, other := range []string{
			"keycloak://user/f00dcafe-0000-4000-8000-000000000001",
			"core://application/agent",
		} {
			res, err := client.Subscribe(aliceCtx, connect.NewRequest(&userv1.SubscribeRequest{
				User:  other,
				Items: []*userv1.SubscriptionItem{{DocumentUuid: docAllowed}},
			}))
			test.Mustf(t, err, "subscribe %s where alice may write", other)

			test.Equalf(t, 1, len(res.Msg.Subscriptions), "subscriptions of %s", other)
			test.Equalf(t, other, res.Msg.Subscriptions[0].User, "subscribed user")

			listed, err := client.ListSubscriptions(adminCtx,
				connect.NewRequest(&userv1.ListSubscriptionsRequest{User: other}))
			test.Mustf(t, err, "list the subscriptions of %s as admin", other)

			expectStrings(t, []string{docAllowed},
				subscriptionDocuments(listed.Msg.Subscriptions),
				"subscriptions of "+other)
		}

		_, err := client.Subscribe(aliceCtx, connect.NewRequest(&userv1.SubscribeRequest{
			User:  "keycloak://user/",
			Items: []*userv1.SubscriptionItem{{DocumentUuid: docAllowed}},
		}))
		expectInvalidArgument(t, err, "user")
	})

	t.Run("unsubscribe self", func(t *testing.T) {
		_, err := client.Unsubscribe(aliceCtx, connect.NewRequest(&userv1.UnsubscribeRequest{
			DocumentUuids: []string{docDefault, docTTL, docRefused},
		}))
		test.Mustf(t, err, "unsubscribe self")

		listed, err := client.ListSubscriptions(aliceCtx,
			connect.NewRequest(&userv1.ListSubscriptionsRequest{}))
		test.Mustf(t, err, "list own subscriptions")

		expectStrings(t, []string{docUntil, docPermanent},
			subscriptionDocuments(listed.Msg.Subscriptions),
			"subscriptions left after the unsubscribe")

		_, err = client.Unsubscribe(aliceCtx,
			connect.NewRequest(&userv1.UnsubscribeRequest{}))
		expectInvalidArgument(t, err, "document_uuids")

		_, err = client.Unsubscribe(aliceCtx, connect.NewRequest(&userv1.UnsubscribeRequest{
			DocumentUuids: []string{"nope"},
		}))
		expectInvalidArgument(t, err, "document_uuids.0")
	})

	t.Run("scope", func(t *testing.T) {
		_, err := client.Subscribe(producerCtx, connect.NewRequest(&userv1.SubscribeRequest{
			Items: []*userv1.SubscriptionItem{{DocumentUuid: docDefault}},
		}))
		test.IsRPCError(t, err, connect.CodePermissionDenied)

		_, err = client.ListSubscriptions(ctx,
			connect.NewRequest(&userv1.ListSubscriptionsRequest{}))
		test.IsRPCError(t, err, connect.CodeUnauthenticated)
	})
}

func TestNotificationsIngest(t *testing.T) {
	regenerate := os.Getenv("REGENERATE") == "true"
	testData := filepath.Join("..", "testdata")

	eu := startElephantUser(t)
	ctx := t.Context()
	client := eu.Notifications

	const (
		alice = "core://user/alice"
		bob   = "core://user/bob"
		desk  = "core://unit/desk"

		// Wire events are about the wire itself: readers hear of them
		// through a subscription to the wire's uuid.
		wireWatched = "a2000000-0000-4000-8000-000000000001"
		wireQuiet   = "a2000000-0000-4000-8000-000000000002"
		article     = "a2000000-0000-4000-8000-000000000003"

		eventWatched = "e2000000-0000-4000-8000-000000000001"
		eventDesk    = "e2000000-0000-4000-8000-000000000002"
		eventQuiet   = "e2000000-0000-4000-8000-000000000003"
		threadRef    = "b2000000-0000-4000-8000-000000000001"
	)

	aliceCtx := bearerContext(ctx, eu.AccessToken(t,
		notificationClaims("alice", "user", "core://org/test", desk)))
	bobCtx := bearerContext(ctx, eu.AccessToken(t,
		notificationClaims("bob", "user", "")))
	adminCtx := bearerContext(ctx, eu.AccessToken(t,
		notificationClaims("admin", "user doc_admin", "core://org/test")))
	producerCtx := bearerContext(ctx, eu.AccessToken(t,
		notificationClaims("producer", "user_notify", "")))

	for _, c := range []context.Context{aliceCtx, bobCtx} {
		_, err := client.Subscribe(c, connect.NewRequest(&userv1.SubscribeRequest{
			Items: []*userv1.SubscriptionItem{{DocumentUuid: wireWatched}},
		}))
		test.Mustf(t, err, "subscribe to the watched document")
	}

	list := func(c context.Context) []*userv1.Notification {
		t.Helper()

		res, err := client.ListNotifications(c,
			connect.NewRequest(&userv1.ListNotificationsRequest{}))
		test.Mustf(t, err, "list notifications")

		return res.Msg.Notifications
	}

	watched := &userv1.IngestRequest{
		DocumentUuid: wireWatched,
		DocumentType: "tt/wire",
		Kind:         "se.tt.wires.updated",
		EventUuid:    eventWatched,
		Meta:         map[string]string{"headline": "Wires updated"},
	}

	t.Run("without a recipient", func(t *testing.T) {
		_, err := client.Ingest(producerCtx, connect.NewRequest(watched))
		test.Mustf(t, err, "ingest an event about the watched document")

		aliceRows := list(aliceCtx)
		bobRows := list(bobCtx)

		test.Equalf(t, 1, len(aliceRows), "alice's rows")
		test.Equalf(t, 1, len(bobRows), "bob's rows")
		test.Equalf(t, alice, aliceRows[0].Key, "alice's row key")
		test.Equalf(t, bob, bobRows[0].Key, "bob's row key")
		test.Equalf(t, "core://user/producer", aliceRows[0].Actor,
			"the actor defaults to the caller")

		test.MessageAgainstGolden(t, regenerate,
			&userv1.ListNotificationsResponse{Notifications: aliceRows},
			filepath.Join(testData, "list_notifications_1.json"),
			test.IgnoreTimestamps{Fields: []string{"created"}})

		// The idempotent repeat writes nothing new.
		_, err = client.Ingest(producerCtx, connect.NewRequest(watched))
		test.Mustf(t, err, "repeat the event")

		test.Equalf(t, 1, len(list(aliceCtx)), "alice's rows after the repeat")
		test.Equalf(t, 1, len(list(bobCtx)), "bob's rows after the repeat")
	})

	t.Run("with a recipient", func(t *testing.T) {
		_, err := client.Ingest(producerCtx, connect.NewRequest(&userv1.IngestRequest{
			DocumentUuid: wireWatched,
			Kind:         "se.tt.jobs.finished",
			Actor:        "core://application/jobs",
			EventUuid:    eventDesk,
			Recipient:    desk,
		}))
		test.Mustf(t, err, "ingest an event for the desk")

		aliceRows := list(aliceCtx)

		expectStrings(t, []string{eventDesk, eventWatched},
			notificationEvents(aliceRows),
			"alice reads the desk's row and her own, newest first")
		test.Equalf(t, desk, aliceRows[0].Key, "the desk row's key")
		test.Equalf(t, "core://application/jobs", aliceRows[0].Actor,
			"the actor as given")

		expectStrings(t, []string{eventWatched}, notificationEvents(list(bobCtx)),
			"the document's subscribers get nothing from a recipient event")
	})

	t.Run("with a keycloak user recipient", func(t *testing.T) {
		const (
			kcUser  = "keycloak://user/f00dcafe-0000-4000-8000-000000000002"
			eventKC = "e2000000-0000-4000-8000-000000000004"
		)

		_, err := client.Ingest(producerCtx, connect.NewRequest(&userv1.IngestRequest{
			DocumentUuid: wireWatched,
			Kind:         "se.tt.jobs.finished",
			EventUuid:    eventKC,
			Recipient:    kcUser,
		}))
		test.Mustf(t, err, "ingest an event for a keycloak user")

		// elephantine keeps a sub claim with a scheme as is, so the
		// keycloak user reads the row under its own subject.
		kcCtx := bearerContext(ctx, eu.AccessToken(t,
			notificationClaims(kcUser, "user", "")))

		rows := list(kcCtx)

		expectStrings(t, []string{eventKC}, notificationEvents(rows),
			"the keycloak user's rows")
		test.Equalf(t, kcUser, rows[0].Key, "the row's key")
	})

	t.Run("a document without subscribers", func(t *testing.T) {
		_, err := client.Ingest(producerCtx, connect.NewRequest(&userv1.IngestRequest{
			DocumentUuid: wireQuiet,
			DocumentType: "tt/wire",
			Kind:         "se.tt.wires.updated",
			EventUuid:    eventQuiet,
		}))
		test.Mustf(t, err, "ingest an event nobody subscribes to")

		var count int64

		err = eu.DB.QueryRow(ctx,
			"SELECT count(*) FROM notification WHERE event_uuid = $1",
			eventQuiet).Scan(&count)
		test.Mustf(t, err, "count the rows of the event")
		test.Equalf(t, int64(0), count, "nothing written")
	})

	t.Run("scope", func(t *testing.T) {
		for _, c := range []context.Context{aliceCtx, adminCtx} {
			_, err := client.Ingest(c, connect.NewRequest(watched))
			test.IsRPCError(t, err, connect.CodePermissionDenied)
		}
	})

	t.Run("arguments", func(t *testing.T) {
		valid := func() *userv1.IngestRequest {
			return &userv1.IngestRequest{
				DocumentUuid: wireWatched,
				DocumentType: "tt/wire",
				Kind:         "se.tt.wires.updated",
				EventUuid:    "e2000000-0000-4000-8000-000000000099",
			}
		}

		// Refs belong to the comment-shaped events: a comment on an
		// article refers to its thread and comment.
		commentShaped := func(r *userv1.IngestRequest) {
			r.DocumentUuid = article
			r.DocumentType = "core/article"
			r.Kind = "se.ecms.user.comments.created"
		}

		cases := []struct {
			argument string
			change   func(r *userv1.IngestRequest)
		}{
			{"document_uuid", func(r *userv1.IngestRequest) { r.DocumentUuid = "" }},
			{"document_uuid", func(r *userv1.IngestRequest) { r.DocumentUuid = "nope" }},
			{"kind", func(r *userv1.IngestRequest) { r.Kind = "" }},
			{"kind", func(r *userv1.IngestRequest) { r.Kind = "updated" }},
			{"event_uuid", func(r *userv1.IngestRequest) { r.EventUuid = "" }},
			{"event_uuid", func(r *userv1.IngestRequest) { r.EventUuid = "nope" }},
			{"recipient", func(r *userv1.IngestRequest) { r.Recipient = "core://group/x" }},
			{"recipient", func(r *userv1.IngestRequest) { r.Recipient = "core://user/" }},
			{"refs.0.uuid", func(r *userv1.IngestRequest) {
				commentShaped(r)
				r.Refs = []*userv1.Reference{{Rel: "thread", Uuid: "nope"}}
			}},
			{"refs.0.rel", func(r *userv1.IngestRequest) {
				commentShaped(r)
				r.Refs = []*userv1.Reference{{Uuid: threadRef}}
			}},
		}

		for _, c := range cases {
			req := valid()
			c.change(req)

			_, err := client.Ingest(producerCtx, connect.NewRequest(req))
			expectInvalidArgument(t, err, c.argument)
		}
	})
}

func TestNotificationsList(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()
	client := eu.Notifications

	const (
		wire = "a3000000-0000-4000-8000-000000000001"

		alice = "core://user/alice"
		unitA = "core://unit/a"
		unitB = "core://unit/b"
		org   = "core://org/x"
	)

	aliceCtx := bearerContext(ctx, eu.AccessToken(t,
		notificationClaims("alice", "user", org, unitA, unitB)))
	producerCtx := bearerContext(ctx, eu.AccessToken(t,
		notificationClaims("producer", "user_notify", "")))

	// One event per key the caller reads, and one under a key it does
	// not. Each ingest commits before the next, so created is ascending.
	keys := []string{alice, unitA, "core://unit/other", unitB, org, alice}

	var mine []string

	for i, key := range keys {
		event := fmt.Sprintf("e3000000-0000-4000-8000-%012d", i)

		_, err := client.Ingest(producerCtx, connect.NewRequest(&userv1.IngestRequest{
			DocumentUuid: wire,
			DocumentType: "tt/wire",
			Kind:         "se.tt.wires.updated",
			EventUuid:    event,
			Recipient:    key,
		}))
		test.Mustf(t, err, "ingest event %d", i)

		if key != "core://unit/other" {
			mine = append([]string{event}, mine...)
		}

		time.Sleep(2 * time.Millisecond)
	}

	listPage := func(before string, limit int32) []*userv1.Notification {
		t.Helper()

		res, err := client.ListNotifications(aliceCtx,
			connect.NewRequest(&userv1.ListNotificationsRequest{
				Before: before,
				Limit:  limit,
			}))
		test.Mustf(t, err, "list notifications before %q", before)

		return res.Msg.Notifications
	}

	all := listPage("", 0)

	expectStrings(t, mine, notificationEvents(all),
		"every key's rows, newest first")

	var (
		paged  []string
		before string
	)

	for range 10 {
		page := listPage(before, 2)
		if len(page) == 0 {
			break
		}

		if len(page) > 2 {
			t.Fatalf("got %d rows for a limit of 2", len(page))
		}

		paged = append(paged, notificationEvents(page)...)
		before = page[len(page)-1].Created
	}

	expectStrings(t, mine, paged, "paging on created")

	_, err := client.ListNotifications(aliceCtx,
		connect.NewRequest(&userv1.ListNotificationsRequest{Limit: -1}))
	expectInvalidArgument(t, err, "limit")

	_, err = client.ListNotifications(aliceCtx,
		connect.NewRequest(&userv1.ListNotificationsRequest{Before: "yesterday"}))
	expectInvalidArgument(t, err, "before")

	t.Run("limit clamping", func(t *testing.T) {
		const bulkKey = "core://user/bulk"

		for i := range 210 {
			_, err := eu.Store.InsertNotifications(ctx, internal.NotificationSourceEvent{
				Family:       internal.NotificationFamilyIngest,
				Kind:         "se.tt.wires.updated",
				DocumentUUID: uuid.MustParse(wire),
				DocumentType: "tt/wire",
				Actor:        "core://application/test",
				EventUUID:    uuid.New(),
			}, []string{bulkKey})
			test.Mustf(t, err, "insert bulk row %d", i)
		}

		bulkCtx := bearerContext(ctx, eu.AccessToken(t,
			notificationClaims("bulk", "user", "")))

		for _, c := range []struct {
			limit int32
			want  int
		}{
			{0, 50},
			{10, 10},
			{200, 200},
			{500, 200},
		} {
			res, err := client.ListNotifications(bulkCtx,
				connect.NewRequest(&userv1.ListNotificationsRequest{Limit: c.limit}))
			test.Mustf(t, err, "list with limit %d", c.limit)
			test.Equalf(t, c.want, len(res.Msg.Notifications),
				"rows for limit %d", c.limit)
		}
	})
}

// TestNotificationsWakeup checks that an ingested row sends its {key, id}
// wakeup on the notification_update channel.
func TestNotificationsWakeup(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	const recipient = "core://user/woken"

	producerCtx := bearerContext(ctx, eu.AccessToken(t,
		notificationClaims("producer", "user_notify", "")))
	recipientCtx := bearerContext(ctx, eu.AccessToken(t,
		notificationClaims("woken", "user", "")))

	events := make(chan internal.NotificationEvent, 8)

	go eu.Store.Notifications.Listen(ctx, events,
		func(e internal.NotificationEvent) bool {
			return e.Key == recipient
		})

	// Let the listener register before the write.
	time.Sleep(50 * time.Millisecond)

	_, err := eu.Notifications.Ingest(producerCtx, connect.NewRequest(&userv1.IngestRequest{
		DocumentUuid: "a4000000-0000-4000-8000-000000000001",
		DocumentType: "tt/wire",
		Kind:         "se.tt.wires.updated",
		EventUuid:    "e4000000-0000-4000-8000-000000000001",
		Recipient:    recipient,
	}))
	test.Mustf(t, err, "ingest an event")

	var got internal.NotificationEvent

	select {
	case got = <-events:
	case <-time.After(5 * time.Second):
		t.Fatal("no wakeup for the ingested row")
	}

	res, err := eu.Notifications.ListNotifications(recipientCtx,
		connect.NewRequest(&userv1.ListNotificationsRequest{}))
	test.Mustf(t, err, "list the recipient's rows")
	test.Equalf(t, 1, len(res.Msg.Notifications), "the recipient's rows")

	row := res.Msg.Notifications[0]

	test.Equalf(t, row.Key, got.Key, "wakeup key")
	test.Equalf(t, row.Id, got.ID, "wakeup id")
}

// TestNotificationsBodies pins the raw Connect JSON a caller gets from the
// notifications service: field names in protojson's lowerCamelCase and the
// Connect error envelope.
func TestNotificationsBodies(t *testing.T) {
	regenerate := os.Getenv("REGENERATE") == "true"
	dataDir := filepath.Join("..", "testdata", t.Name())

	eu := startElephantUser(t)

	userToken := eu.AccessToken(t, userClaims("tester", "user"))
	producerToken := eu.AccessToken(t, notificationClaims("producer", "user_notify", ""))

	scrub := func(res rpcResponse, list string, field string) rpcResponse {
		items, _ := res.Body[list].([]any)
		for _, item := range items {
			obj, ok := item.(map[string]any)
			if ok {
				delete(obj, field)
			}
		}

		return res
	}

	subscribe := scrub(eu.postJSON(t, userToken,
		notificationsPath+"Subscribe",
		`{"items":[{"document_uuid":"a5000000-0000-4000-8000-000000000001",`+
			`"ttl_seconds":"3600"},{"document_uuid":`+
			`"a5000000-0000-4000-8000-000000000002","permanent":true}]}`,
		nil), "subscriptions", "expires")

	test.AgainstGolden(t, regenerate, subscribe,
		filepath.Join(dataDir, "subscribe-connect.json"))

	ingest := eu.postJSON(t, producerToken,
		notificationsPath+"Ingest",
		`{"document_uuid":"a5000000-0000-4000-8000-000000000001",`+
			`"document_type":"core/article",`+
			`"kind":"se.ecms.user.comments.created",`+
			`"refs":[{"rel":"thread","uuid":"b5000000-0000-4000-8000-000000000001"},`+
			`{"rel":"comment","uuid":"c5000000-0000-4000-8000-000000000001"}],`+
			`"event_uuid":"e5000000-0000-4000-8000-000000000001",`+
			`"meta":{"excerpt":"Ask Anna."}}`,
		nil)

	test.AgainstGolden(t, regenerate, ingest,
		filepath.Join(dataDir, "ingest-connect.json"))

	list := scrub(eu.postJSON(t, userToken,
		notificationsPath+"ListNotifications", `{}`, nil),
		"notifications", "created")

	test.AgainstGolden(t, regenerate, list,
		filepath.Join(dataDir, "list-notifications-connect.json"))

	refused := eu.postJSON(t, userToken,
		notificationsPath+"Ingest",
		`{"document_uuid":"a5000000-0000-4000-8000-000000000003",`+
			`"document_type":"tt/wire","kind":"se.tt.wires.updated",`+
			`"event_uuid":"e5000000-0000-4000-8000-000000000002"}`,
		nil)

	test.AgainstGolden(t, regenerate, refused,
		filepath.Join(dataDir, "ingest-permission-denied-connect.json"))
}
