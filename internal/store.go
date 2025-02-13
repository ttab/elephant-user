package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine/pg"
)

type NotifyChannel = string

const (
	NotifyChannelMessageUpdate      NotifyChannel = "message_update"
	NotifyChannelInboxMessageUpdate NotifyChannel = "inbox_message_update"
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

		Messages:      pg.NewFanOut[MessageEvent](NotifyChannelMessageUpdate),
		InboxMessages: pg.NewFanOut[MessageEvent](NotifyChannelInboxMessageUpdate),
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

// ListInboxMessages implements Store.
func (s *PGStore) ListInboxMessages(
	ctx context.Context, recipient string, beforeID int64, size int64,
) ([]InboxMessage, error) {
	rows, err := s.q.ListInboxMessages(ctx, postgres.ListInboxMessagesParams{
		Recipient: recipient,
		BeforeID:  beforeID,
		Limit:     size,
	})
	if err != nil {
		return nil, fmt.Errorf("list inbox messages: %w", err)
	}

	var res []InboxMessage

	for i := range rows {
		var doc newsdoc.Document

		err = json.Unmarshal(rows[i].Payload, &doc)
		if err != nil {
			return nil, fmt.Errorf("unmarshal payload: %w", err)
		}

		msg := InboxMessage{
			Recipient: rows[i].Recipient,
			ID:        rows[i].ID,
			Created:   rows[i].Created.Time,
			CreatedBy: rows[i].CreatedBy,
			Updated:   rows[i].Updated.Time,
			IsRead:    rows[i].IsRead,
			Payload:   &doc,
		}

		res = append(res, msg)
	}

	return res, nil
}

// InsertInboxMessage implements Store.
func (s *PGStore) InsertInboxMessage(ctx context.Context, message InboxMessage) (outErr error) {
	tx, err := s.dbpool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// We defer a rollback, rollback after commit won't be treated as an
	// error.
	defer pg.Rollback(tx, &outErr)

	q := postgres.New(tx)

	var nextID int64

	// Retry on primary key violation which can happen
	// if another instance inserts the first message before this one.
	for attempt := 0; attempt < 2; attempt++ {
		lock, err := q.GetMessageWriteLock(ctx, postgres.GetMessageWriteLockParams{
			Recipient:   message.Recipient,
			MessageType: string(MessageTypeInbox),
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get message lock: %w", err)
		}

		nextID = lock.CurrentMessageID.Int64 + 1

		payload, err := json.Marshal(message.Payload)
		if err != nil {
			return fmt.Errorf("marshal message payload: %w", err)
		}

		err = q.InsertInboxMessage(ctx, postgres.InsertInboxMessageParams{
			Recipient: message.Recipient,
			ID:        nextID,
			Created:   pg.Time(message.Created),
			CreatedBy: message.CreatedBy,
			Updated:   pg.Time(message.Updated),
			IsRead:    message.IsRead,
			Payload:   payload,
		})

		if err == nil {
			break
		}

		if !pg.IsConstraintError(err, "inbox_message_pkey") {
			return fmt.Errorf("insert inbox message: %w", err)
		}
	}

	err = q.UpsertMessageWriteLock(ctx, postgres.UpsertMessageWriteLockParams{
		Recipient:        message.Recipient,
		MessageType:      string(MessageTypeInbox),
		CurrentMessageID: pg.BigintOrNull(nextID),
	})
	if err != nil {
		return fmt.Errorf("upsert message lock: %w", err)
	}

	err = notifyInboxMessageUpdated(ctx, q, MessageEvent{
		ID:        nextID,
		Recipient: message.Recipient,
	})
	if err != nil {
		return fmt.Errorf("send notification: %w", err)
	}

	err = tx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

// InsertMessage implements Store.
func (s *PGStore) InsertMessage(ctx context.Context, message Message) (outErr error) {
	tx, err := s.dbpool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// We defer a rollback, rollback after commit won't be treated as an
	// error.
	defer pg.Rollback(tx, &outErr)

	q := postgres.New(tx)

	var nextID int64

	// Retry on primary key violation which can happen
	// if another instance inserts the first message before this one.
	for attempt := 0; attempt < 2; attempt++ {
		lock, err := q.GetMessageWriteLock(ctx, postgres.GetMessageWriteLockParams{
			Recipient:   message.Recipient,
			MessageType: string(MessageTypeSystem),
		})
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("get message lock: %w", err)
		}

		nextID = lock.CurrentMessageID.Int64 + 1

		payload, err := json.Marshal(message.Payload)
		if err != nil {
			return fmt.Errorf("marshal message payload: %w", err)
		}

		err = q.InsertMessage(ctx, postgres.InsertMessageParams{
			Recipient: message.Recipient,
			ID:        nextID,
			Type:      pg.TextOrNull(message.Type),
			Created:   pg.Time(message.Created),
			CreatedBy: message.CreatedBy,
			DocUuid:   pg.PUUID(message.DocUUID),
			DocType:   pg.TextOrNull(message.DocType),
			Payload:   payload,
		})

		if err == nil {
			break
		}

		if !pg.IsConstraintError(err, "message_pkey") {
			return fmt.Errorf("insert message: %w", err)
		}
	}

	err = q.UpsertMessageWriteLock(ctx, postgres.UpsertMessageWriteLockParams{
		Recipient:        message.Recipient,
		MessageType:      string(MessageTypeSystem),
		CurrentMessageID: pg.BigintOrNull(nextID),
	})
	if err != nil {
		return fmt.Errorf("upsert message lock: %w", err)
	}

	err = notifyMessageUpdated(ctx, q, MessageEvent{
		ID:        nextID,
		Recipient: message.Recipient,
	})
	if err != nil {
		return fmt.Errorf("send notification: %w", err)
	}

	err = tx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

func notifyMessageUpdated(
	ctx context.Context, q *postgres.Queries,
	payload MessageEvent,
) error {
	return pgNotify(ctx, q, NotifyChannelMessageUpdate, payload)
}

func notifyInboxMessageUpdated(
	ctx context.Context, q *postgres.Queries,
	payload MessageEvent,
) error {
	return pgNotify(ctx, q, NotifyChannelInboxMessageUpdate, payload)
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
