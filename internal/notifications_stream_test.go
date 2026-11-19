package internal_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	userv1 "github.com/ttab/elephant-api/elephant/user/v1"
	"github.com/ttab/elephant-user/internal"
	"github.com/ttab/elephantine/test"
)

// streamTimeout bounds every wait for something on a stream.
const streamTimeout = 5 * time.Second

// streamReader receives a stream's messages on a goroutine, so that a test
// can wait for them with a timeout. Notifications and keepalives arrive on
// separate channels; done is closed when the stream ends, and err then
// holds what it ended with.
type streamReader struct {
	rows       chan *userv1.Notification
	keepalives chan *userv1.Keepalive
	done       chan struct{}
	err        error
	cancel     context.CancelFunc
}

// openStream opens a stream with the caller's context and reads it until it
// ends. The stream is closed when the test ends.
func openStream(
	ctx context.Context, t *testing.T, eu TestElephantUser,
	req *userv1.StreamRequest,
) *streamReader {
	t.Helper()

	ctx, cancel := context.WithCancel(ctx)

	stream, err := eu.Notifications.Stream(ctx, connect.NewRequest(req))
	if err != nil {
		cancel()
		t.Fatalf("open the stream: %v", err)
	}

	r := streamReader{
		rows:       make(chan *userv1.Notification, 64),
		keepalives: make(chan *userv1.Keepalive, 64),
		done:       make(chan struct{}),
		cancel:     cancel,
	}

	go func() {
		defer close(r.done)

		for stream.Receive() {
			msg := stream.Msg()

			switch {
			case msg.GetNotification() != nil:
				r.rows <- msg.GetNotification()
			case msg.GetKeepalive() != nil:
				select {
				case r.keepalives <- msg.GetKeepalive():
				default:
				}
			}
		}

		r.err = stream.Err()

		closeErr := stream.Close()
		if closeErr != nil && r.err == nil {
			r.err = closeErr
		}
	}()

	t.Cleanup(func() {
		r.cancel()
		<-r.done
	})

	return &r
}

// row waits for the next notification.
func (r *streamReader) row(t *testing.T) *userv1.Notification {
	t.Helper()

	select {
	case n := <-r.rows:
		return n
	case <-r.done:
		t.Fatalf("the stream ended waiting for a row: %v", r.err)
	case <-time.After(streamTimeout):
		t.Fatal("no row on the stream")
	}

	return nil
}

// rowsInOrder waits for n notifications and returns their event uuids.
func (r *streamReader) rowsInOrder(t *testing.T, n int) []string {
	t.Helper()

	events := make([]string, n)

	for i := range events {
		events[i] = r.row(t).EventUuid
	}

	return events
}

// quiet checks that no notification arrives for a while.
func (r *streamReader) quiet(t *testing.T) {
	t.Helper()

	select {
	case n := <-r.rows:
		t.Fatalf("unexpected row %s/%d, event %s", n.Key, n.Id, n.EventUuid)
	case <-time.After(200 * time.Millisecond):
	}
}

// ended waits for the stream to end and returns its error.
func (r *streamReader) ended(t *testing.T, within time.Duration) error {
	t.Helper()

	select {
	case <-r.done:
		return r.err
	case <-time.After(within):
		t.Fatal("the stream did not end")
	}

	return nil
}

func streamEvent(i int) string {
	return fmt.Sprintf("e5000000-0000-4000-8000-%012d", i)
}

// insertRow writes a row for the event under the key through the store.
func insertRow(
	t *testing.T, eu TestElephantUser, key string, event string,
) internal.Notification {
	t.Helper()

	rows, err := eu.Store.InsertNotifications(t.Context(),
		internal.NotificationSourceEvent{
			Family:       internal.NotificationFamilyIngest,
			Kind:         "se.tt.wires.updated",
			DocumentUUID: uuid.MustParse("a5000000-0000-4000-8000-000000000001"),
			DocumentType: "tt/wire",
			Actor:        "core://application/test",
			EventUUID:    uuid.MustParse(event),
		}, []string{key})
	test.Mustf(t, err, "insert row for %s under %s", event, key)
	test.Equalf(t, 1, len(rows), "rows written for %s", event)

	return rows[0]
}

// ageRow moves a row's created time back, behind the store's back.
func ageRow(t *testing.T, eu TestElephantUser, event string, age time.Duration) {
	t.Helper()

	_, err := eu.DB.Exec(t.Context(),
		`UPDATE notification SET created = created - make_interval(secs => $2)
		 WHERE event_uuid = $1`, event, age.Seconds())
	test.Mustf(t, err, "age the row for %s", event)
}

// TestNotificationsStream covers the replay on start, the replay window, a
// unit key joining at now, the cursor bump, and live rows from Ingest with a
// recipient and through a subscription.
func TestNotificationsStream(t *testing.T) {
	eu := startElephantUser(t)
	ctx := t.Context()

	const (
		alice = "core://user/alice"
		desk  = "core://unit/desk"
		org   = "core://org/stream"
		wire  = "a5000000-0000-4000-8000-000000000002"
	)

	aliceCtx := bearerContext(ctx, eu.AccessToken(t,
		notificationClaims("alice", "user", org, desk)))
	producerCtx := bearerContext(ctx, eu.AccessToken(t,
		notificationClaims("producer", "user_notify", "")))

	// Before the first stream: one row under alice's key older than the
	// default window, two inside it, and a unit row the join skips.
	insertRow(t, eu, alice, streamEvent(0))
	ageRow(t, eu, streamEvent(0), 48*time.Hour)

	first := insertRow(t, eu, alice, streamEvent(1))
	second := insertRow(t, eu, alice, streamEvent(2))
	unitRow := insertRow(t, eu, desk, streamEvent(3))

	stream := openStream(aliceCtx, t, eu, &userv1.StreamRequest{})

	expectStrings(t, []string{streamEvent(1), streamEvent(2)},
		stream.rowsInOrder(t, 2), "replay inside the window, in id order")
	stream.quiet(t)

	cursors, err := eu.Store.GetNotificationCursors(ctx, alice)
	test.Mustf(t, err, "read alice's cursors")
	test.Equalf(t, second.ID, cursors[alice], "user cursor after the replay")
	test.Equalf(t, unitRow.ID, cursors[desk], "unit cursor joined at now")
	test.Equalf(t, int64(0), cursors[org], "org cursor joined without rows")
	test.Equalf(t, first.ID+1, second.ID, "consecutive ids")

	// Live: the listener is registered before the replay, so no sleep is
	// needed for it, but the convention stands for the stream's loop.
	time.Sleep(50 * time.Millisecond)

	_, err = eu.Notifications.Ingest(producerCtx, connect.NewRequest(&userv1.IngestRequest{
		DocumentUuid: wire,
		DocumentType: "tt/wire",
		Kind:         "se.tt.wires.updated",
		EventUuid:    streamEvent(4),
		Recipient:    desk,
	}))
	test.Mustf(t, err, "ingest to the unit")

	live := stream.row(t)
	test.Equalf(t, streamEvent(4), live.EventUuid, "live unit row")
	test.Equalf(t, desk, live.Key, "live unit row key")

	_, err = eu.Notifications.Subscribe(aliceCtx, connect.NewRequest(&userv1.SubscribeRequest{
		Items: []*userv1.SubscriptionItem{{DocumentUuid: wire}},
	}))
	test.Mustf(t, err, "subscribe alice to the wire")

	_, err = eu.Notifications.Ingest(producerCtx, connect.NewRequest(&userv1.IngestRequest{
		DocumentUuid: wire,
		DocumentType: "tt/wire",
		Kind:         "se.tt.wires.updated",
		EventUuid:    streamEvent(5),
	}))
	test.Mustf(t, err, "ingest to the wire's subscribers")

	live = stream.row(t)
	test.Equalf(t, streamEvent(5), live.EventUuid, "live subscription row")
	test.Equalf(t, alice, live.Key, "live subscription row key")
	stream.quiet(t)

	cursors, err = eu.Store.GetNotificationCursors(ctx, alice)
	test.Mustf(t, err, "read alice's cursors after the live rows")
	test.Equalf(t, live.Id, cursors[alice], "user cursor after the live row")
	test.Equalf(t, unitRow.ID+1, cursors[desk], "unit cursor after the live row")

	stream.cancel()
	test.IsRPCError(t, stream.ended(t, streamTimeout), connect.CodeCanceled)

	// While no stream is open: a unit row, which the next stream replays
	// now that the unit has a cursor.
	insertRow(t, eu, desk, streamEvent(6))

	stream = openStream(aliceCtx, t, eu, &userv1.StreamRequest{})

	expectStrings(t, []string{streamEvent(6)}, stream.rowsInOrder(t, 1),
		"replay from the cursors")
	stream.quiet(t)
	stream.cancel()
	test.IsRPCError(t, stream.ended(t, streamTimeout), connect.CodeCanceled)

	t.Run("replay window", func(t *testing.T) {
		const carol = "core://user/carol"

		carolCtx := bearerContext(ctx, eu.AccessToken(t,
			notificationClaims("carol", "user", "")))

		insertRow(t, eu, carol, streamEvent(10))
		ageRow(t, eu, streamEvent(10), 5*24*time.Hour)
		insertRow(t, eu, carol, streamEvent(11))
		ageRow(t, eu, streamEvent(11), 48*time.Hour)
		insertRow(t, eu, carol, streamEvent(12))

		// Three days: the two-day-old row and the new one.
		stream := openStream(carolCtx, t, eu, &userv1.StreamRequest{
			ReplayWindowSeconds: 3 * 24 * 3600,
		})

		expectStrings(t, []string{streamEvent(11), streamEvent(12)},
			stream.rowsInOrder(t, 2), "replay inside three days")
		stream.quiet(t)
	})

	t.Run("stale cursor", func(t *testing.T) {
		const erin = "core://user/erin"

		withDeskCtx := bearerContext(ctx, eu.AccessToken(t,
			notificationClaims("erin", "user", "", desk)))
		withoutDeskCtx := bearerContext(ctx, eu.AccessToken(t,
			notificationClaims("erin", "user", "")))

		// The first stream joins the desk at now, giving erin a desk
		// cursor.
		stream := openStream(withDeskCtx, t, eu, &userv1.StreamRequest{})
		stream.quiet(t)
		stream.cancel()
		test.IsRPCError(t, stream.ended(t, streamTimeout), connect.CodeCanceled)

		cursors, err := eu.Store.GetNotificationCursors(ctx, erin)
		test.Mustf(t, err, "read erin's cursors")

		deskCursor, ok := cursors[desk]
		test.Equalf(t, true, ok, "erin has a desk cursor")

		deskRow := insertRow(t, eu, desk, streamEvent(30))
		insertRow(t, eu, erin, streamEvent(31))

		// Without the unit the desk cursor is ignored: only erin's
		// own row.
		stream = openStream(withoutDeskCtx, t, eu, &userv1.StreamRequest{})

		expectStrings(t, []string{streamEvent(31)}, stream.rowsInOrder(t, 1),
			"replay without the desk")
		stream.quiet(t)
		stream.cancel()
		test.IsRPCError(t, stream.ended(t, streamTimeout), connect.CodeCanceled)

		cursors, err = eu.Store.GetNotificationCursors(ctx, erin)
		test.Mustf(t, err, "read erin's cursors without the desk")
		test.Equalf(t, deskCursor, cursors[desk],
			"the desk cursor is kept, not dropped")

		// With the unit again the kept cursor replays the desk row.
		stream = openStream(withDeskCtx, t, eu, &userv1.StreamRequest{})

		expectStrings(t, []string{streamEvent(30)}, stream.rowsInOrder(t, 1),
			"replay from the kept desk cursor")
		stream.quiet(t)

		cursors, err = eu.Store.GetNotificationCursors(ctx, erin)
		test.Mustf(t, err, "read erin's cursors after the desk replay")
		test.Equalf(t, deskRow.ID, cursors[desk], "desk cursor after the replay")
	})

	t.Run("invalid", func(t *testing.T) {
		stream := openStream(aliceCtx, t, eu, &userv1.StreamRequest{
			ReplayWindowSeconds: -1,
		})

		expectInvalidArgument(t, stream.ended(t, streamTimeout),
			"replay_window_seconds")

		stream = openStream(producerCtx, t, eu, &userv1.StreamRequest{})

		test.IsRPCError(t, stream.ended(t, streamTimeout),
			connect.CodePermissionDenied)
	})
}

// TestNotificationsStreamKeepaliveAndEnd covers the keepalive on a short
// interval, two streams of one subject both receiving a live row, and the
// end on token expiry.
func TestNotificationsStreamKeepaliveAndEnd(t *testing.T) {
	eu := startElephantUser(t,
		internal.WithStreamKeepalive(100*time.Millisecond))
	ctx := t.Context()

	const bob = "core://user/bob"

	bobCtx := bearerContext(ctx, eu.AccessToken(t,
		notificationClaims("bob", "user", "")))

	t.Run("keepalive", func(t *testing.T) {
		stream := openStream(bobCtx, t, eu, &userv1.StreamRequest{})

		var keepalive *userv1.Keepalive

		select {
		case keepalive = <-stream.keepalives:
		case <-time.After(streamTimeout):
			t.Fatal("no keepalive on a quiet stream")
		}

		serverTime, err := time.Parse(time.RFC3339Nano, keepalive.ServerTime)
		test.Mustf(t, err, "parse the keepalive's server time")

		if d := time.Since(serverTime).Abs(); d > time.Minute {
			t.Fatalf("keepalive server time %s is %s off",
				keepalive.ServerTime, d)
		}
	})

	t.Run("two streams", func(t *testing.T) {
		one := openStream(bobCtx, t, eu, &userv1.StreamRequest{})
		two := openStream(bobCtx, t, eu, &userv1.StreamRequest{})

		// Let both streams finish their replay and reach the loop.
		time.Sleep(50 * time.Millisecond)

		insertRow(t, eu, bob, streamEvent(20))

		test.Equalf(t, streamEvent(20), one.row(t).EventUuid,
			"the first stream's live row")
		test.Equalf(t, streamEvent(20), two.row(t).EventUuid,
			"the second stream's live row")
	})

	t.Run("token expiry", func(t *testing.T) {
		claims := notificationClaims("dave", "user", "")
		claims.ExpiresAt = jwt.NewNumericDate(time.Now().Add(2 * time.Second))

		daveCtx := bearerContext(ctx, eu.AccessToken(t, claims))

		stream := openStream(daveCtx, t, eu, &userv1.StreamRequest{})

		test.IsRPCError(t, stream.ended(t, 3*streamTimeout),
			connect.CodeUnauthenticated)
	})
}

// TestNotificationsStreamDrain checks that stopping the application, as a
// deploy does, ends an open stream with unavailable.
func TestNotificationsStreamDrain(t *testing.T) {
	eu := startElephantUser(t)

	frankCtx := bearerContext(t.Context(), eu.AccessToken(t,
		notificationClaims("frank", "user", "")))

	stream := openStream(frankCtx, t, eu, &userv1.StreamRequest{})

	// Let the stream finish its replay and reach the loop.
	time.Sleep(50 * time.Millisecond)

	eu.StopApplication()

	test.IsRPCError(t, stream.ended(t, streamTimeout),
		connect.CodeUnavailable)
}
