package internal

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/ttab/elephant-user/postgres"
	"github.com/ttab/elephantine/pg"
)

// sequenceEventLog is the sequence_counter row that hands out eventlog ids.
const sequenceEventLog = "eventlog"

// Interface guard.
var _ SettingsStore = &PGStore{}

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
) error {
	return pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		q := postgres.New(tx)

		err := q.UpsertUser(ctx, postgres.UpsertUserParams{
			Sub:     update.Owner,
			Created: pg.Time(time.Now()),
			Kind:    ownerToUserKind(update.Owner),
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

		return nil
	})
}

func (s *PGStore) DeleteDocument(
	ctx context.Context, owner string, application string,
	docType string, key string,
) error {
	return pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		q := postgres.New(tx)

		_, err := q.DeleteDocument(ctx, postgres.DeleteDocumentParams{
			Owner:       owner,
			Application: application,
			Type:        docType,
			Key:         key,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			// Nothing was deleted, so there is no change to log.
			return nil
		} else if err != nil {
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

		return nil
	})
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
) error {
	return pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		q := postgres.New(tx)

		err := q.UpsertUser(ctx, postgres.UpsertUserParams{
			Sub:     owner,
			Created: pg.Time(time.Now()),
			Kind:    postgres.UserKindUser,
		})
		if err != nil {
			return fmt.Errorf("upsert user: %w", err)
		}

		// Lock property rows in a stable order so that concurrent writes with
		// overlapping keys cannot deadlock on each other.
		slices.SortFunc(updates, func(a, b PropertyUpdate) int {
			return cmp.Or(
				cmp.Compare(a.Application, b.Application),
				cmp.Compare(a.Key, b.Key),
			)
		})

		events := make([]postgres.InsertEventLogParams, 0, len(updates))

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

			events = append(events, postgres.InsertEventLogParams{
				Owner:        owner,
				Type:         postgres.EventTypeUpdate,
				ResourceKind: postgres.ResourceKindProperty,
				Application:  prop.Application,
				UpdatedBy:    owner,
				Key:          prop.Key,
				Payload:      nil,
			})
		}

		err = logAndNotifyAll(ctx, q, events)
		if err != nil {
			return fmt.Errorf("log and notify: %w", err)
		}

		return nil
	})
}

func (s *PGStore) DeleteProperties(
	ctx context.Context, owner string,
	deletes []PropertyDelete,
) error {
	return pg.WithTX(ctx, s.dbpool, func(tx pgx.Tx) error {
		q := postgres.New(tx)

		// Lock property rows in a stable order so that concurrent writes with
		// overlapping keys cannot deadlock on each other.
		slices.SortFunc(deletes, func(a, b PropertyDelete) int {
			return cmp.Or(
				cmp.Compare(a.Application, b.Application),
				cmp.Compare(a.Key, b.Key),
			)
		})

		events := make([]postgres.InsertEventLogParams, 0, len(deletes))

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

			events = append(events, postgres.InsertEventLogParams{
				Owner:        owner,
				Type:         postgres.EventTypeDelete,
				ResourceKind: postgres.ResourceKindProperty,
				Application:  prop.Application,
				UpdatedBy:    owner,
				Key:          prop.Key,
				Payload:      nil,
			})
		}

		err := logAndNotifyAll(ctx, q, events)
		if err != nil {
			return fmt.Errorf("log and notify: %w", err)
		}

		return nil
	})
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

// logAndNotify appends a single eventlog entry, see logAndNotifyAll.
func logAndNotify(
	ctx context.Context, q *postgres.Queries,
	params postgres.InsertEventLogParams,
) error {
	return logAndNotifyAll(ctx, q, []postgres.InsertEventLogParams{params})
}

// logAndNotifyAll appends eventlog entries and notifies listeners. Ids are
// reserved from the eventlog sequence counter, which row-locks the counter
// for the rest of the transaction so that ids are handed out in commit
// order. Timestamps are assigned here, after the counter is taken, so that
// created order matches id order.
//
// The counter is a lock shared by all eventlog writers, so it must be the
// last lock the transaction acquires: callers must be done writing data
// rows before calling, or two writers can deadlock on data row vs counter.
func logAndNotifyAll(
	ctx context.Context, q *postgres.Queries,
	entries []postgres.InsertEventLogParams,
) error {
	if len(entries) == 0 {
		return nil
	}

	lastID, err := q.ReserveSequenceValues(ctx, postgres.ReserveSequenceValuesParams{
		Name:  sequenceEventLog,
		Count: int64(len(entries)),
	})
	if err != nil {
		return fmt.Errorf("reserve eventlog ids: %w", err)
	}

	firstID := lastID - int64(len(entries)) + 1

	for i, params := range entries {
		params.ID = firstID + int64(i)
		params.Created = pg.Time(time.Now())

		err = q.InsertEventLog(ctx, params)
		if err != nil {
			return fmt.Errorf("insert event log: %w", err)
		}

		err = notifyEventLogUpdated(ctx, q, EventLogEvent{
			ID:    params.ID,
			Owner: params.Owner,
		})
		if err != nil {
			return fmt.Errorf("send notification: %w", err)
		}
	}

	return nil
}

func notifyEventLogUpdated(
	ctx context.Context, q *postgres.Queries,
	payload EventLogEvent,
) error {
	return pgNotify(ctx, q, NotifyChannelEventLogUpdate, payload)
}

func ownerToUserKind(owner string) postgres.UserKind {
	if strings.HasPrefix(owner, "core://unit/") {
		return postgres.UserKindUnit
	}

	if strings.HasPrefix(owner, "core://org/") {
		return postgres.UserKindOrg
	}

	return postgres.UserKindUser
}
