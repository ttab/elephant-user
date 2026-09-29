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
	NotifyChannelEventLogUpdate     NotifyChannel = "event_log_update"
	NotifyChannelSchemaUpdate       NotifyChannel = "schema_update"
	NotifyChannelDeprecationUpdate  NotifyChannel = "deprecation_update"
)

type PGStore struct {
	logger *slog.Logger
	dbpool *pgxpool.Pool
	q      *postgres.Queries

	Messages      *pg.FanOut[MessageEvent]
	InboxMessages *pg.FanOut[MessageEvent]
	EventLog      *pg.FanOut[EventLogEvent]
	Schemas       *pg.FanOut[SchemaEvent]
	Deprecations  *pg.FanOut[DeprecationEvent]
}

func NewPGStore(
	logger *slog.Logger, dbpool *pgxpool.Pool,
) *PGStore {
	return &PGStore{
		logger: logger,
		dbpool: dbpool,
		q:      postgres.New(dbpool),

		Messages:      pg.NewFanOut[MessageEvent](NotifyChannelMessageUpdate),
		InboxMessages: pg.NewFanOut[MessageEvent](NotifyChannelInboxMessageUpdate),
		EventLog:      pg.NewFanOut[EventLogEvent](NotifyChannelEventLogUpdate),
		Schemas:       pg.NewFanOut[SchemaEvent](NotifyChannelSchemaUpdate),
		Deprecations:  pg.NewFanOut[DeprecationEvent](NotifyChannelDeprecationUpdate),
	}
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
		s.EventLog,
		s.Schemas,
		s.Deprecations,
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

// RunCleaner removes expired messages on one instance at a time, supervised
// by a job lock: once on acquiring the lock and then at the given interval.
// Blocks until the context is cancelled.
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
			s.sweepOldMessages(ctx)

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
					s.sweepOldMessages(ctx)
				}
			}
		})
	if err != nil {
		return fmt.Errorf("run cleaner job lock: %w", err)
	}

	return nil
}

// sweepOldMessages runs one retention sweep. A failure is logged and left
// for the next tick rather than surfaced: releasing the lock over a
// transient error only hands the same failure to another replica.
func (s *PGStore) sweepOldMessages(ctx context.Context) {
	err := s.removeOldMessages(ctx)
	if err != nil && ctx.Err() == nil {
		s.logger.ErrorContext(ctx, "remove old messages",
			elephantine.LogKeyError, err)
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
