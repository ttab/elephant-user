package internal_test

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/ttab/elephant-user/internal"
	"github.com/ttab/elephantine/test"
)

const (
	feedUser  = "core://user/reader"
	feedOther = "core://user/other"
	feedUnit  = "core://unit/desk"
)

// feedEvent is an ingest event about a new document.
func feedEvent() internal.NotificationSourceEvent {
	return internal.NotificationSourceEvent{
		Family:       internal.NotificationFamilyIngest,
		Kind:         "se.ecms.local.test.updated",
		DocumentUUID: uuid.New(),
		DocumentType: "core/article",
		Actor:        "core://application/test",
		EventUUID:    uuid.New(),
	}
}

// expiresAround fails unless got is within a second of want.
func expiresAround(t *testing.T, want time.Time, got *time.Time, what string) {
	t.Helper()

	if got == nil {
		t.Fatalf("%s: no expiry, want %s", what, want.Format(time.RFC3339))
	}

	d := got.Sub(want)
	if d < -time.Second || d > time.Second {
		t.Fatalf("%s: expires %s, want %s", what,
			got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

func TestNotificationsSubscriptions(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	var (
		defaultDoc   = uuid.New()
		ttlDoc       = uuid.New()
		untilDoc     = uuid.New()
		permanentDoc = uuid.New()
		expiredDoc   = uuid.New()
	)

	until := time.Now().Add(72 * time.Hour).Truncate(time.Microsecond)
	past := time.Now().Add(-time.Minute)

	subs, err := eu.Store.Subscribe(ctx, feedUser, []internal.SubscriptionItem{
		{DocumentUUID: defaultDoc},
		{DocumentUUID: ttlDoc, TTL: time.Hour},
		{DocumentUUID: untilDoc, Until: &until},
		{DocumentUUID: permanentDoc, Permanent: true},
		{DocumentUUID: expiredDoc, Until: &past},
	})
	test.Mustf(t, err, "subscribe")

	test.Equalf(t, 5, len(subs), "subscriptions answered")

	listed, err := eu.Store.ListSubscriptions(ctx, feedUser)
	test.Mustf(t, err, "list subscriptions")

	test.Equalf(t, 4, len(listed), "live subscriptions, the expired one left out")

	byDoc := make(map[uuid.UUID]internal.Subscription)
	for _, s := range listed {
		byDoc[s.DocumentUUID] = s
	}

	now := time.Now()

	expiresAround(t, now.Add(internal.DefaultSubscriptionTTL),
		byDoc[defaultDoc].Expires, "default ttl")
	expiresAround(t, now.Add(time.Hour), byDoc[ttlDoc].Expires, "explicit ttl")
	expiresAround(t, until, byDoc[untilDoc].Expires, "until")

	if byDoc[permanentDoc].Expires != nil {
		t.Fatalf("permanent subscription expires %s", byDoc[permanentDoc].Expires)
	}

	if _, ok := byDoc[expiredDoc]; ok {
		t.Fatal("expired subscription listed as live")
	}

	subscribers, err := eu.Store.ListDocumentSubscribers(ctx, expiredDoc)
	test.Mustf(t, err, "list subscribers of the expired document")

	test.Equalf(t, 0, len(subscribers), "subscribers of the expired document")

	// Subscribing again refreshes: the permanent subscription becomes a
	// TTL one and the expired one comes back.
	_, err = eu.Store.Subscribe(ctx, feedUser, []internal.SubscriptionItem{
		{DocumentUUID: permanentDoc, TTL: 2 * time.Hour},
		{DocumentUUID: expiredDoc},
	})
	test.Mustf(t, err, "subscribe again")

	listed, err = eu.Store.ListSubscriptions(ctx, feedUser)
	test.Mustf(t, err, "list refreshed subscriptions")

	test.Equalf(t, 5, len(listed), "live subscriptions after the refresh")

	for _, s := range listed {
		byDoc[s.DocumentUUID] = s
	}

	now = time.Now()

	expiresAround(t, now.Add(2*time.Hour), byDoc[permanentDoc].Expires,
		"refreshed permanent subscription")
	expiresAround(t, now.Add(internal.DefaultSubscriptionTTL),
		byDoc[expiredDoc].Expires, "refreshed expired subscription")

	_, err = eu.Store.Subscribe(ctx, feedOther, []internal.SubscriptionItem{
		{DocumentUUID: ttlDoc, Permanent: true},
	})
	test.Mustf(t, err, "subscribe another subject")

	subscribers, err = eu.Store.ListDocumentSubscribers(ctx, ttlDoc)
	test.Mustf(t, err, "list subscribers")

	test.EqualDiff(t, []string{feedOther, feedUser}, subscribers,
		"subscribers, sorted")

	err = eu.Store.Unsubscribe(ctx, feedUser, []uuid.UUID{ttlDoc, uuid.New()})
	test.Mustf(t, err, "unsubscribe")

	subscribers, err = eu.Store.ListDocumentSubscribers(ctx, ttlDoc)
	test.Mustf(t, err, "list subscribers after unsubscribe")

	test.EqualDiff(t, []string{feedOther}, subscribers,
		"subscribers after unsubscribe")

	for _, item := range []internal.SubscriptionItem{
		{DocumentUUID: ttlDoc, TTL: time.Hour, Permanent: true},
		{DocumentUUID: ttlDoc, TTL: time.Hour, Until: &until},
		{DocumentUUID: ttlDoc, Until: &until, Permanent: true},
		{DocumentUUID: ttlDoc, TTL: -time.Hour},
	} {
		_, err := eu.Store.Subscribe(ctx, feedUser,
			[]internal.SubscriptionItem{item})
		if !errors.Is(err, internal.ErrInvalidSubscriptionLifetime) {
			t.Fatalf("subscribe with %+v: got %v, want an invalid lifetime",
				item, err)
		}
	}
}

func TestNotificationsDefaultSubscriptionTTL(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	metrics, err := internal.NewMetrics(prometheus.NewRegistry())
	test.Mustf(t, err, "create metrics")

	store := internal.NewPGStore(
		slog.New(test.NewLogHandler(t, slog.LevelInfo)), eu.DB, metrics,
		internal.WithSubscriptionTTL(time.Hour))

	doc := uuid.New()

	subs, err := store.Subscribe(ctx, feedUser, []internal.SubscriptionItem{
		{DocumentUUID: doc},
	})
	test.Mustf(t, err, "subscribe")

	expiresAround(t, time.Now().Add(time.Hour), subs[0].Expires,
		"configured default ttl")
}

func TestNotificationsCursor(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	cursors, err := eu.Store.GetNotificationCursors(ctx, feedUser)
	test.Mustf(t, err, "get cursors before any bump")

	test.Equalf(t, 0, len(cursors), "cursors before any bump")
	test.Equalf(t, int64(0), cursors[feedUser], "an absent cursor reads as zero")

	for _, position := range []int64{5, 3, 7, 6} {
		err := eu.Store.BumpNotificationCursor(ctx, feedUser, feedUser, position)
		test.Mustf(t, err, "bump to %d", position)
	}

	err = eu.Store.BumpNotificationCursor(ctx, feedUser, feedUnit, 2)
	test.Mustf(t, err, "bump the unit cursor")

	err = eu.Store.BumpNotificationCursor(ctx, feedOther, feedUser, 100)
	test.Mustf(t, err, "bump another subject's cursor")

	cursors, err = eu.Store.GetNotificationCursors(ctx, feedUser)
	test.Mustf(t, err, "get cursors")

	test.EqualDiff(t, map[string]int64{feedUser: 7, feedUnit: 2}, cursors,
		"cursors only move forward, per subject and key")

	// Pods bumping the same cursor at once leave the highest position.
	var (
		start = make(chan struct{})
		wg    sync.WaitGroup
	)

	for i := range 20 {
		wg.Go(func() {
			<-start

			err := eu.Store.BumpNotificationCursor(
				ctx, feedUser, feedUnit, int64(10+i))
			if err != nil {
				t.Errorf("bump %d: %v", i, err)
			}
		})
	}

	close(start)
	wg.Wait()

	cursors, err = eu.Store.GetNotificationCursors(ctx, feedUser)
	test.Mustf(t, err, "get cursors after racing bumps")

	test.Equalf(t, int64(29), cursors[feedUnit], "position after racing bumps")
}

func TestNotificationsReads(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	first := feedEvent()
	first.Refs = []internal.NotificationReference{
		{Rel: "thread", UUID: uuid.NewString(), Type: "core/comment-thread"},
	}
	first.Meta = map[string]string{"headline": "First"}

	written, err := eu.Store.InsertNotifications(ctx, first,
		[]string{feedUser, feedUnit, feedOther})
	test.Mustf(t, err, "write the first event")

	test.Equalf(t, 3, len(written), "rows written for the first event")

	for range 4 {
		_, err := eu.Store.InsertNotifications(ctx, feedEvent(),
			[]string{feedUser})
		test.Mustf(t, err, "write a user event")
	}

	all, err := eu.Store.ListNotificationsAfterID(ctx, feedUser, 0,
		time.Time{}, 100)
	test.Mustf(t, err, "list every user row")

	test.Equalf(t, 5, len(all), "user rows")
	test.EqualDiff(t, first.Refs, all[0].Refs, "refs read back")
	test.EqualDiff(t, first.Meta, all[0].Meta, "meta read back")
	test.Equalf(t, 0, len(all[1].Refs), "refs of an event without any")
	test.Equalf(t, 0, len(all[1].Meta), "meta of an event without any")

	// A replay window that starts just after the first event.
	windowed, err := eu.Store.ListNotificationsAfterID(ctx, feedUser, 0,
		written[0].Created.Add(time.Microsecond), 100)
	test.Mustf(t, err, "list the user rows in the window")

	test.Equalf(t, 4, len(windowed), "rows inside the replay window")
	test.Equalf(t, int64(2), windowed[0].ID, "first row inside the window")

	page, err := eu.Store.ListNotificationsAfterID(ctx, feedUser, 2,
		time.Time{}, 2)
	test.Mustf(t, err, "list a page after id 2")

	test.EqualDiff(t, []int64{3, 4}, []int64{page[0].ID, page[1].ID},
		"page after id 2")

	// The first event has one created time under all three keys, so a
	// page boundary falls between rows that tie on created.
	keys := []string{feedUser, feedUnit, feedOther}
	tied := written[0].Created

	newest, err := eu.Store.ListNotificationsBefore(ctx, keys,
		all[1].Created, "", 0, 2)
	test.Mustf(t, err, "list the newest page of the tied rows")

	test.Equalf(t, 2, len(newest), "rows on the first page")
	test.Equalf(t, feedUser, newest[0].Key, "ties broken on key, descending")
	test.Equalf(t, feedOther, newest[1].Key, "second tied row")

	last := newest[len(newest)-1]

	next, err := eu.Store.ListNotificationsBefore(ctx, keys,
		last.Created, last.Key, last.ID, 10)
	test.Mustf(t, err, "list the page after the tied rows")

	test.Equalf(t, 1, len(next), "rows on the next page")
	test.Equalf(t, feedUnit, next[0].Key, "the tied row the first page left")
	test.Equalf(t, first.EventUUID, next[0].EventUUID, "the tied row's event")

	strictly, err := eu.Store.ListNotificationsBefore(ctx, keys,
		tied, "", 0, 10)
	test.Mustf(t, err, "list rows strictly before the tied time")

	test.Equalf(t, 0, len(strictly), "rows strictly before the tied time")

	everything, err := eu.Store.ListNotificationsBefore(ctx, keys,
		time.Now().Add(time.Minute), "", 0, 100)
	test.Mustf(t, err, "list every row")

	test.Equalf(t, 7, len(everything), "rows across the keys")

	for i := 1; i < len(everything); i++ {
		if everything[i].Created.After(everything[i-1].Created) {
			t.Fatalf("row %d is newer than the row before it", i)
		}
	}
}

func TestNotificationsCleaner(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	reg := prometheus.NewRegistry()

	metrics, err := internal.NewMetrics(reg)
	test.Mustf(t, err, "create metrics")

	store := internal.NewPGStore(
		slog.New(test.NewLogHandler(t, slog.LevelInfo)), eu.DB, metrics,
		internal.WithNotificationRetention(time.Hour))

	kept := feedEvent()

	_, err = store.InsertNotifications(ctx, kept, []string{feedUser})
	test.Mustf(t, err, "write a recent row")

	// More old rows than one cleaner batch, so the cleaner has to loop.
	const oldRows = 2500

	_, err = eu.DB.Exec(ctx, `
INSERT INTO notification (key, id, kind, document_uuid, document_type,
       actor, event_uuid, created)
SELECT 'core://unit/old', n, 'se.ecms.local.test.updated', gen_random_uuid(),
       'core/article', 'core://application/test', gen_random_uuid(),
       now() - INTERVAL '2 hours'
FROM generate_series(1, $1::bigint) AS n`, oldRows)
	test.Mustf(t, err, "write old rows")

	past := time.Now().Add(-time.Minute)
	liveDoc := uuid.New()
	permanentDoc := uuid.New()

	_, err = store.Subscribe(ctx, feedUser, []internal.SubscriptionItem{
		{DocumentUUID: uuid.New(), Until: &past},
		{DocumentUUID: uuid.New(), Until: &past},
		{DocumentUUID: liveDoc},
		{DocumentUUID: permanentDoc, Permanent: true},
	})
	test.Mustf(t, err, "subscribe")

	cleanerCtx, cancel := context.WithCancel(ctx)

	var (
		cleanerWG  sync.WaitGroup
		cleanerErr error
	)

	cleanerWG.Go(func() {
		cleanerErr = store.RunCleaner(cleanerCtx, time.Hour, reg)
	})

	deleted := func(item string) float64 {
		return testutil.ToFloat64(metrics.CleanerDeletions.WithLabelValues(item))
	}

	deadline := time.Now().Add(20 * time.Second)

	for deleted(internal.CleanerDeletedNotification) < oldRows ||
		deleted(internal.CleanerDeletedSubscription) < 2 {
		if time.Now().After(deadline) {
			break
		}

		time.Sleep(50 * time.Millisecond)
	}

	cancel()
	cleanerWG.Wait()

	// Cancelling the job lock surfaces as an error; the app's stop scope
	// treats it as a clean exit.
	if cleanerErr != nil && !errors.Is(cleanerErr, context.Canceled) {
		t.Fatalf("run the cleaner: %v", cleanerErr)
	}

	test.Equalf(t, float64(oldRows), deleted(internal.CleanerDeletedNotification),
		"notifications counted as deleted")
	test.Equalf(t, float64(2), deleted(internal.CleanerDeletedSubscription),
		"subscriptions counted as deleted")

	var remaining int64

	err = eu.DB.QueryRow(ctx, `SELECT count(*) FROM notification`).Scan(&remaining)
	test.Mustf(t, err, "count remaining rows")

	test.Equalf(t, int64(1), remaining, "rows left after the cleaner")

	rows, err := store.ListNotificationsAfterID(ctx, feedUser, 0, time.Time{}, 10)
	test.Mustf(t, err, "list the kept row")

	test.Equalf(t, 1, len(rows), "kept rows")
	test.Equalf(t, kept.EventUUID, rows[0].EventUUID, "the recent row is kept")

	var subscriptions int64

	err = eu.DB.QueryRow(ctx,
		`SELECT count(*) FROM notification_subscription`).Scan(&subscriptions)
	test.Mustf(t, err, "count remaining subscriptions")

	test.Equalf(t, int64(2), subscriptions,
		"the live and the permanent subscription are kept")
}
