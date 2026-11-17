package internal

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine/pg"
)

// InsertNotifications is the fan-out: it writes the event as one feed row
// under each of the keys, in one transaction, and returns the rows it
// wrote. A key that already has a row for the event gets nothing, so a
// repeated event is a no-op. The keys are resolved by the caller; see
// ListDocumentSubscribers for the subscribers of a document.
func (s *PGStore) InsertNotifications(
	ctx context.Context, event NotificationSourceEvent, keys []string,
) ([]Notification, error) {
	var written []Notification

	err := pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		rows, err := insertNotifications(ctx, postgres.New(tx), event, keys)
		if err != nil {
			return err
		}

		written = rows

		return nil
	})
	if err != nil {
		return nil, err
	}

	s.countNotifications(event.Family, len(written))

	return written, nil
}

// countNotifications counts written feed rows by family. A caller that
// runs insertNotifications in its own transaction calls it after commit.
func (s *PGStore) countNotifications(family NotificationFamily, n int) {
	if n == 0 {
		return
	}

	label := NotificationFamilyOther

	switch family {
	case NotificationFamilyIngest, NotificationFamilyComments,
		NotificationFamilySettings, NotificationFamilyInbox,
		NotificationFamilyMessages:
		label = string(family)
	}

	s.metrics.NotificationsWritten.WithLabelValues(label).Add(float64(n))
}

// insertNotifications writes the event under each key inside the caller's
// transaction, see InsertNotifications.
//
// Ids are per key and commit-ordered: each key's notification_sequence row
// is upserted and row-locked until commit, the nextMessageID pattern. The
// keys are locked in sorted order so that two writers with overlapping key
// sets cannot deadlock, and all of them before the rows are stamped, so
// created order follows id order within a key. The sequence rows must be
// the last locks the transaction takes: a caller that locks rows of its own
// does so before calling.
//
// Keys that already have a row for the event are skipped before their
// sequence row is taken, so a retry consumes no id. Two writers racing with
// the same event both pass that check; the loser's insert is a no-op under
// ON CONFLICT DO NOTHING and leaves a gap in the key's ids, which a reader
// tailing on id > position never notices.
func insertNotifications(
	ctx context.Context, q *postgres.Queries,
	event NotificationSourceEvent, keys []string,
) ([]Notification, error) {
	if len(keys) == 0 {
		return nil, nil
	}

	sorted := slices.Clone(keys)
	slices.Sort(sorted)
	sorted = slices.Compact(sorted)

	notified, err := q.ListNotifiedKeys(ctx, postgres.ListNotifiedKeysParams{
		Keys:      sorted,
		EventUuid: event.EventUUID,
	})
	if err != nil {
		return nil, fmt.Errorf("list keys already notified: %w", err)
	}

	sorted = slices.DeleteFunc(sorted, func(key string) bool {
		return slices.Contains(notified, key)
	})

	if len(sorted) == 0 {
		return nil, nil
	}

	refs, meta, err := marshalNotificationData(event.Refs, event.Meta)
	if err != nil {
		return nil, err
	}

	ids := make([]int64, len(sorted))

	for i, key := range sorted {
		id, err := q.NextNotificationID(ctx, key)
		if err != nil {
			return nil, fmt.Errorf("advance notification sequence: %w", err)
		}

		ids[i] = id
	}

	created := time.Now()

	var written []Notification

	for i, key := range sorted {
		n, err := q.InsertNotification(ctx, postgres.InsertNotificationParams{
			Key:          key,
			ID:           ids[i],
			Kind:         event.Kind,
			DocumentUuid: event.DocumentUUID,
			DocumentType: event.DocumentType,
			Refs:         refs,
			Actor:        event.Actor,
			EventUuid:    event.EventUUID,
			Created:      pg.Time(created),
			Meta:         meta,
		})
		if err != nil {
			return nil, fmt.Errorf("insert notification: %w", err)
		}

		if n == 0 {
			// Lost the race to another write of the same event.
			continue
		}

		err = pgNotify(ctx, q, NotifyChannelNotificationUpdate,
			NotificationEvent{
				Key: key,
				ID:  ids[i],
			})
		if err != nil {
			return nil, fmt.Errorf("send notification: %w", err)
		}

		written = append(written, Notification{
			Key:          key,
			ID:           ids[i],
			Kind:         event.Kind,
			DocumentUUID: event.DocumentUUID,
			DocumentType: event.DocumentType,
			Refs:         event.Refs,
			Actor:        event.Actor,
			EventUUID:    event.EventUUID,
			Created:      created,
			Meta:         event.Meta,
		})
	}

	return written, nil
}

// marshalNotificationData encodes refs as a JSON array and meta as a JSON
// object, empty rather than null when there is nothing to say.
func marshalNotificationData(
	refs []NotificationReference, meta map[string]string,
) ([]byte, []byte, error) {
	if refs == nil {
		refs = []NotificationReference{}
	}

	if meta == nil {
		meta = map[string]string{}
	}

	refsData, err := json.Marshal(refs)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal notification refs: %w", err)
	}

	metaData, err := json.Marshal(meta)
	if err != nil {
		return nil, nil, fmt.Errorf("marshal notification meta: %w", err)
	}

	return refsData, metaData, nil
}

// ListNotificationsAfterID returns up to size of the key's rows with an id
// above afterID, created at or after notBefore, oldest first.
func (s *PGStore) ListNotificationsAfterID(
	ctx context.Context, key string, afterID int64,
	notBefore time.Time, size int64,
) ([]Notification, error) {
	rows, err := s.q.ListNotificationsAfterID(ctx, postgres.ListNotificationsAfterIDParams{
		Key:       key,
		AfterID:   afterID,
		NotBefore: pg.Time(notBefore),
		Limit:     size,
	})
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}

	return notificationsFromRows(rows)
}

// ListNotificationsBefore returns up to size rows across the keys, newest
// first. The order is created, then key, then id, all descending, since
// rows share a created time: one event has the same time under every key it
// was written to. Paging is on that whole position: a page holds the rows
// before (before, beforeKey, beforeID), so passing the last row's created,
// key and id never skips a row that ties with it. An empty beforeKey and a
// zero beforeID give the rows created strictly before before.
func (s *PGStore) ListNotificationsBefore(
	ctx context.Context, keys []string,
	before time.Time, beforeKey string, beforeID int64, size int64,
) ([]Notification, error) {
	rows, err := s.q.ListNotificationsBefore(ctx, postgres.ListNotificationsBeforeParams{
		Keys:      keys,
		Before:    pg.Time(before),
		BeforeKey: beforeKey,
		BeforeID:  beforeID,
		Limit:     size,
	})
	if err != nil {
		return nil, fmt.Errorf("list notifications: %w", err)
	}

	return notificationsFromRows(rows)
}

func notificationsFromRows(rows []postgres.Notification) ([]Notification, error) {
	res := make([]Notification, len(rows))

	for i, r := range rows {
		n, err := notificationFromRow(r)
		if err != nil {
			return nil, err
		}

		res[i] = n
	}

	return res, nil
}

func notificationFromRow(r postgres.Notification) (Notification, error) {
	n := Notification{
		Key:          r.Key,
		ID:           r.ID,
		Kind:         r.Kind,
		DocumentUUID: r.DocumentUuid,
		DocumentType: r.DocumentType,
		Actor:        r.Actor,
		EventUUID:    r.EventUuid,
		Created:      r.Created.Time,
	}

	err := json.Unmarshal(r.Refs, &n.Refs)
	if err != nil {
		return n, fmt.Errorf("unmarshal refs of notification %s/%d: %w",
			r.Key, r.ID, err)
	}

	err = json.Unmarshal(r.Meta, &n.Meta)
	if err != nil {
		return n, fmt.Errorf("unmarshal meta of notification %s/%d: %w",
			r.Key, r.ID, err)
	}

	return n, nil
}

// Subscribe subscribes the subject to the documents of the items and
// returns the subscriptions as stored, ordered by document uuid.
// Subscribing again replaces the expiry. An item without a lifetime gets
// the store's default TTL. When a document is given twice, the last item
// for it stands.
func (s *PGStore) Subscribe(
	ctx context.Context, subject string, items []SubscriptionItem,
) ([]Subscription, error) {
	now := time.Now()

	byDocument := make(map[uuid.UUID]Subscription, len(items))

	for _, item := range items {
		expires, err := s.subscriptionExpiry(item, now)
		if err != nil {
			return nil, err
		}

		sub := Subscription{
			Subject:      subject,
			DocumentUUID: item.DocumentUUID,
		}

		if !item.Permanent {
			sub.Expires = &expires
		}

		byDocument[item.DocumentUUID] = sub
	}

	// Upsert in document order so that two subscribes of one subject to
	// overlapping documents lock the rows in the same order.
	subs := slices.SortedFunc(maps.Values(byDocument),
		func(a, b Subscription) int {
			return bytes.Compare(a.DocumentUUID[:], b.DocumentUUID[:])
		})

	err := pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		q := postgres.New(tx)

		for _, sub := range subs {
			err := q.UpsertSubscription(ctx, postgres.UpsertSubscriptionParams{
				Subject:      sub.Subject,
				DocumentUuid: sub.DocumentUUID,
				ExpiresAt:    pg.PTime(sub.Expires),
			})
			if err != nil {
				return fmt.Errorf("upsert subscription: %w", err)
			}
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	return subs, nil
}

// subscriptionExpiry normalises the lifetime of an item into an expiry. A
// permanent item has none and gets the zero time.
func (s *PGStore) subscriptionExpiry(
	item SubscriptionItem, now time.Time,
) (time.Time, error) {
	given := 0

	if item.TTL != 0 {
		given++
	}

	if item.Until != nil {
		given++
	}

	if item.Permanent {
		given++
	}

	if given > 1 || item.TTL < 0 {
		return time.Time{}, ErrInvalidSubscriptionLifetime
	}

	switch {
	case item.Permanent:
		return time.Time{}, nil
	case item.Until != nil:
		return *item.Until, nil
	case item.TTL > 0:
		return now.Add(item.TTL), nil
	default:
		return now.Add(s.subscriptionTTL), nil
	}
}

// Unsubscribe removes the subject's subscriptions to the documents.
// Documents the subject is not subscribed to are ignored.
func (s *PGStore) Unsubscribe(
	ctx context.Context, subject string, documentUUIDs []uuid.UUID,
) error {
	_, err := s.q.DeleteSubscriptions(ctx, postgres.DeleteSubscriptionsParams{
		Subject:       subject,
		DocumentUuids: documentUUIDs,
	})
	if err != nil {
		return fmt.Errorf("delete subscriptions: %w", err)
	}

	return nil
}

// ListSubscriptions returns the subject's live subscriptions, ordered by
// document uuid. Live means permanent or expiring in the future.
func (s *PGStore) ListSubscriptions(
	ctx context.Context, subject string,
) ([]Subscription, error) {
	rows, err := s.q.ListSubscriptions(ctx, postgres.ListSubscriptionsParams{
		Subject: subject,
		Now:     pg.Time(time.Now()),
	})
	if err != nil {
		return nil, fmt.Errorf("list subscriptions: %w", err)
	}

	res := make([]Subscription, len(rows))

	for i, r := range rows {
		res[i] = Subscription{
			Subject:      r.Subject,
			DocumentUUID: r.DocumentUuid,
			Expires:      timePointer(r.ExpiresAt),
		}
	}

	return res, nil
}

// ListDocumentSubscribers returns the subjects with a live subscription to
// the document, sorted: the keys a document event is written under.
func (s *PGStore) ListDocumentSubscribers(
	ctx context.Context, documentUUID uuid.UUID,
) ([]string, error) {
	rows, err := s.q.ListDocumentSubscriptions(ctx, postgres.ListDocumentSubscriptionsParams{
		DocumentUuid: documentUUID,
		Now:          pg.Time(time.Now()),
	})
	if err != nil {
		return nil, fmt.Errorf("list document subscriptions: %w", err)
	}

	subjects := make([]string, len(rows))

	for i, r := range rows {
		subjects[i] = r.Subject
	}

	slices.Sort(subjects)

	return subjects, nil
}

// GetNotificationCursors returns the subject's delivered position per key.
// A key with no cursor is absent, which reads as position zero.
func (s *PGStore) GetNotificationCursors(
	ctx context.Context, subject string,
) (map[string]int64, error) {
	rows, err := s.q.GetNotificationCursors(ctx, subject)
	if err != nil {
		return nil, fmt.Errorf("get notification cursors: %w", err)
	}

	cursors := make(map[string]int64, len(rows))

	for _, r := range rows {
		cursors[r.Key] = r.Position
	}

	return cursors, nil
}

// BumpNotificationCursor moves the subject's cursor for the key forward to
// position. It never moves backwards, so pods bumping the same cursor
// concurrently leave the highest position.
func (s *PGStore) BumpNotificationCursor(
	ctx context.Context, subject string, key string, position int64,
) error {
	err := s.q.BumpNotificationCursor(ctx, postgres.BumpNotificationCursorParams{
		Subject:  subject,
		Key:      key,
		Position: position,
	})
	if err != nil {
		return fmt.Errorf("bump notification cursor: %w", err)
	}

	return nil
}
