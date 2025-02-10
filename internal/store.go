package internal

import (
	"context"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ttab/elephantine/pg"
	"github.com/ttab/elephantine/pg/postgres"
)

const (
	ChannelMessageUpdate      = "message_update"
	ChannelInboxMessageUpdate = "inbox_message_update"
)

type PGStore struct {
	logger *slog.Logger
	dbpool *pgxpool.Pool
	q      *postgres.Queries

	Messages      *pg.FanOut[MessageEvent]
	InboxMessages *pg.FanOut[MessageEvent]
}

// Interface guard.
var _ Store = &PGStore{}

func NewPGStore(
	logger *slog.Logger, dbpool *pgxpool.Pool,
) *PGStore {
	return &PGStore{
		logger: logger,
		dbpool: dbpool,
		q:      postgres.New(dbpool),

		Messages:      pg.NewFanOut[MessageEvent](ChannelMessageUpdate),
		InboxMessages: pg.NewFanOut[MessageEvent](ChannelInboxMessageUpdate),
	}
}

// OnMessageUpdate notifies the channel ch of message updates for a recipient.
// Subscription is automatically cancelled once the context is cancelled.
//
// Note that we don't provide any delivery guarantees for these events.
// non-blocking send is used on ch, so if it's unbuffered events will be
// discarded if the receiver is busy.
func (s *PGStore) OnMessageUpdate(
	ctx context.Context, ch chan MessageEvent,
	recipient string, afterID int64,
) {
	go s.Messages.Listen(ctx, ch, func(msg MessageEvent) bool {
		return msg.Recipient == recipient && msg.ID > afterID
	})
}

// OnInboxMessageUpdate notifies the channel ch of inbox message updates
// for a recipient.
// Subscription is automatically cancelled once the context is cancelled.
//
// Note that we don't provide any delivery guarantees for these events.
// non-blocking send is used on ch, so if it's unbuffered events will be
// discarded if the receiver is busy.
func (s *PGStore) OnInboxMessageUpdate(
	ctx context.Context, ch chan MessageEvent,
	recipient string, afterID int64,
) {
	go s.InboxMessages.Listen(ctx, ch, func(msg MessageEvent) bool {
		return msg.Recipient == recipient && msg.ID > afterID
	})
}
