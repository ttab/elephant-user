package internal

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/ttab/elephant-api/newsdoc"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine"
	"github.com/ttab/elephantine/pg"
)

var ErrDocNotFound = errors.New("document not found")

type NotifyChannel = string

const (
	NotifyChannelMessageUpdate      NotifyChannel = "message_update"
	NotifyChannelInboxMessageUpdate NotifyChannel = "inbox_message_update"
	NotifyChannelEventLogUpdate     NotifyChannel = "event_log_update"
)

type PGStore struct {
	logger *slog.Logger
	dbpool *pgxpool.Pool
	q      *postgres.Queries

	Messages      *pg.FanOut[MessageEvent]
	InboxMessages *pg.FanOut[MessageEvent]
	EventLog      *pg.FanOut[EventLogEvent]
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
		EventLog:      pg.NewFanOut[EventLogEvent](NotifyChannelEventLogUpdate),
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

func (s *PGStore) OnEventLogUpdate(
	ctx context.Context, ch chan EventLogEvent,
	owners []string, afterID int64,
) {
	ownerMap := make(map[string]bool)
	for _, o := range owners {
		ownerMap[o] = true
	}

	go s.EventLog.Listen(ctx, ch, func(msg EventLogEvent) bool {
		return ownerMap[msg.Owner] && msg.ID > afterID
	})
}

// GetLatestInboxMessageID implements Store.
func (s *PGStore) GetLatestInboxMessageID(
	ctx context.Context, recipient string,
) (int64, error) {
	id, err := s.q.GetLatestInboxMessageId(ctx, recipient)
	if err != nil {
		return -1, fmt.Errorf("get latest message id: %w", err)
	}

	return id, nil
}

// ListInboxMessagesBeforeID implements Store.
//
//nolint:dupl
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

// ListInboxMessagesAfterID implements Store.
//
//nolint:dupl
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

// GetLatestMessageID implements Store.
func (s *PGStore) GetLatestMessageID(
	ctx context.Context, recipient string,
) (int64, error) {
	id, err := s.q.GetLatestMessageId(ctx, recipient)
	if err != nil {
		return -1, fmt.Errorf("get latest message id: %w", err)
	}

	return id, nil
}

// ListMessagesAfterID implements Store.
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
		var payload map[string]string

		err = json.Unmarshal(rows[i].Payload, &payload)
		if err != nil {
			return nil, fmt.Errorf("unmarshal payload: %w", err)
		}

		msg := Message{
			Recipient: rows[i].Recipient,
			ID:        rows[i].ID,
			Created:   rows[i].Created.Time,
			CreatedBy: rows[i].CreatedBy,
			DocUUID:   pg.ToUUIDPointer(rows[i].DocUuid),
			DocType:   rows[i].DocType.String,
			Payload:   payload,
		}

		res = append(res, msg)
	}

	return res, nil
}

// InsertInboxMessage implements Store.
func (s *PGStore) InsertInboxMessage(
	ctx context.Context, message InboxMessage,
) (outErr error) {
	tx, err := s.dbpool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// We defer a rollback, rollback after commit won't be treated as an
	// error.
	defer pg.Rollback(tx, &outErr)

	q := postgres.New(tx)

	err = q.UpsertUser(ctx, postgres.UpsertUserParams{
		Sub:     message.Recipient,
		Created: pg.Time(message.Created),
		Kind:    postgres.UserKindUser,
	})
	if err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}

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
func (s *PGStore) InsertMessage(
	ctx context.Context, message Message,
) (outErr error) {
	tx, err := s.dbpool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	// We defer a rollback, rollback after commit won't be treated as an
	// error.
	defer pg.Rollback(tx, &outErr)

	q := postgres.New(tx)

	err = q.UpsertUser(ctx, postgres.UpsertUserParams{
		Sub:     message.Recipient,
		Created: pg.Time(message.Created),
		Kind:    postgres.UserKindUser,
	})
	if err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}

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

// UpdateInboxMessage implements Store.
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

// DeleteInboxMessage implements Store.
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

func (s *PGStore) GetDocument(
	ctx context.Context, owner string, application string,
	docType string, key string,
) (*Document, error) {
	row, err := s.q.GetDocument(ctx, postgres.GetDocumentParams{
		Owner:       owner,
		Application: application,
		Type:        docType,
		Key:         key,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrDocNotFound
	} else if err != nil {
		return nil, fmt.Errorf("get document: %w", err)
	}

	return &Document{
		Owner:         row.Owner,
		Application:   row.Application,
		Type:          row.Type,
		Key:           row.Key,
		Title:         row.Title,
		Version:       row.Version,
		SchemaVersion: row.SchemaVersion,
		Created:       row.Created.Time,
		Updated:       row.Updated.Time,
		UpdatedBy:     row.UpdatedBy,
		Payload:       row.Payload,
	}, nil
}

func (s *PGStore) ListDocuments(
	ctx context.Context, owners []string, application string,
	docType string, includePayload bool,
) ([]*Document, error) {
	if includePayload {
		rows, err := s.q.ListDocumentsFull(ctx, postgres.ListDocumentsFullParams{
			Owners:      owners,
			Application: pg.TextOrNull(application),
			Type:        pg.TextOrNull(docType),
		})
		if err != nil {
			return nil, fmt.Errorf("list full documents: %w", err)
		}

		docs := make([]*Document, len(rows))
		for i, r := range rows {
			docs[i] = &Document{
				Owner:         r.Owner,
				Application:   r.Application,
				Type:          r.Type,
				Key:           r.Key,
				Version:       r.Version,
				SchemaVersion: r.SchemaVersion,
				Title:         r.Title,
				Created:       r.Created.Time,
				Updated:       r.Updated.Time,
				UpdatedBy:     r.UpdatedBy,
				Payload:       r.Payload,
			}
		}

		return docs, nil
	}

	rows, err := s.q.ListDocumentsMetadata(ctx, postgres.ListDocumentsMetadataParams{
		Owners:      owners,
		Application: pg.TextOrNull(application),
		Type:        pg.TextOrNull(docType),
	})
	if err != nil {
		return nil, fmt.Errorf("list documents metadata: %w", err)
	}

	docs := make([]*Document, len(rows))
	for i, r := range rows {
		docs[i] = &Document{
			Owner:         r.Owner,
			Application:   r.Application,
			Type:          r.Type,
			Key:           r.Key,
			Version:       r.Version,
			SchemaVersion: r.SchemaVersion,
			Title:         r.Title,
			Created:       r.Created.Time,
			Updated:       r.Updated.Time,
			UpdatedBy:     r.UpdatedBy,
			Payload:       nil,
		}
	}

	return docs, nil
}

func (s *PGStore) UpdateDocument(
	ctx context.Context, update DocumentUpdate,
) (outErr error) {
	tx, err := s.dbpool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer pg.Rollback(tx, &outErr)

	q := postgres.New(tx)

	err = q.UpsertUser(ctx, postgres.UpsertUserParams{
		Sub:     update.Owner,
		Created: pg.Time(time.Now()),
		Kind:    postgres.UserKindUser,
	})
	if err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}

	version, err := q.UpsertDocument(ctx, postgres.UpsertDocumentParams{
		Owner:         update.Owner,
		Application:   update.Application,
		Type:          update.Type,
		Key:           update.Key,
		SchemaVersion: update.SchemaVersion,
		Title:         update.Title,
		Payload:       update.Payload,
		UpdatedBy:     update.UpdatedBy,
	})
	if err != nil {
		return fmt.Errorf("upsert document: %w", err)
	}

	err = logAndNotify(ctx, q, postgres.InsertEventLogParams{
		Owner:        update.Owner,
		Type:         postgres.EventTypeUpdate,
		ResourceKind: postgres.ResourceKindDocument,
		Application:  update.Application,
		DocumentType: pg.Text(update.Type),
		Key:          update.Key,
		Version:      pg.Int64(version),
		UpdatedBy:    update.UpdatedBy,
		Payload:      nil,
	})
	if err != nil {
		return fmt.Errorf("log and notify: %w", err)
	}

	err = tx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

func (s *PGStore) DeleteDocument(
	ctx context.Context, owner string, application string,
	docType string, key string,
) (outErr error) {
	tx, err := s.dbpool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer pg.Rollback(tx, &outErr)

	q := postgres.New(tx)

	_, err = q.DeleteDocument(ctx, postgres.DeleteDocumentParams{
		Owner:       owner,
		Application: application,
		Type:        docType,
		Key:         key,
	})
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("delete document: %w", err)
	}

	err = logAndNotify(ctx, q, postgres.InsertEventLogParams{
		Owner:        owner,
		Type:         postgres.EventTypeDelete,
		ResourceKind: postgres.ResourceKindDocument,
		Application:  application,
		DocumentType: pg.Text(docType),
		Key:          key,
		UpdatedBy:    owner,
		Payload:      nil,
	})
	if err != nil {
		return fmt.Errorf("log and notify: %w", err)
	}

	err = tx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

func (s *PGStore) GetProperties(
	ctx context.Context, owner string,
	application string, keys []string,
) ([]Property, error) {
	rows, err := s.q.GetProperties(ctx, postgres.GetPropertiesParams{
		Owner:       owner,
		Application: pg.TextOrNull(application),
		Keys:        keys,
	})
	if err != nil {
		return nil, fmt.Errorf("get properties: %w", err)
	}

	props := make([]Property, len(rows))

	for i, r := range rows {
		props[i] = Property{
			Owner:       r.Owner,
			Application: r.Application,
			Key:         r.Key,
			Value:       r.Value,
			Created:     r.Created.Time,
			Updated:     r.Updated.Time,
		}
	}

	return props, nil
}

func (s *PGStore) SetProperties(
	ctx context.Context, owner string,
	updates []PropertyUpdate,
) (outErr error) {
	tx, err := s.dbpool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer pg.Rollback(tx, &outErr)

	q := postgres.New(tx)

	err = q.UpsertUser(ctx, postgres.UpsertUserParams{
		Sub:     owner,
		Created: pg.Time(time.Now()),
		Kind:    postgres.UserKindUser,
	})
	if err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}

	for _, prop := range updates {
		err := q.UpsertProperty(ctx, postgres.UpsertPropertyParams{
			Owner:       owner,
			Application: prop.Application,
			Key:         prop.Key,
			Value:       prop.Value,
		})
		if err != nil {
			return fmt.Errorf("upsert property %s/%s: %w", prop.Application, prop.Key, err)
		}

		err = logAndNotify(ctx, q, postgres.InsertEventLogParams{
			Owner:        owner,
			Type:         postgres.EventTypeUpdate,
			ResourceKind: postgres.ResourceKindProperty,
			Application:  prop.Application,
			UpdatedBy:    owner,
			Key:          prop.Key,
			Payload:      nil,
		})
		if err != nil {
			return fmt.Errorf("log and notify: %w", err)
		}
	}

	err = tx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

func (s *PGStore) DeleteProperties(
	ctx context.Context, owner string,
	deletes []PropertyDelete,
) (outErr error) {
	tx, err := s.dbpool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}

	defer pg.Rollback(tx, &outErr)

	q := postgres.New(tx)

	for _, prop := range deletes {
		_, err := q.DeleteProperty(ctx, postgres.DeletePropertyParams{
			Owner:       owner,
			Application: prop.Application,
			Key:         prop.Key,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		} else if err != nil {
			return fmt.Errorf("delete property %s/%s: %w", prop.Application, prop.Key, err)
		}

		err = logAndNotify(ctx, q, postgres.InsertEventLogParams{
			Owner:        owner,
			Type:         postgres.EventTypeDelete,
			ResourceKind: postgres.ResourceKindProperty,
			Application:  prop.Application,
			UpdatedBy:    owner,
			Key:          prop.Key,
			Payload:      nil,
		})
		if err != nil {
			return fmt.Errorf("log and notify: %w", err)
		}
	}

	err = tx.Commit(ctx)
	if err != nil {
		return fmt.Errorf("commit transaction: %w", err)
	}

	return nil
}

func (s *PGStore) GetLatestEventLogID(
	ctx context.Context, owners []string,
) (int64, error) {
	id, err := s.q.GetLatestEventLogId(ctx, owners)
	if err != nil {
		return -1, fmt.Errorf("get latest eventlog id: %w", err)
	}

	return id, nil
}

func (s *PGStore) GetEventLogEntriesAfterID(
	ctx context.Context, owners []string,
	afterID int64, limit int64,
) ([]EventLogEntry, error) {
	rows, err := s.q.GetEventLogEntriesAfterId(ctx, postgres.GetEventLogEntriesAfterIdParams{
		Owners:  owners,
		AfterID: afterID,
		Limit:   limit,
	})
	if err != nil {
		return nil, fmt.Errorf("get event log entries: %w", err)
	}

	events := make([]EventLogEntry, len(rows))
	for i, r := range rows {
		events[i] = EventLogEntry{
			ID:           r.ID,
			Owner:        r.Owner,
			Type:         r.Type,
			ResourceKind: r.ResourceKind,
			Application:  r.Application,
			DocumentType: r.DocumentType.String,
			Key:          r.Key,
			Version:      r.Version.Int64,
			UpdatedBy:    r.UpdatedBy,
			Created:      r.Created.Time,
			// Payload is currently unused and reserved for future extensibility.
			Payload: r.Payload,
		}
	}

	return events, nil
}

func logAndNotify(
	ctx context.Context, q *postgres.Queries,
	params postgres.InsertEventLogParams,
) error {
	id, err := q.InsertEventLog(ctx, params)
	if err != nil {
		return fmt.Errorf("insert event log: %w", err)
	}

	err = notifyEventLogUpdated(ctx, q, EventLogEvent{
		ID:    id,
		Owner: params.Owner,
	})
	if err != nil {
		return fmt.Errorf("send notification: %w", err)
	}

	return nil
}

func notifyEventLogUpdated(
	ctx context.Context, q *postgres.Queries,
	payload EventLogEvent,
) error {
	return pgNotify(ctx, q, NotifyChannelEventLogUpdate, payload)
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

func (s *PGStore) RunCleaner(ctx context.Context, period time.Duration) {
	for {
		select {
		case <-time.After(period):
		case <-ctx.Done():
			return
		}

		jobLock, err := pg.NewJobLock(s.dbpool, s.logger, "cleaner", pg.JobLockOptions{
			PingInterval:  10 * time.Second,
			StaleAfter:    1 * time.Minute,
			CheckInterval: 20 * time.Second,
			Timeout:       5 * time.Second,
		})
		if err != nil {
			s.logger.ErrorContext(ctx, "failed to create job lock",
				elephantine.LogKeyError, err)

			continue
		}

		err = jobLock.RunWithContext(ctx, s.removeOldMessages)
		if err != nil {
			s.logger.ErrorContext(
				ctx, "lock cleaner error",
				elephantine.LogKeyError, err,
			)
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
