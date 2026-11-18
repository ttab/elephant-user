package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
	"github.com/ttab/elephantine/pg/joblock"
)

type NotifyChannel = string

const (
	NotifyChannelMessageUpdate      NotifyChannel = "message_update"
	NotifyChannelInboxMessageUpdate NotifyChannel = "inbox_message_update"
	NotifyChannelInboxStateUpdate   NotifyChannel = "inbox_state_update"
	NotifyChannelEventLogUpdate     NotifyChannel = "event_log_update"
	NotifyChannelSchemaUpdate       NotifyChannel = "schema_update"
	NotifyChannelDeprecationUpdate  NotifyChannel = "deprecation_update"
	NotifyChannelNotificationUpdate NotifyChannel = "notification_update"
)

type PGStore struct {
	logger  *slog.Logger
	dbpool  *pgxpool.Pool
	q       *postgres.Queries
	metrics *Metrics

	subscriptionTTL       time.Duration
	notificationRetention time.Duration

	Messages      *pg.FanOut[MessageEvent]
	InboxMessages *pg.FanOut[MessageEvent]
	InboxStates   *pg.FanOut[InboxStateEvent]
	EventLog      *pg.FanOut[EventLogEvent]
	Schemas       *pg.FanOut[SchemaEvent]
	Deprecations  *pg.FanOut[DeprecationEvent]
	Notifications *pg.FanOut[NotificationEvent]
}

// PGStoreOption configures a PGStore.
type PGStoreOption func(s *PGStore)

// WithSubscriptionTTL sets how long a subscription lives when no lifetime
// is given, overriding DefaultSubscriptionTTL.
func WithSubscriptionTTL(ttl time.Duration) PGStoreOption {
	return func(s *PGStore) {
		s.subscriptionTTL = ttl
	}
}

// WithNotificationRetention sets how old a feed row gets before the
// cleaner deletes it, overriding DefaultNotificationRetention.
func WithNotificationRetention(retention time.Duration) PGStoreOption {
	return func(s *PGStore) {
		s.notificationRetention = retention
	}
}

func NewPGStore(
	logger *slog.Logger, dbpool *pgxpool.Pool, metrics *Metrics,
	opts ...PGStoreOption,
) *PGStore {
	s := PGStore{
		logger:  logger,
		dbpool:  dbpool,
		q:       postgres.New(dbpool),
		metrics: metrics,

		subscriptionTTL:       DefaultSubscriptionTTL,
		notificationRetention: DefaultNotificationRetention,

		Messages:      pg.NewFanOut[MessageEvent](NotifyChannelMessageUpdate),
		InboxMessages: pg.NewFanOut[MessageEvent](NotifyChannelInboxMessageUpdate),
		InboxStates:   pg.NewFanOut[InboxStateEvent](NotifyChannelInboxStateUpdate),
		EventLog:      pg.NewFanOut[EventLogEvent](NotifyChannelEventLogUpdate),
		Schemas:       pg.NewFanOut[SchemaEvent](NotifyChannelSchemaUpdate),
		Deprecations:  pg.NewFanOut[DeprecationEvent](NotifyChannelDeprecationUpdate),
		Notifications: pg.NewFanOut[NotificationEvent](NotifyChannelNotificationUpdate),
	}

	for _, opt := range opts {
		opt(&s)
	}

	return &s
}

// NewSubscriber creates the PostgreSQL LISTEN subscriber that feeds all
// store notification channels. The pool must be a direct connection
// (LISTEN is incompatible with transaction pooling). Run it with
// Subscriber.Run; it blocks until the context is cancelled.
func (s *PGStore) NewSubscriber(
	pool *pgxpool.Pool, opts ...pg.SubscriberOption,
) *pg.Subscriber {
	return pg.NewSubscriber(s.logger, pool, []pg.ChannelSubscription{
		s.Messages,
		s.InboxMessages,
		s.InboxStates,
		s.EventLog,
		s.Schemas,
		s.Deprecations,
		s.Notifications,
	}, opts...)
}

func pgNotify[T any](
	ctx context.Context, q *postgres.Queries,
	channel NotifyChannel, payload T,
) error {
	message, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload for notification: %w", err)
	}

	err = q.Notify(ctx, postgres.NotifyParams{
		Channel: channel,
		Message: string(message),
	})
	if err != nil {
		return fmt.Errorf("publish notification payload to channel: %w", err)
	}

	return nil
}

// RunCleaner removes expired messages, expired subscriptions and feed rows
// past the retention on one instance at a time, supervised by a job lock:
// once on acquiring the lock and then at the given interval. Blocks until
// the context is cancelled.
func (s *PGStore) RunCleaner(
	ctx context.Context, interval time.Duration,
	metricsRegisterer prometheus.Registerer,
) error {
	err := joblock.Run(ctx, s.dbpool, s.logger, "user", "cleaner",
		joblock.Options{
			PingInterval:      10 * time.Second,
			StaleAfter:        1 * time.Minute,
			CheckInterval:     20 * time.Second,
			Timeout:           5 * time.Second,
			MetricsRegisterer: metricsRegisterer,
		},
		func(ctx context.Context) error {
			// Sweep as soon as the lock is held: a lock that changes
			// hands more often than the interval would otherwise never
			// reach a tick. Sweeps are idempotent deletes, so running
			// one early costs nothing.
			s.sweep(ctx)

			ticker := time.NewTicker(interval)
			defer ticker.Stop()

			for {
				select {
				case <-ctx.Done():
					// Lock loss or shutdown, not a failure: the
					// job lock treats a returned error as a failed
					// run and counts a restart.
					return nil
				case <-ticker.C:
					s.sweep(ctx)
				}
			}
		})
	if err != nil {
		return fmt.Errorf("run cleaner job lock: %w", err)
	}

	return nil
}

// sweep runs one retention sweep. The steps are independent, so a failing
// one does not stop the others. A failure is logged and left for the next
// tick rather than surfaced: releasing the lock over a transient error only
// hands the same failure to another replica.
func (s *PGStore) sweep(ctx context.Context) {
	steps := []struct {
		name string
		fn   func(ctx context.Context) error
	}{
		{name: "remove old messages", fn: s.removeOldMessages},
		{name: "remove expired subscriptions", fn: s.removeExpiredSubscriptions},
		{name: "remove old notifications", fn: s.removeOldNotifications},
	}

	for _, step := range steps {
		err := step.fn(ctx)
		if err != nil && ctx.Err() == nil {
			s.logger.ErrorContext(ctx, step.name,
				elephantine.LogKeyError, err)
		}
	}
}

func (s *PGStore) removeOldMessages(ctx context.Context) error {
	s.logger.Debug("removing old messages")

	err := s.q.DeleteOldMessages(ctx)
	if err != nil {
		return fmt.Errorf("delete old messages: %w", err)
	}

	err = s.q.DeleteOldInboxMessages(ctx)
	if err != nil {
		return fmt.Errorf("delete old inbox messages: %w", err)
	}

	return nil
}

// cleanerBatchSize bounds how many rows one delete statement removes, so a
// large backlog is worked off in short transactions.
const cleanerBatchSize = 1000

// removeExpiredSubscriptions deletes subscriptions whose expiry has passed,
// in batches until none are left.
func (s *PGStore) removeExpiredSubscriptions(ctx context.Context) error {
	now := pg.Time(time.Now())

	return s.deleteInBatches(ctx, CleanerDeletedSubscription,
		func(ctx context.Context) (int64, error) {
			n, err := s.q.DeleteExpiredSubscriptions(ctx,
				postgres.DeleteExpiredSubscriptionsParams{
					Now:   now,
					Limit: cleanerBatchSize,
				})
			if err != nil {
				return 0, fmt.Errorf("delete expired subscriptions: %w", err)
			}

			return n, nil
		})
}

// removeOldNotifications deletes feed rows older than the retention, in
// batches until none are left.
func (s *PGStore) removeOldNotifications(ctx context.Context) error {
	before := pg.Time(time.Now().Add(-s.notificationRetention))

	return s.deleteInBatches(ctx, CleanerDeletedNotification,
		func(ctx context.Context) (int64, error) {
			n, err := s.q.DeleteOldNotifications(ctx,
				postgres.DeleteOldNotificationsParams{
					Before: before,
					Limit:  cleanerBatchSize,
				})
			if err != nil {
				return 0, fmt.Errorf("delete old notifications: %w", err)
			}

			return n, nil
		})
}

// deleteInBatches runs deleteBatch until it removes less than a full batch,
// counting the deleted rows under the given item.
func (s *PGStore) deleteInBatches(
	ctx context.Context, item string,
	deleteBatch func(ctx context.Context) (int64, error),
) error {
	for {
		n, err := deleteBatch(ctx)
		if err != nil {
			return err
		}

		s.metrics.CleanerDeletions.WithLabelValues(item).Add(float64(n))

		if n < cleanerBatchSize {
			return nil
		}
	}
}
