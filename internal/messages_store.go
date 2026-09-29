package internal

import (
	"context"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine/pg"
)

// Interface guard.
var _ MessagesStore = &PGStore{}

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

// GetLatestInboxMessageID implements [MessagesStore].
func (s *PGStore) GetLatestInboxMessageID(
	ctx context.Context, recipient string,
) (int64, error) {
	id, err := s.q.GetLatestInboxMessageId(ctx, recipient)
	if err != nil {
		return -1, fmt.Errorf("get latest message id: %w", err)
	}

	return id, nil
}

// ListInboxMessagesBeforeID implements [MessagesStore].
func (s *PGStore) ListInboxMessagesBeforeID(
	ctx context.Context, recipient string, beforeID int64, size int64,
) ([]InboxMessage, error) {
	rows, err := s.q.ListInboxMessagesBeforeId(ctx, postgres.ListInboxMessagesBeforeIdParams{
		Recipient: recipient,
		BeforeID:  beforeID,
		Limit:     size,
	})
	if err != nil {
		return nil, fmt.Errorf("list inbox messages: %w", err)
	}

	var res []InboxMessage

	for i := range rows {
		msg := InboxMessage{
			Recipient: rows[i].Recipient,
			ID:        rows[i].ID,
			Created:   rows[i].Created.Time,
			CreatedBy: rows[i].CreatedBy,
			Updated:   rows[i].Updated.Time,
			IsRead:    rows[i].IsRead,
			Payload:   rows[i].Payload,
		}

		res = append(res, msg)
	}

	return res, nil
}

// ListInboxMessagesAfterID implements [MessagesStore].
func (s *PGStore) ListInboxMessagesAfterID(
	ctx context.Context, recipient string, afterID int64, size int64,
) ([]InboxMessage, error) {
	rows, err := s.q.ListInboxMessagesAfterId(ctx, postgres.ListInboxMessagesAfterIdParams{
		Recipient: recipient,
		AfterID:   afterID,
		Limit:     size,
	})
	if err != nil {
		return nil, fmt.Errorf("list inbox messages: %w", err)
	}

	var res []InboxMessage

	for i := range rows {
		msg := InboxMessage{
			Recipient: rows[i].Recipient,
			ID:        rows[i].ID,
			Created:   rows[i].Created.Time,
			CreatedBy: rows[i].CreatedBy,
			Updated:   rows[i].Updated.Time,
			IsRead:    rows[i].IsRead,
			Payload:   rows[i].Payload,
		}

		res = append(res, msg)
	}

	return res, nil
}

// GetLatestMessageID implements [MessagesStore].
func (s *PGStore) GetLatestMessageID(
	ctx context.Context, recipient string,
) (int64, error) {
	id, err := s.q.GetLatestMessageId(ctx, recipient)
	if err != nil {
		return -1, fmt.Errorf("get latest message id: %w", err)
	}

	return id, nil
}

// ListMessagesAfterID implements [MessagesStore].
func (s *PGStore) ListMessagesAfterID(
	ctx context.Context, recipient string, afterID int64, size int64,
) ([]Message, error) {
	rows, err := s.q.ListMessagesAfterId(ctx, postgres.ListMessagesAfterIdParams{
		Recipient: recipient,
		AfterID:   afterID,
		Limit:     size,
	})
	if err != nil {
		return nil, fmt.Errorf("list messages: %w", err)
	}

	var res []Message

	for i := range rows {
		msg := Message{
			Recipient: rows[i].Recipient,
			ID:        rows[i].ID,
			Created:   rows[i].Created.Time,
			CreatedBy: rows[i].CreatedBy,
			DocUUID:   pg.ToUUIDPointer(rows[i].DocUuid),
			DocType:   rows[i].DocType.String,
			Payload:   rows[i].Payload,
		}

		res = append(res, msg)
	}

	return res, nil
}

// InsertInboxMessage implements [MessagesStore].
func (s *PGStore) InsertInboxMessage(
	ctx context.Context, message InboxMessage,
) error {
	return pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		q := postgres.New(tx)

		err := q.UpsertUser(ctx, postgres.UpsertUserParams{
			Sub:     message.Recipient,
			Created: pg.Time(message.Created),
			Kind:    postgres.UserKindUser,
		})
		if err != nil {
			return fmt.Errorf("upsert user: %w", err)
		}

		nextID, err := nextMessageID(ctx, q, message.Recipient, MessageTypeInbox)
		if err != nil {
			return err
		}

		err = q.InsertInboxMessage(ctx, postgres.InsertInboxMessageParams{
			Recipient: message.Recipient,
			ID:        nextID,
			Created:   pg.Time(message.Created),
			CreatedBy: message.CreatedBy,
			Updated:   pg.Time(message.Updated),
			IsRead:    message.IsRead,
			Payload:   message.Payload,
		})
		if err != nil {
			return fmt.Errorf("insert inbox message: %w", err)
		}

		err = notifyInboxMessageUpdated(ctx, q, MessageEvent{
			ID:        nextID,
			Recipient: message.Recipient,
		})
		if err != nil {
			return fmt.Errorf("send notification: %w", err)
		}

		return nil
	})
}

// InsertMessage implements [MessagesStore].
func (s *PGStore) InsertMessage(
	ctx context.Context, message Message,
) error {
	return pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		q := postgres.New(tx)

		err := q.UpsertUser(ctx, postgres.UpsertUserParams{
			Sub:     message.Recipient,
			Created: pg.Time(message.Created),
			Kind:    postgres.UserKindUser,
		})
		if err != nil {
			return fmt.Errorf("upsert user: %w", err)
		}

		nextID, err := nextMessageID(ctx, q, message.Recipient, MessageTypeSystem)
		if err != nil {
			return err
		}

		err = q.InsertMessage(ctx, postgres.InsertMessageParams{
			Recipient: message.Recipient,
			ID:        nextID,
			Type:      pg.TextOrNull(message.Type),
			Created:   pg.Time(message.Created),
			CreatedBy: message.CreatedBy,
			DocUuid:   pg.PUUID(message.DocUUID),
			DocType:   pg.TextOrNull(message.DocType),
			Payload:   message.Payload,
		})
		if err != nil {
			return fmt.Errorf("insert message: %w", err)
		}

		err = notifyMessageUpdated(ctx, q, MessageEvent{
			ID:        nextID,
			Recipient: message.Recipient,
		})
		if err != nil {
			return fmt.Errorf("send notification: %w", err)
		}

		return nil
	})
}

// nextMessageID allocates the next per-recipient message id. The upsert
// creates the lock row on first use and row-locks it for the rest of the
// transaction, which serializes writers per recipient and keeps ids gapless
// and commit-ordered.
func nextMessageID(
	ctx context.Context, q *postgres.Queries,
	recipient string, messageType MessageType,
) (int64, error) {
	nextID, err := q.NextMessageID(ctx, postgres.NextMessageIDParams{
		Recipient:   recipient,
		MessageType: string(messageType),
	})
	if err != nil {
		return 0, fmt.Errorf("advance message lock: %w", err)
	}

	return nextID, nil
}

// UpdateInboxMessage implements [MessagesStore].
func (s *PGStore) UpdateInboxMessage(
	ctx context.Context, recipient string, id int64, isRead bool,
) error {
	err := s.q.UpdateInboxMessage(ctx, postgres.UpdateInboxMessageParams{
		Recipient: recipient,
		ID:        id,
		IsRead:    isRead,
	})
	if err != nil {
		return fmt.Errorf("update inbox message: %w", err)
	}

	return nil
}

// DeleteInboxMessage implements [MessagesStore].
func (s *PGStore) DeleteInboxMessage(
	ctx context.Context, recipient string, id int64,
) error {
	err := s.q.DeleteInboxMessage(ctx, postgres.DeleteInboxMessageParams{
		Recipient: recipient,
		ID:        id,
	})
	if err != nil {
		return fmt.Errorf("delete inbox message: %w", err)
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
