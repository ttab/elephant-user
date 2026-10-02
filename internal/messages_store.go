package internal

import (
	"context"
	"errors"
	"fmt"
	"time"

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

func notifyMessageUpdated(
	ctx context.Context, q *postgres.Queries,
	payload MessageEvent,
) error {
	return pgNotify(ctx, q, NotifyChannelMessageUpdate, payload)
}

// sequenceInbox is the sequence_counter row that hands out inbox message
// ids: one commit-ordered id space across every recipient.
const sequenceInbox = "inbox"

// OnInboxMessageUpdate notifies ch of inbox messages pushed to any of the
// reader's owners with an id above afterID. Subscription is cancelled with
// the context. Delivery is best effort: a non-blocking send on ch, so a
// busy receiver with an unbuffered channel misses events.
func (s *PGStore) OnInboxMessageUpdate(
	ctx context.Context, ch chan MessageEvent,
	reader InboxReader, afterID int64,
) {
	ownerSet := make(map[string]bool, len(reader.Owners))
	for _, o := range reader.Owners {
		ownerSet[o] = true
	}

	go s.InboxMessages.Listen(ctx, ch, func(msg MessageEvent) bool {
		return ownerSet[msg.Recipient] && msg.ID > afterID
	})
}

// GetLatestInboxMessageID implements [MessagesStore].
func (s *PGStore) GetLatestInboxMessageID(
	ctx context.Context, reader InboxReader,
) (int64, error) {
	id, err := s.q.GetLatestInboxMessageId(ctx, reader.Owners)
	if err != nil {
		return -1, fmt.Errorf("get latest inbox message id: %w", err)
	}

	return id, nil
}

// ListInboxMessagesBeforeID implements [MessagesStore].
func (s *PGStore) ListInboxMessagesBeforeID(
	ctx context.Context, reader InboxReader, beforeID int64, size int64,
) ([]InboxMessage, error) {
	rows, err := s.q.ListInboxMessagesBeforeId(ctx, postgres.ListInboxMessagesBeforeIdParams{
		Subject:  reader.Subject,
		Owners:   reader.Owners,
		BeforeID: beforeID,
		Limit:    size,
	})
	if err != nil {
		return nil, fmt.Errorf("list inbox messages: %w", err)
	}

	res := make([]InboxMessage, len(rows))

	for i, r := range rows {
		res[i] = InboxMessage{
			ID:        r.ID,
			UUID:      r.UUID,
			Recipient: r.Recipient,
			Created:   r.Created.Time,
			CreatedBy: r.CreatedBy,
			IsRead:    r.IsRead,
			Payload:   r.Payload,
		}
	}

	return res, nil
}

// ListInboxMessagesAfterID implements [MessagesStore].
func (s *PGStore) ListInboxMessagesAfterID(
	ctx context.Context, reader InboxReader, afterID int64, size int64,
) ([]InboxMessage, error) {
	rows, err := s.q.ListInboxMessagesAfterId(ctx, postgres.ListInboxMessagesAfterIdParams{
		Subject: reader.Subject,
		Owners:  reader.Owners,
		AfterID: afterID,
		Limit:   size,
	})
	if err != nil {
		return nil, fmt.Errorf("list inbox messages: %w", err)
	}

	res := make([]InboxMessage, len(rows))

	for i, r := range rows {
		res[i] = InboxMessage{
			ID:        r.ID,
			UUID:      r.UUID,
			Recipient: r.Recipient,
			Created:   r.Created.Time,
			CreatedBy: r.CreatedBy,
			IsRead:    r.IsRead,
			Payload:   r.Payload,
		}
	}

	return res, nil
}

// InsertInboxMessage implements [MessagesStore]. It returns the id of the
// stored message, or of the message already stored for the same recipient
// and payload uuid, so a retried push is answered like the original. A
// stored message with the same uuid but a different payload is
// ErrInboxMessageConflict.
func (s *PGStore) InsertInboxMessage(
	ctx context.Context, message InboxMessage,
) (int64, error) {
	lookup := postgres.GetInboxMessageByUUIDParams{
		Payload:   message.Payload,
		Recipient: message.Recipient,
		UUID:      message.UUID,
	}

	// A retry of a push whose response was lost is the common duplicate;
	// answer it without touching the counter.
	id, found, err := existingInboxMessage(ctx, s.q, lookup)
	if err != nil {
		return 0, err
	}

	if found {
		return id, nil
	}

	err = pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		q := postgres.New(tx)

		// The counter is the first and only lock this transaction
		// takes. It serialises every inbox push, so the uuid check
		// below cannot race another push of the same document, and
		// the transaction locks no data row that a writer holding the
		// counter could be waiting on. Unlike the eventlog counter it
		// therefore need not come last.
		next, err := q.ReserveSequenceValues(ctx, postgres.ReserveSequenceValuesParams{
			Name:  sequenceInbox,
			Count: 1,
		})
		if err != nil {
			return fmt.Errorf("reserve inbox message id: %w", err)
		}

		existing, found, err := existingInboxMessage(ctx, q, lookup)
		if err != nil {
			return err
		}

		if found {
			id = existing

			return nil
		}

		err = q.InsertInboxMessage(ctx, postgres.InsertInboxMessageParams{
			ID:        next,
			UUID:      message.UUID,
			Recipient: message.Recipient,
			Created:   pg.Time(message.Created),
			CreatedBy: message.CreatedBy,
			Payload:   message.Payload,
		})
		if err != nil {
			return fmt.Errorf("insert inbox message: %w", err)
		}

		err = notifyInboxMessageUpdated(ctx, q, MessageEvent{
			ID:        next,
			Recipient: message.Recipient,
		})
		if err != nil {
			return fmt.Errorf("send notification: %w", err)
		}

		id = next

		return nil
	})
	if err != nil {
		return 0, err
	}

	return id, nil
}

// existingInboxMessage reports the id of the message already stored for
// the recipient and payload uuid in lookup, if any. One with a different
// payload is ErrInboxMessageConflict.
func existingInboxMessage(
	ctx context.Context, q *postgres.Queries,
	lookup postgres.GetInboxMessageByUUIDParams,
) (int64, bool, error) {
	row, err := q.GetInboxMessageByUUID(ctx, lookup)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}

	if err != nil {
		return 0, false, fmt.Errorf("look up inbox message by uuid: %w", err)
	}

	if !row.SamePayload {
		return 0, false, ErrInboxMessageConflict
	}

	return row.ID, true, nil
}

// SetInboxMessageRead implements [MessagesStore]. The message must be
// addressed to one of the reader's owners, otherwise
// ErrInboxMessageNotFound.
func (s *PGStore) SetInboxMessageRead(
	ctx context.Context, reader InboxReader, id int64,
	isRead bool, updated time.Time,
) error {
	return pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		q := postgres.New(tx)

		msg, err := readableInboxMessage(ctx, q, reader, id)
		if err != nil {
			return err
		}

		err = q.SetInboxMessageRead(ctx, postgres.SetInboxMessageReadParams{
			MessageID: id,
			Subject:   reader.Subject,
			IsRead:    isRead,
			Updated:   pg.Time(updated),
		})
		if err != nil {
			return fmt.Errorf("set inbox message read state: %w", err)
		}

		return notifyInboxStateUpdated(ctx, q, InboxStateEvent{
			ID:        id,
			Recipient: msg.Recipient,
			Subject:   reader.Subject,
			CreatedBy: msg.CreatedBy,
		})
	})
}

// HideInboxMessage implements [MessagesStore]. It hides the message for the
// reading subject only; the message stays for every other reader. The
// message must be addressed to one of the reader's owners, otherwise
// ErrInboxMessageNotFound.
func (s *PGStore) HideInboxMessage(
	ctx context.Context, reader InboxReader, id int64, updated time.Time,
) error {
	return pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		q := postgres.New(tx)

		msg, err := readableInboxMessage(ctx, q, reader, id)
		if err != nil {
			return err
		}

		err = q.HideInboxMessage(ctx, postgres.HideInboxMessageParams{
			MessageID: id,
			Subject:   reader.Subject,
			Updated:   pg.Time(updated),
		})
		if err != nil {
			return fmt.Errorf("hide inbox message: %w", err)
		}

		return notifyInboxStateUpdated(ctx, q, InboxStateEvent{
			ID:        id,
			Recipient: msg.Recipient,
			Subject:   reader.Subject,
			CreatedBy: msg.CreatedBy,
		})
	})
}

// readableInboxMessage loads the message with the given id if it is
// addressed to one of the reader's owners. Anything else is
// ErrInboxMessageNotFound: a reader must not learn whether an id exists in
// somebody else's inbox.
func readableInboxMessage(
	ctx context.Context, q *postgres.Queries,
	reader InboxReader, id int64,
) (postgres.GetInboxMessageForReaderRow, error) {
	msg, err := q.GetInboxMessageForReader(ctx, postgres.GetInboxMessageForReaderParams{
		ID:     id,
		Owners: reader.Owners,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return msg, ErrInboxMessageNotFound
	}

	if err != nil {
		return msg, fmt.Errorf("get inbox message: %w", err)
	}

	return msg, nil
}

func notifyInboxMessageUpdated(
	ctx context.Context, q *postgres.Queries,
	payload MessageEvent,
) error {
	return pgNotify(ctx, q, NotifyChannelInboxMessageUpdate, payload)
}

func notifyInboxStateUpdated(
	ctx context.Context, q *postgres.Queries,
	payload InboxStateEvent,
) error {
	return pgNotify(ctx, q, NotifyChannelInboxStateUpdate, payload)
}
