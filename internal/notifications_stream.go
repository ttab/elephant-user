package internal

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"connectrpc.com/connect"
	userv1 "github.com/ttab/elephant-api/elephant/user/v1"
	"github.com/ttab/elephantine/rpc"
)

const (
	// DefaultStreamKeepalive is how long a stream may stay quiet before
	// the server sends a keepalive. It has to stay under the ingress idle
	// timeout, 60 s on the AWS ALB.
	DefaultStreamKeepalive = 20 * time.Second
	// DefaultStreamReplay is the replay window of a stream whose request
	// asks for none.
	DefaultStreamReplay = 24 * time.Hour
	// DefaultStreamReplayMax caps the replay window a request may ask
	// for: 7 days.
	DefaultStreamReplayMax = 7 * 24 * time.Hour
)

const (
	// streamPageSize is how many rows of one key a replay reads at a
	// time.
	streamPageSize = 100
	// streamWakeupBuffer is how many wakeups a stream holds while it is
	// busy sending. A wakeup that does not fit is dropped and the stream
	// recovers by re-reading its keys, see notificationStream.listen.
	streamWakeupBuffer = 64
)

// NotificationsServiceOption configures a NotificationsService.
type NotificationsServiceOption func(s *NotificationsService)

// WithStreamKeepalive sets how long a stream may stay quiet before a
// keepalive is sent, overriding DefaultStreamKeepalive.
func WithStreamKeepalive(interval time.Duration) NotificationsServiceOption {
	return func(s *NotificationsService) {
		s.streamKeepalive = interval
	}
}

// WithStreamReplayDefault sets the replay window of a stream whose request
// asks for none, overriding DefaultStreamReplay.
func WithStreamReplayDefault(window time.Duration) NotificationsServiceOption {
	return func(s *NotificationsService) {
		s.streamReplayDefault = window
	}
}

// WithStreamReplayMax sets the largest replay window a request may ask for,
// overriding DefaultStreamReplayMax.
func WithStreamReplayMax(window time.Duration) NotificationsServiceOption {
	return func(s *NotificationsService) {
		s.streamReplayMax = window
	}
}

// replayWindow resolves the requested replay window in seconds: zero is
// the default, and more than the maximum is the maximum.
func (s *NotificationsService) replayWindow(seconds int64) time.Duration {
	if seconds == 0 {
		return min(s.streamReplayDefault, s.streamReplayMax)
	}

	if seconds >= int64(s.streamReplayMax/time.Second) {
		return s.streamReplayMax
	}

	return time.Duration(seconds) * time.Second
}

// Stream implements [userv1connect.NotificationsServiceHandler]. The keys
// are the caller's subject, org and units. It replays the rows after the
// subject's cursor for each key, no older than the replay window, then
// pushes live rows as their wakeups arrive and a keepalive whenever it has
// been quiet for the keepalive interval. Each stream listens on the
// notification_update fan-out with a filter on its own key set, and
// nothing about it is stored: every pod's LISTEN receives every wakeup, so
// two pods holding streams for one subject both push. It ends only when
// its context does: on token expiry, a drain or the client going away, and
// it returns the context's cause, never nil.
func (s *NotificationsService) Stream(
	ctx context.Context, req *connect.Request[userv1.StreamRequest],
	stream *connect.ServerStream[userv1.StreamResponse],
) error {
	auth, err := rpc.RequireAnyScope(ctx, ScopeUser)
	if err != nil {
		return err
	}

	if req.Msg.ReplayWindowSeconds < 0 {
		return rpc.InvalidArgument("replay_window_seconds",
			"cannot be negative")
	}

	window := s.replayWindow(req.Msg.ReplayWindowSeconds)

	ctx, cancel := rpc.ContextWithTokenExpiry(ctx)
	defer cancel()

	subject := auth.Claims.Subject

	keys := getAllOwners(auth)
	slices.Sort(keys)
	keys = slices.Compact(keys)

	s.logger.DebugContext(ctx, "opened notification stream",
		"keys", len(keys),
		"replay_window", window)

	ns := notificationStream{
		service:   s,
		stream:    stream,
		subject:   subject,
		keys:      keys,
		floor:     time.Now().Add(-window),
		positions: make(map[string]int64, len(keys)),
		wakeups:   make(chan NotificationEvent, streamWakeupBuffer),
		overflow:  make(chan struct{}, 1),
	}

	return ns.run(ctx)
}

// notificationStream is the state of one open stream. positions is the
// highest id this stream has pushed or skipped per key; it starts at the
// subject's cursor and moves forward with every push. It is the stream's
// own, so that two streams of one subject, which share the stored cursor,
// both push a live row.
type notificationStream struct {
	service *NotificationsService
	stream  *connect.ServerStream[userv1.StreamResponse]
	subject string
	keys    []string
	// floor is the replay window's lower bound on created, fixed when
	// the stream opens.
	floor     time.Time
	positions map[string]int64

	keepalive *time.Timer

	wakeups chan NotificationEvent
	// overflow is signalled, and overflowed set, when a wakeup did not
	// fit in wakeups and was dropped.
	overflow   chan struct{}
	overflowed atomic.Bool
}

func (ns *notificationStream) run(ctx context.Context) error {
	listenCtx, stopListening := context.WithCancel(ctx)

	var listening sync.WaitGroup

	// The listener ends with the stream: stopListening is deferred after
	// Wait, so it runs first.
	defer listening.Wait()
	defer stopListening()

	// Listen before the replay so that a row written while the replay
	// runs is announced to this stream; one the replay already pushed is
	// then skipped by its position. The listener registers on its own
	// goroutine while the replay reads the cursors, so a row committed
	// before it has registered and after the replay read its key is not
	// announced: the window the long-polls have, and the next stream's
	// replay delivers the row.
	listening.Go(func() {
		ns.listen(listenCtx)
	})

	// Send the response headers before anything else: a client's call
	// returns only once they arrive, so a stream with nothing to replay
	// would otherwise keep its client waiting for the first keepalive.
	err := ns.send(ctx, nil)
	if err != nil {
		return err
	}

	err = ns.replay(ctx)
	if err != nil {
		return err
	}

	ns.keepalive = time.NewTimer(ns.service.streamKeepalive)
	defer ns.keepalive.Stop()

	for {
		select {
		case <-ctx.Done():
			return streamEnded(ctx)
		case <-ns.overflow:
			err := ns.recoverOverflow(ctx)
			if err != nil {
				return err
			}
		case e := <-ns.wakeups:
			// A wakeup that arrives after a dropped one must not move
			// the position past the dropped row before it is read.
			err := ns.recoverOverflow(ctx)
			if err != nil {
				return err
			}

			err = ns.pushLive(ctx, e)
			if err != nil {
				return err
			}
		case <-ns.keepalive.C:
			err := ns.sendKeepalive(ctx)
			if err != nil {
				return err
			}
		}
	}
}

// listen registers the stream with the Notifications fan-out until the
// context ends. The filter does the delivery itself so that the stream
// learns when a wakeup is dropped: the fan-out drops on a full listener
// channel without telling anyone. It answers false, so the fan-out never
// sends to the channel it was given, which exists only to register.
func (ns *notificationStream) listen(ctx context.Context) {
	ns.service.notifications.Listen(ctx, make(chan NotificationEvent),
		func(e NotificationEvent) bool {
			if !slices.Contains(ns.keys, e.Key) {
				return false
			}

			select {
			case ns.wakeups <- e:
			default:
				ns.overflowed.Store(true)
				ns.service.metrics.StreamWakeupsDropped.Inc()

				select {
				case ns.overflow <- struct{}{}:
				default:
				}
			}

			return false
		})
}

// replay pushes, key by key in sorted key order and in id order within a
// key, the rows after the subject's cursor and inside the replay window.
// The subject's own key without a cursor starts from the oldest row inside
// the window. A unit or org key without a cursor starts at now: its cursor
// is set to the newest id under the key and nothing is pushed for it.
// Cursors for keys the token no longer carries are ignored.
func (ns *notificationStream) replay(ctx context.Context) error {
	store := ns.service.store

	cursors, err := store.GetNotificationCursors(ctx, ns.subject)
	if err != nil {
		return ns.failure(ctx, "get notification cursors", err)
	}

	for _, key := range ns.keys {
		position, ok := cursors[key]

		if !ok && key != ns.subject {
			err := ns.joinAtNow(ctx, key)
			if err != nil {
				return err
			}

			continue
		}

		ns.positions[key] = position

		err := ns.pushAfterPosition(ctx, key, StreamPushReplay)
		if err != nil {
			return err
		}
	}

	return nil
}

// joinAtNow sets the cursor of a key the subject has never read to the
// newest id under it, zero for a key without rows, so that the key's
// history is not replayed. Ids within a key follow created order, so the
// newest row is the one with the highest id.
func (ns *notificationStream) joinAtNow(ctx context.Context, key string) error {
	store := ns.service.store

	newest, err := store.ListNotificationsBefore(
		ctx, []string{key}, notificationsNewest, "", 0, 1)
	if err != nil {
		return ns.failure(ctx, "read the newest notification", err)
	}

	var position int64

	if len(newest) > 0 {
		position = newest[0].ID
	}

	err = store.BumpNotificationCursor(ctx, ns.subject, key, position)
	if err != nil {
		return ns.failure(ctx, "set the notification cursor", err)
	}

	ns.positions[key] = position

	return nil
}

// pushAfterPosition pages through the key's rows after the stream's
// position, created at or after the replay floor, and pushes them in id
// order.
func (ns *notificationStream) pushAfterPosition(
	ctx context.Context, key string, reason string,
) error {
	for {
		rows, err := ns.service.store.ListNotificationsAfterID(
			ctx, key, ns.positions[key], ns.floor, streamPageSize)
		if err != nil {
			return ns.failure(ctx, "list notifications", err)
		}

		for _, n := range rows {
			err := ns.push(ctx, n, reason)
			if err != nil {
				return err
			}
		}

		if len(rows) < streamPageSize {
			return nil
		}
	}
}

// recoverOverflow catches up after dropped wakeups, if there were any,
// with the replay's read from each key's position. The floor is the
// replay's too: without it a subject whose old rows the window excluded
// would be sent all of them.
func (ns *notificationStream) recoverOverflow(ctx context.Context) error {
	if !ns.overflowed.Swap(false) {
		return nil
	}

	for _, key := range ns.keys {
		err := ns.pushAfterPosition(ctx, key, StreamPushLive)
		if err != nil {
			return err
		}
	}

	return nil
}

// pushLive pushes the row a wakeup names, unless the stream is already
// past it. A row that is gone, deleted by the cleaner, is skipped.
func (ns *notificationStream) pushLive(
	ctx context.Context, e NotificationEvent,
) error {
	if e.ID <= ns.positions[e.Key] {
		return nil
	}

	rows, err := ns.service.store.ListNotificationsAfterID(
		ctx, e.Key, e.ID-1, time.Time{}, 1)
	if err != nil {
		return ns.failure(ctx, "read the notification", err)
	}

	if len(rows) == 0 || rows[0].ID != e.ID {
		return nil
	}

	return ns.push(ctx, rows[0], StreamPushLive)
}

// push sends a row, then moves the stream's position and the subject's
// cursor to it.
func (ns *notificationStream) push(
	ctx context.Context, n Notification, reason string,
) error {
	err := ns.send(ctx, &userv1.StreamResponse{
		Payload: &userv1.StreamResponse_Notification{
			Notification: notificationToRPC(n),
		},
	})
	if err != nil {
		return err
	}

	ns.service.metrics.StreamNotificationsSent.WithLabelValues(reason).Inc()

	ns.positions[n.Key] = n.ID

	err = ns.service.store.BumpNotificationCursor(ctx, ns.subject, n.Key, n.ID)
	if err != nil {
		return ns.failure(ctx, "bump the notification cursor", err)
	}

	return nil
}

func (ns *notificationStream) sendKeepalive(ctx context.Context) error {
	err := ns.send(ctx, &userv1.StreamResponse{
		Payload: &userv1.StreamResponse_Keepalive{
			Keepalive: &userv1.Keepalive{
				ServerTime: formatNotificationTime(time.Now()),
			},
		},
	})
	if err != nil {
		return err
	}

	ns.service.metrics.StreamKeepalives.Inc()

	return nil
}

// send sends a message unless the stream has ended, and restarts the
// keepalive interval once the live phase has begun. A nil message sends
// only the response headers.
func (ns *notificationStream) send(
	ctx context.Context, msg *userv1.StreamResponse,
) error {
	if ctx.Err() != nil {
		return streamEnded(ctx)
	}

	err := ns.stream.Send(msg)
	if err != nil {
		return ns.failure(ctx, "send on the stream", err)
	}

	if ns.keepalive != nil {
		ns.keepalive.Reset(ns.service.streamKeepalive)
	}

	return nil
}

// failure is the error a stream ends with when an action fails: the
// context's cause when the context has ended, which is why the action
// failed, else internal.
func (ns *notificationStream) failure(
	ctx context.Context, action string, err error,
) error {
	if ctx.Err() != nil {
		return streamEnded(ctx)
	}

	return rpc.Internalf("%s: %w", action, err)
}

// streamEnded is the error a stream returns when its context ends: the
// context's cause, which elephantine's drain interceptor answers as
// unavailable for a drain and unauthenticated for an expired token. A
// client gone away is canceled. Never nil, so that a client cannot read an
// ended stream as a clean end of data.
func streamEnded(ctx context.Context) error {
	return fmt.Errorf("end the notification stream: %w", context.Cause(ctx))
}
